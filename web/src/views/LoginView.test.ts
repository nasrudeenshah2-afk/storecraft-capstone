import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from '../stores/session'
import { fakeServer } from '../testing/fakeServer'
import LoginView from './LoginView.vue'

describe('LoginView', () => {
  let server: ReturnType<typeof fakeServer>

  beforeEach(() => {
    sessionStorage.clear()
    setActivePinia(createPinia())
    server = fakeServer()
    vi.stubGlobal('fetch', server.fetch)
  })

  async function signIn(token: string, role: 'tenant' | 'admin') {
    const wrapper = mount(LoginView)
    await wrapper.get('input[type="password"]').setValue(token)
    await wrapper.get(`input[type="radio"][value="${role}"]`).setValue(true)
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    return wrapper
  }

  it('signs a tenant in after the server accepts the token', async () => {
    await signIn(server.tenantToken, 'tenant')
    const session = useSessionStore()
    expect(session.isLoggedIn).toBe(true)
    expect(session.role).toBe('tenant')
    expect(server.requests[0]?.url).toBe('/v1/keys')
  })

  it('signs an admin in by checking an admin-only route', async () => {
    await signIn(server.adminToken, 'admin')
    expect(useSessionStore().role).toBe('admin')
    expect(server.requests[0]?.url).toBe('/v1/replication/status')
  })

  it('explains a token/role mix-up instead of signing in', async () => {
    let wrapper = await signIn(server.adminToken, 'tenant')
    expect(wrapper.get('[role="alert"]').text()).toContain('This is an admin token')
    expect(useSessionStore().isLoggedIn).toBe(false)

    wrapper = await signIn(server.tenantToken, 'admin')
    expect(wrapper.get('[role="alert"]').text()).toContain('This is a tenant token')
    expect(useSessionStore().isLoggedIn).toBe(false)
  })

  it('rejects an unknown token and an empty field', async () => {
    let wrapper = await signIn('nope', 'tenant')
    expect(wrapper.get('[role="alert"]').text()).toContain('did not accept')

    wrapper = await signIn('   ', 'tenant')
    expect(wrapper.text()).toContain('Paste your token.')
    expect(useSessionStore().isLoggedIn).toBe(false)
  })
})
