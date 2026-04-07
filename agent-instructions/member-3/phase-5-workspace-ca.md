# Phase 5 — WorkspaceCA Generation

Implement WorkspaceCA generation per workspace. Needs intermediate.go loaded in svc.intermediateKey.

Status: Completed

---

## File: `controller/internal/pki/workspace.go`

**Path:** `controller/internal/pki/workspace.go`

```go
package pki

import (
    "context"
    "crypto/rand"
    "crypto/x509"
    "crypto/x509/pkix"
    "fmt"
    "net/url"
)

// GenerateWorkspaceCA implements pki.Service.
// Called by the bootstrap transaction during workspace creation.
// Creates a new WorkspaceCA signed by the Intermediate CA.
//
// The tenant_id is embedded in the certificate's Subject Alternative Name
// as a URI: URI:tenant:<tenantID>
//
// This is the isolation anchor. When verifying any leaf cert
// (device cert, connector cert) in the future:
//   1. Verify cert chain (WorkspaceCA → Intermediate CA → Root CA)
//   2. Verify cert not expired
//   3. Extract SAN from WorkspaceCA → get tenantID
//   4. Compare tenantID from cert SAN with tenantID from JWT claim
//   If step 4 fails → reject, even if chain is cryptographically valid
//
// This prevents a cert issued for Workspace A from being used
// to access Workspace B's resources. The chain alone is not enough.
func (s *serviceImpl) GenerateWorkspaceCA(ctx context.Context,
    tenantID string) (*WorkspaceCAResult, error) {

    if s.intermediateKey == nil {
        // Should never happen — Init() panics if Intermediate CA is not loaded.
        // This is a defensive check.
        return nil, fmt.Errorf(
            "intermediate CA not initialized — pki.Init() may not have been called")
    }

    // Step 1: generate EC P-384 keypair for this WorkspaceCA
    privKey, err := generateECKeyPair()
    if err != nil {
        return nil, err
    }

    // Step 2: build the WorkspaceCA certificate template
    serial, err := newSerialNumber()
    if err != nil {
        return nil, err
    }

    notBefore, notAfter := certValidity(2) // WorkspaceCA valid for 2 years

    // Build the SAN URI that embeds the tenant_id.
    // Format: URI:tenant:<tenantID>
    // This URI has no semantic meaning to X.509 — it is just a string.
    // Our verification code reads it during cert chain verification.
    tenantURI := &url.URL{
        Scheme: "tenant",
        Opaque: tenantID,
    }

    template := &x509.Certificate{
        SerialNumber: serial,
        Subject: pkix.Name{
            CommonName:   "workspace-" + tenantID,
            Organization: []string{"ZECURITY Workspace"},
        },
        NotBefore:             notBefore,
        NotAfter:              notAfter,
        IsCA:                  true,
        KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
        BasicConstraintsValid: true,
        MaxPathLen:            0,
        MaxPathLenZero:        true,
        // SAN URI embeds the tenant_id — this is the isolation anchor
        URIs: []*url.URL{tenantURI},
    }

    // Step 3: sign with Intermediate CA private key
    certDER, err := x509.CreateCertificate(
        rand.Reader,
        template,
        s.intermediateKey.cert,    // parent = Intermediate CA
        &privKey.PublicKey,
        s.intermediateKey.privKey, // signed by Intermediate CA key
    )
    if err != nil {
        return nil, fmt.Errorf("create workspace CA cert: %w", err)
    }

    certPEM := encodeCertToPEM(certDER)

    // Step 4: encrypt WorkspaceCA private key
    // Context = tenant_id (unique per workspace).
    // HKDF(master_secret, tenant_id) produces a unique encryption key
    // per workspace. Compromising one workspace's encryption key does
    // not help decrypt any other workspace's CA key.
    encKey, nonce, err := encryptPrivateKey(
        privKey, s.masterSecret, tenantID)
    if err != nil {
        return nil, err
    }

    // Zero the plaintext private key from memory
    defer func() { privKey.D.SetInt64(0) }()

    return &WorkspaceCAResult{
        EncryptedPrivateKey: encKey,
        Nonce:               nonce,
        CertificatePEM:      certPEM,
        KeyAlgorithm:        "EC-P384",
        NotBefore:           notBefore,
        NotAfter:            notAfter,
    }, nil
}
```

---

## Verification Checklist

```
[x] GenerateWorkspaceCA returns WorkspaceCAResult with all fields populated
[x] WorkspaceCA cert SAN contains URI:tenant:<tenantID>
[x] WorkspaceCA cert signed by Intermediate CA
[x] WorkspaceCA cert chain: WorkspaceCA → Intermediate CA → Root CA
[x] Two calls with different tenantIDs produce different certs
[x] Two calls with different tenantIDs produce different encrypted keys
    (HKDF context differs — same master secret, different output)
[x] WorkspaceCA private key zeroed from memory after encryption
[x] GenerateWorkspaceCA returns error if intermediateKey is nil
```
