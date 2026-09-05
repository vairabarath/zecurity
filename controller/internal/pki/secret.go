package pki

// This file exposes the CA key-storage encryption primitive for reuse by other
// subsystems that must persist a secret at rest — the first consumer is the
// per-workspace OIDC client_secret (PENDING-04 / ADR-023). Rather than
// re-implement AES-GCM elsewhere, callers reuse the exact, audited primitive
// (AES-256-GCM + HKDF-SHA256, secret.go → crypto.go) under the PKI master secret.

// EncryptSecret encrypts an arbitrary secret at rest under the PKI master
// secret, using the same AES-256-GCM + HKDF-SHA256 primitive as CA private-key
// storage. It returns base64-encoded ciphertext and nonce (store both).
//
// context MUST be a caller-unique, domain-separated label so the derived key
// never collides with another subsystem's — e.g. "idp-client-secret:"+tenantID.
// (Workspace CA keys derive under the bare tenantID; the "idp-client-secret:"
// prefix keeps IdP secrets cryptographically distinct.)
//
// Rotation caveat: secrets encrypted here share the PKI_MASTER_SECRET lifecycle
// — rotating that secret renders previously stored ciphertext undecryptable,
// exactly as with CA key material.
func (s *serviceImpl) EncryptSecret(plaintext []byte, context string) (string, string, error) {
	return sealWithDerivedKey(plaintext, s.masterSecret, context)
}

// DecryptSecret reverses EncryptSecret. The same context supplied at encryption
// time must be passed here or decryption fails.
func (s *serviceImpl) DecryptSecret(ciphertextB64, nonceB64, context string) ([]byte, error) {
	return openWithDerivedKey(ciphertextB64, nonceB64, s.masterSecret, context)
}
