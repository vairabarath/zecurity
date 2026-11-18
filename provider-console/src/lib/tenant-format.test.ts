import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/client'
import { SLUG_PATTERN, slugFromName, tenantCreateErrorMessage } from './tenant-format'

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
    for (const code of ['slug_taken', 'invalid_slug', 'invalid_name', 'invalid_email', 'plan_not_found', 'forbidden']) {
      expect(tenantCreateErrorMessage(new ApiError({ status: 400, code }))).not.toMatch(/Something went wrong/)
    }
    expect(tenantCreateErrorMessage(new TypeError('x'))).toMatch(/Couldn’t reach/)
  })
})
