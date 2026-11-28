package provider

import (
	"errors"
	"fmt"
)

// Provider roles (the CHECK constraint of 025, widened by 075; one role per
// operator). auditor, billing and support are Sprint 27 O1 (D-73).
const (
	RoleSuperAdmin = "super-admin"
	RoleRelayOps   = "relay-ops"
	RoleAuditor    = "auditor"
	RoleBilling    = "billing"
	RoleSupport    = "support"
)

// Roles lists every provider role, in display order.
var Roles = []string{RoleSuperAdmin, RoleRelayOps, RoleAuditor, RoleBilling, RoleSupport}

// Provider action verbs, dotted and namespaced by target type. Who may do
// what is the role matrix (roleActions, D-72), not the name: the comments
// below record why each action was added and who had it before Sprint 27
// (super-admin, relay-ops); roleActions is the source of truth.
const (
	ActionRelayCreate        = "relay.create"
	ActionRelayIssueToken    = "relay.issue_token"
	ActionRelayDelete        = "relay.delete"
	ActionProviderUserManage = "provider_user.manage"
	ActionAuditView          = "audit.view"
	ActionRelayRevoke        = "relay.revoke"
	// Drain and resume (Sprint 22 Phase D, D-32): relay-ops may do both
	// (D-31), through the relay.* prefix.
	ActionRelayDrain  = "relay.drain"
	ActionRelayResume = "relay.resume"

	// Read actions (Sprint 21 Phase R, D-05). decide()'s prefix rule gives
	// relay-ops relay.read only; tenant.* is super-admin only (D-06) and
	// cert.read spans every tenant's certificates, so it is super-admin only
	// too. Provider audit reads reuse ActionAuditView above.
	ActionRelayRead  = "relay.read"
	ActionTenantRead = "tenant.read"
	// ActionTenantListRead (Sprint 27 O1, D-73) gates the tenant list only
	// (GET /provider/tenants), so billing can pick a tenant without the
	// detail with its agents (tenant.read).
	ActionTenantListRead = "tenant.list.read"
	ActionCertRead       = "cert.read"
	// Control-plane health (quick-win F25): not under relay.*, so
	// super-admin only.
	ActionHealthRead = "health.read"

	// Relay pools (D-37 … D-41). relay-ops manage pools and move relays
	// through the relay.* prefix; changing a pool's region or residency is a
	// super-admin exception (it changes what tenants were promised), and
	// assigning a tenant's pool is a tenant.* action, so super-admin only
	// (D-06).
	ActionRelayPoolCreate       = "relay.pool.create"
	ActionRelayPoolUpdate       = "relay.pool.update"
	ActionRelayPoolSetPlacement = "relay.pool.set_placement"
	ActionRelayPoolDelete       = "relay.pool.delete"
	ActionRelayMove             = "relay.move"
	ActionTenantRelayPoolAssign = "tenant.relay_pool.assign"
	// ActionRelayPlacementRejected is audited by the system (no operator):
	// a residency tenant's connector reported a relay outside its pools.
	ActionRelayPlacementRejected = "relay.placement_rejected"

	// Relay link capacity (Sprint 23 B2, D-42; D-31 amended): under relay.*,
	// so relay-ops and super-admins may set it.
	ActionRelayCapacitySet = "relay.capacity.set"

	// Tenant lifecycle (Sprint 23 S1, D-45): suspend and resume a tenant.
	// tenant.* is super-admin only (D-06); decide() denies it to relay-ops.
	ActionTenantSuspend = "tenant.suspend"
	ActionTenantResume  = "tenant.resume"
	// Tenant deletion (Sprint 23 S4, D-46): schedule, cancel and legal hold.
	// tenant.* is super-admin only (D-06).
	ActionTenantDelete       = "tenant.delete"
	ActionTenantDeleteCancel = "tenant.delete.cancel"
	ActionTenantLegalHoldSet = "tenant.legal_hold.set"
	// ActionTenantPurged is the system-actor provider audit row a completed
	// purge writes (not a permission).
	ActionTenantPurged = "tenant.purged"
	// Plans (Sprint 23 B1, D-43). Plans are commercial/tenant data: not
	// under relay.*, so decide() keeps them super-admin only (D-06), and
	// assigning a tenant's plan is a tenant.* action.
	ActionPlanRead         = "plan.read"
	ActionPlanCreate       = "plan.create"
	ActionPlanUpdate       = "plan.update"
	ActionPlanDelete       = "plan.delete"
	ActionPlanSetDefault   = "plan.set_default"
	ActionTenantPlanAssign = "tenant.plan.assign"

	// Quotas (Sprint 23 S3, D-47): a per-tenant quota override. A tenant.*
	// action, so super-admin only (D-06).
	ActionTenantQuotaSet = "tenant.quota.set"
	// Tenant creation (Sprint 23 S2, D-44): create a workspace and invite
	// its first admin. tenant.* is super-admin only (D-06).
	ActionTenantCreate = "tenant.create"
	// Re-issue the first-admin invitation of a tenant whose first admin
	// never signed in (S2, D-44).
	ActionTenantFirstAdminReinvite = "tenant.first_admin.reinvite"
)

