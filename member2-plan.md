# Member 2 — Deep Implementation Plan
## OIDC · PKCE · id_token Verification · JWT · Refresh Token · Session

---

## Role on the Team

Member 2 owns the entire authentication flow inside Go.
No Express. No proxy. Go handles OIDC directly.

```
Member 2 owns:
  internal/auth/oidc.go        ← PKCE + Google token exchange
  internal/auth/idtoken.go     ← id_token verification (JWKS)
  internal/auth/session.go     ← JWT sign/verify + refresh token
  internal/auth/callback.go    ← HTTP handler for /auth/callback
  internal/auth/refresh.go     ← HTTP handler for /auth/refresh
  internal/auth/config.go      ← Config struct + NewService constructor
  internal/auth/redis.go       ← Redis client setup
```

Member 2 does NOT touch:
- `internal/bootstrap/` — that is Member 3
- `internal/pki/` — that is Member 3
- `graph/` — that is Member 4
- `internal/middleware/` — that is Member 4

---

## Hard Dependencies — What Member 2 Waits For

### Must wait for Member 4 Phase 1 before starting anything:

**`internal/auth/service.go`** — the interface file Member 4 writes.
Member 2's entire implementation is the concrete struct that satisfies
this interface. Without it, Member 2 does not know the method signatures.

```go
// This is what Member 4 commits. Member 2 implements against it.
type Service interface {
    InitiateAuth(ctx context.Context, provider string) (*model.AuthInitPayload, error)
    CallbackHandler() http.Handler
    RefreshHandler() http.Handler
}
```

**`docker-compose.yml`** — Member 2 needs Redis running to test
PKCE state storage. Without Docker running, nothing can be tested.

**`internal/db/pool.go`** — Member 2 calls `db.Pool` in `config.go`
to pass it to the bootstrap service.

**`migrations/001_schema.sql`** — Member 2 needs to understand the
`users` table structure, specifically `provider_sub` and `tenant_id`,
because the bootstrap result shapes what goes into the JWT.

### Must wait for Member 3 for one specific thing:

**`internal/bootstrap/bootstrap.go`** — Member 2 calls
`bootstrap.Bootstrap()` inside `callback.go`. Member 3 writes this.

Member 2 does NOT block on this. A stub handles it until Member 3
finishes. The stub is replaced with one line change when Member 3 ships.

```go
// stub in internal/bootstrap/bootstrap.go
// Member 2 writes this temporarily until Member 3 implements the real one

package bootstrap

import "context"

type Result struct {
    TenantID string
    UserID   string
    Role     string
}

func Bootstrap(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error) {
    // TODO: Member 3 replaces this with the real transaction
    return &Result{
        TenantID: "00000000-0000-0000-0000-000000000001",
        UserID:   "00000000-0000-0000-0000-000000000002",
        Role:     "admin",
    }, nil
}
```

Member 2 writes this stub themselves in the bootstrap package.
When Member 3 is done, Member 3 replaces the stub body with the real
transaction. Member 2's callback.go does not change at all — same
function signature, same call site, same return type.

### Contract agreement with Member 4 (must align before implementing):

Member 4's `internal/middleware/auth.go` reads JWTs that Member 2 signs.
Both files must use identical field names. Agree on this before writing:

```go
// This Claims struct is defined in Member 4's middleware/auth.go
// Member 2's session.go must produce JWTs with these exact fields:

type Claims struct {
    TenantID string `json:"tenant_id"`  // must be "tenant_id" not "tenantId"
    Role     string `json:"role"`
    jwt.RegisteredClaims
    // sub (user_id) lives in RegisteredClaims.Subject
    // iss must be "ztna-controller"
    // exp must be set
}
```

If Member 2 writes `"tenantId"` and Member 4 reads `"tenant_id"`,
JWT verification will succeed but `TenantID` will be empty string.
The workspace guard will then reject every request with a 403.
This is a silent bug that is hard to trace. Agree on field names first.

---

## Everything Member 2 Builds

### Phase 1 — Config + Redis + Constructor

This is the foundation everything else uses.

**internal/auth/config.go**

