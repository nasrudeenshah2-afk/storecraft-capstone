import { mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { createApiClient } from '../api/client'
import { errorResponse, jsonResponse, stubFetch } from '../testing/fakeServer'
import { useUsage, USAGE_DEBOUNCE_MS } from './useUsage'

/** Runs useUsage inside a component (it stops itself on unmount). */
function withUsage(handler: Parameters<typeof stubFetch>[0], onUnauthorized = vi.fn()) {
  const stub = stubFetch(handler)
  let state!: ReturnType<typeof useUsage>
  const w = mount(
    defineComponent({
      setup() {
        state = useUsage(createApiClient({ getToken: () => 't', fetch: stub.fetch }), onUnauthorized)
        return () => h('div')
      },
    }),
  )
  return { state, stub, w, onUnauthorized }
}

const body = (keys: number, seq: number) => ({ tenant_id: 1, keys: { used: keys, limit: 10 }, bytes: { used: 1, limit: null }, seq })

describe('useUsage', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('refreshes once for a burst of change events (debounced)', async () => {
    let keys = 1
    const { state, stub, w } = withUsage(() => jsonResponse(200, body(keys, keys)))
    await state.load()
    keys = 4
    state.changed()
    state.changed()
    state.changed()
    await vi.advanceTimersByTimeAsync(USAGE_DEBOUNCE_MS)
    expect(stub.requests).toHaveLength(2)
    expect(state.usage.value?.keys.used).toBe(4)
    w.unmount()
  })

  it('polls in the background until stopped (also by unmounting)', async () => {
    const { state, stub, w } = withUsage(() => jsonResponse(200, body(1, 1)))
    state.start(1000)
    await vi.advanceTimersByTimeAsync(3000)
    expect(stub.requests).toHaveLength(4)
    w.unmount()
    await vi.advanceTimersByTimeAsync(5000)
    expect(stub.requests).toHaveLength(4)
  })

  it('never goes back to an older answer', async () => {
    let seq = 5
    const { state } = withUsage(() => jsonResponse(200, body(seq, seq)))
    await state.load()
    seq = 3
    await state.load()
    expect(state.usage.value?.seq).toBe(5)
  })

  it('shows an error only when there is nothing to show yet; 401 signs out', async () => {
    let fail = true
    const { state, onUnauthorized } = withUsage(() => (fail ? errorResponse(401, 'unauthorized', 'x') : jsonResponse(200, body(2, 2))))
    await state.load()
    expect(state.status.value).toBe('error')
    expect(onUnauthorized).toHaveBeenCalledOnce()
    fail = false
    await state.load()
    expect(state.status.value).toBe('ready')
    fail = true
    await state.load()
    expect(state.status.value).toBe('ready') // keeps the numbers it has
  })
})
