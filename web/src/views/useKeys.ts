import { ref } from 'vue'
import { newIdempotencyKey, type ApiClient, type Change, type Entry } from '../api/client'
import { ApiError, describeError, type ErrorCode, type QuotaExceededDetails } from '../api/errors'

/**
 * The outcome of an action. Validation failures say which form field they
 * belong to; API failures carry the error code (and, for quota_exceeded, the
 * error details).
 */
export type Result =
  | { ok: true }
  | { ok: false; code: 'validation'; field: 'key' | 'value'; message: string }
  | { ok: false; code: ErrorCode; message: string; details?: QuotaExceededDetails | null }

/**
 * Checks a key name before it is sent. Keys may contain "/", but empty, "."
 * and ".." path segments would be rewritten by the server's router.
 */
export function validateKeyName(key: string): string | null {
  if (key === '') {
    return 'Enter a key name.'
  }
  const segments = key.split('/')
  if (segments.some((s) => s === '' || s === '.' || s === '..')) {
    return 'A key cannot start or end with "/", contain "//", or have "." or ".." between slashes.'
  }
  return null
}

/** Parses the value field: any JSON value is allowed. */
export function parseJsonValue(text: string): { ok: true; value: unknown } | { ok: false; message: string } {
  if (text.trim() === '') {
    return { ok: false, message: 'Enter a JSON value, for example "hello", 42 or {"a": 1}.' }
  }
  try {
    return { ok: true, value: JSON.parse(text) as unknown }
  } catch {
    return { ok: false, message: 'This is not valid JSON. Text needs double quotes, for example "hello".' }
  }
}

/** Compact one-line JSON for the table, shortened if long. */
export function previewValue(value: unknown, max = 80): string {
  const s = JSON.stringify(value) ?? 'null'
  return s.length > max ? s.slice(0, max - 1) + '…' : s
}

/** Pretty JSON for the editor. */
export function editableValue(value: unknown): string {
  return JSON.stringify(value, null, 2) ?? 'null'
}

/**
 * Key management for one tenant: listing (with a prefix filter), create,
 * edit and delete. Every failure becomes a user-facing message chosen by the
 * error code. onUnauthorized is called when the token stops being accepted.
 */
