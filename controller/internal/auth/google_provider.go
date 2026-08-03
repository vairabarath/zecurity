package auth

import (
	"context"
	"net/url"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
)

// GoogleProvider is the Bootstrap-tier Google adapter (a managed platform IdP,
// PENDING-04 / ADR-024). It is bound to a Google OAuth client (env-sourced) and
// implements providers.IdentityProvider by reusing the existing Google exchange
// (googleTokenExchange) and verification (VerifyGoogleIDToken) — so the Google
// login path stays behavior-identical while flowing through the adapter seam.
type GoogleProvider struct {
	clientID     string
	clientSecret string
}

// NewGoogleProvider binds a Google adapter to a client credential pair. The web
// server and the CLI pass their own (distinct) Google client credentials.
func NewGoogleProvider(clientID, clientSecret string) *GoogleProvider {
	return &GoogleProvider{clientID: clientID, clientSecret: clientSecret}
}

// AuthURL builds the Google authorization URL. Nonce is intentionally omitted:
// Google's flow is protected by PKCE + signed state and VerifyGoogleIDToken does
// not consume a nonce, so this stays identical to the pre-federation URL.
//
// TODO(PENDING-04): converge Google onto OIDC nonce validation so every provider
// verifies id_token nonce uniformly (requires VerifyGoogleIDToken to accept and
// check an expected nonce). Deferred to keep the Google path byte-identical here.
func (g *GoogleProvider) AuthURL(_ context.Context, params providers.AuthURLParams) (string, error) {
	q := url.Values{}
	q.Set("client_id", g.clientID)
	q.Set("redirect_uri", params.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("code_challenge", params.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", params.State)
	return "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode(), nil
}

// Authenticate exchanges the code with Google and verifies the id_token, then
// maps the Google claims onto the neutral AuthenticationContext. expectedNonce
// is ignored — Google's flow does not use an OIDC nonce (see AuthURL).
func (g *GoogleProvider) Authenticate(ctx context.Context, code, codeVerifier, redirectURI, _ string) (*providers.AuthenticationContext, error) {
	tok, err := googleTokenExchange(ctx, code, codeVerifier, redirectURI, g.clientID, g.clientSecret)
	if err != nil {
		return nil, err
	}
	claims, err := VerifyGoogleIDToken(ctx, tok.IDToken, g.clientID)
	if err != nil {
		return nil, err
	}
	return googleClaimsToAuthContext(claims), nil
}

// googleClaimsToAuthContext normalizes verified Google id_token claims into the
// provider-agnostic AuthenticationContext. Google does not populate amr/acr in
// our claims struct; those remain empty until an IdP that emits them is wired.
func googleClaimsToAuthContext(c *GoogleClaims) *providers.AuthenticationContext {
	var authTime int64
	if c.IssuedAt != nil {
		authTime = c.IssuedAt.Unix()
	}
	return &providers.AuthenticationContext{
		Provider:      "google",
		Issuer:        c.Issuer,
		Subject:       c.Sub,
		Email:         c.Email,
		Name:          c.Name,
		EmailVerified: c.EmailVerified,
		AuthTime:      authTime,
	}
}
