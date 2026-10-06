// Wire types for the provider API (controller/internal/provider). Field names
// match the JSON exactly.

/** POST /provider/auth/login → 200 */
export interface LoginResponse {
  token: string
  expires_in: number
  password_change_required: boolean
}

/** POST /provider/auth/password → 200. The previous token is dead (generation bump). */
export interface SessionResponse {
  token: string
  expires_in: number
}

/**
 * GET /provider/me → 200. `last_login_at` is stamped by the successful login
 * that started this session, so the UI labels it "Signed in at".
 * `role` comes from the database row; it is typed as string because the
 * console must cope with a role it doesn't know (it then grants nothing).
 */
export interface Me {
  user_id: string
  email: string
  role: string
  last_login_at: string | null
}

/** Every error body is `{"error": "<code>"}`; `password_policy` adds `detail`. */
export interface ErrorBody {
  error?: string
  detail?: string
}

/** Error codes the console reacts to. Anything else is shown generically. */
export const ErrorCode = {
  // auth handlers
  invalidRequest: 'invalid_request',
  invalidCredentials: 'invalid_credentials',
  tooManyAttempts: 'too_many_attempts',
  loginUnavailable: 'login_unavailable',
  passwordPolicy: 'password_policy',
  serverError: 'server_error',
  // RequireProvider middleware
  passwordChangeRequired: 'password_change_required',
  notAProviderUser: 'not a provider user',
  // operator routes (Phase U). GET /provider/users answers 403 with
  // "provider action forbidden" instead; match operator 403s on the status.
  forbidden: 'forbidden',
  notFound: 'not_found',
  invalidEmail: 'invalid_email',
  invalidRole: 'invalid_role',
  cannotModifySelf: 'cannot_modify_self',
  lastSuperAdmin: 'last_super_admin',
  alreadyExists: 'already_exists',
  existsDisabled: 'exists_disabled',
  accountDisabled: 'account_disabled',
  serviceUnavailable: 'service_unavailable',
  // client-side only
  networkError: 'network_error',
} as const

/**
 * One provider operator as GET /provider/users returns it (Phase U
 * OperatorView). It never carries a password or password hash.
 */
export interface OperatorView {
  id: string
  email: string
  role: string
  disabled_at: string | null
  created_at: string
  last_login_at: string | null
  must_change_password: boolean
  /** False after enable (the old credential is cleared) until a reset. */
  has_password: boolean
}

/**
 * POST /provider/users → 201. `temporary_password` is shown once and then
 * discarded; it must never reach storage, the URL or a log.
 */
export interface CreateOperatorResponse {
  user: OperatorView
  temporary_password: string
}

/** PATCH /provider/users/{id} → 200. */
export interface ChangeRoleResponse {
  user: OperatorView
}

/** POST /provider/users/{id}/reset-password → 200. Shown once, like on create. */
export interface ResetPasswordResponse {
  temporary_password: string
}

// ── Phase R read APIs ────────────────────────────────────────────────────────
// Transport types mirror the controller's providerquery DTOs field for field
// (snake_case, nullable where the API sends null, [] for empty collections).
// They carry only what the API returns: no client-added fields.

/** A JSON value as returned in audit `details` (opaque to the console). */
export type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue }

/** One page of a keyset-paginated list. `next_cursor` is null on the last page. */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
}

export const RELAY_STATUSES = ['pending', 'active', 'inactive', 'revoked', 'deleted'] as const
export type RelayStatus = (typeof RELAY_STATUSES)[number]

/** GET /provider/relays item. */
export interface RelaySummary {
  id: string
  name: string
  status: RelayStatus
  version: string | null
  hostname: string | null
  public_addr: string | null
  address_scope: string | null
  capacity_label: string
  connection_count: number
  max_connections: number
  cert_serial: string | null
  cert_not_after: string | null
  /** The fresher of the persisted heartbeat and the Valkey liveness key. */
  last_heartbeat_at: string | null
  attached_connectors: number
  created_at: string
}

