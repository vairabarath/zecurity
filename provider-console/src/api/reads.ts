import { request } from './client'
import type {
  AuditEntry,
  CertificateExpiryResult,
  CertificateKind,
  Page,
  RelayDetail,
  RelayStatus,
  RelaySummary,
  TenantDetail,
  TenantStatus,
  TenantSummary,
} from './types'

// Provider read APIs (Phase R). GET only: these wrappers add no writes to the
// console's API surface (api-surface.test.ts). Errors follow C-a's handling
// in client.ts (session-ending 401/403 end the session; other errors surface
// as ApiError).
//
// Query strings are built only from each endpoint's allowlisted filter keys,
// so nothing else (tokens, passwords, response data) can reach a URL.

export interface RelayListParams {
  status?: RelayStatus
  limit?: number
  cursor?: string
}

export interface TenantListParams {
  status?: TenantStatus
  limit?: number
  cursor?: string
}

/** `since`/`until` are RFC 3339; the window is half-open [since, until). */
export interface AuditQueryParams {
  action?: string
  target_type?: string
  target_id?: string
  provider_email?: string
  since?: string
  until?: string
  limit?: number
  cursor?: string
}

/** `within` is a Go duration (e.g. "720h"); default 720h, max 8760h. */
export interface CertificateParams {
  within?: string
  kind?: CertificateKind
  tenant_id?: string
  limit?: number
  cursor?: string
}

const RELAY_LIST_KEYS = ['status', 'limit', 'cursor'] as const
const TENANT_LIST_KEYS = ['status', 'limit', 'cursor'] as const
const AUDIT_KEYS = ['action', 'target_type', 'target_id', 'provider_email', 'since', 'until', 'limit', 'cursor'] as const
const CERTIFICATE_KEYS = ['within', 'kind', 'tenant_id', 'limit', 'cursor'] as const

/**
 * "?a=1&b=2" from the allowlisted keys of params, skipping undefined, null
 * and empty values; "" when nothing is set. Keys outside the allowlist are
 * dropped even if present at runtime.
 */
export function buildQuery(params: object | undefined, allowed: readonly string[]): string {
  if (!params) return ''
  const values = params as Record<string, unknown>
  const qs = new URLSearchParams()
  for (const key of allowed) {
    const v = values[key]
    if (v === undefined || v === null || v === '') continue
    if (typeof v !== 'string' && typeof v !== 'number') continue
    qs.set(key, String(v))
  }
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export function listRelays(params?: RelayListParams): Promise<Page<RelaySummary>> {
  return request<Page<RelaySummary>>('GET', '/provider/relays' + buildQuery(params, RELAY_LIST_KEYS))
}

export function getRelay(id: string): Promise<RelayDetail> {
  return request<RelayDetail>('GET', `/provider/relays/${encodeURIComponent(id)}`)
}

export function listTenants(params?: TenantListParams): Promise<Page<TenantSummary>> {
  return request<Page<TenantSummary>>('GET', '/provider/tenants' + buildQuery(params, TENANT_LIST_KEYS))
}

/**
 * Each successful call writes one tenant.read provider audit row (OQ-1).
 * Call it only on explicit navigation, Refresh or Retry; never prefetch,
 * poll or refetch in the background.
 */
export function getTenant(id: string): Promise<TenantDetail> {
  return request<TenantDetail>('GET', `/provider/tenants/${encodeURIComponent(id)}`)
}

export function queryAudit(params?: AuditQueryParams): Promise<Page<AuditEntry>> {
  return request<Page<AuditEntry>>('GET', '/provider/audit' + buildQuery(params, AUDIT_KEYS))
}

export function getCertificates(params?: CertificateParams): Promise<CertificateExpiryResult> {
  return request<CertificateExpiryResult>('GET', '/provider/certificates' + buildQuery(params, CERTIFICATE_KEYS))
}
