package providers

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/auth/mapping"
)

// OIDCProvider is the generic OpenID Connect adapter (PENDING-04 `manual_oidc`).
// It is bound to one connection's config (issuer + client credentials) and is
// self-contained: OIDC discovery + a per-issuer JWKS cache + code exchange +
// id_token verification. It imports nothing from internal/auth, so the adapter
// layer stays a leaf.
type OIDCProvider struct {
	providerLabel string // AuthenticationContext.Provider, e.g. "oidc"
	issuer        string
	clientID      string
	clientSecret  string
	discoveryURL  string // optional override; else <issuer>/.well-known/openid-configuration
	scopes        string // space-delimited; defaults to "openid email profile"
	// subjectClaim is the OIDC claim (e.g. "sub", "email", "oid") the login
	// adapter reads to produce AuthenticationContext.Subject. Empty ⇒ legacy
	// default "sub" (ADR-025 §3.1). Set via SetSubjectClaim; not part of the
	// constructor signature so the discovery-only callers (TestIdpConnection)
	// are untouched.
	subjectClaim string

	http *http.Client
}

// NewOIDCProvider builds an adapter for one workspace OIDC connection.
func NewOIDCProvider(providerLabel, issuer, clientID, clientSecret, discoveryURL, scopes string) *OIDCProvider {
	if providerLabel == "" {
		providerLabel = "oidc"
	}
	if scopes == "" {
		scopes = "openid email profile"
	}
	return &OIDCProvider{
		providerLabel: providerLabel,
		issuer:        issuer,
		clientID:      clientID,
		clientSecret:  clientSecret,
		discoveryURL:  discoveryURL,
		scopes:        scopes,
		http:          &http.Client{Timeout: 10 * time.Second},
	}
}

// SetSubjectClaim configures the OIDC claim read at login to derive the
// canonical identity subject (ADR-025 §3.1). An empty value leaves the
// legacy default ("sub"). It is a post-construction setter so the
// discovery-only callers that build an OIDCProvider without a connection
// (e.g. the admin testIdpConnection OIDC probe) are unaffected.
func (p *OIDCProvider) SetSubjectClaim(claim string) {
	p.subjectClaim = claim
}

// oidcDiscovery is the subset of the discovery document we use.
type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func (p *OIDCProvider) discoveryEndpoint() string {
	if p.discoveryURL != "" {
		return p.discoveryURL
	}
	return strings.TrimRight(p.issuer, "/") + "/.well-known/openid-configuration"
}

// discover returns the issuer's discovery document, preferring the per-issuer
// cache. This is the hot path (login: AuthURL/Authenticate) and its behavior is
// deliberately unchanged: a warm cache entry short-circuits the network.
func (p *OIDCProvider) discover(ctx context.Context) (*oidcDiscovery, error) {
	if d, ok := discoveryCache.get(p.issuer); ok {
		return d, nil
	}
	return p.fetchDiscovery(ctx, true)
}

// fetchDiscovery performs the real network fetch and the full validation of the
// discovery document, ALWAYS bypassing the read cache. It is the single place
// the document is parsed and checked (issuer match + required endpoints).
// cacheResult controls whether a successful document is written to the cache.
//
// It exists as a separate entry point because discoveryCache is keyed on the
// issuer ALONE (global, 1h TTL) while discoveryEndpoint() prefers an explicit
// discoveryURL override. A cache-consulting check is therefore not a validation:
// a warm entry — from a login, an earlier probe, or ANOTHER workspace's
// connection to the same issuer — would return success without a request, and
// would never fetch a bogus discoveryURL override at all. Callers that must
// genuinely verify an operator-supplied configuration (ProbeFresh, used by
// createIdpConnection) come through here with cacheResult=false, so an admin
// action can never seed or refresh the cache the login path reads.
func (p *OIDCProvider) fetchDiscovery(ctx context.Context, cacheResult bool) (*oidcDiscovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.discoveryEndpoint(), nil)
	if err != nil {
		return nil, fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovery endpoint status %d", resp.StatusCode)
	}
	var d oidcDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode discovery: %w", err)
	}
	// The issuer in the document must match the configured issuer (OIDC spec).
	if d.Issuer != p.issuer {
		return nil, fmt.Errorf("discovery issuer mismatch: configured %q, document %q", p.issuer, d.Issuer)
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" || d.JWKSURI == "" {
		return nil, fmt.Errorf("discovery document missing required endpoints")
	}
	if cacheResult {
		discoveryCache.set(p.issuer, &d)
	}
	return &d, nil
}

