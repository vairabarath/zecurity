package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/yourorg/ztna/controller/graph/model"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
)

// InitiateAuth implements auth.Service.InitiateAuth.
// Called by: the initiateAuth GraphQL mutation resolver (Member 4 writes the resolver in
//
//	graph/resolvers/schema.resolvers.go → calls authService.InitiateAuth()).
//
// What it does:
//  1. Resolves the identity connection (Bootstrap platform IdP for `provider`)
//     and selects its protocol adapter via the factory (providerFor).
//  2. Generates PKCE (verifier + challenge) and an OIDC nonce.
//  3. Generates a signed state value for CSRF protection.
//  4. Stores {verifier, workspaceName, nonce, connection_id} in Redis by state.
//  5. Builds the provider's authorization URL via the adapter.
//  6. Returns the URL + state to the caller.
//
// Provider selection is fully factory-driven — there is no per-provider
// branching here. Google/bootstrap stays behavior-identical (GoogleProvider
// builds the same URL); enterprise OIDC is reachable once a connectionId is
// supplied (Chunk F).
func (s *serviceImpl) InitiateAuth(ctx context.Context, provider string, workspaceName, connectionID *string) (*model.AuthInitPayload, error) {
	// 1. Resolve the connection + its adapter (single switch point). connectionID,
	// when set (from the discovery API), is the only selector; else the platform
	// IdP for `provider` is used. Invalid or non-active connections fail closed.
	cid := ""
	if connectionID != nil {
		cid = *connectionID
	}
	conn, err := s.resolveConnection(ctx, provider, cid)
	if err != nil {
		return nil, fmt.Errorf("resolve connection: %w", err)
	}
	if conn.Status != "active" {
		return nil, fmt.Errorf("resolve connection: connection is not active")
	}
	adapter, err := providerForFn(conn, s.googleCreds())
	if err != nil {
		return nil, fmt.Errorf("select provider: %w", err)
	}

	// 2. PKCE: 64 random bytes → base64url (86 chars, within RFC 7636 43–128).
	verifierBytes := make([]byte, 64)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generate code_verifier: %w", err)
	}
	codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	hash := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])

	// 2b. OIDC nonce — generated once, round-trips via Redis, enforced by OIDC
	// adapters at the callback (Google adapter ignores it — see google_provider.go).
	nonce, err := generateNonce()
	if err != nil {
		return nil, err
	}

	// 3. Signed state (CSRF): HMAC nonce that survives the redirect.
	state, err := generateSignedState(s.cfg.JWTSecret)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	// 4. Store the login scratchpad keyed by state (single-use, 5-min TTL).
	ws := ""
	if workspaceName != nil {
		ws = *workspaceName
	}
	if err := s.redisClient.SetPKCEState(ctx, state, PKCEState{
		CodeVerifier:  codeVerifier,
		WorkspaceName: ws,
		Nonce:         nonce,
		ConnectionID:  conn.ID,
	}); err != nil {
		return nil, fmt.Errorf("store pkce state: %w", err)
	}

	// 5. Build the authorization URL via the adapter.
	redirectURL, err := adapter.AuthURL(ctx, providers.AuthURLParams{
		State:         state,
		Nonce:         nonce,
		CodeChallenge: codeChallenge,
		RedirectURI:   s.cfg.RedirectURI,
	})
	if err != nil {
		return nil, fmt.Errorf("build auth url: %w", err)
	}

	return &model.AuthInitPayload{
		RedirectURL: redirectURL,
		State:       state,
	}, nil
}

// generateNonce returns a 256-bit base64url OIDC nonce.
func generateNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
