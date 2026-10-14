import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApiClient } from '../api/client'
import { useFlagsStore } from '../stores/flags'
import { useSessionStore } from '../stores/session'
import { errorResponse, jsonResponse, stubFetch, type RecordedRequest } from '../testing/fakeServer'
import QuotaEnforcementPanel from './QuotaEnforcementPanel.vue'
import TenantQuotaEditor from './TenantQuotaEditor.vue'

// Regression tests for the console findings of the independent review (B13
// triage, docs/capstone/review-triage.md): R1, R4, R5, R7.

const mounted: VueWrapper[] = []

beforeEach(() => {
  sessionStorage.clear()
  setActivePinia(createPinia())
  useSessionStore().login('admin-token', 'admin')
})

afterEach(() => {
  for (const w of mounted.splice(0)) w.unmount()
  document.body.innerHTML = ''
  vi.unstubAllGlobals()
})

const tenants = {
  tenants: [1, 2].map((id) => ({ id, name: `t${id}`, quota: { max_keys: 1000, max_bytes: 1048576, version: 1 }, usage: { keys: 0, bytes: 0 } })),
}

function field(w: VueWrapper, label: string) {
  const l = w.findAll('label').find((x) => x.text().startsWith(label))
  return w.get(`#${l?.attributes('for') ?? 'missing'}`)
}

describe('TenantQuotaEditor', () => {
  it('R1: a late answer for the previously chosen tenant never fills the form of the current one', async () => {
    const pending = new Map<string, (r: Response) => void>()
    const stub = stubFetch((req: RecordedRequest) => {
      if (req.url === '/v1/admin/tenants') return jsonResponse(200, tenants)
      return new Promise<Response>((resolve) => pending.set(req.url, resolve))
    })
    vi.stubGlobal('fetch', stub.fetch)
    const w = mount(TenantQuotaEditor, { attachTo: document.body })
    mounted.push(w)
    await flushPromises()
    await w.get('select').setValue('1')
    await w.get('select').setValue('2')
    pending.get('/v1/admin/tenants/2/quota')?.(jsonResponse(200, { max_keys: 5, max_bytes: null, version: 1 }))
    await flushPromises()
    pending.get('/v1/admin/tenants/1/quota')?.(jsonResponse(200, { max_keys: 999, max_bytes: null, version: 1 })) // late
    await flushPromises()
    expect((field(w, 'Max keys').element as HTMLInputElement).value).toBe('5')
  })

  it('R4: if reading the latest values fails inside the conflict dialog, the dialog says so', async () => {
    let gets = 0
    const stub = stubFetch((req: RecordedRequest) => {
      if (req.url === '/v1/admin/tenants') return jsonResponse(200, tenants)
      if (req.method === 'GET') {
        gets++
        if (gets >= 3) throw new TypeError('Failed to fetch') // the third read fails
        return jsonResponse(200, { max_keys: 1000, max_bytes: 1048576, version: gets })
      }
      return errorResponse(412, 'precondition_failed', 'stale') // every save is stale
    })
    vi.stubGlobal('fetch', stub.fetch)
    const w = mount(TenantQuotaEditor, { attachTo: document.body })
    mounted.push(w)
    await flushPromises()
    await w.get('select').setValue('1')
    await flushPromises()
    await w.get('form').trigger('submit') // 412 -> conflict dialog (second GET)
    await flushPromises()
    const overwrite = Array.from(document.body.querySelectorAll('button')).find((b) => b.textContent?.includes('Overwrite'))
    overwrite?.click() // 412 again -> third GET fails
    await flushPromises()
    const alert = document.body.querySelector('[role="dialog"] [role="alert"]')
    expect(alert?.textContent ?? '').toContain('Could not reach the server')
  })

  it('R7: typing does not show a field error before Save', async () => {
    const stub = stubFetch((req: RecordedRequest) =>
      req.url === '/v1/admin/tenants' ? jsonResponse(200, tenants) : jsonResponse(200, { max_keys: 1000, max_bytes: 1048576, version: 1 }),
    )
    vi.stubGlobal('fetch', stub.fetch)
    const w = mount(TenantQuotaEditor, { attachTo: document.body })
    mounted.push(w)
    await flushPromises()
    await w.get('select').setValue('1')
    await flushPromises()
    await field(w, 'Max keys').setValue('-1')
    await flushPromises()
    expect(w.text()).not.toContain('Enter a whole number')
    await w.get('form').trigger('submit')
    expect(w.text()).toContain('Enter a whole number')
  })
})

describe('QuotaEnforcementPanel', () => {
  it('R5: when the setting cannot be read, it says so and offers Try again', async () => {
    let fail = true
    const stub = stubFetch(() => (fail ? errorResponse(500, 'internal', 'x') : jsonResponse(200, { quota_enforcement: { enabled: false, version: 2 } })))
    useFlagsStore().start(createApiClient({ getToken: () => 'admin-token', fetch: stub.fetch }), 60_000)
    await flushPromises()
    const w = mount(QuotaEnforcementPanel, { attachTo: document.body })
    mounted.push(w)
    await flushPromises()
    const alert = w.find('[role="alert"]')
    expect(alert.exists() && alert.text().includes('Could not read the current setting')).toBe(true)
    fail = false
    const retry = w.findAll('button').find((b) => b.text() === 'Try again')
    await retry?.trigger('click')
    await flushPromises()
    expect(w.text()).toContain('Quota enforcement is off')
    useFlagsStore().stop()
  })
})
