// Package providers defines the provider-agnostic authentication contract used
// by the identity federation layer (PENDING-04, ADR-022). An IdentityProvider
// knows one authentication protocol (OIDC today; SAML/broker later) and nothing
// about users, linking, or sessions. It returns a normalized
// AuthenticationContext; the rest of Zecurity never learns which IdP was used.
//
// This is a leaf package: it must not import internal/auth (or anything above
// it) so both internal/auth and the graph resolvers can depend on it without an
// import cycle.
package providers

import "context"

// AuthenticationContext is the normalized result of a successful authentication.
// It is intentionally extensible: provider-specific claims live in RawClaims so
// new IdPs (which surface different claims like `hd`, `department`,
// `employeeType`) never force a change to this struct's shape.
//
// Boundary (ADR-022): this describes WHO authenticated and HOW — it is NOT an
// authorization decision. In particular GroupsHint is a transient login-time
// hint only and must never be fed to the policy engine as an effective
// authorization set.
type AuthenticationContext struct {
	Provider      string // adapter/provider label, e.g. "google", "oidc"
	Issuer        string // OIDC issuer (iss)
	Subject       string // immutable per-issuer subject (sub) — the identity anchor
	Email         string
	Name          string
	EmailVerified bool

	// Authentication context (feeds PENDING-06 step-up / PENDING-08-09 trust).
	AMR      []string // authentication methods references
	ACR      string   // authentication context class reference
	AuthTime int64    // seconds since epoch of the authentication event

	// GroupsHint is a TRANSIENT login-time hint ONLY — never the effective
	// authorization set. Effective groups come from the internal model / SCIM.
	GroupsHint []string

	// RawClaims carries provider-specific claims so this struct never has to
	// grow a field per IdP. Optional; may be nil.
	RawClaims map[string]any
}

// AuthURLParams are the per-login values the caller injects into the provider's
// authorization-redirect URL. The caller owns PKCE + nonce generation and their
// storage (keyed by State); the adapter only assembles the URL.
type AuthURLParams struct {
	State         string // CSRF/lookup key that survives the redirect
	Nonce         string // OIDC nonce (empty for providers that don't use it)
	CodeChallenge string // PKCE S256 challenge (base64url)
	RedirectURI   string // where the IdP sends the code back
}

// IdentityProvider is one authentication protocol adapter. Implementations are
// "protocol only": they build the authorization-redirect URL and, on the way
// back, exchange the code and verify the token — returning normalized claims.
// They know nothing about users, linking, or sessions.
type IdentityProvider interface {
	// AuthURL builds the IdP authorization-redirect URL. It may perform OIDC
	// discovery (hence ctx + error); static providers ignore ctx.
	AuthURL(ctx context.Context, p AuthURLParams) (string, error)

	// Authenticate performs the provider's code-exchange + token verification
	// and returns provider-agnostic claims. redirectURI, when non-empty,
	// overrides the adapter's default (the CLI supplies a loopback URI).
	// expectedNonce, when non-empty, must equal the token's nonce claim.
	Authenticate(ctx context.Context, code, codeVerifier, redirectURI, expectedNonce string) (*AuthenticationContext, error)
}
