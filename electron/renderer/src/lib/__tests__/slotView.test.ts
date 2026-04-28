import { describe, expect, it } from 'vitest'
import {
  classifyVerdict,
  verdictChipVariant,
  verdictLabel,
} from '../slotView'

describe('verdictChipVariant', () => {
  it('maps success tokens to approved', () => {
    const successTokens = [
      'succeeded',
      'success',
      'approve',
      'approved',
      'accept',
      'accepted',
      'pass',
      'passes',
      'ok',
      'done',
    ]
    for (const t of successTokens) {
      expect(verdictChipVariant(t)).toBe('approved')
    }
  })

  it('maps revise / replan to revise', () => {
    expect(verdictChipVariant('revise')).toBe('revise')
    expect(verdictChipVariant('revise plan')).toBe('revise')
    expect(verdictChipVariant('replan')).toBe('revise')
  })

  it('maps fix / reject / fail / deny to fix', () => {
    expect(verdictChipVariant('fix')).toBe('fix')
    expect(verdictChipVariant('reject')).toBe('fix')
    expect(verdictChipVariant('fail')).toBe('fix')
    expect(verdictChipVariant('deny')).toBe('fix')
  })

  it('falls back to pending for unknown words and null', () => {
    expect(verdictChipVariant('mystery')).toBe('pending')
    expect(verdictChipVariant('')).toBe('pending')
    expect(verdictChipVariant(null)).toBe('pending')
  })

  it('is case-insensitive', () => {
    expect(verdictChipVariant('APPROVED')).toBe('approved')
    expect(verdictChipVariant('SuCCeEdED')).toBe('approved')
    expect(verdictChipVariant('REVISE')).toBe('revise')
  })
})

describe('classifyVerdict', () => {
  it('shares the same classifier logic as verdictChipVariant', () => {
    // The chip variant is a passthrough of classifyVerdict.
    expect(classifyVerdict('succeeded')).toBe('approved')
    expect(classifyVerdict('revise')).toBe('revise')
    expect(classifyVerdict('fix')).toBe('fix')
    expect(classifyVerdict(null)).toBe('pending')
  })
})

describe('verdictLabel', () => {
  it('brackets and upper-cases the verdict', () => {
    expect(verdictLabel('approved')).toBe('[ APPROVED ]')
    expect(verdictLabel('succeeded')).toBe('[ SUCCEEDED ]')
  })

  it('falls back to [ PENDING ] for null', () => {
    expect(verdictLabel(null)).toBe('[ PENDING ]')
  })
})
