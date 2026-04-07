# Phase 3 — id_token Verification

This is the most security-critical file Member 2 writes.
Every check here must be present. Skipping any one of them is a vulnerability.

---

## File: `internal/auth/idtoken.go`

**Path:** `internal/auth/idtoken.go`

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
// Called by: VerifyGoogleIDToken() below, CallbackHandler() in callback.go (Step 5)
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
// Called by: CallbackHandler() in callback.go (Step 5)
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
    // Called: getGooglePublicKey() below
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
// Called by: VerifyGoogleIDToken() above
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

    // Called: fetchGoogleJWKS() below
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
// Called by: getGooglePublicKey() above (on cache miss or expiry)
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

## Phase 3 Checklist

```
✓ JWKS fetched from Google on first call
✓ JWKS cached for 1 hour
✓ cache refreshed when kid not found
✓ signature verified against correct RSA public key
✓ aud checked against GOOGLE_CLIENT_ID
✓ iss checked: "accounts.google.com" or "https://accounts.google.com"
✓ exp checked: token not expired
✓ email_verified == true enforced
✓ sub present and non-empty
```
