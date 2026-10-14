import { getCurrentScope, onScopeDispose, readonly, ref } from 'vue'
import type { ApiClient, Change } from '../api/client'
import { ApiError } from '../api/errors'
import { SSEParser } from '../api/sse'

/**
 * idle: not started; connecting: first stream connection; live: streaming;
 * reconnecting: stream lost, retrying; fallback: streaming keeps failing, so
 * changes come from long-polling GET /v1/changes; stopped: stopped for good
 * (for example the token was rejected).
 */
export type FeedStatus = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'fallback' | 'stopped'

export interface FeedTiming {
  /** First retry delay; doubles per failure (with jitter) up to maxBackoffMs. */
  minBackoffMs: number
  maxBackoffMs: number
  /** Reconnect if nothing (not even a heartbeat) arrives for this long. */
  watchdogMs: number
  /** Consecutive stream failures before switching to long-poll. */
  fallbackAfterFailures: number
  /** While in fallback, try the stream again after this long. */
  retryStreamAfterMs: number
}

export const defaultFeedTiming: FeedTiming = {
  minBackoffMs: 500,
  maxBackoffMs: 10_000,
  watchdogMs: 45_000, // the server sends a heartbeat every 15s by default
  fallbackAfterFailures: 3,
  retryStreamAfterMs: 60_000,
}

export interface FeedOptions {
  api: ApiClient
  /** Called for every change, in seq order, exactly once per seq. */
  onChange: (change: Change) => void
  /** Called (once) when the server rejects the token; the feed then stops. */
  onUnauthorized: () => void
  timing?: Partial<FeedTiming>
  /** How many recent changes `recent` keeps. */
  keepRecent?: number
}

/** Delay before retry number `attempt` (1-based): exponential with jitter. */
export function backoffDelay(attempt: number, timing: Pick<FeedTiming, 'minBackoffMs' | 'maxBackoffMs'>, random = Math.random): number {
  const base = Math.min(timing.maxBackoffMs, timing.minBackoffMs * 2 ** Math.max(0, attempt - 1))
  return Math.round(base / 2 + random() * (base / 2))
}

class StreamEnded extends Error {}
class StreamStalled extends Error {}
class StreamUnsupported extends Error {}

/**
 * Follows the tenant's change feed. Uses GET /v1/changes/stream with fetch and
 * a streaming reader (not EventSource: the token must go in the Authorization
 * header, never in the URL). Reconnects from the last seen id (Last-Event-ID)
 * with backoff, and falls back to long-polling GET /v1/changes if the stream
 * keeps failing. Stops automatically when the calling component unmounts.
 */
export function useChangeFeed(options: FeedOptions) {
  const timing: FeedTiming = { ...defaultFeedTiming, ...options.timing }
  const keepRecent = options.keepRecent ?? 20

  const status = ref<FeedStatus>('idle')
  /** The seq of the last change delivered (the resume point). */
  const lastSeq = ref(0)
  /** The most recent changes, newest last. */
  const recent = ref<Change[]>([])

  let controller: AbortController | null = null
  let generation = 0

  function deliver(change: Change): void {
    if (change.seq <= lastSeq.value) {
      return // already delivered (for example replayed after a reconnect)
    }
    lastSeq.value = change.seq
    recent.value = [...recent.value, change].slice(-keepRecent)
    options.onChange(change)
  }

  function sleep(ms: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve) => {
      if (signal.aborted) {
        resolve()
        return
      }
      const timer = setTimeout(done, ms)
      function done(): void {
        clearTimeout(timer)
        signal.removeEventListener('abort', done)
        resolve()
      }
      signal.addEventListener('abort', done)
    })
  }

  /** Reads one stream until it ends or fails; returns only by throwing. */
  async function readStream(signal: AbortSignal, onOpen: () => void): Promise<never> {
    const res = await options.api.openChangeStream(lastSeq.value, signal)
    const body = res.body
    if (body === null) {
      throw new StreamUnsupported('streaming responses are not supported here')
    }
    onOpen()
    const reader = body.getReader()
    const decoder = new TextDecoder()
    const parser = new SSEParser()
    try {
      for (;;) {
        let timer: ReturnType<typeof setTimeout> | undefined
        const stalled = new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new StreamStalled('no data from the stream')), timing.watchdogMs)
        })
        let chunk: ReadableStreamReadResult<Uint8Array>
        try {
          chunk = await Promise.race([reader.read(), stalled])
        } finally {
          clearTimeout(timer)
        }
        if (chunk.done) {
          throw new StreamEnded('the server closed the stream')
        }
        for (const ev of parser.push(decoder.decode(chunk.value, { stream: true }))) {
          if (ev.event !== 'change') {
            continue
          }
          const change = JSON.parse(ev.data) as Change
          deliver(change)
        }
      }
    } finally {
      reader.cancel().catch(() => undefined)
    }
  }

  async function run(gen: number, signal: AbortSignal): Promise<void> {
    let failures = 0
    let mode: 'stream' | 'fallback' = 'stream'
    let fallbackSince = 0
    let everLive = false
    const current = () => gen === generation && !signal.aborted

    while (current()) {
      if (mode === 'stream') {
        status.value = everLive || failures > 0 ? 'reconnecting' : 'connecting'
        try {
          await readStream(signal, () => {
            status.value = 'live'
            everLive = true
            failures = 0
          })
        } catch (err) {
          if (!current()) {
            return
          }
          if (err instanceof ApiError && err.code === 'unauthorized') {
            status.value = 'stopped'
            options.onUnauthorized()
            return
          }
          failures += 1
          if (err instanceof StreamUnsupported || failures >= timing.fallbackAfterFailures) {
            mode = 'fallback'
            fallbackSince = Date.now()
            status.value = 'fallback'
            continue
          }
          status.value = 'reconnecting'
          await sleep(backoffDelay(failures, timing), signal)
        }
        continue
      }

      // Fallback: long-poll /v1/changes.
      status.value = 'fallback'
      try {
        const res = await options.api.changes(lastSeq.value, { signal })
        for (const change of res.data.changes) {
          deliver(change)
        }
        failures = 0
      } catch (err) {
        if (!current()) {
          return
        }
        if (err instanceof ApiError && err.code === 'unauthorized') {
          status.value = 'stopped'
          options.onUnauthorized()
          return
        }
        failures += 1
        await sleep(backoffDelay(failures, timing), signal)
      }
      if (Date.now() - fallbackSince >= timing.retryStreamAfterMs) {
        mode = 'stream' // try streaming again
        failures = 0
      }
    }
  }

  /** Starts following changes after `since` (restarts if already running). */
  function start(since: number): void {
    stop()
    generation += 1
    lastSeq.value = since
    controller = new AbortController()
    void run(generation, controller.signal)
  }

  /** Stops following changes and cancels any open request. */
  function stop(): void {
    generation += 1
    controller?.abort()
    controller = null
    if (status.value !== 'stopped') {
      status.value = 'idle' // "stopped" (token rejected) stays visible until the next start
    }
  }

  if (getCurrentScope() !== undefined) {
    onScopeDispose(stop)
  }

  return {
    status: readonly(status),
    lastSeq: readonly(lastSeq),
    recent: readonly(recent),
    start,
    stop,
  }
}