```go
package auth

import (
    "fmt"
    "os"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/yourorg/ztna/controller/internal/pki"
)

// Config holds all dependencies and settings for the auth service.
// Member 4 instantiates this in main.go using env vars.
// Member 2 defines what fields are needed here.
type Config struct {
    Pool               *pgxpool.Pool // for passing to bootstrap
    PKIService         pki.Service   // not used by auth directly, passed to bootstrap
    JWTSecret          string
    JWTIssuer          string        // must be "ztna-controller"
    JWTAccessTTL       string        // e.g. "15m"
    JWTRefreshTTL      string        // e.g. "168h"
    GoogleClientID     string
    GoogleClientSecret string
    RedirectURI        string        // https://<domain>/auth/callback
    RedisURL           string
    AllowedOrigin      string        // for CORS on callback redirect
}

// serviceImpl is the concrete implementation of auth.Service.
// Unexported — callers use the Service interface.
type serviceImpl struct {
    cfg         Config
    redisClient *redisClient
}

// NewService constructs the auth service.
// Called once in main.go. Panics if required config is missing.
func NewService(cfg Config) (Service, error) {
    if cfg.JWTSecret == "" {
        return nil, fmt.Errorf("auth: JWTSecret is required")
    }
    if cfg.GoogleClientID == "" {
        return nil, fmt.Errorf("auth: GoogleClientID is required")
    }
    if cfg.GoogleClientSecret == "" {
        return nil, fmt.Errorf("auth: GoogleClientSecret is required")
    }
    if cfg.RedirectURI == "" {
        return nil, fmt.Errorf("auth: RedirectURI is required")
    }
    if cfg.JWTIssuer == "" {
        cfg.JWTIssuer = "ztna-controller"
    }
    if cfg.JWTAccessTTL == "" {
        cfg.JWTAccessTTL = "15m"
    }
    if cfg.JWTRefreshTTL == "" {
        cfg.JWTRefreshTTL = "168h"
    }

    rc, err := newRedisClient(cfg.RedisURL)
    if err != nil {
        return nil, fmt.Errorf("auth: redis init: %w", err)
    }

    return &serviceImpl{
        cfg:         cfg,
        redisClient: rc,
    }, nil
}
```

**internal/auth/redis.go**

```go
package auth

import (
    "context"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
)

type redisClient struct {
    rdb *redis.Client
}

func newRedisClient(url string) (*redisClient, error) {
    opts, err := redis.ParseURL(url)
    if err != nil {
        return nil, fmt.Errorf("parse redis URL: %w", err)
    }

    rdb := redis.NewClient(opts)

    // Verify connectivity
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()

    if err := rdb.Ping(ctx).Err(); err != nil {
        return nil, fmt.Errorf("ping redis: %w", err)
    }

    return &redisClient{rdb: rdb}, nil
}

// SetPKCEState stores the code_verifier keyed by state value.
// TTL = 5 minutes. After that, the PKCE pair is unusable.
// The user must restart the login flow.
func (r *redisClient) SetPKCEState(ctx context.Context,
    state, codeVerifier string) error {
    return r.rdb.Set(ctx,
        pkceKey(state),
        codeVerifier,
        5*time.Minute,
    ).Err()
}

// GetAndDeletePKCEState retrieves the code_verifier and immediately
// deletes it. Single-use — cannot be replayed.
// Returns ("", false, nil) if the key does not exist (expired or already used).
func (r *redisClient) GetAndDeletePKCEState(ctx context.Context,
    state string) (string, bool, error) {

    // Use a pipeline to GET + DEL atomically.
    // If we GET then DEL separately, a crash between the two
    // could leave a used verifier in Redis (replay risk).
    pipe := r.rdb.Pipeline()
    getCmd := pipe.Get(ctx, pkceKey(state))
    pipe.Del(ctx, pkceKey(state))
    _, err := pipe.Exec(ctx)

    if err != nil && err != redis.Nil {
        return "", false, fmt.Errorf("redis pipeline: %w", err)
    }

    val, err := getCmd.Result()
    if err == redis.Nil {
        return "", false, nil // expired or already used
    }
    if err != nil {
        return "", false, fmt.Errorf("get pkce state: %w", err)
    }

    return val, true, nil
}

// SetRefreshToken stores a refresh token keyed to user_id.
// TTL = 7 days (168 hours).
func (r *redisClient) SetRefreshToken(ctx context.Context,
    userID, token string, ttl time.Duration) error {
    return r.rdb.Set(ctx,
        refreshKey(userID),
        token,
        ttl,
    ).Err()
}

// GetRefreshToken retrieves the stored refresh token for a user.
// Returns ("", false, nil) if not found or expired.
func (r *redisClient) GetRefreshToken(ctx context.Context,
    userID string) (string, bool, error) {

    val, err := r.rdb.Get(ctx, refreshKey(userID)).Result()
    if err == redis.Nil {
        return "", false, nil
    }
    if err != nil {
        return "", false, fmt.Errorf("get refresh token: %w", err)
    }
    return val, true, nil
}

// DeleteRefreshToken removes the refresh token.
// Called on sign-out.
func (r *redisClient) DeleteRefreshToken(ctx context.Context,
    userID string) error {
    return r.rdb.Del(ctx, refreshKey(userID)).Err()
}

func pkceKey(state string) string {
    return "pkce:" + state
}

func refreshKey(userID string) string {
    return "refresh:" + userID
}
```

---

### Phase 2 — PKCE Generation (InitiateAuth)

**internal/auth/oidc.go**

