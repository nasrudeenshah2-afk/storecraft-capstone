import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useToastStore } from '../composables/useToast'
import { useSessionStore } from '../stores/session'
import { fakeServer } from '../testing/fakeServer'
import KeysView from './KeysView.vue'

describe('KeysView', () => {
  let server: ReturnType<typeof fakeServer>
  const mounted: VueWrapper[] = []

  beforeEach(() => {
    sessionStorage.clear()
    setActivePinia(createPinia())
    server = fakeServer()
    vi.stubGlobal('fetch', server.fetch)
    useSessionStore().login(server.tenantToken, 'tenant')
  })

  afterEach(() => {
    for (const w of mounted.splice(0)) {
      w.unmount() // stops the change feed
    }
    document.body.innerHTML = ''
  })

  async function mountView() {
    const wrapper = mount(KeysView, { attachTo: document.body })
    mounted.push(wrapper)
    await flushPromises()
    return wrapper
  }

  function rowKeys(wrapper: VueWrapper): string[] {
    return wrapper.findAll('tbody th').map((th) => th.text())
  }

  function rowText(wrapper: VueWrapper, key: string): string {
    return wrapper.findAll('tbody tr').find((tr) => tr.get('th').text() === key)?.text() ?? ''
  }

  function dialogs(wrapper: VueWrapper) {
    return wrapper.findAll('[role="dialog"]')
  }

  /** Waits until the feed shows "Live". */
  async function untilLive(wrapper: VueWrapper): Promise<void> {
    await vi.waitFor(() => expect(wrapper.get('[data-testid="feed-status"]').text()).toBe('Live'))
  }

  it('lists the keys from the server with value and version', async () => {
    server.externalPut('greeting', 'hello')
    server.externalPut('greeting', 'hello again')
    server.externalPut('count', 3)
    const wrapper = await mountView()
    expect(rowKeys(wrapper)).toEqual(['count', 'greeting'])
    expect(wrapper.text()).toContain('"hello again"')
    expect(wrapper.get('p.status').text()).toContain('2 keys')
  })

  it('edits a key with If-Match set to the version shown, then shows the new version', async () => {
    server.externalPut('greeting', 'hello')
    server.externalPut('greeting', 'hello again') // version 2
    const wrapper = await mountView()

    await wrapper.get('button[aria-label="Edit greeting"]').trigger('click')
    expect(wrapper.find('[role="dialog"]').text()).toContain('Editing version 2')
    const textarea = wrapper.get('[role="dialog"] textarea')
    expect((textarea.element as HTMLTextAreaElement).value).toBe('"hello again"')

    await textarea.setValue('"edited"')
    await wrapper.get('#key-editor').trigger('submit')
    await flushPromises()

    const put = server.requests.find((q) => q.method === 'PUT')
    expect(put?.headers.get('If-Match')).toBe('"2"')
    expect(wrapper.find('[role="dialog"]').exists()).toBe(false)
    expect(rowText(wrapper, 'greeting')).toContain('"edited"')
    expect(rowText(wrapper, 'greeting')).toContain('3')
    expect(useToastStore().toasts.map((t) => t.message)).toContain('Saved "greeting".')
  })

  it('creates a key and shows a field error for invalid JSON without sending it', async () => {
    const wrapper = await mountView()
    await wrapper.get('button.btn-primary').trigger('click') // "New key"
    const inputs = wrapper.findAll('[role="dialog"] input')
    await inputs[0]?.setValue('settings/theme')
    await wrapper.get('[role="dialog"] textarea').setValue('dark')
    await wrapper.get('#key-editor').trigger('submit')
    await flushPromises()
    expect(wrapper.get('[role="dialog"] textarea').attributes('aria-invalid')).toBe('true')
    expect(server.requests.some((q) => q.method === 'PUT')).toBe(false)

    await wrapper.get('[role="dialog"] textarea').setValue('"dark"')
    await wrapper.get('#key-editor').trigger('submit')
    await flushPromises()
    expect(server.requests.find((q) => q.method === 'PUT')?.headers.get('If-None-Match')).toBe('*')
    expect(rowKeys(wrapper)).toEqual(['settings/theme'])
  })

  it('deletes after confirmation', async () => {
    server.externalPut('doomed', 1)
    server.externalPut('kept', 2)
    const wrapper = await mountView()
    await wrapper.get('button[aria-label="Delete doomed"]').trigger('click')
    expect(wrapper.get('[role="dialog"]').text()).toContain('Delete doomed?')
    expect(server.requests.some((q) => q.method === 'DELETE')).toBe(false) // nothing until confirmed

    const confirm = wrapper.findAll('[role="dialog"] button').find((b) => b.text() === 'Delete')
    await confirm?.trigger('click')
    await flushPromises()
    expect(server.requests.find((q) => q.method === 'DELETE')?.url).toBe('/v1/keys/doomed')
    expect(rowKeys(wrapper)).toEqual(['kept'])
  })

  it('signs out when the token is rejected', async () => {
    useSessionStore().login('stale-token', 'tenant')
    await mountView()
    expect(useSessionStore().isLoggedIn).toBe(false)
  })

  describe('live updates', () => {
    it('starts the feed from the list seq and shows "Live"', async () => {
      server.externalPut('a', 1)
      server.externalPut('b', 2)
      const wrapper = await mountView()
      await untilLive(wrapper)
      const stream = server.requests.find((q) => q.url === '/v1/changes/stream')
      expect(stream?.headers.get('Last-Event-ID')).toBe('2') // the list was read at seq 2
      expect(stream?.headers.get('Authorization')).toBe(`Bearer ${server.tenantToken}`)
      expect(stream?.url).not.toContain(server.tenantToken)
    })

    it('applies puts, overwrites and deletes made by another client', async () => {
      server.externalPut('stays', 'x')
      const wrapper = await mountView()
      await untilLive(wrapper)

      server.externalPut('new-key', 'hello')
      await vi.waitFor(() => expect(rowKeys(wrapper)).toEqual(['new-key', 'stays']))

      server.externalPut('new-key', 'overwritten')
      await vi.waitFor(() => expect(rowText(wrapper, 'new-key')).toContain('"overwritten"'))
      expect(rowText(wrapper, 'new-key')).toContain('2')

      server.externalDelete('stays')
      await vi.waitFor(() => expect(rowKeys(wrapper)).toEqual(['new-key']))
    })

    it('tells the editor when the key changes underneath it', async () => {
      server.externalPut('k', 'v1')
      const wrapper = await mountView()
      await untilLive(wrapper)
      await wrapper.get('button[aria-label="Edit k"]').trigger('click')
      server.externalPut('k', 'v2')
      await vi.waitFor(() => expect(wrapper.get('.live-notice').text()).toContain('now version 2'))
    })
  })

  describe('conflict dialog (412)', () => {
    async function editThenConflict(theirs: 'changed' | 'deleted') {
      server.externalPut('greeting', 'hello')
      const wrapper = await mountView()
      await wrapper.get('button[aria-label="Edit greeting"]').trigger('click')
      await wrapper.get('[role="dialog"] textarea').setValue('"my careful edit"')
      if (theirs === 'changed') {
        server.externalPut('greeting', 'someone else') // the key moves on underneath
      } else {
        server.externalDelete('greeting')
      }
      await wrapper.get('#key-editor').trigger('submit')
      await flushPromises()
      return wrapper
    }

    function conflict(wrapper: VueWrapper) {
      const d = dialogs(wrapper).find((x) => x.text().includes('Conflict'))
      if (d === undefined) throw new Error('no conflict dialog')
      return d
    }

    function button(wrapper: VueWrapper, label: string) {
      const b = conflict(wrapper).findAll('button').find((x) => x.text() === label)
      if (b === undefined) throw new Error(`no button ${label}`)
      return b
    }

    it('shows your value and the latest value, and keeps your text', async () => {
      const wrapper = await editThenConflict('changed')
      const d = conflict(wrapper)
      expect(d.text()).toContain('Your value')
      expect(d.text()).toContain('"my careful edit"')
      expect(d.text()).toContain('Latest value (version 2)')
      expect(d.text()).toContain('"someone else"')
      expect(server.data.get('greeting')?.value).toBe('someone else')
    })

    it('Overwrite re-sends your value with the latest version', async () => {
      const wrapper = await editThenConflict('changed')
      await button(wrapper, 'Overwrite with my value').trigger('click')
      await flushPromises()
      const puts = server.requests.filter((q) => q.method === 'PUT')
      expect(puts.map((q) => q.headers.get('If-Match'))).toEqual(['"1"', '"2"'])
      expect(server.data.get('greeting')).toEqual({ value: 'my careful edit', version: 3 })
      expect(dialogs(wrapper)).toHaveLength(0)
    })

    it('Overwrite that races again shows the newest value instead of failing silently', async () => {
      const wrapper = await editThenConflict('changed')
      server.externalPut('greeting', 'third writer')
      await button(wrapper, 'Overwrite with my value').trigger('click')
      await flushPromises()
      expect(conflict(wrapper).text()).toContain('"third writer"')
      expect(conflict(wrapper).get('[role="alert"]').text()).toContain('changed again')
    })

    it('Reload puts the latest value in the editor and edits that version', async () => {
      const wrapper = await editThenConflict('changed')
      await button(wrapper, 'Reload latest').trigger('click')
      await flushPromises()
      const editor = dialogs(wrapper)
      expect(editor).toHaveLength(1)
      expect(editor[0]?.text()).toContain('Editing version 2')
      expect((editor[0]?.get('textarea').element as HTMLTextAreaElement).value).toBe('"someone else"')
    })

    it('Cancel goes back to the editor with your text', async () => {
      const wrapper = await editThenConflict('changed')
      await button(wrapper, 'Cancel').trigger('click')
      await flushPromises()
      const editor = dialogs(wrapper)
      expect(editor).toHaveLength(1)
      expect((editor[0]?.get('textarea').element as HTMLTextAreaElement).value).toBe('"my careful edit"')
    })

    it('a key deleted meanwhile can be re-created with your value', async () => {
      const wrapper = await editThenConflict('deleted')
      expect(conflict(wrapper).text()).toContain('deleted this key')
      await button(wrapper, 'Re-create with my value').trigger('click')
      await flushPromises()
      const last = server.requests.filter((q) => q.method === 'PUT').at(-1)
      expect(last?.headers.get('If-None-Match')).toBe('*')
      expect(server.data.get('greeting')).toEqual({ value: 'my careful edit', version: 1 })
    })
  })

  describe('over quota at save (AC-UI-3)', () => {
    /** Answers PUTs of new keys with the given error envelope; everything else goes to the fake server. */
    function rejectPuts(error: Record<string, unknown>) {
      const inner = server.fetch
      vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
        if (init?.method === 'PUT') {
          return new Response(JSON.stringify({ error }), { status: 403, headers: { 'Content-Type': 'application/json' } })
        }
        return inner(input, init)
      })
    }

    async function createAndSave(wrapper: VueWrapper): Promise<void> {
      await wrapper.get('button.btn-primary').trigger('click') // "New key"
      await wrapper.findAll('[role="dialog"] input')[0]?.setValue('new/key')
      await wrapper.get('[role="dialog"] textarea').setValue('{"keep": "my text"}')
      await wrapper.get('#key-editor').trigger('submit')
      await flushPromises()
    }

    it('keeps the editor open with my input and explains the limit from the details, not the server text', async () => {
      rejectPuts({ code: 'quota_exceeded', message: 'SERVER TEXT', details: { dimension: 'keys', used: 1000, limit: 1000, requested: 1 } })
      const wrapper = await mountView()
      await createAndSave(wrapper)
      const editor = dialogs(wrapper)[0]
      const alert = editor?.get('[role="alert"]').text() ?? ''
      expect(alert).toContain('key limit (1,000 of 1,000 keys are used)')
      expect(alert).toContain('Delete keys you no longer need, or ask an admin to raise the quota.')
      expect(alert).not.toContain('SERVER TEXT')
      expect((editor?.get('textarea').element as HTMLTextAreaElement).value).toBe('{"keep": "my text"}')
    })

    it('a forbidden 403 reads differently', async () => {
      rejectPuts({ code: 'forbidden', message: 'SERVER TEXT' })
      const wrapper = await mountView()
      await createAndSave(wrapper)
      const alert = dialogs(wrapper)[0]?.get('[role="alert"]').text() ?? ''
      expect(alert).toBe('This token is not allowed to do that.')
    })
  })
})
