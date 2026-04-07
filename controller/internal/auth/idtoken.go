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
// Used by: VerifyGoogleIDToken() below.
// Consumed by: CallbackHandler() in callback.go (Step 5–6) to extract email, sub, name.
type GoogleClaims struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	Sub           string `json:"sub"` // immutable subject — use this as provider_sub, NOT email
	jwt.RegisteredClaims
}

// jwksCache caches Google's public keys to avoid fetching on every request.
// Keys are refreshed when they expire (1h TTL) or a new kid is encountered.
// Thread-safe via sync.RWMutex — multiple goroutines can read concurrently.
var jwksCache struct {
	sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

const jwksURL = "https://www.googleapis.com/oauth2/v3/certs"
const jwksCacheTTL = 1 * time.Hour

// VerifyGoogleIDToken verifies a Google id_token and returns the claims.
// Called by: CallbackHandler() in callback.go (Step 5).
//
// Checks performed (ALL must pass — skipping any one is a vulnerability):
//
//  1. Signature valid (signed by Google's current RSA public key)
//  2. aud == our Google Client ID (prevents tokens issued for other apps)
//  3. iss == "accounts.google.com" or "https://accounts.google.com"
//  4. exp > now() (token not expired)
//  5. email_verified == true (Google confirmed the email)
//  6. sub is present and non-empty (immutable identity anchor)
//
// Skipping check 2 (aud) allows tokens issued for other Google apps
// to authenticate users in your system — that is an auth bypass.
func VerifyGoogleIDToken(ctx context.Context, idToken, clientID string) (*GoogleClaims, error) {
	// Parse without verification first to extract the key ID (kid) from the header.
	// This tells us which RSA public key Google used to sign this token.
	unverified, _, err := new(jwt.Parser).ParseUnverified(idToken, &GoogleClaims{})
	if err != nil {
		return nil, fmt.Errorf("parse id_token: %w", err)
	}

	kid, ok := unverified.Header["kid"].(string)
	if !ok || kid == "" {
		return nil, fmt.Errorf("id_token missing kid header")
	}

	// Get Google's public key for this kid.
	// Called: getGooglePublicKey() below (uses JWKS cache).
	pubKey, err := getGooglePublicKey(ctx, kid)
	if err != nil {
		return nil, fmt.Errorf("get google public key: %w", err)
	}

	// Now parse WITH full cryptographic verification.
	claims := &GoogleClaims{}
	token, err := jwt.ParseWithClaims(
		idToken, claims,
		func(t *jwt.Token) (interface{}, error) {
			// Check 1: signing method must be RS256.
			// Blocks alg=none and alg=HS256 attacks.
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return pubKey, nil
		},
		// Check 3: issuer must be Google.
		// jwt/v5 only supports a single issuer per WithIssuer call,
		// so we verify manually after parsing (see below).

		// Check 4: expiry must be present and in the future.
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return nil, fmt.Errorf("id_token verification failed: %w", err)
	}

	// Check 2: audience must be our client ID.
	// Prevents accepting tokens issued for other Google OAuth apps.
	aud, err := claims.GetAudience()
	if err != nil || !containsString(aud, clientID) {
		return nil, fmt.Errorf("id_token audience mismatch: expected %s", clientID)
	}

	// Check 3 (continued): issuer must be Google.
	// Google tokens use either form — both are valid per Google's docs.
	iss, err := claims.GetIssuer()
	if err != nil || (iss != "accounts.google.com" && iss != "https://accounts.google.com") {
		return nil, fmt.Errorf("id_token issuer invalid: %s", iss)
	}

	// Check 5: email must be verified by Google.
	// Unverified emails could be spoofed — never trust them for identity.
	if !claims.EmailVerified {
		return nil, fmt.Errorf("id_token email not verified")
	}

	// Check 6: sub must be present.
	// Sub is the immutable identity anchor — used as provider_sub in our DB.
	if claims.Sub == "" {
		return nil, fmt.Errorf("id_token missing sub claim")
	}

	return claims, nil
}

// getGooglePublicKey returns the RSA public key for a given kid.
// Uses a local cache with 1-hour TTL to avoid hammering Google's JWKS endpoint.
// Called by: VerifyGoogleIDToken() above.
func getGooglePublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	// Try cache first (read lock — allows concurrent readers).
	jwksCache.RLock()
	if time.Since(jwksCache.fetchedAt) < jwksCacheTTL {
		if key, ok := jwksCache.keys[kid]; ok {
			jwksCache.RUnlock()
			return key, nil
		}
	}
	jwksCache.RUnlock()

	// Cache miss or expired — fetch fresh keys (write lock).
	jwksCache.Lock()
	defer jwksCache.Unlock()

	// Double-check after acquiring write lock (another goroutine may have fetched).
	if time.Since(jwksCache.fetchedAt) < jwksCacheTTL {
		if key, ok := jwksCache.keys[kid]; ok {
			return key, nil
		}
	}

	// Called: fetchGoogleJWKS() below.
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

// fetchGoogleJWKS fetches Google's current JWKS and returns a map of kid → RSA public key.
// Called by: getGooglePublicKey() above (on cache miss or expiry).
// Google rotates keys periodically — this ensures we always have the current set.
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

	// Parse the JWKS JSON response.
	// Each key has a kid (key ID), n (modulus), and e (exponent) for RSA.
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
			continue // skip malformed keys
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue // skip malformed keys
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

// containsString checks if a string slice contains a target value.
// Called by: VerifyGoogleIDToken() above (audience check).
func containsString(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