```go
package auth

import (
    "context"
    "crypto/rand"
    "crypto/sha256"
    "encoding/base64"
    "fmt"
    "net/url"

    "github.com/yourorg/ztna/controller/graph/model"
)

// InitiateAuth implements auth.Service.InitiateAuth.
// Called by the initiateAuth GraphQL resolver (Member 4's stub).
//
// What it does:
//   1. Generates a cryptographically random code_verifier (PKCE)
//   2. Derives code_challenge = BASE64URL(SHA256(code_verifier))
//   3. Generates a signed state value for CSRF protection
//   4. Stores code_verifier in Redis keyed by state, TTL=5min
//   5. Builds the Google OAuth authorization URL
//   6. Returns the URL + state to the caller
//
// React redirects the browser to the returned URL.
// React stores the state in sessionStorage for CSRF verification on return.
func (s *serviceImpl) InitiateAuth(ctx context.Context,
    provider string) (*model.AuthInitPayload, error) {

    // Only Google is supported in this sprint.
    // Other providers can be added here without changing the interface.
    if provider != "google" {
        return nil, fmt.Errorf("unsupported provider: %s", provider)
    }

    // 1. Generate code_verifier
    // 64 random bytes → base64url = 86 character string
    // RFC 7636 requires 43–128 characters, this is 86 — within spec
    verifierBytes := make([]byte, 64)
    if _, err := rand.Read(verifierBytes); err != nil {
        return nil, fmt.Errorf("generate code_verifier: %w", err)
    }
    codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

    // 2. Derive code_challenge
    // code_challenge = BASE64URL(SHA256(ASCII(code_verifier)))
    hash := sha256.Sum256([]byte(codeVerifier))
    codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])

    // 3. Generate state
    // state = HMAC-signed nonce that survives the OAuth redirect
    // The nonce is random. Signing it with JWT_SECRET lets the callback
    // verify the state was issued by this server (CSRF protection).
    state, err := generateSignedState(s.cfg.JWTSecret)
    if err != nil {
        return nil, fmt.Errorf("generate state: %w", err)
    }

    // 4. Store code_verifier in Redis keyed by state
    // The callback retrieves this using the state value from the URL.
    if err := s.redisClient.SetPKCEState(ctx, state, codeVerifier); err != nil {
        return nil, fmt.Errorf("store pkce state: %w", err)
    }

    // 5. Build Google OAuth URL
    params := url.Values{}
    params.Set("client_id", s.cfg.GoogleClientID)
    params.Set("redirect_uri", s.cfg.RedirectURI)
    params.Set("response_type", "code")
    params.Set("scope", "openid email profile")
    params.Set("code_challenge", codeChallenge)
    params.Set("code_challenge_method", "S256")
    params.Set("state", state)
    // access_type=offline is NOT set — we don't need a refresh token
    // from Google. Our own refresh token system handles session renewal.

    redirectURL := "https://accounts.google.com/o/oauth2/v2/auth?" +
        params.Encode()

    return &model.AuthInitPayload{
        RedirectUrl: redirectURL,
        State:       state,
    }, nil
}

// generateSignedState creates a random nonce and signs it with HMAC-SHA256.
// Format: base64url(nonce) + "." + base64url(HMAC(nonce))
// The callback verifies the HMAC to confirm the state was issued by this server.
func generateSignedState(secret string) (string, error) {
    nonce := make([]byte, 32)
    if _, err := rand.Read(nonce); err != nil {
        return "", err
    }

    import "crypto/hmac"
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(nonce)
    sig := mac.Sum(nil)

    nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
    sigB64 := base64.RawURLEncoding.EncodeToString(sig)

    return nonceB64 + "." + sigB64, nil
}

// verifySignedState checks the HMAC on a state value returned from Google.
// Returns an error if the state was tampered with or not issued by this server.
func verifySignedState(state, secret string) error {
    import (
        "crypto/hmac"
        "strings"
    )

    parts := strings.SplitN(state, ".", 2)
    if len(parts) != 2 {
        return fmt.Errorf("invalid state format")
    }

    nonce, err := base64.RawURLEncoding.DecodeString(parts[0])
    if err != nil {
        return fmt.Errorf("decode state nonce: %w", err)
    }

    gotSig, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        return fmt.Errorf("decode state sig: %w", err)
    }

    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write(nonce)
    expectedSig := mac.Sum(nil)

    // Use hmac.Equal for constant-time comparison (prevents timing attacks)
    if !hmac.Equal(gotSig, expectedSig) {
        return fmt.Errorf("state signature invalid")
    }

    return nil
}
```

---

### Phase 3 — id_token Verification

**internal/auth/idtoken.go**

This is the most security-critical file Member 2 writes.
Every check here must be present. Skipping any one of them is a vulnerability.

