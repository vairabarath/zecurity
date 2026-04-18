package auth

import (
	"context"
	"strings"
	"testing"
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
	}

	_, err := svc.InitiateAuth(context.Background(), "github", nil)
	if err == nil {
		t.Fatal("expected error for unsupported provider")
	}
	if !strings.Contains(err.Error(), "unsupported provider") {
		t.Fatalf("expected 'unsupported provider' error, got: %v", err)
	}
}

func TestInitiateAuth_Google_Success(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
	}

	result, err := svc.InitiateAuth(context.Background(), "google", nil)
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
	}

	result, err := svc.InitiateAuth(context.Background(), "google", nil)
	if err != nil {
		t.Fatalf("InitiateAuth: %v", err)
	}

	// The code_verifier should be stored in Redis keyed by state.
	verifier, workspaceName, found, err := rc.GetAndDeletePKCEState(context.Background(), result.State)
	if err != nil {
		t.Fatalf("GetAndDeletePKCEState: %v", err)
	}
	if !found {
		t.Fatal("PKCE state not found in Redis after InitiateAuth")
	}
	// code_verifier = base64url(64 bytes) = 86 chars (RFC 7636 range: 43–128).
	if len(verifier) < 43 || len(verifier) > 128 {
		t.Fatalf("code_verifier length %d outside RFC 7636 range [43,128]", len(verifier))
	}
	if workspaceName != "" {
		t.Fatalf("expected empty workspace name, got %q", workspaceName)
	}
}
