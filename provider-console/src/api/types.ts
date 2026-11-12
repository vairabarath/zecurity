// Wire types for the provider API (controller/internal/provider). Field names
// match the JSON exactly.

/** POST /provider/auth/login → 200 */
export interface LoginResponse {
  token: string
  expires_in: number
  password_change_required: boolean
  /**
   * Sprint 22 Phase T: set when `token` is a confined MFA-stage token. The
   * operator must verify a TOTP code ("verify") or set TOTP up ("enroll")
   * before any other route accepts them.
   */
  mfa?: MFAStage
}

export type MFAStage = 'verify' | 'enroll'

/**
 * POST /provider/auth/password and the TOTP verify/confirm calls → 200. The
 * previous token is dead (generation bump). After a password change an
 * operator who requires TOTP gets an MFA-stage token (`mfa`), never a full one.
 */
export interface SessionResponse {
  token: string
  expires_in: number
  mfa?: MFAStage
}

/** POST /provider/auth/totp/enroll → 200. Shown once; never stored. */
export interface TOTPEnrollResponse {
  secret: string
  otpauth_uri: string
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
  // TOTP (Sprint 22 Phase T)
  invalidCode: 'invalid_code',
  mfaRequired: 'mfa_required',
  totpAlreadyEnabled: 'totp_already_enabled',
  totpNotStarted: 'totp_not_started',
  totpNotEnrolled: 'totp_not_enrolled',
  reasonRequired: 'reason_required',
  reasonTooLong: 'reason_too_long',
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
  // relay create (Sprint 22 Phase A/E)
  invalidName: 'invalid_name',
  invalidDnsSan: 'invalid_dns_san',
  invalidIpSan: 'invalid_ip_san',
  // relay revoke/delete (Sprint 22 Phase A/X)
  alreadyRevoked: 'already_revoked',
  alreadyDeleted: 'already_deleted',
  confirmationMismatch: 'confirmation_mismatch',
  // relay drain/resume (Sprint 22 Phase D)
  invalidState: 'invalid_state',
  lastEligibleRelay: 'last_eligible_relay',
  // relay pools (D-37 … D-41)
  invalidRegion: 'invalid_region',
  residencyNeedsRegion: 'residency_needs_region',
  nameTaken: 'name_taken',
  defaultPool: 'default_pool',
  poolInUse: 'pool_in_use',
  poolNotEmpty: 'pool_not_empty',
  poolNotFound: 'pool_not_found',
  poolHasNoEligibleRelay: 'pool_has_no_eligible_relay',
  invalidMigration: 'invalid_migration',
  // failover pool (D-39)
  failoverSameAsPrimary: 'failover_same_as_primary',
  failoverRegionMismatch: 'failover_region_mismatch',
  failoverNeedsResidency: 'failover_needs_residency',
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
  /** TOTP confirmed (Sprint 22 Phase T). */
  totp_enabled: boolean
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

/**
 * POST /provider/relays → 201 (Sprint 22 Phase E). `provisioning_token` is
 * single-use, valid for 24 hours, shown once and then discarded; it must
 * never reach storage, the URL or a log. The controller addresses are empty
 * strings when the controller doesn't have them configured.
 */
export interface CreateRelayResponse {
  relay_id: string
  provisioning_token: string
  expires_at: string
  ca_fingerprint: string
  controller_grpc_addr: string
  controller_http_addr: string
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
  /** Set while the relay drains (Phase D); null otherwise and on revoked/deleted relays. */
  draining_since: string | null
  /** Drain progress, decided by the controller from fresh telemetry only. */
  drain_state: DrainState | null
  /** F5: null when the relay doesn't report byte counters, and for revoked/deleted relays. */
  bandwidth: RelayBandwidth | null
  /** The relay pool it belongs to (D-37); every relay has exactly one. */
  pool: RelayPoolRef
  /** Workspace-policy enforcement on this relay (D-40). */
  policy: RelayPolicyStatus
}

/**
 * D-40: `supported` the relay build can enforce a workspace policy;
 * `applied_version` the policy version it last reported applying (null: none);
 * `current` it has applied its pool's current policy version.
 */
export interface RelayPolicyStatus {
  supported: boolean
  applied_version: number | null
  current: boolean
}

/**
 * F5: bytes per second forwarded on bridged client streams, averaged over
 * window_seconds up to measured_at (the gap between two stored heartbeats).
 * The rates and window are null while measuring (first sample, or after a
 * relay restart).
 */
export interface RelayBandwidth {
  measured_at: string
  window_seconds: number | null
  to_connector_bytes_per_sec: number | null
  to_client_bytes_per_sec: number | null
}

/**
 * Phase D (P4): `migrating` connectors still placed on the relay;
 * `unverified` no fresh measured count yet (or inside the 120 s window);
 * `streams_open` streams still bridged; `drained` none, as of the last heartbeat.
 */
export type DrainState = 'migrating' | 'unverified' | 'streams_open' | 'drained'

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
  /** In its pool's relay list, which connectors are offered right now (Phase D, D-38). */
  in_relay_list: boolean
  /** How many relays of the same pool are in that list (the guard is per pool, D-41). */
  eligible_relay_count: number
}

// ── Relay pools (D-37 … D-41) ───────────────────────────────────────────────