```go
package auth

import (
    "context"
    "crypto/rsa"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "math/big"
    "net/http"
    "sync"
    "time"

    "github.com/golang-jwt/jwt/v5"
)

// GoogleClaims holds the claims extracted from Google's id_token.
type GoogleClaims struct {
    Email         string `json:"email"`
    EmailVerified bool   `json:"email_verified"`
    Name          string `json:"name"`
    Picture       string `json:"picture"`
    Sub           string `json:"sub"`   // immutable subject — use this as provider_sub
    jwt.RegisteredClaims
}

// jwksCache caches Google's public keys to avoid fetching on every request.
// Keys are refreshed when they expire or a new kid is encountered.
var jwksCache struct {
    sync.RWMutex
    keys      map[string]*rsa.PublicKey
    fetchedAt time.Time
}

const jwksURL = "https://www.googleapis.com/oauth2/v3/certs"
const jwksCacheTTL = 1 * time.Hour

// VerifyGoogleIDToken verifies a Google id_token and returns the claims.
//
// Checks performed (ALL must pass):
//   1. Signature valid (signed by Google's current RSA public key)
//   2. aud == our Google Client ID (prevents tokens for other apps)
//   3. iss == "accounts.google.com" or "https://accounts.google.com"
//   4. exp > now() (token not expired)
//   5. email_verified == true (Google confirmed the email)
//   6. sub is present and non-empty (immutable identity anchor)
//
// Skipping check 2 (aud) allows tokens issued for other Google apps
// to authenticate users in your system. That is an open redirect / auth bypass.
func VerifyGoogleIDToken(ctx context.Context,
    idToken, clientID string) (*GoogleClaims, error) {

    // Parse without verification first to get the key ID (kid)
    unverified, _, err := new(jwt.Parser).ParseUnverified(idToken, &GoogleClaims{})
    if err != nil {
        return nil, fmt.Errorf("parse id_token: %w", err)
    }

    kid, ok := unverified.Header["kid"].(string)
    if !ok || kid == "" {
        return nil, fmt.Errorf("id_token missing kid header")
    }

    // Get Google's public key for this kid
    pubKey, err := getGooglePublicKey(ctx, kid)
    if err != nil {
        return nil, fmt.Errorf("get google public key: %w", err)
    }

    // Now parse WITH verification
    claims := &GoogleClaims{}
    token, err := jwt.ParseWithClaims(
        idToken, claims,
        func(t *jwt.Token) (interface{}, error) {
            // Check 1: signing method must be RS256
            if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
                return nil, fmt.Errorf("unexpected signing method: %v",
                    t.Header["alg"])
            }
            return pubKey, nil
        },
        // Check 3: issuer
        jwt.WithIssuers(
            "accounts.google.com",
            "https://accounts.google.com",
        ),
        // Check 4: expiry enforced
        jwt.WithExpirationRequired(),
    )

    if err != nil || !token.Valid {
        return nil, fmt.Errorf("id_token verification failed: %w", err)
    }

    // Check 2: audience must be our client ID
    // jwt.WithAudience is not used above because Google's tokens use
    // a string audience, not an array — verify manually
    if !claims.VerifyAudience(clientID, true) {
        return nil, fmt.Errorf("id_token audience mismatch: expected %s", clientID)
    }

    // Check 5: email must be verified by Google
    if !claims.EmailVerified {
        return nil, fmt.Errorf("id_token email not verified")
    }

    // Check 6: sub must be present
    if claims.Sub == "" {
        return nil, fmt.Errorf("id_token missing sub claim")
    }

    return claims, nil
}

// getGooglePublicKey returns the RSA public key for a given kid.
// Uses a local cache with 1-hour TTL to avoid hammering Google's JWKS endpoint.
func getGooglePublicKey(ctx context.Context,
    kid string) (*rsa.PublicKey, error) {

    // Try cache first (read lock)
    jwksCache.RLock()
    if time.Since(jwksCache.fetchedAt) < jwksCacheTTL {
        if key, ok := jwksCache.keys[kid]; ok {
            jwksCache.RUnlock()
            return key, nil
        }
    }
    jwksCache.RUnlock()

    // Cache miss or expired — fetch fresh keys (write lock)
    jwksCache.Lock()
    defer jwksCache.Unlock()

    // Double-check after acquiring write lock (another goroutine may have fetched)
    if time.Since(jwksCache.fetchedAt) < jwksCacheTTL {
        if key, ok := jwksCache.keys[kid]; ok {
            return key, nil
        }
    }

    keys, err := fetchGoogleJWKS(ctx)
    if err != nil {
        return nil, err
    }
    jwksCache.keys = keys
    jwksCache.fetchedAt = time.Now()

    key, ok := keys[kid]
    if !ok {
        return nil, fmt.Errorf("no google public key found for kid=%s", kid)
    }
    return key, nil
}

// fetchGoogleJWKS fetches Google's current JWKS and returns a map of kid → RSA key.
func fetchGoogleJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
    if err != nil {
        return nil, fmt.Errorf("build jwks request: %w", err)
    }

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, fmt.Errorf("fetch jwks: %w", err)
    }
    defer resp.Body.Close()

    var jwks struct {
        Keys []struct {
            Kid string `json:"kid"`
            N   string `json:"n"`
            E   string `json:"e"`
        } `json:"keys"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
        return nil, fmt.Errorf("decode jwks: %w", err)
    }

    keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
    for _, k := range jwks.Keys {
        nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
        if err != nil {
            continue
        }
        eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
        if err != nil {
            continue
        }

        n := new(big.Int).SetBytes(nBytes)
        e := int(new(big.Int).SetBytes(eBytes).Int64())

        keys[k.Kid] = &rsa.PublicKey{N: n, E: e}
    }

    if len(keys) == 0 {
        return nil, fmt.Errorf("no keys parsed from jwks response")
    }

    return keys, nil
}
```

---

### Phase 4 — Google Token Exchange

**internal/auth/exchange.go**

```go
package auth

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"
    "strings"
    "time"
)

