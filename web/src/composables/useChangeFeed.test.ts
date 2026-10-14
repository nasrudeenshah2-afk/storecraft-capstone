import { afterEach, describe, expect, it, vi } from 'vitest'
import { effectScope } from 'vue'
import { createApiClient, type Change } from '../api/client'
import { errorResponse, fakeServer, stubFetch } from '../testing/fakeServer'
import { backoffDelay, useChangeFeed, type FeedTiming } from './useChangeFeed'

const fast: Partial<FeedTiming> = {
  minBackoffMs: 10,
  maxBackoffMs: 40,
  watchdogMs: 200,
  fallbackAfterFailures: 3,
  retryStreamAfterMs: 300,
}

function setup(server = fakeServer({ pollTimeoutMs: 50 }), timing: Partial<FeedTiming> = fast) {
  const api = createApiClient({ getToken: () => server.tenantToken, fetch: server.fetch })
  const received: Change[] = []
  const onUnauthorized = vi.fn()
  const scope = effectScope()
  const feed = scope.run(() => useChangeFeed({ api, timing, onUnauthorized, onChange: (c) => received.push(c) }))
  if (feed === undefined) throw new Error('scope did not run')
  return { server, feed, received, onUnauthorized, scope }
}

const scopes: Array<{ stop: () => void }> = []
afterEach(() => {
  for (const s of scopes.splice(0)) s.stop()
})

function track<T extends { scope: { stop: () => void } }>(t: T): T {
  scopes.push(t.scope)
  return t
}