// Probe fetches the OIDC discovery document and returns the issuer it
// advertises. Used by the admin testIdpConnection mutation to verify a
// connection's issuer/discovery URL is reachable and well-formed (matching
// issuer, required endpoints present) without running a full login.
//
// It is cache-consulting: a warm per-issuer entry answers without a network
// request. Use ProbeFresh where the point is to validate a configuration the
// operator just supplied.
func (p *OIDCProvider) Probe(ctx context.Context) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	return d.Issuer, nil
}

// ProbeFresh is Probe with the discovery cache bypassed on read: it always
// performs the network fetch and re-runs the full document validation.
//
// This is what a create/update-time configuration check needs. It proves
// exactly two things and nothing more:
//
//  1. the configured issuer (or explicit discoveryURL) is reachable, and
//  2. it serves a valid OIDC discovery document whose `issuer` matches the
//     configured issuer and which advertises the endpoints we require.
//
// It does NOT validate the OAuth client_id or client_secret, the redirect URI,
// or that any user can actually log in — discovery is an unauthenticated,
// public endpoint and no credential is sent. Callers must not describe a
// successful ProbeFresh as verified credentials.
//
// It is also cache-NEUTRAL: it neither reads nor writes discoveryCache, so an
// admin probe cannot seed or refresh the entry the login path consults. Login
// caching behavior is therefore exactly as it was before this entry point
// existed.
func (p *OIDCProvider) ProbeFresh(ctx context.Context) (string, error) {
	d, err := p.fetchDiscovery(ctx, false)
	if err != nil {
		return "", err
	}
	return d.Issuer, nil
}

// CredentialVerdict is the outcome of a client-credential verification probe.
type CredentialVerdict string

const (
	// CredentialsValid: the IdP authenticated the client_id/client_secret pair.
	CredentialsValid CredentialVerdict = "valid"
	// CredentialsInvalid: the IdP explicitly rejected client authentication.
	CredentialsInvalid CredentialVerdict = "invalid"
	// CredentialsInconclusive: no verdict could be established (unreachable
	// IdP, unparseable or unrecognised response). Callers under a
	// "block unless proven correct" policy MUST treat this as a failure.
	CredentialsInconclusive CredentialVerdict = "inconclusive"
)

// credentialProbeCode is a deliberately invalid authorization code. It exists
// only to make the token request well-formed enough that the IdP proceeds to
// authenticate the client; it can never redeem anything.
const credentialProbeCode = "zecurity-credential-probe-not-a-real-code"

// VerifyClientCredentials proves whether the configured client_id/client_secret
// actually authenticate against the IdP, WITHOUT a user login. This is the one
// place credentials are verified; createIdpConnection, updateIdpConnection and
// testIdpConnection all route through it.
//
// HOW IT WORKS. It POSTs to the discovered token_endpoint with
// grant_type=authorization_code and an invalid code. Per RFC 6749 §5.2,
// `invalid_client` is the ONLY error meaning client authentication failed;
// every other error is returned AFTER the client authenticated, and therefore
// proves the credentials are correct. Verified empirically against Okta: a
// request carrying BOTH a bogus code and a bad client_id returns
// `invalid_client`, not `invalid_grant` — client authentication is evaluated
// before the grant, which is what makes this probe sound.
//
// WHY authorization_code AND NOT client_credentials. The obvious probe is the
// client-credentials grant, but it is not universally enabled: Okta's org
// authorization server does not advertise `client_credentials` in
// grant_types_supported at all, so probing with it would fail for a perfectly
// valid login connection. authorization_code is necessarily enabled for a
// connection whose purpose is interactive login.
//
// Discovery is fetched FRESH and cache-neutrally (fetchDiscovery(ctx, false)),
// so the token_endpoint probed is the current one advertised by the configured
// issuer/discoveryURL, and an admin probe can neither seed nor refresh the
// cache the login path reads.
//
// SECRET HANDLING: the returned reason carries only the IdP's bounded OAuth
// error code (e.g. "invalid_client") or a locally constructed transport
// description — never the response body, and never any credential. Nothing here
// is logged.
func (p *OIDCProvider) VerifyClientCredentials(
	ctx context.Context,
	redirectURI string,
) (CredentialVerdict, string) {
	if p.clientID == "" || p.clientSecret == "" {
		return CredentialsInconclusive, "no client ID / client secret is configured to verify"
	}

	d, err := p.fetchDiscovery(ctx, false)
	if err != nil {
		// Discovery is a prerequisite, not a credential signal.
		return CredentialsInconclusive, "OIDC discovery failed: " + err.Error()
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", credentialProbeCode)
	form.Set("redirect_uri", redirectURI)
	// client_secret_post, matching exchange() and Okta's advertised
	// token_endpoint_auth_methods_supported.
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return CredentialsInconclusive, "could not build the token request"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		// Deliberately not %w/err.Error(): a transport error can embed the
		// request URL. The status-free description is enough for an admin.
		return CredentialsInconclusive, "the token endpoint could not be reached"
	}
	defer resp.Body.Close()

	// Cap the read: this is an untrusted upstream and we only need a small
	// JSON error envelope.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return CredentialsInconclusive, "the token endpoint response could not be read"
	}

	// A 200 means the client authenticated (the invalid code cannot redeem, so
	// this is unexpected — but it is still positive proof of authentication).
	if resp.StatusCode == http.StatusOK {
		return CredentialsValid, ""
	}

	code := oauthErrorCode(body)

	// `invalid_client` is the definitive client-authentication failure.
	if code == "invalid_client" {
		return CredentialsInvalid, "invalid_client"
	}
	// RFC 6749 §5.2 pairs client-authentication failure with 401. Honour it
	// even when the body carries no recognisable code.
	if resp.StatusCode == http.StatusUnauthorized && code == "" {
		return CredentialsInvalid, "the IdP rejected client authentication (HTTP 401)"
	}
	// Any OTHER OAuth error was raised after the client authenticated.
	if code != "" {
		return CredentialsValid, ""
	}
	return CredentialsInconclusive, fmt.Sprintf(
		"the token endpoint returned HTTP %d with no recognisable OAuth error code", resp.StatusCode)
}

