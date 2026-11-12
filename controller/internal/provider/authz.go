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
)

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

// CanReadRelays gates GET /provider/relays and /provider/relays/{id}.
func (a *Authz) CanReadRelays(actor Actor, target Target) error {
	return decide(actor, ActionRelayRead, target)
}

// CanReadTenants gates GET /provider/tenants and /provider/tenants/{id}.
func (a *Authz) CanReadTenants(actor Actor, target Target) error {
	return decide(actor, ActionTenantRead, target)
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
//	              and relay.pool.set_placement
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
	}
	return fmt.Errorf("%w: role %q may not %q", ErrForbidden, actor.Role, action)
}
