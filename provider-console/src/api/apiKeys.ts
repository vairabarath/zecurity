import { request } from './client'

// Provider API keys (Sprint 27 K2, F35; D-76). Super-admin only; the
// controller decides. Create and revoke are console writes (D-30 as amended
// by D-76) and are listed in api-surface.test.ts on purpose. The full key is
// in the create response only: it is handed straight to the show-once dialog
// and never kept here or in storage.

/** The roles a key may have (never super-admin, D-76). */
export const API_KEY_ROLES = ['relay-ops', 'auditor', 'billing', 'support'] as const
export type ApiKeyRole = (typeof API_KEY_ROLES)[number]

export type ApiKeyStatus = 'active' | 'expired' | 'revoked'

export interface ApiKey {
  id: string
  name: string
  role: ApiKeyRole
  /** The first 12 characters of the key (zpk_ + 8), the only part ever shown again. */
  prefix: string
  status: ApiKeyStatus
  created_by: string
  created_at: string
  expires_at: string
  last_used_at: string | null
  last_used_ip: string | null
  allowed_cidrs: string[]
  revoked_at: string | null
  revoked_by: string | null
  revoke_reason: string | null
}

export interface NewApiKey {
  name: string
  role: ApiKeyRole
  /** RFC 3339; at most 365 days ahead (the server checks). */
  expires_at: string
  allowed_cidrs?: string[]
}

function keyPath(id: string): string {
  return `/provider/api-keys/${encodeURIComponent(id)}`
}

export function listApiKeys(): Promise<{ items: ApiKey[] }> {
  return request<{ items: ApiKey[] }>('GET', '/provider/api-keys')
}

/** The full key is in this response only. */
export function createApiKey(fields: NewApiKey, reason: string): Promise<{ api_key: ApiKey; key: string }> {
  return request<{ api_key: ApiKey; key: string }>('POST', '/provider/api-keys', { body: { ...fields, reason } })
}

/** Revocation is final; the server checks the typed name (D-17 style). */
export function revokeApiKey(id: string, reason: string, confirmName: string): Promise<{ api_key: ApiKey }> {
  return request<{ api_key: ApiKey }>('POST', `${keyPath(id)}/revoke`, { body: { reason, confirm_name: confirmName } })
}