// Alerts (Sprint 24 A1, D-50, D-53). alert.read: super-admins see every
// alert; relay-ops see platform relay and pool alerts only (the handler
// filters by kind, never showing tenant alerts or tenant names, D-40). Rule
// management (read included) is super-admin only.
const (
	ActionAlertRead       = "alert.read"
	ActionAlertRuleRead   = "alert.rule.read"
	ActionAlertRuleCreate = "alert.rule.create"
	ActionAlertRuleUpdate = "alert.rule.update"
	ActionAlertRuleDelete = "alert.rule.delete"

	// Delivery channels (Sprint 24 A2, D-52, D-53): super-admin only.
	ActionAlertChannelRead   = "alert.channel.read"
	ActionAlertChannelCreate = "alert.channel.create"
	ActionAlertChannelUpdate = "alert.channel.update"
	ActionAlertChannelDelete = "alert.channel.delete"
	ActionAlertChannelTest   = "alert.channel.test"
	ActionAlertChannelRotate = "alert.channel.rotate_secret"
)

// ActionTimelineRelayAudit (Sprint 24 T1, D-55) lets the incident timeline
// show relay and pool operation audit rows (RelayOpsTimelineAuditActions
// only, with actor and reason, never the raw details). It is not audit.view:
// the full provider audit log stays super-admin only. Deliberately outside
// relay.*, so the grant to relay-ops is explicit (roleActions).
const ActionTimelineRelayAudit = "timeline.relay_audit"

// RelayOpsTimelineAuditActions are the provider audit actions a relay-ops
// timeline may show (D-55): relay and pool operations only. An allowlist on
// purpose: relay.placement_rejected targets a relay but records a tenant
// (a tenant event), and tenant.relay_pool.assign targets a tenant; a new
// relay or pool action is hidden from relay-ops until it is added here.
var RelayOpsTimelineAuditActions = []string{
	ActionRelayCreate,
	ActionRelayIssueToken,
	ActionRelayRevoke,
	ActionRelayDelete,
	ActionRelayDrain,
	ActionRelayResume,
	ActionRelayMove,
	ActionRelayCapacitySet,
	ActionRelayPoolCreate,
	ActionRelayPoolUpdate,
	ActionRelayPoolSetPlacement,
	ActionRelayPoolDelete,
	"relay_pool.failover", // relay.ActionPoolFailover (system)
	"relay_pool.failback", // relay.ActionPoolFailback (system)
}

