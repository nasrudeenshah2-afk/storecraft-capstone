import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApiClient } from '../api/client'
import { errorResponse, jsonResponse, stubFetch } from '../testing/fakeServer'
import { useFlagsStore } from './flags'

describe('flags store', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    setActivePinia(createPinia())
  })
  afterEach(() => {
    useFlagsStore().stop()
    vi.useRealTimers()
  })

  it('reads the flag at once and then on every interval; hidden until the first read', async () => {
    let enabled = false
    let version = 0
    const stub = stubFetch(() => jsonResponse(200, { quota_enforcement: { enabled, version } }))
    const flags = useFlagsStore()
    expect(flags.quotaEnabled).toBe(false)
    expect(flags.quotaEnforcement).toBeNull()

    flags.start(createApiClient({ getToken: () => 't', fetch: stub.fetch }), 3000)
    await vi.advanceTimersByTimeAsync(0)
    expect(flags.quotaEnforcement).toEqual({ enabled: false, version: 0 })

    enabled = true
    version = 1
    await vi.advanceTimersByTimeAsync(3000)
    expect(flags.quotaEnabled).toBe(true)
    expect(stub.requests.map((r) => r.url)).toEqual(['/v1/flags', '/v1/flags'])
  })

  it('keeps the last known state when a read fails, and forgets it on stop', async () => {
    let fail = false
    const stub = stubFetch(() => (fail ? errorResponse(500, 'internal', 'x') : jsonResponse(200, { quota_enforcement: { enabled: true, version: 4 } })))
    const flags = useFlagsStore()
    flags.start(createApiClient({ getToken: () => 't', fetch: stub.fetch }), 1000)
    await vi.advanceTimersByTimeAsync(0)
    fail = true
    await vi.advanceTimersByTimeAsync(1000)
    expect(flags.quotaEnabled).toBe(true)

    flags.stop()
    expect(flags.quotaEnforcement).toBeNull()
    await vi.advanceTimersByTimeAsync(5000)
    expect(stub.requests).toHaveLength(2) // no polling after stop
  })

  it('takes a new state from a successful toggle without waiting for the poll', () => {
    const flags = useFlagsStore()
    flags.set({ enabled: true, version: 7 })
    expect(flags.quotaEnabled).toBe(true)
  })
})
