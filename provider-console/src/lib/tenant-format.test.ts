import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/client'
import { SLUG_PATTERN, slugFromName, tenantCreateErrorMessage, timeUntil } from './tenant-format'

describe('tenant-format', () => {
  it('suggests a valid slug from the name', () => {
    expect(slugFromName('Acme Corp')).toBe('acme-corp')
    expect(slugFromName('  Ünïcode & Co. ')).toBe('n-code-co')
    expect(slugFromName('a'.repeat(70))).toHaveLength(60)
    expect(slugFromName(`${'a'.repeat(59)} b`)).toBe('a'.repeat(59))
    for (const n of ['Acme Corp', 'x', `${'a'.repeat(59)} b`]) expect(SLUG_PATTERN.test(slugFromName(n))).toBe(true)
  })

  it('accepts the same slugs as the controller', () => {
    for (const ok of ['a', 'acme', 'acme-corp', '0x1', 'a'.repeat(60)]) expect(SLUG_PATTERN.test(ok)).toBe(true)
    for (const bad of ['', '-a', 'a-', 'A', 'a_b', 'a.b', 'a'.repeat(61)]) expect(SLUG_PATTERN.test(bad)).toBe(false)
  })

  it('maps every controller error code', () => {
    for (const code of ['slug_taken', 'invalid_slug', 'invalid_name', 'invalid_email', 'plan_not_found', 'forbidden', 'first_admin_exists', 'email_has_account', 'tenant_not_active', 'not_found']) {
      expect(tenantCreateErrorMessage(new ApiError({ status: 400, code }))).not.toMatch(/Something went wrong/)
    }
    expect(tenantCreateErrorMessage(new TypeError('x'))).toMatch(/Couldn’t reach/)
  })
})

describe('timeUntil (S4 countdown)', () => {
  const now = Date.parse('2026-11-19T10:00:00Z')
  it('says minutes, hours or days, and null once due', () => {
    expect(timeUntil('2026-11-19T10:02:00Z', now)).toBe('in 2 minutes')
    expect(timeUntil('2026-11-19T10:00:20Z', now)).toBe('in 1 minute')
    expect(timeUntil('2026-11-19T15:00:00Z', now)).toBe('in 5 hours')
    expect(timeUntil('2026-12-19T10:00:00Z', now)).toBe('in 30 days')
    expect(timeUntil('2026-11-19T09:59:00Z', now)).toBeNull()
  })
})