// Metrics (Sprint 24 M1, F23, D-49; D-40 extended). Relay and fleet relay
// series are relay.* (relay-ops and super-admin); per-tenant series and
// fleet totals of tenant data are tenant.* (super-admin only). Two actions,
// so the matrix settles it without per-series logic there.
const (
	ActionRelayMetricsRead  = "relay.metrics.read"
	ActionTenantMetricsRead = "tenant.metrics.read"
)

// Usage (Sprint 25 U2, F33, D-60): usage is tenant data, so every usage read
// is tenant.usage.read (super-admin, and auditor and billing since D-73;
// relay-ops 403).
// A read of one tenant's usage is audited as tenant.usage.read (as a tenant
// detail view, OQ-1); the fleet list as one usage.fleet.read row per request.
// Both end in ".read", so the incident timeline leaves them out (T1).
const (
	ActionTenantUsageRead = "tenant.usage.read"
	ActionUsageFleetRead  = "usage.fleet.read"
)

// Billing (Sprint 26 F34, D-70): billing is tenant data, so every billing
// action was super-admin only (relay-ops 403; billing and auditor since D-73; none of them
// is in relayOpsAlsoAllowed). Card writes reuse the plan.* actions; reading
// the billing settings is plan.read (the Plans page needs the currency).
const (
	ActionBillingSettingsWrite  = "billing.settings.write"
	ActionBillingSettingsUpdate = "billing.settings.update" // audit action of a settings write
	ActionInvoiceRead           = "invoice.read"
	ActionInvoiceWrite          = "invoice.write"
	ActionInvoicePrune          = "invoice.prune" // audit action of the retention prune (D-67)
	// Audit actions of the invoice API (P3): reads, one row per request.
	ActionInvoiceFleetRead = "invoice.fleet.read"
	// Audit actions of the invoice writes (invoice.write), reason and
	// before/after each.
	ActionInvoiceIssue      = "invoice.issue"
	ActionInvoiceMarkPaid   = "invoice.mark_paid"
	ActionInvoiceVoid       = "invoice.void"
	ActionInvoiceRegenerate = "invoice.regenerate"
	ActionInvoiceResend     = "invoice.resend"
)

// Break-glass (Sprint 27 G1, D-75): the console banner lists unacknowledged
// recover-admin events and a super-admin acknowledges one with a note.
// Super-admin only (no role row lists them). break_glass.notified is the
// audit row of the email to super-admins (system actor).
const (
	ActionBreakGlassRead        = "break_glass.read"
	ActionBreakGlassAcknowledge = "break_glass.acknowledge"
	ActionBreakGlassNotified    = "break_glass.notified"
)

// Provider API keys (Sprint 27 K1, D-76): listing, creating and revoking
// keys. Super-admin only (no role row lists them), and only from a session:
// the key routes refuse a key in RequireProvider.
const (
	ActionAPIKeyRead   = "api_key.read"
	ActionAPIKeyCreate = "api_key.create"
	ActionAPIKeyRevoke = "api_key.revoke"
)

// actionClass says what an action constant is: a read, a write, or only the
// name of an audit row (never passed to decide()). Every Action* constant has
// exactly one entry (TestActionClass_EveryConstant); K1 uses it to refuse
// every write for an API key (IsWriteAction).
type actionClass int

const (
	classRead actionClass = iota + 1
	classWrite
	classAuditOnly
)

