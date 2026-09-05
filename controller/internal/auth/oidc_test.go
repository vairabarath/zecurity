package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/idp"
)

func TestGenerateSignedState_Format(t *testing.T) {
	state, err := generateSignedState("test-secret")
	if err != nil {
		t.Fatalf("generateSignedState: %v", err)
	}

	// State format: base64url(nonce) + "." + base64url(HMAC)
	parts := strings.SplitN(state, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts separated by '.', got %d", len(parts))
	}
	if parts[0] == "" || parts[1] == "" {
		t.Fatal("nonce or signature part is empty")
	}
}

func TestGenerateSignedState_Unique(t *testing.T) {
	// Two calls should produce different states (random nonce).
	s1, _ := generateSignedState("secret")
	s2, _ := generateSignedState("secret")
	if s1 == s2 {
		t.Fatal("expected unique states, got identical")
	}
}

func TestVerifySignedState_Valid(t *testing.T) {
	secret := "my-jwt-secret"
	state, err := generateSignedState(secret)
	if err != nil {
		t.Fatalf("generateSignedState: %v", err)
	}

	if err := verifySignedState(state, secret); err != nil {
		t.Fatalf("expected valid state, got error: %v", err)
	}
}

func TestVerifySignedState_WrongSecret(t *testing.T) {
	state, _ := generateSignedState("correct-secret")

	err := verifySignedState(state, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestVerifySignedState_TamperedNonce(t *testing.T) {
	state, _ := generateSignedState("secret")

	// Tamper with the nonce part (flip a character).
	parts := strings.SplitN(state, ".", 2)
	tampered := "AAAA" + parts[0][4:] + "." + parts[1]

	err := verifySignedState(tampered, "secret")
	if err == nil {
		t.Fatal("expected error for tampered nonce")
	}
}

func TestVerifySignedState_InvalidFormat(t *testing.T) {
	if err := verifySignedState("no-dot-separator", "secret"); err == nil {
		t.Fatal("expected error for missing dot separator")
	}
	if err := verifySignedState("", "secret"); err == nil {
		t.Fatal("expected error for empty state")
	}
}

func TestInitiateAuth_UnsupportedProvider(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
		idpStore:    googleConnStore(),
	}

	// An unconfigured provider now resolves to no connection → fail closed.
	_, err := svc.InitiateAuth(context.Background(), "github", nil, nil)
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
	if !strings.Contains(err.Error(), "connection not found") {
		t.Fatalf("expected connection-not-found error, got: %v", err)
	}
}

func TestInitiateAuth_ByConnectionID_Success(t *testing.T) {
	rc, _ := newTestValkey(t)
	tenant := "ws-acme"
	store := &fakeConnStore{byID: map[string]*idp.Connection{
		"okta-conn": {ID: "okta-conn", Provider: "oidc", Protocol: "oidc", Status: "active", Issuer: "https://acme.okta.com", TenantID: &tenant},
	}}
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: store}
	withFakeProvider(t, &fakeProvider{}) // fake adapter → no network for AuthURL

	cid := "okta-conn"
	result, err := svc.InitiateAuth(context.Background(), "", nil, &cid)
	if err != nil {
		t.Fatalf("InitiateAuth by connectionId: %v", err)
	}
	// The scratchpad must record the selected connection.
	st, found, _ := rc.GetAndDeletePKCEState(context.Background(), result.State)
	if !found || st.ConnectionID != "okta-conn" {
		t.Fatalf("expected stored connection_id okta-conn, got %+v", st)
	}
}

func TestInitiateAuth_InvalidConnectionID_FailsClosed(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: &fakeConnStore{}} // no such id
	cid := "does-not-exist"
	if _, err := svc.InitiateAuth(context.Background(), "", nil, &cid); err == nil {
		t.Fatal("invalid connectionId must fail closed")
	}
}

func TestInitiateAuth_DisabledConnectionID_FailsClosed(t *testing.T) {
	rc, _ := newTestValkey(t)
	tenant := "ws-acme"
	store := &fakeConnStore{byID: map[string]*idp.Connection{
		"okta-conn": {ID: "okta-conn", Provider: "oidc", Protocol: "oidc", Status: "disabled", Issuer: "https://acme.okta.com", TenantID: &tenant},
	}}
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: store}
	cid := "okta-conn"
	if _, err := svc.InitiateAuth(context.Background(), "", nil, &cid); err == nil {
		t.Fatal("disabled connectionId must fail closed")
	}
}

func TestInitiateAuth_Google_Success(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
		idpStore:    googleConnStore(),
	}

	result, err := svc.InitiateAuth(context.Background(), "google", nil, nil)
	if err != nil {
		t.Fatalf("InitiateAuth: %v", err)
	}

	// Check redirect URL contains required params.
	url := result.RedirectURL
	if !strings.HasPrefix(url, "https://accounts.google.com/o/oauth2/v2/auth?") {
		t.Fatalf("unexpected URL prefix: %s", url)
	}
	for _, param := range []string{"client_id=", "redirect_uri=", "code_challenge=", "code_challenge_method=S256", "state=", "scope=", "response_type=code"} {
		if !strings.Contains(url, param) {
			t.Fatalf("URL missing param %q: %s", param, url)
		}
	}

	// URL must NOT contain client_secret.
	if strings.Contains(url, "client_secret") {
		t.Fatal("redirect URL must NOT contain client_secret")
	}

	// State should be non-empty and verifiable.
	if result.State == "" {
		t.Fatal("state is empty")
	}
	if err := verifySignedState(result.State, svc.cfg.JWTSecret); err != nil {
		t.Fatalf("returned state failed verification: %v", err)
	}
}

func TestInitiateAuth_StoresVerifierInRedis(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
		idpStore:    googleConnStore(),
	}

	result, err := svc.InitiateAuth(context.Background(), "google", nil, nil)
	if err != nil {
		t.Fatalf("InitiateAuth: %v", err)
	}

	// The code_verifier should be stored in Redis keyed by state.
	st, found, err := rc.GetAndDeletePKCEState(context.Background(), result.State)
	if err != nil {
		t.Fatalf("GetAndDeletePKCEState: %v", err)
	}
	if !found {
		t.Fatal("PKCE state not found in Redis after InitiateAuth")
	}
	// code_verifier = base64url(64 bytes) = 86 chars (RFC 7636 range: 43–128).
	if len(st.CodeVerifier) < 43 || len(st.CodeVerifier) > 128 {
		t.Fatalf("code_verifier length %d outside RFC 7636 range [43,128]", len(st.CodeVerifier))
	}
	if st.WorkspaceName != "" {
		t.Fatalf("expected empty workspace name, got %q", st.WorkspaceName)
	}
}
