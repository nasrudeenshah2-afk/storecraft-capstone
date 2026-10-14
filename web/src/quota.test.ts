import { describe, expect, it } from 'vitest'
import {
  LIMIT_FIELD_ERROR,
  OVER_QUOTA_ADVICE,
  bytesText,
  formatBytes,
  formatCount,
  keysText,
  meterState,
  overQuotaMessage,
  parseLimitField,
} from './quota'

describe('formatting', () => {
  it('formats counts with thousands separators', () => {
    expect(formatCount(0)).toBe('0')
    expect(formatCount(1000)).toBe('1,000')
    expect(formatCount(1047000)).toBe('1,047,000')
  })

  it('formats byte sizes in binary units with at most one decimal', () => {
    expect(formatBytes(0)).toBe('0 bytes')
    expect(formatBytes(1)).toBe('1 byte')
    expect(formatBytes(1023)).toBe('1,023 bytes')
    expect(formatBytes(1024)).toBe('1 KB')
    expect(formatBytes(3482)).toBe('3.4 KB')
    expect(formatBytes(1048576)).toBe('1 MB')
    expect(formatBytes(2097152)).toBe('2 MB')
    expect(formatBytes(1572864)).toBe('1.5 MB')
  })

  it('writes the meter texts of the spec (AC-UI-1)', () => {
    expect(keysText(12, 1000)).toBe('12 of 1,000 keys')
    expect(keysText(3, null)).toBe('3 keys (unlimited)')
    expect(keysText(1, null)).toBe('1 key (unlimited)')
    expect(bytesText(3482, 1048576)).toBe('3.4 KB of 1 MB')
    expect(bytesText(10, null)).toBe('10 bytes (unlimited)')
  })
})

describe('meterState', () => {
  it('is near at 80% or more, over above the limit, unlimited without one', () => {
    expect(meterState(79, 100)).toBe('normal')
    expect(meterState(80, 100)).toBe('near')
    expect(meterState(100, 100)).toBe('near')
    expect(meterState(101, 100)).toBe('over')
    expect(meterState(5, null)).toBe('unlimited')
    expect(meterState(0, 0)).toBe('near')
    expect(meterState(1, 0)).toBe('over')
  })
})

describe('overQuotaMessage', () => {
  it('names the limit, uses the exact numbers and says what to do', () => {
    const keys = overQuotaMessage({ dimension: 'keys', used: 1000, limit: 1000, requested: 1 })
    expect(keys).toContain('key limit')
    expect(keys).toContain('1,000 of 1,000 keys')
    expect(keys).toContain(OVER_QUOTA_ADVICE)

    const bytes = overQuotaMessage({ dimension: 'bytes', used: 1047000, limit: 1048576, requested: 2048 })
    expect(bytes).toContain('byte limit')
    expect(bytes).toContain('1,047,000 of 1,048,576 bytes')
    expect(bytes).toContain('2,048 more')
    expect(bytes).toContain(OVER_QUOTA_ADVICE)
  })
})

describe('parseLimitField', () => {
  it('accepts whole numbers of 0 or more and empty (unlimited)', () => {
    expect(parseLimitField('')).toBeNull()
    expect(parseLimitField('   ')).toBeNull()
    expect(parseLimitField('0')).toBe(0)
    expect(parseLimitField(' 250 ')).toBe(250)
  })

  it('rejects negative, decimal, text and unsafe numbers', () => {
    for (const bad of ['-1', '1.5', 'ten', '1e3', '0x10', '99999999999999999999']) {
      expect(parseLimitField(bad), bad).toBeUndefined()
    }
    expect(LIMIT_FIELD_ERROR).toContain('leave empty for unlimited')
  })
})