var actionClasses = map[string]actionClass{
	// relays and pools
	ActionRelayRead:              classRead,
	ActionRelayMetricsRead:       classRead,
	ActionRelayCreate:            classWrite,
	ActionRelayIssueToken:        classWrite,
	ActionRelayRevoke:            classWrite,
	ActionRelayDelete:            classWrite,
	ActionRelayDrain:             classWrite,
	ActionRelayResume:            classWrite,
	ActionRelayMove:              classWrite,
	ActionRelayCapacitySet:       classWrite,
	ActionRelayPoolCreate:        classWrite,
	ActionRelayPoolUpdate:        classWrite,
	ActionRelayPoolSetPlacement:  classWrite,
	ActionRelayPoolDelete:        classWrite,
	ActionRelayPlacementRejected: classAuditOnly,
	// tenants
	ActionTenantRead:               classRead,
	ActionTenantListRead:           classRead,
	ActionTenantMetricsRead:        classRead,
	ActionTenantUsageRead:          classRead,
	ActionTenantRelayPoolAssign:    classWrite,
	ActionTenantSuspend:            classWrite,
	ActionTenantResume:             classWrite,
	ActionTenantDelete:             classWrite,
	ActionTenantDeleteCancel:       classWrite,
	ActionTenantLegalHoldSet:       classWrite,
	ActionTenantQuotaSet:           classWrite,
	ActionTenantCreate:             classWrite,
	ActionTenantFirstAdminReinvite: classWrite,
	ActionTenantPlanAssign:         classWrite,
	ActionTenantPurged:             classAuditOnly,
	ActionUsageFleetRead:           classAuditOnly,
	// plans and billing
	ActionPlanRead:              classRead,
	ActionPlanCreate:            classWrite,
	ActionPlanUpdate:            classWrite,
	ActionPlanDelete:            classWrite,
	ActionPlanSetDefault:        classWrite,
	ActionBillingSettingsWrite:  classWrite,
	ActionBillingSettingsUpdate: classAuditOnly,
	ActionInvoiceRead:           classRead,
	ActionInvoiceWrite:          classWrite,
	ActionInvoicePrune:          classAuditOnly,
	ActionInvoiceFleetRead:      classAuditOnly,
	ActionInvoiceIssue:          classAuditOnly,
	ActionInvoiceMarkPaid:       classAuditOnly,
	ActionInvoiceVoid:           classAuditOnly,
	ActionInvoiceRegenerate:     classAuditOnly,
	ActionInvoiceResend:         classAuditOnly,
	// alerts and timeline
	ActionAlertRead:          classRead,
	ActionAlertRuleRead:      classRead,
	ActionAlertChannelRead:   classRead,
	ActionTimelineRelayAudit: classRead,
	ActionAlertRuleCreate:    classWrite,
	ActionAlertRuleUpdate:    classWrite,
	ActionAlertRuleDelete:    classWrite,
	ActionAlertChannelCreate: classWrite,
	ActionAlertChannelUpdate: classWrite,
	ActionAlertChannelDelete: classWrite,
	ActionAlertChannelTest:   classWrite,
	ActionAlertChannelRotate: classWrite,
	// platform
	ActionAuditView:          classRead,
	ActionCertRead:           classRead,
	ActionHealthRead:         classRead,
	ActionProviderUserManage: classWrite,
	// break-glass (G1)
	ActionBreakGlassRead:        classRead,
	ActionBreakGlassAcknowledge: classWrite,
	ActionBreakGlassNotified:    classAuditOnly,
	// API keys (K1)
	ActionAPIKeyRead:   classRead,
	ActionAPIKeyCreate: classWrite,
	ActionAPIKeyRevoke: classWrite,
}

// IsWriteAction reports whether action is anything but a classified read.
// Unknown actions count as writes (fail closed): an API key (K1, D-76) may
// only ever be allowed a read.
func IsWriteAction(action string) bool {
	return actionClasses[action] != classRead
}

// actionSet is a set of action constants.
type actionSet map[string]bool

func newActionSet(actions ...string) actionSet {
	s := make(actionSet, len(actions))
	for _, a := range actions {
		s[a] = true
	}
	return s
}

