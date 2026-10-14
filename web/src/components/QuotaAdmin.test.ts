import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { clearRecordedEvents, recordedEvents } from '../events'
import { useFlagsStore } from '../stores/flags'
import { useSessionStore } from '../stores/session'
import { errorResponse, jsonResponse, stubFetch, type RecordedRequest } from '../testing/fakeServer'
import QuotaEnforcementPanel from './QuotaEnforcementPanel.vue'
import TenantQuotaEditor from './TenantQuotaEditor.vue'

// Unit tests for the admin quota UI (AC-UI-4, AC-UI-5, AC-UI-7); the same
// behaviour runs against the real server in e2e/quota.spec.ts.

const mounted: VueWrapper[] = []

function use(handler: (req: RecordedRequest) => Response) {
  const stub = stubFetch(handler)
  vi.stubGlobal('fetch', stub.fetch)
  return stub
}

beforeEach(() => {
  sessionStorage.clear()
  setActivePinia(createPinia())
  useSessionStore().login('admin-token', 'admin')
})

afterEach(() => {
  for (const w of mounted.splice(0)) w.unmount()
  document.body.innerHTML = ''
  clearRecordedEvents()
  vi.unstubAllGlobals()
})

function dialog(): HTMLElement | null {
  return document.body.querySelector('[role="dialog"]')
}

function button(root: ParentNode, name: string): HTMLButtonElement {
  const b = Array.from(root.querySelectorAll('button')).find((el) => el.textContent?.trim() === name)
  if (b === undefined) throw new Error(`no button "${name}"`)
  return b
}

describe('QuotaEnforcementPanel (AC-UI-7)', () => {
  it('asks for confirmation naming the effect and sends If-Match with the version shown', async () => {
    const stub = use(() => jsonResponse(200, { enabled: true, version: 5 }, { ETag: '"5"' }))
    useFlagsStore().set({ enabled: false, version: 4 })
    const w = mount(QuotaEnforcementPanel, { attachTo: document.body })
    mounted.push(w)
    expect(w.text()).toContain('Quota enforcement is off')

    button(w.element, 'Turn on').click()
    await flushPromises()
    expect(dialog()?.textContent).toContain('Writes over quota will be rejected')
    useFlagsStore().set({ enabled: false, version: 9 }) // a poll while the dialog is open...
    button(dialog() as HTMLElement, 'Turn on').click()
    await flushPromises()

    expect(stub.requests[0]?.headers.get('If-Match')).toBe('"4"') // ...does not change what was confirmed
    expect(stub.requests[0]?.body).toEqual({ enabled: true })
    expect(w.text()).toContain('Quota enforcement is on')
    expect(recordedEvents().map((e) => [e.name, e.props])).toEqual([['quota_flag_toggled', { enabled: true }]])
  })

  it('a 412 asks to reload the current state', async () => {
    use((req) => (req.method === 'PUT' ? errorResponse(412, 'precondition_failed', 'stale') : jsonResponse(200, { quota_enforcement: { enabled: true, version: 6 } })))
    const flags = useFlagsStore()
    flags.set({ enabled: false, version: 4 })
    const w = mount(QuotaEnforcementPanel, { attachTo: document.body })
    mounted.push(w)
    button(w.element, 'Turn on').click()
    await flushPromises()
    button(dialog() as HTMLElement, 'Turn on').click()
    await flushPromises()
    expect(w.get('[role="alert"]').text()).toBe('Someone else changed this setting. Reload to see the current state. Reload')
  })
})

describe('TenantQuotaEditor (AC-UI-4, AC-UI-5)', () => {
  const tenants = { tenants: [{ id: 3, name: 'demo-3', quota: { max_keys: 1000, max_bytes: 1048576, version: 2 }, usage: { keys: 12, bytes: 3482 } }] }
  let current = { max_keys: 1000 as number | null, max_bytes: 1048576 as number | null, version: 2 }

  function server(onPut: (req: RecordedRequest) => Response) {
    return use((req) => {
      if (req.url === '/v1/admin/tenants') return jsonResponse(200, tenants)
      if (req.method === 'GET' && req.url === '/v1/admin/tenants/3/quota') return jsonResponse(200, current, { ETag: `"${current.version}"` })
      return onPut(req)
    })
  }

  async function open() {
    const w = mount(TenantQuotaEditor, { attachTo: document.body })
    mounted.push(w)
    await flushPromises()
    await w.get('select').setValue('3')
    await flushPromises()
    return w
  }

  function field(w: VueWrapper, label: string) {
    const l = w.findAll('label').find((x) => x.text().startsWith(label))
    return w.get(`#${l?.attributes('for') ?? 'missing'}`)
  }

  beforeEach(() => {
    current = { max_keys: 1000, max_bytes: 1048576, version: 2 }
  })

  it('shows quota, usage and version; invalid input is caught and nothing is sent', async () => {
    const stub = server(() => jsonResponse(500, {}))
    const w = await open()
    expect(w.text()).toContain('Usage: 12 keys, 3.4 KB.')
    expect(w.text()).toContain('Version 2')
    expect((field(w, 'Max keys').element as HTMLInputElement).value).toBe('1000')
    for (const bad of ['-1', '1.5', 'ten']) {
      await field(w, 'Max keys').setValue(bad)
      await w.get('form').trigger('submit')
      expect(w.text()).toContain('Enter a whole number of 0 or more, or leave empty for unlimited.')
    }
    expect(stub.requests.filter((r) => r.method === 'PUT')).toHaveLength(0)
  })

  it('saves with If-Match; empty means unlimited', async () => {
    const stub = server((req) => {
      current = { ...(req.body as { max_keys: number | null; max_bytes: number | null }), version: 3 }
      return jsonResponse(200, current, { ETag: '"3"' })
    })
    const w = await open()
    await field(w, 'Max keys').setValue('250')
    await field(w, 'Max bytes').setValue('')
    await w.get('form').trigger('submit')
    await flushPromises()
    const put = stub.requests.find((r) => r.method === 'PUT')
    expect(put?.headers.get('If-Match')).toBe('"2"')
    expect(put?.body).toEqual({ max_keys: 250, max_bytes: null })
    expect(w.text()).toContain('Version 3')
    expect(w.get('[aria-live]').text()).toBe('Quota saved (version 3).')
    expect(recordedEvents().map((e) => e.name)).toEqual(['quota_editor_saved'])
  })

  it('a 412 opens the conflict dialog with both sides; Overwrite uses the latest version', async () => {
    const versions: string[] = []
    server((req) => {
      versions.push(req.headers.get('If-Match') ?? '')
      if (req.headers.get('If-Match') === '"2"') return errorResponse(412, 'precondition_failed', 'stale')
      current = { max_keys: 111, max_bytes: 1048576, version: 4 }
      return jsonResponse(200, current, { ETag: '"4"' })
    })
    const w = await open()
    await field(w, 'Max keys').setValue('111')
    current = { max_keys: 222, max_bytes: 2222, version: 3 } // another admin saved meanwhile
    await w.get('form').trigger('submit')
    await flushPromises()

    const d = dialog() as HTMLElement
    expect(d.textContent).toContain('Your values')
    expect(d.textContent).toContain('Latest values (version 3)')
    expect(d.textContent).toContain('111')
    expect(d.textContent).toContain('222')
    expect(recordedEvents().map((e) => e.name)).toEqual(['quota_editor_conflict'])

    button(d, 'Overwrite with my values').click()
    await flushPromises()
    expect(versions).toEqual(['"2"', '"3"'])
    expect(dialog()).toBeNull()
    expect(w.text()).toContain('Version 4')
  })
})
