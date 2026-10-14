import { onBeforeUnmount, ref } from 'vue'
import type { ApiClient, Usage } from '../api/client'
import { ApiError, describeError } from '../api/errors'

/** Background refresh, in case a change event was missed (for example while reconnecting). */
export const USAGE_POLL_MS = 10_000
/** Change events within this window cause one refresh, not one per event. */
export const USAGE_DEBOUNCE_MS = 250

export type UsageStatus = 'loading' | 'error' | 'ready'

/**
 * The signed-in tenant's usage (GET /v1/usage), kept fresh: call changed()
 * on every change event (debounced refresh) and start() to poll in the
 * background. The newest successful answer wins (answers carry the seq).
 */
export function useUsage(api: ApiClient, onUnauthorized: () => void = () => undefined) {
  const usage = ref<Usage | null>(null)
  const status = ref<UsageStatus>('loading')
  const error = ref('')

  let debounce: ReturnType<typeof setTimeout> | null = null
  let poll: ReturnType<typeof setInterval> | null = null
  let latestRequest = 0

  async function load(): Promise<void> {
    const request = ++latestRequest
    if (usage.value === null) {
      status.value = 'loading'
    }
    try {
      const res = await api.usage()
      if (request !== latestRequest) return // a newer request was started
      if (usage.value === null || res.data.seq >= usage.value.seq) {
        usage.value = res.data
      }
      status.value = 'ready'
      error.value = ''
    } catch (err) {
      if (request !== latestRequest) return
      if (err instanceof ApiError && err.code === 'unauthorized') {
        onUnauthorized()
      }
      if (usage.value === null) {
        status.value = 'error'
        error.value = describeError(err)
      }
      // With numbers already shown, keep them; the next refresh tries again.
    }
  }

  /** Something changed: refresh soon. */
  function changed(): void {
    if (debounce !== null) clearTimeout(debounce)
    debounce = setTimeout(() => {
      debounce = null
      void load()
    }, USAGE_DEBOUNCE_MS)
  }

  /** Loads now and refreshes every intervalMs until stop(). */
  function start(intervalMs = USAGE_POLL_MS): void {
    stop()
    void load()
    poll = setInterval(() => void load(), intervalMs)
  }

  function stop(): void {
    if (poll !== null) clearInterval(poll)
    if (debounce !== null) clearTimeout(debounce)
    poll = debounce = null
  }

  onBeforeUnmount(stop)

  return { usage, status, error, load, changed, start, stop }
}