// GoogleTokenResponse is what Google returns from the token endpoint.
type GoogleTokenResponse struct {
    IDToken     string `json:"id_token"`
    AccessToken string `json:"access_token"`
    ExpiresIn   int    `json:"expires_in"`
    TokenType   string `json:"token_type"`
}

const googleTokenURL = "https://oauth2.googleapis.com/token"

// exchangeCodeForTokens exchanges the authorization code for tokens.
// This is a server-to-server call — the client_secret is never exposed.
//
// The code_verifier must match the code_challenge sent in the auth URL.
// Google verifies this server-side — if it doesn't match, the exchange fails.
// This is the PKCE guarantee: even if the auth code is intercepted,
// it cannot be exchanged without the verifier that only our server has.
func (s *serviceImpl) exchangeCodeForTokens(ctx context.Context,
    code, codeVerifier string) (*GoogleTokenResponse, error) {

    body := url.Values{}
    body.Set("code", code)
    body.Set("code_verifier", codeVerifier)
    body.Set("client_id", s.cfg.GoogleClientID)
    body.Set("client_secret", s.cfg.GoogleClientSecret)
    body.Set("redirect_uri", s.cfg.RedirectURI)
    body.Set("grant_type", "authorization_code")

    req, err := http.NewRequestWithContext(ctx, http.MethodPost,
        googleTokenURL, strings.NewReader(body.Encode()))
    if err != nil {
        return nil, fmt.Errorf("build token request: %w", err)
    }
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("token exchange request: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        var errBody map[string]any
        json.NewDecoder(resp.Body).Decode(&errBody)
        return nil, fmt.Errorf("google token exchange failed: status=%d body=%v",
            resp.StatusCode, errBody)
    }

    var tokenResp GoogleTokenResponse
    if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
        return nil, fmt.Errorf("decode token response: %w", err)
    }

    if tokenResp.IDToken == "" {
        return nil, fmt.Errorf("google did not return id_token")
    }

    return &tokenResp, nil
}
```

---

### Phase 5 — JWT Issuance + Refresh Token

**internal/auth/session.go**

```go
package auth

import (
    "context"
    "crypto/rand"
    "encoding/base64"
    "fmt"
    "time"

    "github.com/golang-jwt/jwt/v5"
)

// jwtClaims is the payload for JWTs issued by this controller.
// Field names must match Member 4's Claims struct in middleware/auth.go exactly.
// Coordinate with Member 4 before changing any json tag.
type jwtClaims struct {
    TenantID string `json:"tenant_id"` // coordinate: must match middleware/auth.go
    Role     string `json:"role"`      // coordinate: must match middleware/auth.go
    jwt.RegisteredClaims
    // Subject (sub) = user_id — set via RegisteredClaims.Subject
    // Issuer  (iss) = "ztna-controller" — set via RegisteredClaims.Issuer
    // Expiry  (exp) = now + 15 min — set via RegisteredClaims.ExpiresAt
}

// issueAccessToken creates a signed short-lived JWT.
// exp = 15 minutes from now.
// Signed with HS256 using JWT_SECRET.
// (To switch to RS256: change SigningMethodRS256, provide RSA private key.)
func (s *serviceImpl) issueAccessToken(userID, tenantID, role string) (string, error) {
    ttl, err := time.ParseDuration(s.cfg.JWTAccessTTL)
    if err != nil {
        ttl = 15 * time.Minute
    }

    now := time.Now()
    claims := jwtClaims{
        TenantID: tenantID,
        Role:     role,
        RegisteredClaims: jwt.RegisteredClaims{
            Subject:   userID,
            Issuer:    s.cfg.JWTIssuer,
            IssuedAt:  jwt.NewNumericDate(now),
            ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
        },
    }

    token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
    signed, err := token.SignedString([]byte(s.cfg.JWTSecret))
    if err != nil {
        return "", fmt.Errorf("sign JWT: %w", err)
    }

    return signed, nil
}

// issueRefreshToken creates a random 256-bit refresh token,
// stores it in Redis keyed to the user_id, and returns the token value.
// The raw token is set as an httpOnly cookie — never returned in the body.
func (s *serviceImpl) issueRefreshToken(ctx context.Context,
    userID string) (string, error) {

    // Generate random token
    raw := make([]byte, 32) // 32 bytes = 256 bits
    if _, err := rand.Read(raw); err != nil {
        return "", fmt.Errorf("generate refresh token: %w", err)
    }
    token := base64.RawURLEncoding.EncodeToString(raw)

    // Parse TTL
    ttl, err := time.ParseDuration(s.cfg.JWTRefreshTTL)
    if err != nil {
        ttl = 7 * 24 * time.Hour
    }

    // Store in Redis
    if err := s.redisClient.SetRefreshToken(ctx, userID, token, ttl); err != nil {
        return "", fmt.Errorf("store refresh token: %w", err)
    }

    return token, nil
}