export interface RelayCertificate {
  serial: string
  issued_at: string
  not_after: string
  revoked_at: string | null
  revocation_reason: string | null
  is_current: boolean
}

export interface RelayAttachment {
  connector_id: string
  tenant_id: string
  attached_at: string
  last_confirmed: string
  source: string
}

/** GET /provider/relays/{id}. Nested lists are capped at 200 (`*_truncated`). */
export interface RelayDetail extends RelaySummary {
  dns_allowlist: string[]
  ip_allowlist: string[]
  certificates: RelayCertificate[]
  certificates_truncated: boolean
  attachments: RelayAttachment[]
  attachments_truncated: boolean
}

export const TENANT_STATUSES = ['provisioning', 'active', 'suspended', 'deleted'] as const
export type TenantStatus = (typeof TENANT_STATUSES)[number]

export interface AgentCounts {
  pending: number
  active: number
  disconnected: number
  revoked: number
}

/** Users are counts by status only: the API never returns tenant user identities (OQ-2). */
export interface UserCounts {
  active: number
  suspended: number
  locked: number
  deleted: number
}

export interface DeviceCounts {
  active: number
  re_enroll_required: number
  renew_pending: number
  revoked: number
}

export interface TenantCounts {
  connectors: AgentCounts
  shields: AgentCounts
  remote_networks: number
  users: UserCounts
  client_devices: DeviceCounts
}

/** GET /provider/tenants item. */
export interface TenantSummary {
  id: string
  slug: string
  name: string
  status: TenantStatus
  trust_domain: string
  created_at: string
  ca_not_after: string | null
  counts: TenantCounts
}

export interface TenantRemoteNetwork {
  id: string
  name: string
  status: string
}

export interface TenantConnector {
  id: string
  name: string
  status: string
  remote_network_id: string
  version: string | null
  cert_not_after: string | null
  last_heartbeat_at: string | null
  revoked_at: string | null
}

export interface TenantShield {
  id: string
  name: string
  status: string
  connector_id: string
  remote_network_id: string
  cert_not_after: string | null
  last_heartbeat_at: string | null
}

/**
 * GET /provider/tenants/{id}. Each successful fetch writes one tenant.read
 * provider audit row (OQ-1), so callers must fetch it only on explicit
 * navigation, Refresh or Retry.
 */
export interface TenantDetail extends TenantSummary {
  remote_networks: TenantRemoteNetwork[]
  remote_networks_truncated: boolean
  connectors: TenantConnector[]
  connectors_truncated: boolean
  shields: TenantShield[]
  shields_truncated: boolean
}

/** GET /provider/audit item (provider operator records only). */
export interface AuditEntry {
  id: string
  created_at: string
  provider_user_id: string | null
  provider_email: string
  action: string
  target_type: string
  target_id: string
  details: JsonValue
  ip_address: string | null
}

export const CERTIFICATE_KINDS = [
  'client_device',
  'connector',
  'controller_grpc',
  'intermediate',
  'relay',
  'root',
  'shield',
  'workspace_ca',
] as const
export type CertificateKind = (typeof CERTIFICATE_KINDS)[number]

export const EXPIRY_BUCKETS = ['expired', 'lt_24h', 'lt_7d', 'lt_30d', 'ok'] as const
export type ExpiryBucket = (typeof EXPIRY_BUCKETS)[number]

export interface CertificateEntry {
  kind: CertificateKind
  tenant_id: string | null
  entity_id: string
  /** null for client devices (device names may identify a person). */
  entity_name: string | null
  serial: string | null
  not_after: string
  bucket: ExpiryBucket
}

/** Counts per bucket over everything matching kind/tenant; ignores `within`. */
export interface CertificateSummary {
  expired: number
  lt_24h: number
  lt_7d: number
  lt_30d: number
  ok: number
}

/** GET /provider/certificates. */
export interface CertificateExpiryResult {
  summary: CertificateSummary
  items: CertificateEntry[]
  next_cursor: string | null
}
