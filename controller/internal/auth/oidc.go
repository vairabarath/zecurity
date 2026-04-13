package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/yourorg/ztna/controller/graph/model"
)

// InitiateAuth implements auth.Service.InitiateAuth.
// Called by: the initiateAuth GraphQL mutation resolver (Member 4 writes the resolver in
//
//	graph/resolvers/schema.resolvers.go → calls authService.InitiateAuth()).
//
// What it does:
//  1. Generates a cryptographically random code_verifier (PKCE, RFC 7636)
//  2. Derives code_challenge = BASE64URL(SHA256(code_verifier))
//  3. Generates a signed state value for CSRF protection
//  4. Stores code_verifier + optional workspaceName in Redis keyed by state, TTL=5min
//  5. Builds the Google OAuth authorization URL
//  6. Returns the URL + state to the caller
//
// React redirects the browser to the returned URL.
// React stores the state in sessionStorage for CSRF verification on return.
func (s *serviceImpl) InitiateAuth(ctx context.Context, provider string, workspaceName *string) (*model.AuthInitPayload, error) {
	// Only Google is supported in this sprint.
	// Other providers can be added here without changing the interface.
	if provider != "google" {
		return nil, fmt.Errorf("unsupported provider: %s", provider)
	}

	// 1. Generate code_verifier
	// 64 random bytes → base64url = 86 character string.
	// RFC 7636 requires 43–128 characters, this is 86 — within spec.
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
	// state = HMAC-signed nonce that survives the OAuth redirect.
	// The nonce is random. Signing it with JWT_SECRET lets the callback
	// verify the state was issued by this server (CSRF protection).
	// Called: generateSignedState() below.
	state, err := generateSignedState(s.cfg.JWTSecret)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	// 4. Store code_verifier in Redis keyed by state.
	// The callback retrieves this using the state value from the URL.
	// If workspaceName is provided (signup flow), it is stored alongside code_verifier.
	// Called: redis.go → SetPKCEState()
	if err := s.redisClient.SetPKCEState(ctx, state, codeVerifier, workspaceName); err != nil {
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

	redirectURL := "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()

	return &model.AuthInitPayload{
		RedirectURL: redirectURL,
		State:       state,
	}, nil
}

// generateSignedState creates a random nonce and signs it with HMAC-SHA256.
// Format: base64url(nonce) + "." + base64url(HMAC(nonce))
// The callback verifies the HMAC to confirm the state was issued by this server.
// Called by: InitiateAuth() above.
func generateSignedState(secret string) (string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(nonce)
	sig := mac.Sum(nil)

	nonceB64 := base64.RawURLEncoding.EncodeToString(nonce)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)

	return nonceB64 + "." + sigB64, nil
}

// verifySignedState checks the HMAC on a state value returned from Google.
// Returns an error if the state was tampered with or not issued by this server.
// Called by: CallbackHandler() in callback.go (Step 2).
func verifySignedState(state, secret string) error {
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

	// Use hmac.Equal for constant-time comparison (prevents timing attacks).
	if !hmac.Equal(gotSig, expectedSig) {
		return fmt.Errorf("state signature invalid")
	}

	return nil
}