/**
 * A relay pool and its placement attributes (D-37). `region` is a validated
 * code or null. `residency` is the pool's setting only; whether its relays
 * enforce it is `residency_enforced` on the pool reads (D-40), so a bare ref
 * must never be presented as enforced.
 */
export interface RelayPoolRef {
  id: string
  name: string
  region: string | null
  residency: boolean
}

/** GET /provider/relay-pools item (both roles; counts only). Default pool first. */
export interface RelayPoolSummary extends RelayPoolRef {
  is_default: boolean
  /** Relays in the pool that aren't deleted. */
  relays: number
  /** Relays in the pool's published relay list. */
  eligible_relays: number
  /** Workspaces whose pool this is. */
  tenants: number
  /**
   * D-40: a residency pool with at least one active relay, all of whose
   * active relays support the policy and have applied the current version.
   * Only this flag may be shown as "enforced".
   */
  residency_enforced: boolean
  /** The pool's current workspace-policy version (D-40). */
  policy_version: number
  /** Workspaces using this pool as their failover pool (D-39). */
  failover_tenants: number
  /** Failover state (D-39); tracked only for pools some tenant uses as primary with a failover pool. */
  state: RelayPoolState
  /** failed_over: since when the pool has had no eligible relay. */
  exhausted_at: string | null
  /** recovering: since when it has had an eligible relay again (switch-back after the hold-down). */
  eligible_since: string | null
  created_at: string
}

export const RELAY_POOL_STATES = ['healthy', 'failed_over', 'recovering'] as const
export type RelayPoolState = (typeof RELAY_POOL_STATES)[number]

export interface RelayPoolRelay {
  id: string
  name: string
  status: string
  eligible: boolean
  /** Workspace-policy state against this pool's current policy_version (D-40). */
  policy: RelayPolicyStatus
}

export interface RelayPoolTenant {
  id: string
  slug: string
  name: string
}

/**
 * GET /provider/relay-pools/{id}. `tenant_list` is null for callers who
 * can't read tenants (relay-ops see the count only, D-41).
 */
export interface RelayPoolDetail extends RelayPoolSummary {
  relay_list: RelayPoolRelay[]
  relay_list_truncated: boolean
  tenant_list: RelayPoolTenant[] | null
  tenant_list_truncated: boolean
}

/** POST /provider/relay-pools → 201. */
export type CreateRelayPoolResponse = RelayPoolRef

/** How a tenant leaves its old pool (D-40): graceful keeps it authorized for a migration window. */
export type PoolMigration = 'graceful' | 'immediate'

/** The tenant's relay pool in GET /provider/tenants/{id} (D-38). */
export interface TenantRelayPool extends RelayPoolRef {
  eligible_relays: number
  /** Set while a graceful migration keeps the old pool authorized, until `until`. */
  previous: (RelayPoolRef & { until: string }) | null
  /** The failover pool (D-39), or null when the tenant has none. */
  failover: (RelayPoolRef & { eligible_relays: number }) | null
  /** The controller has switched the tenant to its failover pool (the primary has no eligible relay). */
  using_failover: boolean
  failed_over_since: string | null
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

/** A tenant's access policy, as counts (quick-win F8). */
export interface PolicyCounts {
  /** Every resource row, as the admin's resource list shows (incl. one being removed). */
  resources: number
  groups: number
  /** Enabled and disabled access rules. */
  access_rules: number
  resource_policies: number
}

export interface TenantCounts {
  connectors: AgentCounts
  shields: AgentCounts
  remote_networks: number
  users: UserCounts
  client_devices: DeviceCounts
  policies: PolicyCounts
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
  relay_pool: TenantRelayPool
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

// ── PKI hierarchy (quick-win F15/F16) ───────────────────────────────────────

/** The CRL a CA signs. Signed on request: `entries` is what a CRL fetched now lists. */
export interface CRLStatus {
  url: string
  covers: string[]
  entries: number
  valid_for_seconds: number
}

export type CANodeKind = 'root' | 'intermediate' | 'workspace_ca'

/** One CA. subject/issuer/serial/not_before are null when its certificate does not parse. */
export interface CANode {
  id: string
  kind: CANodeKind
  name: string
  tenant_id: string | null
  subject: string | null
  issuer: string | null
  serial: string | null
  not_before: string | null
  not_after: string
  /** The CA that signed it (key identifiers); null for a root or when unresolved. */
  parent_id: string | null
  crl: CRLStatus | null
}

/** GET /provider/pki. workspace_cas is one page; the total and cursor make that explicit. */
export interface PKIHierarchy {
  roots: CANode[]
  intermediates: CANode[]
  workspace_cas: CANode[]
  workspace_cas_total: number
  next_cursor: string | null
}

// ── Control-plane health (quick-win F25) ────────────────────────────────────

export type HealthStatus = 'healthy' | 'degraded' | 'unhealthy'

export interface HealthComponent {
  name: string
  status: HealthStatus
  critical: boolean
  latency_ms: number
  /** A fixed check message or "check failed"; null when healthy. Never raw errors. */
  message: string | null
}

/** GET /provider/health. */
export interface HealthResult {
  status: HealthStatus
  checked_at: string
  components: HealthComponent[]
}

