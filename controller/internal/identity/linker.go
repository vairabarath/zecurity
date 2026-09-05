package identity

import "context"

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
}

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