// roleActions is the role matrix (D-72): what each role other than
// super-admin may do, as explicit lists. An action missing from a role's
// list is denied to it; super-admin is allowed everything (decide()).
// TestDecide_FullTable pins every action × role outcome.
var roleActions = map[string]actionSet{
	// relay-ops: exactly the pre-O1 outcome (D-05, D-31, D-41, D-55): the
	// relay.* namespace except revoke, delete and set_placement, plus
	// alert.read (platform relay and pool alerts, filtered by the alerting
	// handlers) and timeline.relay_audit. relay.placement_rejected is only
	// an audit-row name; it is listed because the old prefix rule allowed
	// it, and the table must equal the old outcome.
	RoleRelayOps: newActionSet(
		ActionRelayRead,
		ActionRelayMetricsRead,
		ActionRelayCreate,
		ActionRelayIssueToken,
		ActionRelayDrain,
		ActionRelayResume,
		ActionRelayMove,
		ActionRelayCapacitySet,
		ActionRelayPoolCreate,
		ActionRelayPoolUpdate,
		ActionRelayPoolDelete,
		ActionRelayPlacementRejected,
		ActionAlertRead,
		ActionTimelineRelayAudit,
	),
	// auditor (D-73): every read, no write. Tenant reads are audited as for
	// super-admins (OQ-1). Operator management stays super-admin only.
	RoleAuditor: newActionSet(
		ActionRelayRead,
		ActionRelayMetricsRead,
		ActionTenantRead,
		ActionTenantListRead,
		ActionTenantMetricsRead,
		ActionTenantUsageRead,
		ActionPlanRead, // plan cards and billing settings
		ActionInvoiceRead,
		ActionAlertRead,
		ActionAlertRuleRead,
		ActionAlertChannelRead,
		ActionTimelineRelayAudit,
		ActionAuditView,
		ActionCertRead,
		ActionHealthRead,
	),
	// billing (D-73): plan cards (create, edit), billing settings, invoices,
	// usage, the tenant list and tenant plan assignment. No relay, pool,
	// tenant lifecycle, quota, alert or operator action; plan delete and
	// set-default stay super-admin only.
	RoleBilling: newActionSet(
		ActionPlanRead,
		ActionPlanCreate,
		ActionPlanUpdate,
		ActionTenantPlanAssign,
		ActionBillingSettingsWrite,
		ActionInvoiceRead,
		ActionInvoiceWrite,
		ActionTenantUsageRead,
		ActionTenantListRead,
	),
	// support (D-73): tenant reads (list; detail with agents, audited), the
	// tenants' alerts and timeline (tenant scope, filtered by the alerting
	// handlers; no audit rows), health. No billing, usage, plan cards,
	// provider audit log or writes.
	RoleSupport: newActionSet(
		ActionTenantRead,
		ActionTenantListRead,
		ActionAlertRead,
		ActionHealthRead,
	),
}

// ErrForbidden is returned by decide() when the actor may not perform the
// action. Handlers map this to HTTP 403; any other error maps to 500.
var ErrForbidden = errors.New("provider action forbidden")

// ActorKindAPIKey marks an Actor authenticated by a provider API key (K1,
// D-76). The zero Kind is a person with a provider session.
const ActorKindAPIKey = "api_key"

// Actor is the verified provider identity, injected by RequireProvider.
//
// For a person: UserID, Email, Role from provider_users. For an API key:
// Kind = ActorKindAPIKey, UserID empty, Email = "api_key:<name> (<prefix>)"
// (the audit actor), Role = the key's role, and the Key* fields.
type Actor struct {
	UserID string
	Email  string
	Role   string

	Kind      string
	KeyID     string
	KeyName   string
	KeyPrefix string
}

// IsAPIKey reports whether the actor is an API key, not a person.
func (a Actor) IsAPIKey() bool { return a.Kind == ActorKindAPIKey }

// AuditUserID is the provider_user_id of the actor's audit rows: nil for an
// API key (it is not a provider user).
func (a Actor) AuditUserID() *string {
	if a.IsAPIKey() || a.UserID == "" {
		return nil
	}
	id := a.UserID
	return &id
}

