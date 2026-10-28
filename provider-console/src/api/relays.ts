import { request } from './client'
import type { CreateRelayResponse } from './types'

// Relay mutations (Sprint 22, D-30; the controller decides who may call
// them: create is super-admin and relay-ops, D-31). Phase E adds create.
// The provisioning token comes back in the response body and is handed
// straight to the caller; nothing here keeps it.

export function createRelay(name: string, dnsAllowlist: string[], ipAllowlist: string[]): Promise<CreateRelayResponse> {
  return request<CreateRelayResponse>('POST', '/provider/relays', {
    body: { name, dns_allowlist: dnsAllowlist, ip_allowlist: ipAllowlist },
  })
}
