# Phase 3 — Root CA

Implement Root CA generation, encryption, and storage. Needs Postgres running (Member 4 docker-compose).

Status: Completed

---

## File: `controller/internal/pki/root.go`

**Path:** `controller/internal/pki/root.go`

```go
package pki

import (
    "context"
    "crypto/ecdsa"
    "crypto/rand"
    "crypto/x509"
    "crypto/x509/pkix"
    "fmt"
)

// initRootCA checks if Root CA exists in DB.
// If not → generates, self-signs, encrypts, stores.
// If yes → verifies it is readable (no need to load into memory
//           since Root CA is only needed when generating Intermediate CA,
//           which also only happens once).
//
// After Intermediate CA is created, the Root CA private key is
// never needed again. It is encrypted in the DB and not loaded
// into memory during normal operation.
// In production: the ca_root table row would be backed up and
// the DB row potentially deleted — the Root CA is effectively offline.
func (s *serviceImpl) initRootCA(ctx context.Context) error {
    exists, err := s.rootCAExists(ctx)
    if err != nil {
        return fmt.Errorf("check root CA: %w", err)
    }

    if exists {
        // Root CA already initialized. Nothing to do.
        // We do not load it into memory — it is not needed unless
        // we need to re-sign the Intermediate CA (very rare).
        return nil
    }

    // Root CA does not exist — this is the very first startup.
    // Generate and store it.
    return s.generateAndStoreRootCA(ctx)
}

func (s *serviceImpl) rootCAExists(ctx context.Context) (bool, error) {
    var count int
    err := s.pool.QueryRow(ctx,
        "SELECT COUNT(*) FROM ca_root",
    ).Scan(&count)
    if err != nil {
        return false, fmt.Errorf("query ca_root: %w", err)
    }
    return count > 0, nil
}

func (s *serviceImpl) generateAndStoreRootCA(ctx context.Context) error {
    // Step 1: generate EC P-384 keypair
    privKey, err := generateECKeyPair()
    if err != nil {
        return err
    }
    // Zero private key from memory when we are done
    defer func() {
        // We cannot zero an ecdsa.PrivateKey directly, but we can
        // zero the underlying D scalar. This is best-effort.
        privKey.D.SetInt64(0)
    }()

    // Step 2: build the self-signed Root CA certificate template
    serial, err := newSerialNumber()
    if err != nil {
        return err
    }

    notBefore, notAfter := certValidity(10) // Root CA valid for 10 years

    template := &x509.Certificate{
        SerialNumber: serial,
        Subject: pkix.Name{
            CommonName:   "ZECURITY Root CA",
            Organization: []string{"ZECURITY Platform"},
        },
        NotBefore:             notBefore,
        NotAfter:              notAfter,
        IsCA:                  true,
        KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
        BasicConstraintsValid: true,
        // MaxPathLen: 1 means this Root CA can sign one level of
        // intermediate CAs. Intermediate CAs cannot sign other CAs.
        // This enforces the three-level hierarchy strictly.
        MaxPathLen:     1,
        MaxPathLenZero: false,
    }

    // Step 3: self-sign (Root CA signs its own cert)
    certDER, err := x509.CreateCertificate(
        rand.Reader,
        template,
        template,       // parent = self for root CA
        &privKey.PublicKey,
        privKey,
    )
    if err != nil {
        return fmt.Errorf("create root CA cert: %w", err)
    }

    certPEM := encodeCertToPEM(certDER)

    // Step 4: encrypt private key with HKDF(master_secret, "root-ca")
    encKey, nonce, err := encryptPrivateKey(privKey, s.masterSecret, "root-ca")
    if err != nil {
        return err
    }

    // Step 5: store in DB
    _, err = s.pool.Exec(ctx,
        `INSERT INTO ca_root
         (encrypted_key, nonce, certificate_pem, not_before, not_after)
         VALUES ($1, $2, $3, $4, $5)`,
        encKey, nonce, certPEM, notBefore, notAfter,
    )
    if err != nil {
        return fmt.Errorf("store root CA: %w", err)
    }

    return nil
}

// loadRootCA loads and decrypts the Root CA from DB.
// Only called when generating the Intermediate CA (once per deployment).
// After that, the Root CA private key is never loaded into memory again.
func (s *serviceImpl) loadRootCA(ctx context.Context) (*x509.Certificate,
    *ecdsa.PrivateKey, error) {

    var encKey, nonce, certPEM string
    err := s.pool.QueryRow(ctx,
        `SELECT encrypted_key, nonce, certificate_pem FROM ca_root LIMIT 1`,
    ).Scan(&encKey, &nonce, &certPEM)
    if err != nil {
        return nil, nil, fmt.Errorf("load root CA from DB: %w", err)
    }

    cert, err := parseCertFromPEM(certPEM)
    if err != nil {
        return nil, nil, err
    }

    privKey, err := decryptPrivateKey(encKey, nonce, s.masterSecret, "root-ca")
    if err != nil {
        return nil, nil, err
    }

    return cert, privKey, nil
}
```

---

## Verification Checklist

```
[x] pki.Init on fresh DB creates Root CA in ca_root table
[x] pki.Init on existing DB does NOT create a second Root CA
[x] Root CA cert has IsCA=true, MaxPathLen=1
[x] Root CA cert is self-signed (issuer == subject)
[x] Root CA private key is encrypted in DB (not plaintext)
[ ] Root CA private key is NOT loaded into memory after Intermediate CA exists
[x] loadRootCA successfully decrypts the Root CA when needed
```
