import { describe, expect, it } from 'vitest'
import { SECTIONS, hasRole, isProviderRole, sectionsFor, type Section } from './roles'

const matrix: Section[] = [
  { key: 'home', path: '/', label: 'Home', roles: ['super-admin', 'relay-ops'] },
  { key: 'users', path: '/users', label: 'Provider users', roles: ['super-admin'] },
  { key: 'relays', path: '/relays', label: 'Relays', roles: ['super-admin', 'relay-ops'] },
]

describe('role matrix', () => {
  it('knows exactly the two provider roles', () => {
    expect(isProviderRole('super-admin')).toBe(true)
    expect(isProviderRole('relay-ops')).toBe(true)
    expect(isProviderRole('ADMIN')).toBe(false) // a tenant role
    expect(isProviderRole(undefined)).toBe(false)
  })

  it('every section in the real matrix is reachable by super-admin', () => {
    expect(sectionsFor('super-admin')).toEqual(SECTIONS)
  })

  it('filters sections by role', () => {
    expect(sectionsFor('super-admin', matrix).map((s) => s.key)).toEqual(['home', 'users', 'relays'])
    expect(sectionsFor('relay-ops', matrix).map((s) => s.key)).toEqual(['home', 'relays'])
  })

  it('an unknown or missing role gets nothing', () => {
    expect(sectionsFor('root', matrix)).toEqual([])
    expect(sectionsFor(undefined, matrix)).toEqual([])
    expect(hasRole('super-admin ', ['super-admin'])).toBe(false)
  })
})