export function useKeys(api: ApiClient, onUnauthorized: () => void) {
  const items = ref<Entry[]>([])
  const prefix = ref('')
  /** The prefix the shown list was loaded with. */
  const appliedPrefix = ref('')
  const loading = ref(false)
  const loadError = ref('')
  /** The change-log seq the shown list was read at (0 before the first load). */
  const listSeq = ref(0)

  // Live changes are ordered by seq, not by version: a key's version restarts
  // at 1 when it is deleted and created again, while seq only ever grows.
  /** Highest seq applied per key, so an older change can never undo a newer one. */
  const keySeq = new Map<string, number>()
  /** Changes delivered by the feed and not yet covered by a list snapshot. */
  let pendingChanges: Change[] = []

  function sortByKey(list: Entry[]): Entry[] {
    return list.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0))
  }

  /** Applies one change to the shown list if it is newer than what is shown. */
  function applyToList(change: Change): void {
    if (change.seq <= listSeq.value || change.seq <= (keySeq.get(change.key) ?? 0)) {
      return // already reflected in the list, or older than what was applied
    }
    keySeq.set(change.key, change.seq)
    if (!change.key.startsWith(appliedPrefix.value)) {
      return // outside the current filter
    }
    const rest = items.value.filter((e) => e.key !== change.key)
    if (change.op === 'put') {
      rest.push({ key: change.key, value: change.value, version: change.version })
    }
    items.value = sortByKey(rest)
  }

  /** Feed callback: apply a live change from another client (or this one). */
  function applyChange(change: Change): void {
    pendingChanges.push(change)
    if (pendingChanges.length > 1000) {
      pendingChanges = pendingChanges.slice(-1000)
    }
    applyToList(change)
  }

  function failure(err: unknown, overrides: Partial<Record<ErrorCode, string>> = {}): Result {
    const code: ErrorCode = err instanceof ApiError ? err.code : 'unknown_error'
    if (code === 'unauthorized') {
      onUnauthorized()
    }
    return { ok: false, code, message: describeError(err, overrides), details: err instanceof ApiError ? err.details : null }
  }

  async function load(): Promise<void> {
    loading.value = true
    loadError.value = ''
    const wanted = prefix.value
    try {
      const res = await api.listKeys(wanted)
      items.value = res.data.keys
      appliedPrefix.value = wanted
      listSeq.value = res.data.seq
      keySeq.clear()
      // Changes that arrived while this list was loading but are newer than
      // its snapshot would otherwise be lost: apply them on top.
      pendingChanges = pendingChanges.filter((c) => c.seq > res.data.seq)
      for (const c of pendingChanges) {
        applyToList(c)
      }
    } catch (err) {
      const r = failure(err, { forbidden: 'This token cannot list keys. Sign in with a tenant token.' })
      loadError.value = r.ok ? '' : r.message
    } finally {
      loading.value = false
    }
  }

  async function clearFilter(): Promise<void> {
    prefix.value = ''
    await load()
  }

  /** Creates a key; fails (instead of overwriting) if it already exists. */
  async function create(key: string, valueText: string): Promise<Result> {
    const keyError = validateKeyName(key)
    if (keyError !== null) {
      return { ok: false, code: 'validation', field: 'key', message: keyError }
    }
    const parsed = parseJsonValue(valueText)
    if (!parsed.ok) {
      return { ok: false, code: 'validation', field: 'value', message: parsed.message }
    }
    try {
      await api.putKey(key, parsed.value, { ifNoneMatch: '*', idempotencyKey: newIdempotencyKey() })
    } catch (err) {
      return failure(err, { precondition_failed: `A key named "${key}" already exists. Edit it instead.` })
    }
    await load()
    return { ok: true }
  }

  /** Saves a new value for entry, only if the key is still at the version that was shown. */
  async function save(entry: Entry, valueText: string): Promise<Result> {
    const parsed = parseJsonValue(valueText)
    if (!parsed.ok) {
      return { ok: false, code: 'validation', field: 'value', message: parsed.message }
    }
    try {
      await api.putKey(entry.key, parsed.value, { ifMatch: entry.version, idempotencyKey: newIdempotencyKey() })
    } catch (err) {
      return failure(err, {
        precondition_failed: `Someone else changed "${entry.key}" after you opened it (you were editing version ${entry.version}). Your text is kept: copy it, then cancel and reopen the key to see the latest value.`,
        not_found: `"${entry.key}" was deleted by someone else.`,
      })
    }
    await load()
    return { ok: true }
  }

  async function remove(key: string): Promise<Result> {
    try {
      await api.deleteKey(key, { idempotencyKey: newIdempotencyKey() })
    } catch (err) {
      return failure(err, { not_found: `"${key}" was already deleted.` })
    }
    await load()
    return { ok: true }
  }

  /**
   * The current state of a key, for the conflict dialog: the entry, or null
   * if it no longer exists.
   */
  async function latest(key: string): Promise<{ ok: true; entry: Entry | null } | { ok: false; message: string }> {
    try {
      const res = await api.getKey(key)
      return { ok: true, entry: res.data }
    } catch (err) {
      if (err instanceof ApiError && err.code === 'not_found') {
        return { ok: true, entry: null }
      }
      const r = failure(err)
      return { ok: false, message: r.ok ? '' : r.message }
    }
  }

  return {
    items,
    prefix,
    appliedPrefix,
    loading,
    loadError,
    listSeq,
    load,
    clearFilter,
    create,
    save,
    remove,
    latest,
    applyChange,
  }
}