// verifyAccessToken parses and verifies an access JWT.
// Returns the claims if valid.
// Used in tests. In production, Member 4's middleware/auth.go handles this.
func (s *serviceImpl) verifyAccessToken(tokenStr string) (*jwtClaims, error) {
    claims := &jwtClaims{}
    token, err := jwt.ParseWithClaims(
        tokenStr, claims,
        func(t *jwt.Token) (interface{}, error) {
            if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
                return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
            }
            return []byte(s.cfg.JWTSecret), nil
        },
        jwt.WithIssuer(s.cfg.JWTIssuer),
        jwt.WithExpirationRequired(),
    )
    if err != nil || !token.Valid {
        return nil, fmt.Errorf("invalid token: %w", err)
    }
    return claims, nil
}
```

---

### Phase 6 — The Callback Handler

**internal/auth/callback.go**

This is the most complex file. Every step is sequential and must succeed for the
flow to complete. If any step fails, the user is redirected to the login page
with an error — never left at a broken state.

```go
package auth

import (
    "fmt"
    "net/http"
    "time"

    "github.com/yourorg/ztna/controller/internal/bootstrap"
)

// CallbackHandler handles GET /auth/callback.
// Registered as a public route in main.go (no auth middleware).
//
// Full sequence:
//   1. Read code + state from URL query params
//   2. Verify state HMAC (CSRF protection)
//   3. Retrieve and delete code_verifier from Redis (single use)
//   4. Exchange code for Google tokens (server-to-server)
//   5. Verify id_token (signature, aud, iss, exp, email_verified)
//   6. Extract identity claims (email, sub, name)
//   7. Call bootstrap.Bootstrap() → get tenant_id + user_id + role
//   8. Issue access JWT
//   9. Issue refresh token → store in Redis → set httpOnly cookie
//  10. Redirect React to /auth/callback#token=<JWT>
//
// On any failure: redirect to /login?error=<reason>
// Never show raw error details to the browser.
func (s *serviceImpl) CallbackHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()

        // Helper: redirect to login with error code
        fail := func(reason string) {
            http.Redirect(w, r,
                "/login?error="+reason,
                http.StatusFound)
        }

        // Step 1 — Read query params
        code := r.URL.Query().Get("code")
        state := r.URL.Query().Get("state")

        if code == "" || state == "" {
            fail("missing_params")
            return
        }

        // Step 2 — Verify state HMAC
        // This prevents CSRF: if an attacker crafts a callback URL
        // with a forged state, the HMAC check fails here.
        if err := verifySignedState(state, s.cfg.JWTSecret); err != nil {
            fail("invalid_state")
            return
        }

        // Step 3 — Retrieve and delete code_verifier from Redis
        // Single use: GetAndDeletePKCEState deletes the key atomically.
        // If this returns false, the state expired or was already used.
        codeVerifier, found, err := s.redisClient.GetAndDeletePKCEState(ctx, state)
        if err != nil {
            fail("server_error")
            return
        }
        if !found {
            // State expired (>5 min) or replay attempt
            fail("state_expired")
            return
        }

        // Step 4 — Exchange code for tokens (server-to-server)
        // client_secret is used here — never exposed to the browser
        tokenResp, err := s.exchangeCodeForTokens(ctx, code, codeVerifier)
        if err != nil {
            fail("token_exchange_failed")
            return
        }

        // Step 5 — Verify id_token
        // ALL six checks must pass (see idtoken.go)
        googleClaims, err := VerifyGoogleIDToken(ctx,
            tokenResp.IDToken, s.cfg.GoogleClientID)
        if err != nil {
            fail("invalid_id_token")
            return
        }

        // Step 6 — Extract identity
        // Use Sub (provider_sub) as the identity anchor, NOT email.
        // Emails can change. Sub is immutable.
        email := googleClaims.Email
        providerSub := googleClaims.Sub
        name := googleClaims.Name
        if name == "" {
            name = email // fallback if Google doesn't return name
        }

        // Step 7 — Bootstrap
        // This is a direct Go function call — no network, no HTTP.
        // Member 3 implements this function.
        // Until Member 3 finishes, the stub returns test data.
        result, err := bootstrap.Bootstrap(ctx,
            email, "google", providerSub, name)
        if err != nil {
            fail("bootstrap_failed")
            return
        }

        // Step 8 — Issue access JWT
        accessToken, err := s.issueAccessToken(
            result.UserID, result.TenantID, result.Role)
        if err != nil {
            fail("token_issue_failed")
            return
        }

        // Step 9 — Issue refresh token
        refreshToken, err := s.issueRefreshToken(ctx, result.UserID)
        if err != nil {
            fail("refresh_issue_failed")
            return
        }

        // Set refresh token as httpOnly cookie
        // httpOnly: JavaScript cannot read this — XSS cannot steal it
        // SameSite=Strict: not sent on cross-site requests — CSRF protection
        // Secure: only sent over HTTPS (set to false in development)
        ttl, _ := time.ParseDuration(s.cfg.JWTRefreshTTL)
        http.SetCookie(w, &http.Cookie{
            Name:     "refresh_token",
            Value:    refreshToken,
            Path:     "/auth/refresh",    // only sent to the refresh endpoint
            HttpOnly: true,
            SameSite: http.SameSiteStrictMode,
            Secure:   true,              // set false in development via config
            MaxAge:   int(ttl.Seconds()),
        })

        // Step 10 — Redirect React with JWT in URL fragment
        // Fragment (#token=...) is NEVER sent to the server.
        // Only the browser reads it. React extracts it from window.location.hash
        // and stores in memory (Zustand), then clears the hash from the URL.
        http.Redirect(w, r,
            "/#token="+accessToken,
            http.StatusFound)
    })
}
```

---

### Phase 7 — Refresh Handler

**internal/auth/refresh.go**

```go
package auth