// oauthErrorCode extracts the OAuth error code from a token-endpoint error
// body, accepting BOTH envelopes seen in practice:
//
//	{"error":"invalid_client"}      — RFC 6749 §5.2
//	{"errorCode":"invalid_client"}  — Okta's org authorization server
//
// Returns "" when neither is present.
func oauthErrorCode(body []byte) string {
	var env struct {
		Error     string `json:"error"`
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error != "" {
		return env.Error
	}
	return env.ErrorCode
}

// AuthURL builds the OIDC authorization-redirect URL from the discovered
// authorization_endpoint, injecting PKCE + nonce.
func (p *OIDCProvider) AuthURL(ctx context.Context, params AuthURLParams) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("client_id", p.clientID)
	q.Set("redirect_uri", params.RedirectURI)
	q.Set("response_type", "code")
	q.Set("scope", p.scopes)
	q.Set("state", params.State)
	q.Set("code_challenge", params.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	if params.Nonce != "" {
		q.Set("nonce", params.Nonce)
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// oidcClaims is the id_token payload we consume.
type oidcClaims struct {
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Nonce         string   `json:"nonce"`
	ACR           string   `json:"acr"`
	AMR           []string `json:"amr"`
	AuthTime      int64    `json:"auth_time"`
	jwt.RegisteredClaims
}

// Authenticate exchanges the code and verifies the returned id_token.
func (p *OIDCProvider) Authenticate(ctx context.Context, code, codeVerifier, redirectURI, expectedNonce string) (*AuthenticationContext, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}

	idToken, err := p.exchange(ctx, d.TokenEndpoint, code, codeVerifier, redirectURI)
	if err != nil {
		return nil, err
	}

	claims, rawClaims, err := p.verify(ctx, d.JWKSURI, idToken)
	if err != nil {
		return nil, err
	}

	// nonce binds the id_token to this login (replay protection).
	if expectedNonce != "" && claims.Nonce != expectedNonce {
		return nil, fmt.Errorf("id_token nonce mismatch")
	}
	if claims.Subject == "" {
		return nil, fmt.Errorf("id_token missing sub claim")
	}

	// Derive the canonical identity subject from the configured subjectClaim
	// (ADR-025 §3.1). The raw, verified claims map is used ONLY as an
	// additional representation of an already-validated token — the security
	// validation above (signature, issuer, audience, expiry, nbf, nonce,
	// email_verified) is the authoritative gate and is never bypassed by a
	// custom claim. mapping.ExtractSubjectClaim returns "" when the configured
	// claim is missing/empty, and we fail closed rather than falling back to
	// raw `sub`. Empty subjectClaim ⇒ legacy default "sub".
	subject := mapping.ExtractSubjectClaim(rawClaims, p.subjectClaim)
	if subject == "" {
		return nil, fmt.Errorf("id_token missing configured subject claim %q", subjectClaimName(p.subjectClaim))
	}

	return &AuthenticationContext{
		Provider:      p.providerLabel,
		Issuer:        claims.Issuer,
		Subject:       subject,
		Email:         claims.Email,
		Name:          claims.Name,
		EmailVerified: claims.EmailVerified,
		ACR:           claims.ACR,
		AMR:           claims.AMR,
		AuthTime:      claims.AuthTime,
		RawClaims:     rawClaims,
	}, nil
}

// subjectClaimName returns the effective claim name for error messages.
func subjectClaimName(claim string) string {
	if claim == "" {
		return "sub"
	}
	return claim
}

func (p *OIDCProvider) exchange(ctx context.Context, tokenEndpoint, code, codeVerifier, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token exchange failed: status %d", resp.StatusCode)
	}
	var tr struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.IDToken == "" {
		return "", fmt.Errorf("token response missing id_token")
	}
	return tr.IDToken, nil
}

func (p *OIDCProvider) verify(ctx context.Context, jwksURI, idToken string) (*oidcClaims, map[string]any, error) {
	// Typed parse: the authoritative security gate. All existing validation
	// (signature/JWKS, issuer, audience, expiry, not-before) runs here and
	// MUST remain the only path that decides a token is valid.
	claims := &oidcClaims{}
	_, err := jwt.ParseWithClaims(idToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("id_token missing kid header")
		}
		return jwksForIssuer(ctx, p.http, p.issuer, jwksURI, kid)
	}, jwt.WithExpirationRequired(), jwt.WithAudience(p.clientID), jwt.WithIssuer(p.issuer))
	if err != nil {
		return nil, nil, fmt.Errorf("id_token verification failed: %w", err)
	}
	// Unverified emails must never be trusted for identity.
	if !claims.EmailVerified {
		return nil, nil, fmt.Errorf("id_token email not verified")
	}

	// Raw claims map: an ADDITIONAL representation of the SAME already-validated
	// token (no re-validation, no re-parse of the signature path). Used only so
	// a configured non-default subjectClaim (e.g. "email", "oid") can be read.
	// It is never consulted for issuer/aud/exp/nonce/email_verified — those
	// were already enforced on the typed claims above. A parse failure here is
	// non-fatal: it only means custom claims are unavailable, and the subject
	// derivation below will then fail closed if a non-default claim was needed.
	raw := map[string]any{}
	if mt, _, perr := jwt.NewParser().ParseUnverified(idToken, jwt.MapClaims(raw)); perr != nil {
		_ = mt
		raw = nil
	}

	return claims, raw, nil
}

