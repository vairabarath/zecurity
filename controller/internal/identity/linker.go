package identity

import (
	"context"
	"errors"
)

// ProvisionInput carries what a Provisioner needs to JIT-create a canonical
// user for a never-seen external identity. Email is an invite-matching hint
// ONLY (never an identity key — ADR-024). WorkspaceName is the display name for
// a brand-new workspace on first-time signup; it is ignored for invited joins.
type ProvisionInput struct {
	Email         string
	Provider      string
	Subject       string
	Name          string
	ConnectionID  string
	Issuer        string
	WorkspaceName string
	// ConnectionTenantID is the workspace that OWNS the connection this login
	// came through: set for an ENTERPRISE (BYO) connection, nil for the shared
	// platform tier.
	//
	// It decides whether a first-seen identity may be provisioned at all. On the
	// platform tier a first-time signup legitimately creates a workspace. On an
	// enterprise connection it must NOT: the connection already belongs to one
	// workspace, so anyone in the customer's IdP directory would otherwise get a
	// brand-new Zecurity workspace (as its ADMIN) just by signing in. Such a
	// user joins the owning workspace if invited/provisioned, else is refused
	// with ErrNotInvited.
	ConnectionTenantID *string
}

// ErrNotInvited is returned when a cryptographically proven identity arrives
// through an ENTERPRISE connection but has no user in that workspace and no
// pending invite. The IdP vouched for who they are; it does not follow that
// this workspace has granted them access.
var ErrNotInvited = errors.New("identity is not a member of this workspace and has no pending invitation")

// Provisioner JIT-creates a canonical user (and its external_identities link, in
// one transaction) for an identity the Resolver did not find. Implemented by
// internal/bootstrap.Service (workspace-creating) for the web flow.
type Provisioner interface {
	Provision(ctx context.Context, in ProvisionInput) (*PrincipalCore, error)
}

// Linker decides what to do with a never-seen external identity. Per ADR-024 it
// NEVER auto-merges by email: an unseen (connection, subject) always yields a
// NEW canonical user (or an invited join), created via the Provisioner. Options
// (B) admin-approved link and (C) verification-required are documented
// follow-ups, deliberately not shipped this sprint.
type Linker struct{ prov Provisioner }

// NewLinker wires a Linker over a Provisioner.
func NewLinker(p Provisioner) *Linker { return &Linker{prov: p} }

// Link JIT-creates the canonical user for a first-seen external identity.
func (l *Linker) Link(ctx context.Context, in ProvisionInput) (*PrincipalCore, error) {
	return l.prov.Provision(ctx, in)
}