// AuditDetails returns details for one of the actor's audit rows: for an API
// key a copy with key_id added (never the secret or its hash); otherwise d
// unchanged.
func (a Actor) AuditDetails(d map[string]any) map[string]any {
	if !a.IsAPIKey() {
		return d
	}
	out := make(map[string]any, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	out["key_id"] = a.KeyID
	return out
}

// Target identifies what is being acted on. Fields are unused by the alpha
// policy but carried NOW so partner-scoping (matching target ownership) becomes
// a decide() change later, not a signature change across every call site.
type Target struct {
	Type string
	ID   string
}

// Authz is the provider authorization chokepoint. Every provider operation asks
// exactly one of these; all of them funnel through decide().
type Authz struct{}

func NewAuthz() *Authz { return &Authz{} }

func (a *Authz) CanCreateRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayCreate, target)
}

func (a *Authz) CanIssueProvisioningToken(actor Actor, target Target) error {
	return decide(actor, ActionRelayIssueToken, target)
}

func (a *Authz) CanDeleteRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayDelete, target)
}

func (a *Authz) CanRevokeRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayRevoke, target)
}

func (a *Authz) CanDrainRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayDrain, target)
}

func (a *Authz) CanResumeRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayResume, target)
}

func (a *Authz) CanManageProviderUser(actor Actor, target Target) error {
	return decide(actor, ActionProviderUserManage, target)
}

func (a *Authz) CanViewProviderAudit(actor Actor) error {
	return decide(actor, ActionAuditView, Target{Type: "audit"})
}

// CanCreateRelayPool gates POST /provider/relay-pools (relay-ops, D-41).
func (a *Authz) CanCreateRelayPool(actor Actor, target Target) error {
	return decide(actor, ActionRelayPoolCreate, target)
}

// CanUpdateRelayPool gates PATCH /provider/relay-pools/{id}: renaming is
// relay-ops; a change of region or residency is super-admin only (D-41).
func (a *Authz) CanUpdateRelayPool(actor Actor, target Target, touchesPlacement bool) error {
	if touchesPlacement {
		return decide(actor, ActionRelayPoolSetPlacement, target)
	}
	return decide(actor, ActionRelayPoolUpdate, target)
}

// CanDeleteRelayPool gates DELETE /provider/relay-pools/{id} (relay-ops).
func (a *Authz) CanDeleteRelayPool(actor Actor, target Target) error {
	return decide(actor, ActionRelayPoolDelete, target)
}

// CanMoveRelay gates POST /provider/relays/{id}/pool (relay-ops, D-41).
func (a *Authz) CanMoveRelay(actor Actor, target Target) error {
	return decide(actor, ActionRelayMove, target)
}

// CanAssignTenantRelayPool gates PUT /provider/tenants/{id}/relay-pool
// (super-admin only: tenant.*, D-06, D-41).
func (a *Authz) CanAssignTenantRelayPool(actor Actor, target Target) error {
	return decide(actor, ActionTenantRelayPoolAssign, target)
}

// CanSuspendTenant gates POST /provider/tenants/{id}/suspend (super-admin
// only: tenant.*, D-06, D-45).
func (a *Authz) CanSuspendTenant(actor Actor, target Target) error {
	return decide(actor, ActionTenantSuspend, target)
}

// CanDeleteTenant gates POST /provider/tenants/{id}/delete (super-admin).
func (a *Authz) CanDeleteTenant(actor Actor, target Target) error {
	return decide(actor, ActionTenantDelete, target)
}

// CanCancelTenantDelete gates POST /provider/tenants/{id}/delete/cancel.
func (a *Authz) CanCancelTenantDelete(actor Actor, target Target) error {
	return decide(actor, ActionTenantDeleteCancel, target)
}

// CanSetTenantLegalHold gates PUT /provider/tenants/{id}/legal-hold.
func (a *Authz) CanSetTenantLegalHold(actor Actor, target Target) error {
	return decide(actor, ActionTenantLegalHoldSet, target)
}

