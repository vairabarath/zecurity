# Phase 2 — PKI Service Interface + Init

Define the pki.Service interface and the Init constructor. Member 4 depends on this in main.go.

Status: Completed

---

## File: `controller/internal/pki/service.go`

**Path:** `controller/internal/pki/service.go`

```go
package pki

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "crypto/ecdsa"
    "crypto/x509"
)

// Service is the PKI service interface.
// Member 4 depends on this in main.go.
// Member 3 implements it in serviceImpl.
//
// Agree with Member 4 on this interface before implementing.
// main.go cannot be written until this is settled.
type Service interface {
    // GenerateWorkspaceCA creates a new WorkspaceCA for a workspace.
    // Called by the bootstrap transaction (Member 3's own code).
    // Returns encrypted key material and cert for DB storage.
    // Does NOT write to DB — bootstrap transaction handles that.
    GenerateWorkspaceCA(ctx context.Context,
        tenantID string) (*WorkspaceCAResult, error)
}

// WorkspaceCAResult holds the output of GenerateWorkspaceCA.
// All fields are ready for direct INSERT into workspace_ca_keys table.
type WorkspaceCAResult struct {
    EncryptedPrivateKey string    // AES-256-GCM ciphertext, base64
    Nonce               string    // GCM nonce, base64
    CertificatePEM      string    // PEM-encoded signed WorkspaceCA cert
    KeyAlgorithm        string    // always "EC-P384"
    NotBefore           time.Time
    NotAfter            time.Time
}

// serviceImpl is the concrete PKI service.
// Holds the loaded Intermediate CA key in memory for signing WorkspaceCAs.
// Loaded once in Init(), held for the lifetime of the process.
type serviceImpl struct {
    masterSecret    string
    pool            *pgxpool.Pool
    // intermediateKey is the Intermediate CA private key, held in memory.
    // It is used to sign WorkspaceCA CSRs on demand.
    // It is loaded during Init() and NEVER reloaded.
    // If the process restarts, Init() loads it again.
    intermediateKey *intermediateCAState
}

type intermediateCAState struct {
    cert    *x509.Certificate // parsed cert for signing
    privKey *ecdsa.PrivateKey // decrypted private key for signing
}

// Init is called once in main.go before the HTTP server starts.
// Checks for Root CA and Intermediate CA in DB.
// Creates them if they do not exist.
// Loads the Intermediate CA into memory (needed for WorkspaceCA signing).
// The HTTP server MUST NOT start until Init returns nil.
func Init(ctx context.Context, pool *pgxpool.Pool) (Service, error) {
    masterSecret := os.Getenv("PKI_MASTER_SECRET")
    if masterSecret == "" {
        return nil, fmt.Errorf("PKI_MASTER_SECRET not set")
    }

    svc := &serviceImpl{
        masterSecret: masterSecret,
        pool:         pool,
    }

    // Step 1: initialize Root CA
    if err := svc.initRootCA(ctx); err != nil {
        return nil, fmt.Errorf("init root CA: %w", err)
    }

    // Step 2: initialize Intermediate CA
    // This loads the Intermediate CA into svc.intermediateKey
    if err := svc.initIntermediateCA(ctx); err != nil {
        return nil, fmt.Errorf("init intermediate CA: %w", err)
    }

    return svc, nil
}
```

---

## Verification Checklist

```
[x] pki.Service interface defined with GenerateWorkspaceCA method
[x] WorkspaceCAResult struct matches workspace_ca_keys columns exactly
[x] serviceImpl struct holds pool, masterSecret, and intermediateKey
[x] Init function signature matches what Member 4 calls in main.go
[x] Init returns error if PKI_MASTER_SECRET env var is not set
[x] Init calls initRootCA and initIntermediateCA (to be implemented in next phases)
```
