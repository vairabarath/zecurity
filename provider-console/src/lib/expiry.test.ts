import { describe, expect, it } from 'vitest'
import { EXPIRY_BUCKETS } from '@/api/types'
import { BUCKET_LABELS, BUCKET_VARIANTS, bucketFor, formatRelative } from './expiry'

const now = new Date('2026-10-06T12:00:00.000Z')
const H = 3_600_000
const D = 24 * H
const at = (ms: number) => new Date(now.getTime() + ms).toISOString()

describe('bucketFor mirrors the server boundaries exactly', () => {
  it.each([
    [-1, 'expired'],
    [0, 'expired'], // not_after == now is expired
    [1, 'lt_24h'],
    [D, 'lt_24h'], // exactly 24h is still < 24h bucket
    [D + 1, 'lt_7d'],
    [7 * D, 'lt_7d'],
    [7 * D + 1, 'lt_30d'],
    [30 * D, 'lt_30d'],
    [30 * D + 1, 'ok'],
    [400 * D, 'ok'],
  ])('now %+d ms → %s', (offset, want) => {
    expect(bucketFor(at(offset), now)).toBe(want)
  })

  it('accepts Date values and rejects invalid dates', () => {
    expect(bucketFor(new Date(now.getTime() + 2 * D), now)).toBe('lt_7d')
    expect(bucketFor('not a date', now)).toBeNull()
    expect(bucketFor('', now)).toBeNull()
  })

  it('every bucket has exactly one label and one colour', () => {
    expect(Object.keys(BUCKET_LABELS).sort()).toEqual([...EXPIRY_BUCKETS].sort())
    expect(Object.keys(BUCKET_VARIANTS).sort()).toEqual([...EXPIRY_BUCKETS].sort())
  })
})

describe('formatRelative', () => {
  it.each([
    [null, '—'],
    ['garbage', '—'],
    [at(-10_000), 'just now'],
    [at(-5 * 60_000), '5 min ago'],
    [at(-3 * H), '3 h ago'],
    [at(-D), '1 day ago'],
    [at(-3 * D), '3 days ago'],
    [at(2 * D), 'in 2 days'],
    [at(30_000), 'in under a minute'],
  ])('%s → %s', (value, want) => {
    expect(formatRelative(value, now)).toBe(want)
  })
})