// CanResumeTenant gates POST /provider/tenants/{id}/resume (super-admin only).
func (a *Authz) CanResumeTenant(actor Actor, target Target) error {
	return decide(actor, ActionTenantResume, target)
}

// CanReadRelays gates GET /provider/relays and /provider/relays/{id}.
// CanSetRelayCapacity reports whether actor may set a relay's link capacity
// (relay.capacity.set: relay-ops and super-admin).
func (a *Authz) CanSetRelayCapacity(actor Actor, target Target) error {
	return decide(actor, ActionRelayCapacitySet, target)
}

// CanReadPlans reports whether actor may read plans (plan.read).
func (a *Authz) CanReadPlans(actor Actor) error {
	return decide(actor, ActionPlanRead, Target{Type: "plan"})
}

// CanManagePlans reports whether actor may perform a plan write
// (ActionPlanCreate, ActionPlanUpdate, ActionPlanDelete, ActionPlanSetDefault
// or ActionTenantPlanAssign).
func (a *Authz) CanManagePlans(actor Actor, action string, target Target) error {
	return decide(actor, action, target)
}

// CanReadBillingSettings gates GET /provider/billing/settings (plan.read:
// super-admin, auditor, billing).
func (a *Authz) CanReadBillingSettings(actor Actor) error {
	return decide(actor, ActionPlanRead, Target{Type: "billing_settings"})
}

// CanWriteBillingSettings gates PATCH /provider/billing/settings
// (billing.settings.write: super-admin, billing; D-70, D-73).
func (a *Authz) CanWriteBillingSettings(actor Actor) error {
	return decide(actor, ActionBillingSettingsWrite, Target{Type: "billing_settings"})
}

// CanReadInvoices gates the invoice reads (invoice.read: super-admin,
// auditor, billing; D-70, D-73).
func (a *Authz) CanReadInvoices(actor Actor) error {
	return decide(actor, ActionInvoiceRead, Target{Type: "invoice"})
}

// CanWriteInvoices gates issue, mark paid, void, regenerate and resend
// (invoice.write: super-admin, billing; D-70, D-73).
func (a *Authz) CanWriteInvoices(actor Actor, target Target) error {
	return decide(actor, ActionInvoiceWrite, target)
}

// CanSetTenantQuota gates PUT /provider/tenants/{id}/quotas/{key}
// (super-admin only: tenant.*, D-06, D-47).
func (a *Authz) CanSetTenantQuota(actor Actor, target Target) error {
	return decide(actor, ActionTenantQuotaSet, target)
}

// CanCreateTenant gates POST /provider/tenants (super-admin only: tenant.*,
// D-06, D-44).
func (a *Authz) CanCreateTenant(actor Actor) error {
	return decide(actor, ActionTenantCreate, Target{Type: "tenant"})
}

// CanReinviteFirstAdmin gates POST /provider/tenants/{id}/first-admin-invitation
// (super-admin only: tenant.*).
func (a *Authz) CanReinviteFirstAdmin(actor Actor, target Target) error {
	return decide(actor, ActionTenantFirstAdminReinvite, target)
}

func (a *Authz) CanReadRelays(actor Actor, target Target) error {
	return decide(actor, ActionRelayRead, target)
}

// CanReadTenants gates GET /provider/tenants/{id} and other tenant detail
// (tenant.read: super-admin, auditor, support).
func (a *Authz) CanReadTenants(actor Actor, target Target) error {
	return decide(actor, ActionTenantRead, target)
}

// CanListTenants gates GET /provider/tenants (tenant.list.read: also
// billing, D-73).
func (a *Authz) CanListTenants(actor Actor) error {
	return decide(actor, ActionTenantListRead, Target{Type: "tenant"})
}

