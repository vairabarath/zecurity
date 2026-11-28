// The single role matrix for the provider console (Sprint 21 C3). Routes
// (RequireRole) and the nav both read SECTIONS, so a new page is one entry
// here: C-b added Operators, Phase P added Relays/Tenants/Audit/
// Certificates; relay pools (D-37) added Relay pools; plans (D-43) added Plans;
// alerts (Sprint 24 A3, F26) added Alerts; the incident timeline (T1, F27)
// added Timeline; usage (Sprint 25 U3, F33) added Usage; billing (Sprint 26
// P4, F34) added Invoices and Billing settings; API keys (Sprint 27 K2, F35)
// added API keys.
//
// Sprint 27 O2 (D-72, D-73): five roles; sections and write buttons follow
// the mirrored role matrix through can() / rolesWith().
//
// This is UX only. The controller authorizes every /provider/* request on
// its own (decide()); hiding a section never grants or removes access.

import roleMatrix from './role-matrix.json'

export const PROVIDER_ROLES = ['super-admin', 'relay-ops', 'auditor', 'billing', 'support'] as const
export type ProviderRole = (typeof PROVIDER_ROLES)[number]

/** Display names; an unknown role is shown as its raw value. */
export const ROLE_LABELS: Record<string, string> = {
  'super-admin': 'Super-admin',
  'relay-ops': 'Relay ops',
  // Sprint 27 (D-73): auditor, billing, support (also the roles an API key may have).
  auditor: 'Auditor',
  billing: 'Billing',
  support: 'Support',
}

/** One line per role for the operator dialogs (Sprint 27 O2, D-73). */
export const ROLE_HELP: Record<ProviderRole, string> = {
  'super-admin': 'Everything, including operators, API keys and break-glass acknowledgement.',
  'relay-ops': 'Relays and relay pools; never tenant data. Revoke, delete and pool placement stay with super-admins.',
  auditor: 'Reads everything the console shows, including the provider audit log. Never writes.',
  billing: 'Plan cards, billing settings, invoices, usage, the tenant list and plan assignment.',
  support: 'Reads tenants and their agents, the tenants’ alerts and timeline, and health. Never writes.',
}

/** Roles that must set up two-step verification (TOTP) at first sign-in (D-28, D-74). */
export const TOTP_REQUIRED_ROLES: readonly ProviderRole[] = ['super-admin', 'auditor', 'billing', 'support']

// The role → actions table, mirrored from the controller's decide()
// (role-matrix.json; TestConsoleRoleMatrix keeps the two equal). Action
// names are the server's. super-admin may do everything.
const MATRIX: Record<string, readonly string[]> = roleMatrix.roles

/**
 * Whether `role` may perform the server action `action` (UX only: what to
 * show; the controller decides every request). An unknown or missing role is
 * allowed nothing.
 */
export function can(role: string | undefined, action: string): boolean {
  if (!isProviderRole(role)) return false
  if (role === 'super-admin') return true
  return MATRIX[role]?.includes(action) ?? false
}

/** The roles allowed `action`, for route guards and sections. */
export function rolesWith(action: string): readonly ProviderRole[] {
  return PROVIDER_ROLES.filter((r) => can(r, action))
}

export interface Section {
  key: string
  path: string
  label: string
  roles: readonly ProviderRole[]
  /** Sidebar group heading; sections without one sit at the top. */
  group?: string
}

// Each page is shown to the roles allowed the server action it reads
// (Sprint 27 O2): the list follows the matrix instead of naming roles.
export const SECTIONS: readonly Section[] = [
  { key: 'home', path: '/', label: 'Overview', roles: PROVIDER_ROLES },
  // Alerts and the timeline: relay-ops see relay and pool alerts, support tenant alerts.
  { key: 'alerts', path: '/alerts', label: 'Alerts', roles: rolesWith('alert.read') },
  { key: 'timeline', path: '/timeline', label: 'Timeline', roles: rolesWith('alert.read') },
  { key: 'relays', path: '/relays', label: 'Relays', roles: rolesWith('relay.read'), group: 'Fleet' },
  { key: 'relay-pools', path: '/relay-pools', label: 'Relay pools', roles: rolesWith('relay.read'), group: 'Fleet' },
  // Tenants: the list (billing too); the detail needs tenant.read (TENANT_DETAIL_ROLES).
  { key: 'tenants', path: '/tenants', label: 'Tenants', roles: rolesWith('tenant.list.read'), group: 'Fleet' },
  { key: 'plans', path: '/plans', label: 'Plans', roles: rolesWith('plan.read'), group: 'Fleet' },
  { key: 'usage', path: '/usage', label: 'Usage', roles: rolesWith('tenant.usage.read'), group: 'Fleet' },
  { key: 'invoices', path: '/invoices', label: 'Invoices', roles: rolesWith('invoice.read'), group: 'Billing' },
  // Reading the billing settings is plan.read on the server.
  { key: 'billing-settings', path: '/billing/settings', label: 'Billing settings', roles: rolesWith('plan.read'), group: 'Billing' },
  { key: 'certificates', path: '/certificates', label: 'Certificates', roles: rolesWith('cert.read'), group: 'Trust' },
  { key: 'audit', path: '/audit', label: 'Audit log', roles: rolesWith('audit.view'), group: 'Governance' },
  { key: 'users', path: '/users', label: 'Operators', roles: rolesWith('provider_user.manage'), group: 'Governance' },
  // API keys (Sprint 27 K2, D-76): managing access, super-admin only.
  { key: 'api-keys', path: '/settings/api-keys', label: 'API keys', roles: rolesWith('api_key.read'), group: 'Governance' },
]

/** The tenant detail page needs tenant.read (billing has the list only, D-73). */
export const TENANT_DETAIL_ROLES = rolesWith('tenant.read')

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
