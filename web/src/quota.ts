import type { QuotaExceededDetails } from './api/errors'

// STORE-210 display helpers: numbers, byte sizes, meter states and the
// over-quota message. Pure functions, so every text is unit-tested.

/** 1234 -> "1,234" (fixed locale, so the console reads the same everywhere). */
export function formatCount(n: number): string {
  return n.toLocaleString('en-US')
}

function oneDecimal(n: number): string {
  const r = Math.round(n * 10) / 10
  return Number.isInteger(r) ? String(r) : r.toFixed(1)
}

/** 512 -> "512 bytes", 3482 -> "3.4 KB", 1048576 -> "1 MB" (1 KB = 1024 bytes). */
export function formatBytes(n: number): string {
  if (n < 1024) {
    return n === 1 ? '1 byte' : `${formatCount(n)} bytes`
  }
  if (n < 1024 * 1024) {
    return `${oneDecimal(n / 1024)} KB`
  }
  return `${oneDecimal(n / (1024 * 1024))} MB`
}

export type MeterState = 'unlimited' | 'normal' | 'near' | 'over'

/** At or above this share of the limit a dimension is "almost full". */
export const NEAR_LIMIT = 0.8

export function meterState(used: number, limit: number | null): MeterState {
  if (limit === null) return 'unlimited'
  if (used > limit) return 'over'
  if (used >= limit * NEAR_LIMIT) return 'near'
  return 'normal'
}

/** "12 of 1,000 keys" or "3 keys (unlimited)". */
export function keysText(used: number, limit: number | null): string {
  const unit = (n: number) => (n === 1 ? 'key' : 'keys')
  if (limit === null) return `${formatCount(used)} ${unit(used)} (unlimited)`
  return `${formatCount(used)} of ${formatCount(limit)} ${unit(limit)}`
}

/** "3.4 KB of 1 MB" or "10 bytes (unlimited)". */
export function bytesText(used: number, limit: number | null): string {
  if (limit === null) return `${formatBytes(used)} (unlimited)`
  return `${formatBytes(used)} of ${formatBytes(limit)}`
}

/** What the user can do about a rejected save. */
export const OVER_QUOTA_ADVICE = 'Delete keys you no longer need, or ask an admin to raise the quota.'

/**
 * The message shown when a save is rejected with quota_exceeded. It is built
 * only from the error details (exact numbers), never from the server's text.
 */
export function overQuotaMessage(d: QuotaExceededDetails): string {
  if (d.dimension === 'keys') {
    return `Nothing was saved: this would go over your key limit (${formatCount(d.used)} of ${formatCount(d.limit)} keys are used). ${OVER_QUOTA_ADVICE}`
  }
  return `Nothing was saved: this would go over your byte limit (${formatCount(d.used)} of ${formatCount(d.limit)} bytes are used; this save needs ${formatCount(d.requested)} more). ${OVER_QUOTA_ADVICE}`
}

/**
 * Parses a quota limit field: "" means unlimited (null), otherwise a whole
 * number of 0 or more. Returns undefined when the text is not valid.
 */
export function parseLimitField(text: string): number | null | undefined {
  const t = text.trim()
  if (t === '') return null
  if (!/^\d+$/.test(t)) return undefined
  const n = Number(t)
  return Number.isSafeInteger(n) ? n : undefined
}

export const LIMIT_FIELD_ERROR = 'Enter a whole number of 0 or more, or leave empty for unlimited.'
