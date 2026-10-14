import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import type { ApiClient, Flag } from '../api/client'
import { describeError } from '../api/errors'

/** How often open consoles re-read the flags (AC-UI-6: changes show within 5 s). */
export const FLAG_POLL_MS = 3000

/**
 * The runtime flags, read from GET /v1/flags. This store is the ONE place the
 * console reads quota_enforcement from: every quota UI element asks
 * `quotaEnabled`. Until the first successful read the feature stays hidden.
 */
export const useFlagsStore = defineStore('flags', () => {
  const quotaEnforcement = ref<Flag | null>(null)
  /** The last read failure ('' after a successful read), so the UI can say so (review R5). */
  const error = ref('')
  const quotaEnabled = computed(() => quotaEnforcement.value?.enabled === true)

  let timer: ReturnType<typeof setInterval> | null = null
  let client: ApiClient | null = null

  /** Reads the flags once; a failed read keeps the last known state. */
  async function refresh(): Promise<void> {
    if (client === null) return
    try {
      const res = await client.flags()
      quotaEnforcement.value = res.data.quota_enforcement
      error.value = ''
    } catch (err) {
      // Keep the last known state; the next poll tries again.
      error.value = describeError(err)
    }
  }

  /** Starts polling with api (stops an earlier poll first). */
  function start(api: ApiClient, intervalMs = FLAG_POLL_MS): void {
    stop()
    client = api
    void refresh()
    timer = setInterval(() => void refresh(), intervalMs)
  }

  /** Stops polling and forgets the state (for example on sign-out). */
  function stop(): void {
    if (timer !== null) {
      clearInterval(timer)
      timer = null
    }
    client = null
    quotaEnforcement.value = null
    error.value = ''
  }

  /** Sets the state from a successful admin toggle, without waiting for the next poll. */
  function set(flag: Flag): void {
    quotaEnforcement.value = flag
  }

  return { quotaEnforcement, quotaEnabled, error, refresh, start, stop, set }
})
