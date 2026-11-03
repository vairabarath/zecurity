// Package identity is the identity pipeline (PENDING-04 / ADR-023, ADR-024): the
// stages between a protocol adapter (internal/auth/providers) and session
// minting. It turns a neutral AuthenticationContext into a resolved canonical
// user — resolve → lifecycle-gate → link/JIT-create → Principal — and emits the
// identity events that back the audit trail.
//
// It owns external_identities (the identity key) and canonical-user lifecycle.
// It does NOT own connection config (internal/idp) or the OIDC protocol
// (internal/auth/providers), and it never keys identity on email (ADR-024).
package identity

import "github.com/yourorg/ztna/controller/internal/auth/providers"

// PrincipalCore is the canonical-user half of an authenticated principal —
// independent of the authentication event that produced it. It is what the
// Resolver returns for a returning user and what the Provisioner creates for a
// new one. Generation is users.identity_generation at resolve time; it is
// stamped into the session so a later bump invalidates it (see revocation.go).
type PrincipalCore struct {
	UserID     string
	TenantID   string
	Role       string
	Email      string
	Status     string
	Generation int
}

// Principal is the pivot the pipeline produces before any session exists: a
// resolved canonical user (Core) plus the neutral authentication context that
// proved it this login. The Session Manager mints from this, and later Device
// Trust / Risk / Continuous Authz (PENDING-08/09) enrich it — none of them
// reshape the session, which only references the PrincipalID + generation.
type Principal struct {
	Core PrincipalCore
	Auth *providers.AuthenticationContext
}