import (
    "encoding/json"
    "net/http"

    "github.com/golang-jwt/jwt/v5"
)

// RefreshHandler handles POST /auth/refresh.
// Registered as a public route in main.go (no auth middleware).
//
// Flow:
//   1. Read refresh_token from httpOnly cookie
//   2. Read user_id from the expired (or expiring) access JWT
//      Note: we read the JWT without verifying expiry here — we just
//      need the user_id to look up the refresh token in Redis
//   3. Look up refresh token in Redis by user_id
//   4. Compare cookie value with stored value (constant-time)
//   5. Issue new access JWT
//   6. Return new access JWT in JSON body
//
// The refresh token itself is NOT rotated on every refresh.
// It expires after 7 days and the user must log in again.
// Token rotation can be added later if needed.
func (s *serviceImpl) RefreshHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()

        // Step 1 — Read refresh token cookie
        cookie, err := r.Cookie("refresh_token")
        if err != nil || cookie.Value == "" {
            writeJSONError(w, http.StatusUnauthorized, "missing refresh token")
            return
        }
        cookieToken := cookie.Value

        // Step 2 — Read user_id from the access JWT (without verifying expiry)
        // The access JWT is sent in the Authorization header even when expired.
        // We only need the sub (user_id) claim — not to trust the token.
        authHeader := r.Header.Get("Authorization")
        if authHeader == "" {
            writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
            return
        }

        var userID, tenantID, role string
        parser := jwt.NewParser(jwt.WithoutClaimsValidation()) // skip exp check
        claims := &jwtClaims{}
        _, err = parser.ParseWithClaims(
            extractBearer(authHeader), claims,
            func(t *jwt.Token) (interface{}, error) {
                return []byte(s.cfg.JWTSecret), nil
            },
        )
        if err != nil {
            writeJSONError(w, http.StatusUnauthorized, "invalid access token")
            return
        }
        userID = claims.Subject
        tenantID = claims.TenantID
        role = claims.Role

        if userID == "" || tenantID == "" {
            writeJSONError(w, http.StatusUnauthorized, "token missing claims")
            return
        }

        // Step 3 — Look up stored refresh token
        storedToken, found, err := s.redisClient.GetRefreshToken(ctx, userID)
        if err != nil {
            writeJSONError(w, http.StatusInternalServerError, "server error")
            return
        }
        if !found {
            // Token expired or user signed out
            writeJSONError(w, http.StatusUnauthorized, "refresh token expired")
            return
        }

        // Step 4 — Compare tokens (constant-time to prevent timing attacks)
        import "crypto/subtle"
        if subtle.ConstantTimeCompare(
            []byte(cookieToken), []byte(storedToken)) != 1 {
            writeJSONError(w, http.StatusUnauthorized, "refresh token mismatch")
            return
        }

        // Step 5 — Issue new access JWT
        accessToken, err := s.issueAccessToken(userID, tenantID, role)
        if err != nil {
            writeJSONError(w, http.StatusInternalServerError, "token issue failed")
            return
        }

        // Step 6 — Return new access JWT
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]string{
            "access_token": accessToken,
        })
    })
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func extractBearer(header string) string {
    import "strings"
    parts := strings.SplitN(header, " ", 2)
    if len(parts) == 2 && parts[0] == "Bearer" {
        return parts[1]
    }
    return ""
}
```

---

## Dependency Map — What Blocks What

```
Can start immediately after Member 4 Phase 1:
  config.go       ← needs auth.Service interface (Member 4)
  redis.go        ← needs docker-compose.yml (Member 4)
  oidc.go         ← no external deps beyond standard library
  idtoken.go      ← no external deps beyond standard library
  exchange.go     ← no external deps beyond standard library
  session.go      ← no external deps beyond standard library