describe('useChangeFeed', () => {
  it('streams changes after the given seq, in order, and reports "live"', async () => {
    const t = track(setup())
    t.server.externalPut('old', 1) // seq 1: before the start point
    t.server.externalPut('a', 1) // seq 2
    t.feed.start(1)
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
    await vi.waitFor(() => expect(t.received.map((c) => c.key)).toEqual(['a']))

    t.server.externalPut('b', 2)
    t.server.externalDelete('a')
    await vi.waitFor(() => expect(t.received.map((c) => `${c.op}:${c.key}`)).toEqual(['put:a', 'put:b', 'delete:a']))
    expect(t.feed.lastSeq.value).toBe(4)
    expect(t.feed.recent.value.map((c) => c.seq)).toEqual([2, 3, 4])

    const stream = t.server.requests.find((r) => r.url === '/v1/changes/stream')
    expect(stream?.headers.get('Last-Event-ID')).toBe('1')
    expect(stream?.headers.get('Accept')).toBe('text/event-stream')
    expect(stream?.url).not.toContain(t.server.tenantToken)
  })

  it('reconnects from the last seen id after the stream drops, without duplicates or gaps', async () => {
    const t = track(setup())
    t.feed.start(0)
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
    t.server.externalPut('a', 1)
    await vi.waitFor(() => expect(t.received).toHaveLength(1))

    t.server.dropStreams()
    t.server.externalPut('while-down', 2) // written while the client is reconnecting
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
    t.server.externalPut('after', 3)
    await vi.waitFor(() => expect(t.received.map((c) => c.key)).toEqual(['a', 'while-down', 'after']))

    const streams = t.server.requests.filter((r) => r.url === '/v1/changes/stream')
    expect(streams.length).toBeGreaterThanOrEqual(2)
    expect(streams[1]?.headers.get('Last-Event-ID')).toBe('1') // resumed after the last seen id
    expect(new Set(t.received.map((c) => c.seq)).size).toBe(t.received.length)
  })

  it('shows "reconnecting" while the stream is down and backs off between attempts', async () => {
    const server = fakeServer({ pollTimeoutMs: 50 })
    const t = track(setup(server, { ...fast, minBackoffMs: 30, maxBackoffMs: 30, fallbackAfterFailures: 10 }))
    t.feed.start(0)
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
    server.control.blockStream = true
    server.closeStreams()
    await vi.waitFor(() => expect(t.feed.status.value).toBe('reconnecting'))
    const before = server.requests.filter((r) => r.url === '/v1/changes/stream').length
    await new Promise((r) => setTimeout(r, 120))
    const attempts = server.requests.filter((r) => r.url === '/v1/changes/stream').length - before
    // With a 15-30ms backoff, 120ms allows a handful of attempts, not a busy loop.
    expect(attempts).toBeGreaterThanOrEqual(1)
    expect(attempts).toBeLessThanOrEqual(8)
    server.control.blockStream = false
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
  })

  it('falls back to long-polling after repeated stream failures, then returns to streaming', async () => {
    const t = track(setup())
    t.server.control.streamFailures = 3 // the first three stream attempts fail
    t.feed.start(0)
    await vi.waitFor(() => expect(t.feed.status.value).toBe('fallback'))

    t.server.externalPut('via-poll', 1)
    await vi.waitFor(() => expect(t.received.map((c) => c.key)).toEqual(['via-poll']))
    const poll = t.server.requests.find((r) => r.url.startsWith('/v1/changes?'))
    expect(poll?.url).toBe('/v1/changes?since=0')

    // After retryStreamAfterMs it tries streaming again (failures used up) and goes live.
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'), { timeout: 3000 })
    t.server.externalPut('via-stream', 2)
    await vi.waitFor(() => expect(t.received.map((c) => c.key)).toEqual(['via-poll', 'via-stream']))
  })

  it('reconnects when the stream goes silent (watchdog)', async () => {
    // A stream that sends headers and then nothing at all, forever.
    let streamCalls = 0
    const silent = stubFetch((req) => {
      if (req.url === '/v1/changes/stream') {
        streamCalls += 1
        return new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200 })
      }
      return errorResponse(404, 'not_found', 'x')
    })
    const api = createApiClient({ getToken: () => 't', fetch: silent.fetch })
    const scope = effectScope()
    scopes.push(scope)
    const feed = scope.run(() => useChangeFeed({ api, timing: { ...fast, watchdogMs: 60, fallbackAfterFailures: 100 }, onUnauthorized: vi.fn(), onChange: vi.fn() }))
    feed?.start(0)
    await vi.waitFor(() => expect(streamCalls).toBeGreaterThanOrEqual(3), { timeout: 3000 })
  })

  it('stops and reports an unauthorized token instead of retrying', async () => {
    const server = fakeServer()
    const api = createApiClient({ getToken: () => 'revoked', fetch: server.fetch })
    const onUnauthorized = vi.fn()
    const scope = effectScope()
    scopes.push(scope)
    const feed = scope.run(() => useChangeFeed({ api, timing: fast, onUnauthorized, onChange: vi.fn() }))
    feed?.start(0)
    await vi.waitFor(() => expect(feed?.status.value).toBe('stopped'))
    expect(onUnauthorized).toHaveBeenCalledOnce()
    await new Promise((r) => setTimeout(r, 100))
    expect(server.requests).toHaveLength(1) // no retry storm
  })

  it('cleans up when its component scope ends: the stream is closed and nothing more is requested', async () => {
    const t = setup()
    t.feed.start(0)
    await vi.waitFor(() => expect(t.feed.status.value).toBe('live'))
    expect(t.server.openStreams()).toBe(1)
    t.scope.stop()
    await vi.waitFor(() => expect(t.server.openStreams()).toBe(0))
    expect(t.feed.status.value).toBe('idle')
    const count = t.server.requests.length
    t.server.externalPut('ignored', 1)
    await new Promise((r) => setTimeout(r, 100))
    expect(t.server.requests.length).toBe(count)
    expect(t.received).toHaveLength(0)
  })
})

describe('backoffDelay', () => {
  it('doubles from the minimum, caps at the maximum, and adds jitter in [half, full]', () => {
    const timing = { minBackoffMs: 500, maxBackoffMs: 10_000 }
    expect(backoffDelay(1, timing, () => 1)).toBe(500)
    expect(backoffDelay(2, timing, () => 1)).toBe(1000)
    expect(backoffDelay(3, timing, () => 1)).toBe(2000)
    expect(backoffDelay(20, timing, () => 1)).toBe(10_000)
    expect(backoffDelay(1, timing, () => 0)).toBe(250)
  })
})
