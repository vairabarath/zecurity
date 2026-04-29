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
	WorkspaceID        string
	WorkspaceSlug      string
	Email              string    // set by AuthCallbackHandler after Google verifies identity
	GoogleSub          string    // set by AuthCallbackHandler — used as provider_sub in upsertUser
	CliCodeChallenge   string    // BASE64URL(SHA256(cli_code_verifier)) — verified in TokenExchange
	LocalRedirectURI   string    // http://127.0.0.1:<port>/callback — CLI's local server
	GoogleCodeVerifier string    // controller's own Google PKCE verifier — never leaves server
	CtrlCode           string    // one-time code; set after Google callback completes
	CtrlCodeExpiresAt  time.Time // 60-second TTL after ctrl_code is issued
	ExpiresAt          time.Time // 10-minute overall session TTL
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
func updateSessionCtrlCode(id, email, googleSub, ctrlCode string, expiresAt time.Time) bool {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	s, ok := sessionStore[id]
	if !ok || time.Now().After(s.ExpiresAt) {
		return false
	}
	s.Email = email
	s.GoogleSub = googleSub
	s.CtrlCode = ctrlCode
	s.CtrlCodeExpiresAt = expiresAt
	return true
}
