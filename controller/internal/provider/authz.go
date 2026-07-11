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
)

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

func (a *Authz) CanManageProviderUser(actor Actor, target Target) error {
	return decide(actor, ActionProviderUserManage, target)
}

func (a *Authz) CanViewProviderAudit(actor Actor) error {
	return decide(actor, ActionAuditView, Target{Type: "audit"})
}

// decide is the ONE policy function. Alpha matrix:
//
//	super-admin → every action
//	relay-ops   → the "relay.*" namespace only
//
// target is accepted but not yet consulted (flat model, no partner scoping).
func decide(actor Actor, action string, target Target) error {
	switch actor.Role {
	case RoleSuperAdmin:
		return nil
	case RoleRelayOps:
		if strings.HasPrefix(action, "relay.") {
			return nil
		}
	}
	return fmt.Errorf("%w: role %q may not %q", ErrForbidden, actor.Role, action)
}
