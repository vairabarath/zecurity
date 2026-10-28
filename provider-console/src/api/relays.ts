import { request } from './client'
import type { CreateRelayResponse } from './types'

// Relay mutations (Sprint 22, D-30; the controller decides who may call
// them: create is super-admin and relay-ops; revoke and delete are
// super-admin only, D-31). Phase E adds create, Phase X revoke and delete.
// The provisioning token comes back in the response body and is handed
// straight to the caller; nothing here keeps it.

export function createRelay(name: string, dnsAllowlist: string[], ipAllowlist: string[]): Promise<CreateRelayResponse> {
  return request<CreateRelayResponse>('POST', '/provider/relays', {
    body: { name, dns_allowlist: dnsAllowlist, ip_allowlist: ipAllowlist },
  })
}

function relayPath(id: string): string {
  return `/provider/relays/${encodeURIComponent(id)}`
}

// Phase X (D-17, D-31; super-admin only, the controller decides): the reason
// and the typed relay name go in the body, never the URL. The controller
// trims and checks the reason and compares the name exactly.

export function revokeRelay(id: string, reason: string, confirmName: string): Promise<void> {
  return request<void>('POST', `${relayPath(id)}/revoke`, { body: { reason, confirm_name: confirmName } })
}

/** Revokes the relay's certificates too; a revoked relay may still be deleted. */
export function deleteRelay(id: string, reason: string, confirmName: string): Promise<void> {
  return request<void>('DELETE', relayPath(id), { body: { reason, confirm_name: confirmName } })
}
