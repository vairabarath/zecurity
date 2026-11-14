package client

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// authSession holds the state for one CLI login attempt. It is created by
// InitiateAuth, updated by AuthCallbackHandler, and consumed (deleted) by
// TokenExchange. All access is protected by sessionMu.
type authSession struct {
	WorkspaceID       string
	WorkspaceSlug     string
	ConnectionID      string    // identity_connections id resolved at InitiateAuth (adapter selection)
	Provider          string    // set by AuthCallbackHandler after the IdP verifies identity
	Email             string    // set by AuthCallbackHandler after the IdP verifies identity
	Subject           string    // set by AuthCallbackHandler — immutable per-issuer subject → provider_sub
	IdpMFA            bool      // set by AuthCallbackHandler — the IdP's verified amr proved MFA (PENDING-06 M-3)
	CliCodeChallenge  string    // BASE64URL(SHA256(cli_code_verifier)) — verified in TokenExchange
	LocalRedirectURI  string    // http://127.0.0.1:<port>/callback — CLI's local server
	IdpCodeVerifier   string    // controller's own IdP PKCE verifier — never leaves server
	Nonce             string    // OIDC nonce — round-trips to the IdP, checked at callback
	CtrlCode          string    // one-time code; set after the IdP callback completes
	CtrlCodeExpiresAt time.Time // 60-second TTL after ctrl_code is issued
	ExpiresAt         time.Time // 10-minute overall session TTL

	// Reauth is set when this round trip is a step-up for an enrolled device
	// (PENDING-06 M-5, reauth.go), not a sign-in. Such a session never yields
	// a ctrl_code or tokens.
	Reauth *reauthInfo
}

var (
	sessionMu    sync.Mutex
	sessionStore = map[string]*authSession{}
)

func newSessionID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newCtrlCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func putSession(id string, sess *authSession) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	now := time.Now()
	for k, v := range sessionStore {
		if now.After(v.ExpiresAt) {
			delete(sessionStore, k)
		}
	}
	sessionStore[id] = sess
}

func getSession(id string) (*authSession, bool) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	s, ok := sessionStore[id]
	if !ok || time.Now().After(s.ExpiresAt) {
		delete(sessionStore, id)
		return nil, false
	}
	return s, true
}

// consumeSession deletes and returns the session atomically — single-use.
func consumeSession(id string) (*authSession, bool) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	s, ok := sessionStore[id]
	if !ok || time.Now().After(s.ExpiresAt) {
		delete(sessionStore, id)
		return nil, false
	}
	delete(sessionStore, id)
	return s, true
}

// updateSessionCtrlCode records the verified identity and ctrl_code after the
// Google callback completes. Returns false if the session has already expired.
func updateSessionCtrlCode(id, email, provider, subject string, idpMFA bool, ctrlCode string, expiresAt time.Time) bool {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	s, ok := sessionStore[id]
	if !ok || time.Now().After(s.ExpiresAt) {
		return false
	}
	s.Email = email
	s.Provider = provider
	s.Subject = subject
	s.IdpMFA = idpMFA
	s.CtrlCode = ctrlCode
	s.CtrlCodeExpiresAt = expiresAt
	return true
}
