// Package auth implements Zecurity's human authentication pipeline: it turns an
// external IdP login (Google/OIDC today) into a verified AuthenticationContext,
// then hands off to bootstrap/the identity pipeline. It does NOT decide identity
// linking, sessions beyond issuance, or authorization — those live above it.
//
// This file documents the AUTHENTICATION INVARIANTS established by PENDING-04
// (ADR-023 Identity Philosophy, ADR-024 Identity Linking). They are intentional
// architectural boundaries. Changing any of them is a security-relevant decision
// that must be reviewed against the ADRs — not an incidental refactor.
//
// # 1. Adapter purity (protocol only)
//
// The internal/auth/providers package is a LEAF: it imports only the standard
// library + the JWT library, never internal/idp, a database, sessions, graph, or
// the user model. An IdentityProvider adapter does exactly: build AuthURL,
// exchange the code, verify the token, return *providers.AuthenticationContext.
// It must never look up or create a user, touch a session, or know about tenants.
// AuthenticationContext is provider-neutral (no GoogleClaims, no TenantID/UserID).
//
// # 2. Single provider switch point
//
// providerFor (idp_adapter.go) is the ONLY place that maps a connection to an
// adapter. There is no `if google / else oidc` branching anywhere else. Login
// handlers call providerForFn and receive an IdentityProvider — they never
// construct adapters inline. New protocols are added in providerFor, nowhere else.
//
// # 3. Centralized connection resolution
//
// resolveConnection is the single entry point for turning (provider, connectionID)
// into an *idp.Connection. Connection lookups live in the idp store; the auth
// service never queries identity_connections directly.
//
// # 4. The callback does not trust the browser
//
// On /auth/callback the ONLY values taken from the request are `code` and
// `state`. Everything that decides what happens — the PKCE verifier, the OIDC
// nonce, and the connection_id that selects the adapter — is read from the Redis
// scratchpad keyed by the HMAC-verified `state`. The connection (hence the
// adapter) is ALWAYS re-resolved server-side from the stored connection_id, never
// from anything the browser returns. providerFor is therefore called twice
// (InitiateAuth + callback), by design.
//
// # 5. Nonce integrity (end to end)
//
// The OIDC nonce is generated once in InitiateAuth, stored in the scratchpad, and
// the SAME value is passed as expectedNonce at the callback. It is never
// regenerated or derived. OIDC adapters enforce token.nonce == expectedNonce;
// the Google adapter currently omits it (documented TODO in google_provider.go).
// The PKCE state / scratchpad is single-use (GetDel), so a replayed callback
// finds nothing.
//
// # 6. Fail closed
//
// If the connection is deleted or disabled during the redirect window, or the
// adapter returns any error, authentication fails closed (redirect to
// /login?error=authentication_failed). There is no partial-auth path.
//
// # 7. The boundary is AuthenticationContext
//
// The callback stops at AuthenticationContext and hands to bootstrap.Bootstrap
// (Phase 5 replaces its internals with the identity pipeline: resolve -> link ->
// lifecycle -> principal -> session). Adapters produce identity CLAIMS; user
// creation, linking, lifecycle, and session generation happen ABOVE the adapter.
//
// # 8. Secrets never leak outward
//
// A workspace (Enterprise) OIDC client_secret is encrypted at rest
// (pki.EncryptSecret, context "idp-client-secret:"+tenantID), decrypted only
// inside the idp store, held in Connection.ClientSecret in-process, and MUST NOT
// be serialized into any API response (discovery, GraphQL, logs).
//
// # 9. GroupsHint is a hint, not authorization
//
// AuthenticationContext.GroupsHint is a transient, login-time value. It must NEVER
// be written to anything the policy engine reads. Effective group membership comes
// from the internal model / SCIM (PENDING-05), not from IdP token claims.
//
// # 10. Two-tier connection model
//
// identity_connections.tenant_id IS NULL means a Bootstrap (platform) IdP —
// provider-owned, env-sourced credentials, for workspace creation / recovery /
// break-glass, not routine employee auth. A set tenant_id means an Enterprise
// (workspace) IdP — admin-owned, secret encrypted at rest. Adding an Enterprise
// IdP never silently hides Bootstrap login (no lockout, ADR-024 §5).
//
// # 11. Identity is keyed on (connection, issuer, subject), never email
//
// External identities are keyed on the immutable per-issuer subject, scoped per
// tenant/connection — never on email, which can change or be reassigned. See
// ADR-024 §1.
package auth
