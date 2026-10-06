// The single role matrix for the provider console (Sprint 21 C3). Routes
// (RequireRole) and the nav both read SECTIONS, so a new page is one entry
// here: C-b adds Provider users, Phase P adds Relays/Tenants/Audit/
// Certificates.
//
// This is UX only. The controller authorizes every /provider/* request on
// its own (decide()); hiding a section never grants or removes access.

export const PROVIDER_ROLES = ['super-admin', 'relay-ops'] as const
export type ProviderRole = (typeof PROVIDER_ROLES)[number]

/** Display names; an unknown role is shown as its raw value. */
export const ROLE_LABELS: Record<string, string> = {
  'super-admin': 'Super-admin',
  'relay-ops': 'Relay ops',
}

export interface Section {
  key: string
  path: string
  label: string
  roles: readonly ProviderRole[]
}

export const SECTIONS: readonly Section[] = [
  { key: 'home', path: '/', label: 'Home', roles: PROVIDER_ROLES },
  { key: 'users', path: '/users', label: 'Provider users', roles: ['super-admin'] },
]

/** The roles a section needs, for wrapping its route in RequireRole. */
export function rolesFor(key: string): readonly ProviderRole[] {
  const section = SECTIONS.find((s) => s.key === key)
  if (!section) throw new Error(`unknown section ${key}`)
  return section.roles
}

export function isProviderRole(role: unknown): role is ProviderRole {
  return typeof role === 'string' && (PROVIDER_ROLES as readonly string[]).includes(role)
}

/** An unknown or missing role is allowed nothing. */
export function hasRole(role: string | undefined, allowed: readonly ProviderRole[]): boolean {
  return isProviderRole(role) && allowed.includes(role)
}

export function sectionsFor(role: string | undefined, sections: readonly Section[] = SECTIONS): Section[] {
  return sections.filter((s) => hasRole(role, s.roles))
}