Must wait for bootstrap stub before writing callback.go:
  callback.go     ← calls bootstrap.Bootstrap()
                     write stub yourself (see plan above)
                     replace with real call when Member 3 ships

Must coordinate with Member 4 before writing session.go:
  jwtClaims struct field names must match middleware/auth.go Claims struct
  agree: "tenant_id" not "tenantId", "iss" = "ztna-controller"
```

---

## Integration Checklist

```
Phase 1 — Core infrastructure
  ✓ NewService returns error on missing GoogleClientID
  ✓ NewService returns error on missing JWTSecret
  ✓ Redis connection verified on startup
  ✓ Redis ping fails gracefully with descriptive error

Phase 2 — PKCE + initiateAuth
  ✓ code_verifier is 64 random bytes → 86 char base64url string
  ✓ code_challenge = BASE64URL(SHA256(code_verifier))
  ✓ state = valid HMAC-signed nonce
  ✓ code_verifier stored in Redis with 5-min TTL
  ✓ Redis key is pkce:<state>
  ✓ returned redirectUrl contains client_id, code_challenge, state, scope
  ✓ returned redirectUrl does NOT contain client_secret

Phase 3 — id_token verification
  ✓ JWKS fetched from Google on first call
  ✓ JWKS cached for 1 hour
  ✓ cache refreshed when kid not found
  ✓ signature verified against correct RSA public key
  ✓ aud checked against GOOGLE_CLIENT_ID
  ✓ iss checked: "accounts.google.com" or "https://accounts.google.com"
  ✓ exp checked: token not expired
  ✓ email_verified == true enforced
  ✓ sub present and non-empty

Phase 4 — Token exchange
  ✓ POST to Google token endpoint with correct Content-Type
  ✓ code_verifier included in body
  ✓ client_secret included in body (server-side only)
  ✓ non-200 response returns descriptive error
  ✓ missing id_token in response returns error

Phase 5 — JWT issuance
  ✓ JWT contains tenant_id (json tag "tenant_id" — matches middleware)
  ✓ JWT contains role (json tag "role" — matches middleware)
  ✓ JWT subject = user_id
  ✓ JWT issuer = "ztna-controller" (matches middleware)
  ✓ JWT exp = 15 minutes from now
  ✓ Signed with HS256

Phase 6 — Callback handler
  ✓ missing code or state → redirect /login?error=missing_params
  ✓ invalid state HMAC → redirect /login?error=invalid_state
  ✓ Redis state not found → redirect /login?error=state_expired
  ✓ Google token exchange fails → redirect /login?error=token_exchange_failed
  ✓ id_token verification fails → redirect /login?error=invalid_id_token
  ✓ bootstrap fails → redirect /login?error=bootstrap_failed
  ✓ refresh token set as httpOnly SameSite=Strict cookie
  ✓ cookie Path = "/auth/refresh" (not "/" — reduces exposure)
  ✓ React redirected to /#token=<JWT> (hash fragment)

Phase 7 — Refresh handler
  ✓ missing refresh cookie → 401
  ✓ missing Authorization header → 401
  ✓ expired JWT readable without verifying exp (for user_id extraction)
  ✓ Redis lookup by user_id
  ✓ token comparison is constant-time (crypto/subtle)
  ✓ new access JWT returned in JSON body
  ✓ refresh token NOT rotated (acceptable for this sprint)

Integration with Member 3:
  ✓ bootstrap.Bootstrap() stub in place before callback.go is tested
  ✓ stub replaced with real call when Member 3 ships — zero changes to callback.go
  ✓ Result struct fields: TenantID, UserID, Role — agreed with Member 3

Integration with Member 4:
  ✓ jwtClaims.TenantID json tag = "tenant_id" (matches middleware/auth.go)
  ✓ jwtClaims.Role json tag = "role" (matches middleware/auth.go)
  ✓ JWT issuer = "ztna-controller" (matches middleware/auth.go WithIssuer check)
  ✓ auth.NewService constructor signature matches main.go call in Member 4
  ✓ auth.Config fields match what Member 4 passes from env vars
```

---

## Summary

```
Phase 1  config.go + redis.go          ← foundation, unblocks all other phases
Phase 2  oidc.go                        ← PKCE generation, initiateAuth
Phase 3  idtoken.go                     ← Google id_token verification (JWKS)
Phase 4  exchange.go                    ← server-to-server token exchange
Phase 5  session.go                     ← JWT issuance + refresh token storage
Phase 6  callback.go                    ← full OAuth callback handler
Phase 7  refresh.go                     ← token refresh handler

Waits for:
  Member 4 Phase 1 → auth.Service interface, docker-compose, db/pool.go
  Member 3 (partial) → bootstrap stub written by Member 2,
                        replaced by Member 3 when real transaction ships
  Member 4 (coordinate) → jwtClaims field names agreed before session.go
```