// CanReadAlerts gates GET /provider/alerts and /provider/alert-events
// (super-admin; relay-ops filtered to relay and pool alerts).
// CanReadMetricSeries gates GET /provider/metrics/{series}: tenantData
// series need tenant.metrics.read (super-admin), the rest relay.metrics.read.
func (a *Authz) CanReadMetricSeries(actor Actor, series string, tenantData bool) error {
	if tenantData {
		return decide(actor, ActionTenantMetricsRead, Target{Type: "metric", ID: series})
	}
	return decide(actor, ActionRelayMetricsRead, Target{Type: "metric", ID: series})
}

// CanReadUsage gates GET /provider/usage and /provider/usage/tenants
// (tenant.usage.read: super-admin, auditor, billing; D-60, D-73).
func (a *Authz) CanReadUsage(actor Actor) error {
	return decide(actor, ActionTenantUsageRead, Target{Type: "usage"})
}

func (a *Authz) CanReadAlerts(actor Actor) error {
	return decide(actor, ActionAlertRead, Target{Type: "alert"})
}

// CanReadTimelineRelayAudit gates the relay and pool audit rows of the
// incident timeline (D-55): super-admin and relay-ops.
func (a *Authz) CanReadTimelineRelayAudit(actor Actor) error {
	return decide(actor, ActionTimelineRelayAudit, Target{Type: "timeline"})
}

// CanManageAlertRules gates the alert rule API (ActionAlertRuleRead,
// ActionAlertRuleCreate, ActionAlertRuleUpdate, ActionAlertRuleDelete;
// super-admin only, D-53).
func (a *Authz) CanManageAlertRules(actor Actor, action string, target Target) error {
	return decide(actor, action, target)
}

// CanManageAlertChannels gates the alert channel API (alert.channel.*;
// super-admin only, D-53).
func (a *Authz) CanManageAlertChannels(actor Actor, action string, target Target) error {
	return decide(actor, action, target)
}

// CanReadHealth gates GET /provider/health (super-admin, auditor, support).
func (a *Authz) CanReadHealth(actor Actor) error {
	return decide(actor, ActionHealthRead, Target{Type: "health"})
}

// CanManageBreakGlass gates the break-glass banner routes
// (ActionBreakGlassRead, ActionBreakGlassAcknowledge): super-admin only
// (D-75).
func (a *Authz) CanManageBreakGlass(actor Actor, action string) error {
	return decide(actor, action, Target{Type: "break_glass"})
}

// CanManageAPIKeys gates the API key routes (ActionAPIKeyRead,
// ActionAPIKeyCreate, ActionAPIKeyRevoke): super-admin only (D-76).
func (a *Authz) CanManageAPIKeys(actor Actor, action string) error {
	return decide(actor, action, Target{Type: "api_key"})
}

// CanReadCertificates gates GET /provider/certificates and GET /provider/pki.
func (a *Authz) CanReadCertificates(actor Actor) error {
	return decide(actor, ActionCertRead, Target{Type: "certificate"})
}

// decide is the ONE policy function (D-72): super-admin → every action;
// any other role → allowed iff roleActions lists the action for it. An
// action missing from the table (a new constant, an unknown string) is
// denied to every role except super-admin. An API key actor (K1, D-76) is
// refused every write action first, whatever its role. Replaces the pre-Sprint-27 prefix
// rule (D-05, D-31) with the same outcome for relay-ops.
//
// target is accepted but not yet consulted (flat model, no partner scoping).
func decide(actor Actor, action string, target Target) error {
	if actor.IsAPIKey() {
		// D-76: a key is read-only whatever its role, and never super-admin
		// (the DB refuses such a key too).
		if IsWriteAction(action) || actor.Role == RoleSuperAdmin {
			return fmt.Errorf("%w: api key may not %q", ErrForbidden, action)
		}
	} else if actor.Role == RoleSuperAdmin {
		return nil
	}
	if roleActions[actor.Role][action] {
		return nil
	}
	return fmt.Errorf("%w: role %q may not %q", ErrForbidden, actor.Role, action)
}
