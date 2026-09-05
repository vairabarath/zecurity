package pki

import (
	"bytes"
	"strings"
	"testing"
)

// newTestPKI builds a serviceImpl with only the master secret populated — enough
// to exercise the secret encrypt/decrypt path without a DB or CA material.
func newTestPKI() *serviceImpl {
	return &serviceImpl{masterSecret: "test-master-secret-at-least-32-bytes-long!!"}
}

func TestEncryptDecryptSecret_RoundTrip(t *testing.T) {
	s := newTestPKI()
	const ctx = "idp-client-secret:tenant-abc"
	plaintext := []byte("super-secret-oidc-client-secret")

	ct, nonce, err := s.EncryptSecret(plaintext, ctx)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if ct == "" || nonce == "" {
		t.Fatal("expected non-empty ciphertext and nonce")
	}
	if strings.Contains(ct, string(plaintext)) {
		t.Fatal("ciphertext must not contain the plaintext")
	}

	got, err := s.DecryptSecret(ct, nonce, ctx)
	if err != nil {
		t.Fatalf("DecryptSecret: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestDecryptSecret_WrongContextFails(t *testing.T) {
	s := newTestPKI()
	plaintext := []byte("another-secret")

	ct, nonce, err := s.EncryptSecret(plaintext, "idp-client-secret:tenant-a")
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	// A different HKDF context derives a different key → GCM auth must fail.
	if _, err := s.DecryptSecret(ct, nonce, "idp-client-secret:tenant-b"); err == nil {
		t.Fatal("expected decryption under a different context to fail")
	}
}

func TestEncryptSecret_UniqueNonces(t *testing.T) {
	s := newTestPKI()
	const ctx = "idp-client-secret:tenant-abc"

	_, nonce1, err := s.EncryptSecret([]byte("x"), ctx)
	if err != nil {
		t.Fatalf("EncryptSecret 1: %v", err)
	}
	_, nonce2, err := s.EncryptSecret([]byte("x"), ctx)
	if err != nil {
		t.Fatalf("EncryptSecret 2: %v", err)
	}
	if nonce1 == nonce2 {
		t.Fatal("nonces must be unique per encryption")
	}
}
