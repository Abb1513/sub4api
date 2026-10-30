import { describe, expect, it } from 'vitest'
import { formatCredits } from '../formatters'

describe('formatCredits', () => {
  it('keeps up to 4 decimals and trims trailing zeros', () => {
    expect(formatCredits(0.0283)).toBe('0.0283')
    expect(formatCredits(0.17)).toBe('0.17')
    expect(formatCredits(1.5)).toBe('1.5')
    expect(formatCredits(2)).toBe('2')
    expect(formatCredits(100)).toBe('100')
  })

  it('keeps tiny values visible instead of rounding to 0', () => {
    expect(formatCredits(0.00001)).toBe('0.000010')
  })

  it('abbreviates large totals', () => {
    expect(formatCredits(12345)).toBe('12.3K')
    expect(formatCredits(2_500_000)).toBe('2.5M')
  })

  it('returns 0 for empty or invalid values', () => {
    expect(formatCredits(null)).toBe('0')
    expect(formatCredits(undefined)).toBe('0')
    expect(formatCredits(0)).toBe('0')
    expect(formatCredits(-1)).toBe('0')
    expect(formatCredits(Number.NaN)).toBe('0')
  })
})
