# Phase 1 — Crypto Helpers

Pure functions with no external dependencies. No DB, no pool, no config. Start here immediately.

Status: Completed

---

## File: `controller/internal/pki/crypto.go`

**Path:** `controller/internal/pki/crypto.go`

```go
package pki

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/ecdsa"
    "crypto/elliptic"
    "crypto/hkdf"
    "crypto/rand"
    "crypto/sha256"
    "crypto/x509"
    "encoding/base64"
    "encoding/pem"
    "fmt"
    "math/big"
    "time"
)

// generateECKeyPair generates an EC P-384 keypair.
// P-384 is chosen over P-256 for higher security margin.
// It is NIST-approved and widely supported by TLS libraries.
// The private key is returned as a Go struct — never written to disk
// unencrypted. Caller is responsible for encrypting before storage.
func generateECKeyPair() (*ecdsa.PrivateKey, error) {
    key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
    if err != nil {
        return nil, fmt.Errorf("generate EC P-384 key: %w", err)
    }
    return key, nil
}

// encryptPrivateKey encrypts an ECDSA private key using AES-256-GCM.
//
// The encryption key is derived using HKDF-SHA256 from:
//   - masterSecret: the PKI_MASTER_SECRET env var
//   - context: a string that uniquely identifies what is being encrypted
//     For Root CA:         "root-ca"
//     For Intermediate CA: "intermediate-ca"
//     For WorkspaceCA:     tenant_id (UUID string)
//
// HKDF ensures that each CA's encryption key is unique even though
// they all derive from the same master secret. Compromising one
// encrypted key does not help decrypt the others.
//
// Returns: (ciphertext base64, nonce base64, error)
func encryptPrivateKey(key *ecdsa.PrivateKey,
    masterSecret, context string) (string, string, error) {

    // Marshal private key to DER (binary) for encryption
    keyDER, err := x509.MarshalECPrivateKey(key)
    if err != nil {
        return "", "", fmt.Errorf("marshal private key: %w", err)
    }
    defer zeroBytes(keyDER)

    // Derive a 32-byte (256-bit) encryption key using HKDF-SHA256.
    // On this repo's Go toolchain, the stdlib API is hkdf.Key(...)
    // instead of the older reader-style hkdf.New(...).
    encKey, err := hkdf.Key(
        sha256.New,
        []byte(masterSecret),
        nil,            // salt: nil is valid per RFC 5869
        context,
        32,
    )
    if err != nil {
        return "", "", fmt.Errorf("derive encryption key: %w", err)
    }
    defer zeroBytes(encKey)

    // Encrypt with AES-256-GCM
    block, err := aes.NewCipher(encKey)
    if err != nil {
        return "", "", fmt.Errorf("create AES cipher: %w", err)
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", "", fmt.Errorf("create GCM: %w", err)
    }

    // Generate a random nonce (12 bytes for GCM)
    nonce := make([]byte, gcm.NonceSize())
    if _, err := rand.Read(nonce); err != nil {
        return "", "", fmt.Errorf("generate nonce: %w", err)
    }

    ciphertext := gcm.Seal(nil, nonce, keyDER, nil)

    return base64.StdEncoding.EncodeToString(ciphertext),
        base64.StdEncoding.EncodeToString(nonce),
        nil
}

// decryptPrivateKey reverses encryptPrivateKey.
// Used when loading a CA from DB to sign something.
// The decrypted key must be zeroed from memory after use.
func decryptPrivateKey(ciphertextB64, nonceB64,
    masterSecret, context string) (*ecdsa.PrivateKey, error) {

    ciphertext, err := base64.StdEncoding.DecodeString(ciphertextB64)
    if err != nil {
        return nil, fmt.Errorf("decode ciphertext: %w", err)
    }

    nonce, err := base64.StdEncoding.DecodeString(nonceB64)
    if err != nil {
        return nil, fmt.Errorf("decode nonce: %w", err)
    }

    // Re-derive the same key using the same HKDF inputs.
    encKey, err := hkdf.Key(
        sha256.New,
        []byte(masterSecret),
        nil,
        context,
        32,
    )
    if err != nil {
        return nil, fmt.Errorf("derive decryption key: %w", err)
    }
    defer zeroBytes(encKey)

    block, err := aes.NewCipher(encKey)
    if err != nil {
        return nil, fmt.Errorf("create AES cipher: %w", err)
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("create GCM: %w", err)
    }

    keyDER, err := gcm.Open(nil, nonce, ciphertext, nil)
    if err != nil {
        return nil, fmt.Errorf("decrypt private key: %w", err)
    }
    defer zeroBytes(keyDER)

    privKey, err := x509.ParseECPrivateKey(keyDER)
    if err != nil {
        return nil, fmt.Errorf("parse private key: %w", err)
    }

    return privKey, nil
}

// encodeCertToPEM encodes a DER certificate to PEM format.
func encodeCertToPEM(certDER []byte) string {
    return string(pem.EncodeToMemory(&pem.Block{
        Type:  "CERTIFICATE",
        Bytes: certDER,
    }))
}

// parseCertFromPEM parses a PEM-encoded certificate.
func parseCertFromPEM(certPEM string) (*x509.Certificate, error) {
    block, _ := pem.Decode([]byte(certPEM))
    if block == nil {
        return nil, fmt.Errorf("failed to decode PEM block")
    }
    cert, err := x509.ParseCertificate(block.Bytes)
    if err != nil {
        return nil, fmt.Errorf("parse certificate: %w", err)
    }
    return cert, nil
}

// newSerialNumber generates a random certificate serial number.
// X.509 serial numbers must be unique per CA.
// Using 128 random bits makes collision probability negligible.
func newSerialNumber() (*big.Int, error) {
    serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
    serial, err := rand.Int(rand.Reader, serialLimit)
    if err != nil {
        return nil, fmt.Errorf("generate serial number: %w", err)
    }
    return serial, nil
}

// zeroBytes overwrites a byte slice with zeros.
// Used to clear sensitive key material from memory after use.
// Go's GC does not guarantee when memory is freed, so we zero it explicitly.
func zeroBytes(b []byte) {
    for i := range b {
        b[i] = 0
    }
}

// certValidity returns notBefore and notAfter for a CA certificate.
// Root CA:         10 years
// Intermediate CA: 5 years
// WorkspaceCA:     2 years
func certValidity(years int) (time.Time, time.Time) {
    now := time.Now().UTC()
    // notBefore is backdated by 1 hour to handle clock skew
    // between the issuing server and any verifier.
    notBefore := now.Add(-1 * time.Hour)
    notAfter := now.AddDate(years, 0, 0)
    return notBefore, notAfter
}
```

---

## Verification Checklist

```
[x] generateECKeyPair returns a valid EC P-384 key
[x] encryptPrivateKey and decryptPrivateKey are inverse operations
[x] different contexts produce different encryption keys from same master secret
[x] zeroBytes clears the slice
[x] encodeCertToPEM and parseCertFromPEM are inverse operations
[x] newSerialNumber returns different values on repeated calls
[x] certValidity returns correct time ranges for different CA types
```
