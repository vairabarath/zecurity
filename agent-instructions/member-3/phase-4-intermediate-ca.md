# Phase 4 — Intermediate CA

Implement Intermediate CA generation, signing by Root CA, encryption, and loading into memory. Needs root.go complete.

Status: Completed

---

## File: `controller/internal/pki/intermediate.go`

**Path:** `controller/internal/pki/intermediate.go`

```go
package pki

import (
    "context"
    "crypto/rand"
    "crypto/x509"
    "crypto/x509/pkix"
    "fmt"
)

// initIntermediateCA checks if Intermediate CA exists in DB.
// If not → loads Root CA, generates Intermediate CA, signs with Root CA,
//           stores Intermediate CA, zeros Root CA key from memory.
// If yes → loads Intermediate CA into svc.intermediateKey (in memory).
//
// The Intermediate CA MUST be in memory after this function returns.
// It is needed on every call to GenerateWorkspaceCA.
func (s *serviceImpl) initIntermediateCA(ctx context.Context) error {
    exists, err := s.intermediateCAExists(ctx)
    if err != nil {
        return fmt.Errorf("check intermediate CA: %w", err)
    }

    if !exists {
        if err := s.generateAndStoreIntermediateCA(ctx); err != nil {
            return err
        }
    }

    // Load Intermediate CA into memory
    return s.loadIntermediateCaIntoMemory(ctx)
}

func (s *serviceImpl) intermediateCAExists(ctx context.Context) (bool, error) {
    var count int
    err := s.pool.QueryRow(ctx,
        "SELECT COUNT(*) FROM ca_intermediate",
    ).Scan(&count)
    if err != nil {
        return false, fmt.Errorf("query ca_intermediate: %w", err)
    }
    return count > 0, nil
}

func (s *serviceImpl) generateAndStoreIntermediateCA(ctx context.Context) error {
    // Step 1: load Root CA (needed to sign the Intermediate CA cert)
    rootCert, rootKey, err := s.loadRootCA(ctx)
    if err != nil {
        return fmt.Errorf("load root CA for intermediate signing: %w", err)
    }
    // Zero Root CA private key from memory immediately after use.
    // It is only needed for this one operation.
    defer func() { rootKey.D.SetInt64(0) }()

    // Step 2: generate Intermediate CA keypair
    privKey, err := generateECKeyPair()
    if err != nil {
        return err
    }

    // Step 3: build Intermediate CA certificate template
    serial, err := newSerialNumber()
    if err != nil {
        return err
    }

    notBefore, notAfter := certValidity(5) // Intermediate CA valid for 5 years

    template := &x509.Certificate{
        SerialNumber: serial,
        Subject: pkix.Name{
            CommonName:   "ZECURITY Intermediate CA",
            Organization: []string{"ZECURITY Platform"},
        },
        NotBefore:             notBefore,
        NotAfter:              notAfter,
        IsCA:                  true,
        KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
        BasicConstraintsValid: true,
        // MaxPathLen: 0 means this CA can sign leaf certs (WorkspaceCAs)
        // but WorkspaceCAs cannot sign further CAs.
        // MaxPathLenZero: true means MaxPathLen=0 is enforced.
        MaxPathLen:     0,
        MaxPathLenZero: true,
    }

    // Step 4: sign with Root CA
    certDER, err := x509.CreateCertificate(
        rand.Reader,
        template,
        rootCert,       // parent = Root CA
        &privKey.PublicKey,
        rootKey,        // signed by Root CA private key
    )
    if err != nil {
        return fmt.Errorf("create intermediate CA cert: %w", err)
    }

    certPEM := encodeCertToPEM(certDER)

    // Step 5: encrypt Intermediate CA private key
    encKey, nonce, err := encryptPrivateKey(
        privKey, s.masterSecret, "intermediate-ca")
    if err != nil {
        return err
    }

    // Step 6: store in DB
    _, err = s.pool.Exec(ctx,
        `INSERT INTO ca_intermediate
         (encrypted_key, nonce, certificate_pem, not_before, not_after)
         VALUES ($1, $2, $3, $4, $5)`,
        encKey, nonce, certPEM, notBefore, notAfter,
    )
    if err != nil {
        return fmt.Errorf("store intermediate CA: %w", err)
    }

    return nil
}

func (s *serviceImpl) loadIntermediateCaIntoMemory(ctx context.Context) error {
    var encKey, nonce, certPEM string
    err := s.pool.QueryRow(ctx,
        `SELECT encrypted_key, nonce, certificate_pem
         FROM ca_intermediate LIMIT 1`,
    ).Scan(&encKey, &nonce, &certPEM)
    if err != nil {
        return fmt.Errorf("load intermediate CA from DB: %w", err)
    }

    cert, err := parseCertFromPEM(certPEM)
    if err != nil {
        return err
    }

    privKey, err := decryptPrivateKey(
        encKey, nonce, s.masterSecret, "intermediate-ca")
    if err != nil {
        return fmt.Errorf("decrypt intermediate CA key: %w", err)
    }

    s.intermediateKey = &intermediateCAState{
        cert:    cert,
        privKey: privKey,
    }

    return nil
}
```

---

## Verification Checklist

```
[x] pki.Init on fresh DB creates Intermediate CA in ca_intermediate table
[x] Intermediate CA cert is signed by Root CA
[x] Intermediate CA cert chain verifies against Root CA cert
[x] Intermediate CA cert has IsCA=true, MaxPathLen=0
[x] Intermediate CA private key loaded into svc.intermediateKey after Init
[x] Root CA private key zeroed from memory after signing Intermediate CA
[x] pki.Init on existing DB loads Intermediate CA into memory without creating new one
```