// ── per-issuer discovery + JWKS caches (shared across adapter instances) ──────

type discoveryCacheT struct {
	sync.RWMutex
	m map[string]discoveryEntry
}
type discoveryEntry struct {
	doc       *oidcDiscovery
	fetchedAt time.Time
}

var discoveryCache = &discoveryCacheT{m: map[string]discoveryEntry{}}

const discoveryTTL = 1 * time.Hour

func (c *discoveryCacheT) get(issuer string) (*oidcDiscovery, bool) {
	c.RLock()
	defer c.RUnlock()
	e, ok := c.m[issuer]
	if !ok || time.Since(e.fetchedAt) > discoveryTTL {
		return nil, false
	}
	return e.doc, true
}
func (c *discoveryCacheT) set(issuer string, doc *oidcDiscovery) {
	c.Lock()
	defer c.Unlock()
	c.m[issuer] = discoveryEntry{doc: doc, fetchedAt: nowFn()}
}

// jwksCacheT caches RSA public keys per issuer, keyed by kid, with a TTL.
type jwksCacheT struct {
	sync.RWMutex
	m map[string]jwksEntry
}
type jwksEntry struct {
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

var jwksByIssuer = &jwksCacheT{m: map[string]jwksEntry{}}

const jwksTTL = 1 * time.Hour

// nowFn is time.Now, indirected so tests can keep it deterministic if needed.
var nowFn = time.Now

// jwksForIssuer returns the RSA key for (issuer, kid), fetching/refreshing the
// issuer's JWKS on a cache miss or unknown kid.
func jwksForIssuer(ctx context.Context, hc *http.Client, issuer, jwksURI, kid string) (*rsa.PublicKey, error) {
	jwksByIssuer.RLock()
	e, ok := jwksByIssuer.m[issuer]
	if ok && time.Since(e.fetchedAt) <= jwksTTL {
		if k, found := e.keys[kid]; found {
			jwksByIssuer.RUnlock()
			return k, nil
		}
	}
	jwksByIssuer.RUnlock()

	keys, err := fetchJWKS(ctx, hc, jwksURI)
	if err != nil {
		return nil, err
	}
	jwksByIssuer.Lock()
	jwksByIssuer.m[issuer] = jwksEntry{keys: keys, fetchedAt: nowFn()}
	jwksByIssuer.Unlock()

	k, found := keys[kid]
	if !found {
		return nil, fmt.Errorf("no JWKS key for kid=%s", kid)
	}
	return k, nil
}

func fetchJWKS(ctx context.Context, hc *http.Client, jwksURI string) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, fmt.Errorf("build jwks request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint status %d", resp.StatusCode)
	}
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}
	out := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: int(new(big.Int).SetBytes(eBytes).Int64()),
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no RSA keys parsed from jwks")
	}
	return out, nil
}
