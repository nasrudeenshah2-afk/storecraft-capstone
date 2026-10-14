import { describe, expect, it } from 'vitest'
import { ApiError, describeError, isErrorCode, type ErrorCode } from './errors'

const allCodes: ErrorCode[] = [
  'unauthorized',
  'forbidden',
  'not_found',
  'invalid_request',
  'precondition_failed',
  'method_not_allowed',
  'idempotency_conflict',
  'read_only_replica',
  'quota_exceeded',
  'precondition_required',
  'internal',
  'network_error',
  'unknown_error',
]

describe('describeError', () => {
  it('gives every code its own non-empty message', () => {
    const seen = new Set<string>()
    for (const code of allCodes) {
      const msg = describeError(new ApiError(400, code, ''))
      expect(msg).not.toBe('')
      seen.add(msg)
    }
    expect(seen.size).toBe(allCodes.length)
  })

  it('chooses the message by code, not by the server text', () => {
    const a = describeError(new ApiError(412, 'precondition_failed', 'anything at all'))
    const b = describeError(new ApiError(412, 'precondition_failed', 'something else entirely'))
    expect(a).toBe(b)
    expect(a).toContain('changed')
  })

  it('uses screen-specific overrides for the given codes only', () => {
    const overrides = { precondition_failed: 'Already exists.' }
    expect(describeError(new ApiError(412, 'precondition_failed', ''), overrides)).toBe('Already exists.')
    expect(describeError(new ApiError(409, 'read_only_replica', ''), overrides)).toContain('read-only replica')
  })

  it("appends the server's explanation for invalid_request", () => {
    expect(describeError(new ApiError(400, 'invalid_request', 'missing "value" field'))).toContain('missing "value" field')
  })

  it('treats anything that is not an ApiError as an unexpected response', () => {
    expect(describeError(new Error('boom'))).toBe(describeError(new ApiError(0, 'unknown_error', '')))
  })
})

describe('isErrorCode', () => {
  it('recognises known codes only', () => {
    expect(isErrorCode('read_only_replica')).toBe(true)
    expect(isErrorCode('network_error')).toBe(true)
    expect(isErrorCode('teapot')).toBe(false)
  })
})
