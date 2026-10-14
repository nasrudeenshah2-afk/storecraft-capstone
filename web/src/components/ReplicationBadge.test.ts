import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useSessionStore } from '../stores/session'
import { fakeServer } from '../testing/fakeServer'
import ReplicationBadge from './ReplicationBadge.vue'

describe('ReplicationBadge', () => {
  let server: ReturnType<typeof fakeServer>

  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
    server = fakeServer()
    vi.stubGlobal('fetch', server.fetch)
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('shows role, seqs and lag in text, and refreshes on an interval', async () => {
    useSessionStore().login(server.adminToken, 'admin')
    const wrapper = mount(ReplicationBadge, { props: { intervalMs: 5000 } })
    await flushPromises()
    expect(wrapper.text()).toContain('Primary')
    expect(wrapper.text()).toContain('applied 0')
    expect(wrapper.text()).toContain('lag 0 (in sync)')

    server.externalPut('k', 1)
    await vi.advanceTimersByTimeAsync(5000)
    await flushPromises()
    expect(wrapper.text()).toContain('applied 1')
    expect(server.requests.filter((r) => r.url === '/v1/replication/status')).toHaveLength(2)

    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(server.requests.filter((r) => r.url === '/v1/replication/status')).toHaveLength(2) // stopped
  })

  it('says when the status cannot be read', async () => {
    useSessionStore().login(server.tenantToken, 'tenant') // not allowed to read it
    const wrapper = mount(ReplicationBadge)
    await flushPromises()
    expect(wrapper.text()).toContain('unavailable')
    wrapper.unmount()
  })
})
