package provider

import (
	"errors"
	"fmt"
	"strings"
)

// Provider roles (matches the CHECK constraint in migration 025).
const (
	RoleSuperAdmin = "super-admin"
	RoleRelayOps   = "relay-ops"
)

// Provider action verbs. Dotted, namespaced by target type so a role can be
// granted a whole namespace (e.g. relay-ops → "relay.*").
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
	ActionCertRead   = "cert.read"
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
// relay.*, so the grant to relay-ops is explicit (relayOpsAlsoAllowed).
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
// so decide()'s prefix rules settle it without per-series logic there.
const (
	ActionRelayMetricsRead  = "relay.metrics.read"
	ActionTenantMetricsRead = "tenant.metrics.read"
)

// Usage (Sprint 25 U2, F33, D-60): usage is tenant data, so every usage read
// is tenant.usage.read (super-admin only by decide()'s rule; relay-ops 403).
// A read of one tenant's usage is audited as tenant.usage.read (as a tenant
// detail view, OQ-1); the fleet list as one usage.fleet.read row per request.
// Both end in ".read", so the incident timeline leaves them out (T1).
const (
	ActionTenantUsageRead = "tenant.usage.read"
	ActionUsageFleetRead  = "usage.fleet.read"
)

// Billing (Sprint 26 F34, D-70): billing is tenant data, so every billing
// action is super-admin only by decide()'s rule (relay-ops 403; none of them
// is in relayOpsAlsoAllowed). Card writes reuse the plan.* actions; reading
// the billing settings is plan.read (the Plans page needs the currency).
const (
	ActionBillingSettingsWrite  = "billing.settings.write"
	ActionBillingSettingsUpdate = "billing.settings.update" // audit action of a settings write
	ActionInvoiceRead           = "invoice.read"
	ActionInvoiceWrite          = "invoice.write"
)

// relayOpsAlsoAllowed are the actions outside relay.* that relay-ops may
// perform.
var relayOpsAlsoAllowed = map[string]bool{
	ActionAlertRead:          true,
	ActionTimelineRelayAudit: true,
}

// superAdminOnlyRelayActions are the relay.* verbs relay-ops may NOT perform:
// the destructive ones (D-31), and setting a pool's region or residency (D-41).
var superAdminOnlyRelayActions = map[string]bool{
	ActionRelayRevoke:           true,
	ActionRelayDelete:           true,
	ActionRelayPoolSetPlacement: true,
}

// ErrForbidden is returned by decide() when the actor may not perform the
// action. Handlers map this to HTTP 403; any other error maps to 500.
var ErrForbidden = errors.New("provider action forbidden")

// Actor is the verified provider identity, injected by RequireProvider.
type Actor struct {
	UserID string
	Email  string
	Role   string
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
// super-admin only).
func (a *Authz) CanReadBillingSettings(actor Actor) error {
	return decide(actor, ActionPlanRead, Target{Type: "billing_settings"})
}

// CanWriteBillingSettings gates PATCH /provider/billing/settings
// (billing.settings.write: super-admin only, D-70).
func (a *Authz) CanWriteBillingSettings(actor Actor) error {
	return decide(actor, ActionBillingSettingsWrite, Target{Type: "billing_settings"})
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

// CanReadTenants gates GET /provider/tenants and /provider/tenants/{id}.
func (a *Authz) CanReadTenants(actor Actor, target Target) error {
	return decide(actor, ActionTenantRead, target)
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
// (tenant.usage.read: super-admin only; D-60).
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

// CanReadHealth gates GET /provider/health (super-admin only).
func (a *Authz) CanReadHealth(actor Actor) error {
	return decide(actor, ActionHealthRead, Target{Type: "health"})
}

// CanReadCertificates gates GET /provider/certificates and GET /provider/pki.
func (a *Authz) CanReadCertificates(actor Actor) error {
	return decide(actor, ActionCertRead, Target{Type: "certificate"})
}

// decide is the ONE policy function. Matrix:
//
//	super-admin → every action
//	relay-ops   → the "relay.*" namespace, EXCEPT relay.revoke, relay.delete
//	              and relay.pool.set_placement; plus alert.read (Sprint 24)
//
// The exception is Decision Record D-31 (amends D-05, Sprint 22 Phase A):
// relay-ops may create relays, issue provisioning tokens, read (and later
// drain/resume) relays, but revoking or deleting one is super-admin only.
//
// target is accepted but not yet consulted (flat model, no partner scoping).
func decide(actor Actor, action string, target Target) error {
	switch actor.Role {
	case RoleSuperAdmin:
		return nil
	case RoleRelayOps:
		if strings.HasPrefix(action, "relay.") && !superAdminOnlyRelayActions[action] {
			return nil
		}
		if relayOpsAlsoAllowed[action] {
			return nil
		}
	}
	return fmt.Errorf("%w: role %q may not %q", ErrForbidden, actor.Role, action)
}
