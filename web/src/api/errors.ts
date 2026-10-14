import type { components } from './schema'

/** Error codes the server can return, taken from api/openapi.yaml. */
export type ApiErrorCode = components['schemas']['Error']['error']['code']

/** Codes the client produces itself when there is no usable server answer. */
export type ClientErrorCode = 'network_error' | 'unknown_error'

export type ErrorCode = ApiErrorCode | ClientErrorCode

/** Extra facts on a `quota_exceeded` error (from api/openapi.yaml). */
export type QuotaExceededDetails = components['schemas']['QuotaExceededDetails']

/**
 * An API failure: the HTTP status (0 when the server was not reached), the
 * code, the message and, for `quota_exceeded`, the details.
 */
export class ApiError extends Error {
  readonly status: number
  readonly code: ErrorCode
  readonly details: QuotaExceededDetails | null

  constructor(status: number, code: ErrorCode, message: string, details: QuotaExceededDetails | null = null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
  }
}

/**
 * The message shown for each code. Typed as a full Record, so adding a code to
 * api/openapi.yaml without a message here fails `npm run typecheck`.
 */
const messages: Record<ErrorCode, string> = {
  unauthorized: 'Your token is missing or not valid. Sign in again.',
  forbidden: 'This token is not allowed to do that.',
  not_found: 'That item does not exist (it may have been deleted).',
  invalid_request: 'The server rejected the request as invalid.',
  precondition_failed: 'Someone else changed this item since you loaded it.',
  method_not_allowed: 'The server does not support this action here.',
  idempotency_conflict: 'This request was already used for a different change. Try again.',
  read_only_replica: 'This server is a read-only replica. Connect to the primary to make changes.',
  quota_exceeded: 'This change would go over the storage quota, so nothing was saved.',
  precondition_required: 'This change needs the current version. Reload and try again.',
  internal: 'The server had an unexpected problem. Try again later.',
  network_error: 'Could not reach the server. Check that it is running and try again.',
  unknown_error: 'The server sent an unexpected response.',
}

const knownCodes = new Set<string>(Object.keys(messages))

/** Reports whether s is an error code the client knows. */
export function isErrorCode(s: string): s is ErrorCode {
  return knownCodes.has(s)
}

/**
 * The text to show the user for an error. `overrides` replaces the message
 * for specific codes where the screen knows more (for example a 412 when
 * creating a key means "already exists"). For invalid_request the server's
 * own explanation is appended, because it says which input was wrong.
 */
export function describeError(err: unknown, overrides: Partial<Record<ErrorCode, string>> = {}): string {
  if (!(err instanceof ApiError)) {
    return messages.unknown_error
  }
  const base = overrides[err.code] ?? messages[err.code]
  if (err.code === 'invalid_request' && err.message !== '' && overrides.invalid_request === undefined) {
    return `${base} (${err.message})`
  }
  return base
}
