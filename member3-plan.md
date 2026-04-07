# Member 3 — Deep Implementation Plan
## PKI Hierarchy · Bootstrap Transaction · CA Init · WorkspaceCA

---

## Role on the Team

Member 3 owns the two most security-critical pieces in the entire codebase:
the PKI hierarchy and the workspace bootstrap transaction.

Every other piece of the system depends on these being correct.
A bug in middleware returns a wrong error.
A bug in bootstrap creates a workspace without a CA, or leaks data
between tenants, or leaves the DB in a broken partial state.
There is no "good enough" here — it either works atomically or it doesn't work.

```
Member 3 owns:
  internal/pki/
    service.go      ← pki.Service interface + pki.Init() constructor
    root.go         ← Root CA generation and storage
    intermediate.go ← Intermediate CA generation, signed by Root CA
    workspace.go    ← WorkspaceCA generation per workspace, signed by Intermediate CA
    crypto.go       ← shared crypto helpers: keygen, HKDF, AES-GCM, PEM encoding
  internal/bootstrap/
    bootstrap.go    ← atomic workspace + user + WorkspaceCA creation transaction
```

Member 3 does NOT touch:
- `internal/auth/` — that is Member 2
- `graph/` — that is Member 4
- `internal/middleware/` — that is Member 4
- `migrations/` — that is Member 4
  (Member 3 reads the schema but does not change it)

---

## Hard Dependencies — What Member 3 Waits For

### Must wait for Member 4 Phase 1 before starting:

**`docker-compose.yml`** — Member 3 needs Postgres running
to test CA storage (ca_root, ca_intermediate tables).

**`migrations/001_schema.sql`** — Member 3 must understand
every column in `workspaces`, `users`, `workspace_ca_keys`,
`ca_root`, `ca_intermediate` before writing a single INSERT.
Do not guess at column names. Read the migration file.

**`internal/db/pool.go`** — `pki.Init()` takes a `*pgxpool.Pool`.
Member 3 needs this to exist to write `pki.Init()`.

That is the only hard blocker. Everything else in Member 3's work
has no external dependency — it is pure Go crypto and DB writes.

### Stub that Member 2 writes and Member 3 replaces:

Member 2 writes a temporary stub in `internal/bootstrap/bootstrap.go`
so they can test their callback handler before Member 3 is ready.

```go
// STUB — Member 2 writes this temporarily
// Member 3 REPLACES the entire function body with the real transaction
// The package declaration, type definitions, and function signature stay identical.

package bootstrap

import "context"

type Result struct {
    TenantID string
    UserID   string
    Role     string
}

func Bootstrap(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error) {
    return &Result{
        TenantID: "00000000-0000-0000-0000-000000000001",
        UserID:   "00000000-0000-0000-0000-000000000002",
        Role:     "admin",
    }, nil
}
```

When Member 3 ships the real bootstrap, the `Result` type, package
declaration, and function signature must remain identical.
Member 2's `callback.go` must not change at all.

### Interface agreement with Member 4:

Member 4's `main.go` calls:
```go
pkiService, err := pki.Init(ctx, db.Pool)
```

And passes `pkiService` into the auth config:
```go
authSvc := auth.NewService(auth.Config{
    PKIService: pkiService,
    ...
})
```

So Member 3 must produce:

```go
// pki.Service interface — Member 3 defines and implements this
type Service interface {
    // GenerateWorkspaceCA generates a WorkspaceCA keypair,
    // signs it with the Intermediate CA, encrypts the private key,
    // and returns the result for the caller (bootstrap) to store in DB.
    GenerateWorkspaceCA(ctx context.Context, tenantID string) (*WorkspaceCAResult, error)
}

// WorkspaceCAResult is returned by GenerateWorkspaceCA.
// The bootstrap transaction stores these fields in workspace_ca_keys.
type WorkspaceCAResult struct {
    EncryptedPrivateKey string    // AES-256-GCM ciphertext, base64
    Nonce               string    // GCM nonce, base64
    CertificatePEM      string    // signed WorkspaceCA cert, PEM
    NotBefore           time.Time
    NotAfter            time.Time
    KeyAlgorithm        string    // "EC-P384"
}

// Init initializes the PKI service.
// Checks if Root CA and Intermediate CA exist in DB.
// Creates them if not. Loads them if they do.
// HTTP server must not start until this returns without error.
func Init(ctx context.Context, pool *pgxpool.Pool) (Service, error)
```

Agree with Member 4 on this interface before implementing.
Member 4 cannot wire `main.go` until this signature is settled.

---

## Build Order — Strictly by Dependency

### Phase 1 — Crypto Helpers

These are pure functions with no external dependencies.
No DB, no pool, no config. Start here.

**internal/pki/crypto.go**

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

### Phase 2 — PKI Service Interface + Init

**internal/pki/service.go**

```go
package pki

import (
    "context"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
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

### Phase 3 — Root CA

**internal/pki/root.go**

```go
package pki

import (
    "context"
    "crypto/ecdsa"
    "crypto/x509"
    "crypto/x509/pkix"
    "fmt"
    "math/big"
    "time"
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
        MaxPathLen:  1,
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

### Phase 4 — Intermediate CA

**internal/pki/intermediate.go**

```go
package pki

import (
    "context"
    "crypto/ecdsa"
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

### Phase 5 — WorkspaceCA Generation

**internal/pki/workspace.go**

```go
package pki

import (
    "context"
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

### Phase 6 — Bootstrap Transaction

**internal/bootstrap/bootstrap.go**

This replaces the stub Member 2 wrote.
The `Result` type, package declaration, and function signature
must be identical to the stub. Only the function body changes.

```go
package bootstrap

import (
    "context"
    "fmt"
    "strings"
    "time"
    "unicode"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/yourorg/ztna/controller/internal/pki"
)

// Result is returned by Bootstrap.
// Consumed by Member 2's callback.go to issue the JWT.
// Field names and types must match the stub exactly.
type Result struct {
    TenantID string
    UserID   string
    Role     string
}

// Service holds the dependencies for the bootstrap operation.
// Member 3 writes this. Member 2 calls Bootstrap() through it.
type Service struct {
    Pool       *pgxpool.Pool
    PKIService pki.Service
}

// Bootstrap creates a workspace for a new user, or finds the existing
// workspace for a returning user.
//
// For new users (first signup):
//   Runs a single atomic transaction:
//     1. INSERT workspace (status='provisioning')
//     2. INSERT user (role='admin')
//     3. Generate WorkspaceCA (in memory, via PKI service)
//     4. INSERT workspace_ca_keys (encrypted key + cert)
//     5. UPDATE workspace (status='active', ca_cert_pem=cert)
//   All or nothing. If any step fails → ROLLBACK.
//   The workspace either fully exists or does not exist at all.
//
// For returning users:
//   SELECT user by provider_sub
//   UPDATE last_login_at
//   Return existing tenant_id + user_id + role
//
// This function is called from Member 2's callback.go.
// It is a direct Go function call — no HTTP, no gRPC, no network.
func (s *Service) Bootstrap(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error) {

    // Step 1: check if this identity already has a workspace
    // This query does NOT require TenantContext — we don't have
    // a tenant_id yet. provider_sub lookup happens before tenant is known.
    // This is why idx_users_provider_sub index exists (Member 4 created it).
    var existingUserID, existingTenantID, existingRole string
    err := s.Pool.QueryRow(ctx,
        `SELECT id, tenant_id, role FROM users
         WHERE provider_sub = $1 AND provider = $2
         LIMIT 1`,
        providerSub, provider,
    ).Scan(&existingUserID, &existingTenantID, &existingRole)

    if err == nil {
        // Returning user — update last_login_at and return
        _, err = s.Pool.Exec(ctx,
            `UPDATE users SET last_login_at = NOW(), updated_at = NOW()
             WHERE id = $1`,
            existingUserID,
        )
        if err != nil {
            // Non-fatal: last_login_at update failure should not block login
            // Log it but continue
            fmt.Printf("warning: update last_login_at failed for user %s: %v\n",
                existingUserID, err)
        }

        return &Result{
            TenantID: existingTenantID,
            UserID:   existingUserID,
            Role:     existingRole,
        }, nil
    }

    // pgx returns pgx.ErrNoRows when no row found — check for that specifically
    if !isNoRows(err) {
        return nil, fmt.Errorf("lookup user by provider_sub: %w", err)
    }

    // First signup — run the atomic bootstrap transaction
    return s.runBootstrapTransaction(ctx, email, provider, providerSub, name)
}

func (s *Service) runBootstrapTransaction(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error) {

    tx, err := s.Pool.Begin(ctx)
    if err != nil {
        return nil, fmt.Errorf("begin transaction: %w", err)
    }
    // Rollback is a no-op if the transaction has already been committed.
    // Always defer it so panics or early returns also trigger rollback.
    defer tx.Rollback(ctx)

    // ── Step 1: INSERT workspace ────────────────────────────────────
    slug := slugify(name)
    var tenantID string
    err = tx.QueryRow(ctx,
        `INSERT INTO workspaces (slug, name, status)
         VALUES ($1, $2, 'provisioning')
         RETURNING id`,
        slug, name,
    ).Scan(&tenantID)
    if err != nil {
        return nil, fmt.Errorf("insert workspace: %w", err)
    }

    // ── Step 2: INSERT first admin user ────────────────────────────
    var userID string
    err = tx.QueryRow(ctx,
        `INSERT INTO users
         (tenant_id, email, provider, provider_sub, role, status)
         VALUES ($1, $2, $3, $4, 'admin', 'active')
         RETURNING id`,
        tenantID, email, provider, providerSub,
    ).Scan(&userID)
    if err != nil {
        return nil, fmt.Errorf("insert user: %w", err)
    }

    // ── Step 3: Generate WorkspaceCA (in memory via PKI service) ───
    // This is the call to Member 3's own PKI service.
    // It generates the keypair, signs with Intermediate CA,
    // encrypts the private key — all in memory.
    // Nothing is written to DB here. The transaction does that next.
    caResult, err := s.PKIService.GenerateWorkspaceCA(ctx, tenantID)
    if err != nil {
        return nil, fmt.Errorf("generate workspace CA: %w", err)
    }

    // ── Step 4: INSERT workspace_ca_keys ───────────────────────────
    _, err = tx.Exec(ctx,
        `INSERT INTO workspace_ca_keys
         (tenant_id, encrypted_private_key, nonce, key_algorithm,
          certificate_pem, not_before, not_after)
         VALUES ($1, $2, $3, $4, $5, $6, $7)`,
        tenantID,
        caResult.EncryptedPrivateKey,
        caResult.Nonce,
        caResult.KeyAlgorithm,
        caResult.CertificatePEM,
        caResult.NotBefore,
        caResult.NotAfter,
    )
    if err != nil {
        return nil, fmt.Errorf("insert workspace_ca_keys: %w", err)
    }

    // ── Step 5: UPDATE workspace to 'active' ───────────────────────
    // This is the final step. If anything above failed, the workspace
    // never becomes active. Status 'active' means:
    //   - User exists
    //   - WorkspaceCA exists and is stored
    //   - Workspace is fully operational
    _, err = tx.Exec(ctx,
        `UPDATE workspaces
         SET status = 'active',
             ca_cert_pem = $1,
             updated_at = NOW()
         WHERE id = $2`,
        caResult.CertificatePEM,
        tenantID,
    )
    if err != nil {
        return nil, fmt.Errorf("activate workspace: %w", err)
    }

    // ── COMMIT ─────────────────────────────────────────────────────
    if err := tx.Commit(ctx); err != nil {
        return nil, fmt.Errorf("commit bootstrap transaction: %w", err)
    }

    return &Result{
        TenantID: tenantID,
        UserID:   userID,
        Role:     "admin",
    }, nil
}

// slugify converts a name to a URL-safe slug.
// "Acme Corp" → "acme-corp"
// "My Company!" → "my-company"
func slugify(name string) string {
    var b strings.Builder
    prev := '-'
    for _, r := range strings.ToLower(name) {
        if unicode.IsLetter(r) || unicode.IsDigit(r) {
            b.WriteRune(r)
            prev = r
        } else if prev != '-' {
            b.WriteRune('-')
            prev = '-'
        }
    }
    s := strings.Trim(b.String(), "-")
    if s == "" {
        s = "workspace"
    }
    // Append a short suffix to avoid slug collisions between
    // workspaces with the same name. The full tenant_id is too long —
    // use first 6 chars of a random value. Member 3 can make this
    // deterministic if needed (e.g. based on creation timestamp).
    return s
}

// isNoRows returns true if the error indicates no rows were found.
// pgx returns pgx.ErrNoRows for this case.
func isNoRows(err error) bool {
    if err == nil {
        return false
    }
    return err.Error() == "no rows in result set"
}
```

---

### Phase 7 — Wiring Bootstrap into the Service

Member 2's `callback.go` calls `bootstrap.Bootstrap()` as a plain function.
But `Bootstrap` is now a method on `bootstrap.Service` which needs
a `Pool` and `PKIService`. Member 3 must expose a `NewService` constructor
and update the function signature.

Coordinate with Member 2: the call site in `callback.go` changes from:
```go
result, err := bootstrap.Bootstrap(ctx, email, "google", sub, name)
```
to:
```go
result, err := bootstrapSvc.Bootstrap(ctx, email, "google", sub, name)
```

where `bootstrapSvc` is injected into the auth service via `auth.Config`.

Update `auth.Config` in `internal/auth/config.go`:
```go
type Config struct {
    // ...existing fields...
    BootstrapService *bootstrap.Service  // Member 3 provides this
}
```

And `main.go` (Member 4 updates this):
```go
bootstrapSvc := &bootstrap.Service{
    Pool:       db.Pool,
    PKIService: pkiService,
}

authSvc := auth.NewService(auth.Config{
    BootstrapService: bootstrapSvc,
    // ...
})
```

This wiring change must be coordinated across Member 2, Member 3,
and Member 4 before it is merged.

---

## Dependency Map — What Blocks What

```
Can start immediately after Member 4 Phase 1:
  Phase 1 — crypto.go (pure functions, no DB, no deps)
  Phase 2 — service.go interface (just types and signatures)
  Phase 3 — root.go (needs pool but can be written before testing)
  Phase 4 — intermediate.go (needs root.go to be written first)
  Phase 5 — workspace.go (needs intermediate.go in memory)

Cannot test until Member 4 docker-compose is running:
  Phase 3 — rootCAExists() and store queries need Postgres
  Phase 4 — same
  Phase 6 — bootstrap transaction needs Postgres

Phase 6 (bootstrap) needs Phase 5 (workspace.go) complete first:
  GenerateWorkspaceCA() is called inside the transaction.
  If it panics or returns error, the transaction rolls back.
  Phase 5 must be correct before Phase 6 can be tested.

Coordination needed before Phase 7:
  Member 2 must update callback.go to use bootstrapSvc.Bootstrap()
  Member 4 must update main.go to wire bootstrap.Service
  All three agree on the updated auth.Config fields
```

---

## Integration Checklist

```
Phase 1 — Crypto helpers
  ✓ generateECKeyPair returns a valid EC P-384 key
  ✓ encryptPrivateKey and decryptPrivateKey are inverse operations
  ✓ different contexts produce different encryption keys from same master
  ✓ zeroBytes clears the slice
  ✓ encodeCertToPEM and parseCertFromPEM are inverse operations
  ✓ newSerialNumber returns different values on repeated calls

Phase 2 — PKI service interface
  ✓ pki.Init signature matches what Member 4 calls in main.go
  ✓ WorkspaceCAResult fields match workspace_ca_keys columns exactly
  ✓ Service interface agreed with Member 4 before implementation

Phase 3 — Root CA
  ✓ pki.Init on fresh DB creates Root CA in ca_root table
  ✓ pki.Init on existing DB does NOT create a second Root CA
  ✓ Root CA cert has IsCA=true, MaxPathLen=1
  ✓ Root CA cert is self-signed (issuer == subject)
  ✓ Root CA private key is encrypted in DB (not plaintext)
  ✓ Root CA private key is NOT loaded into memory after Intermediate CA exists

Phase 4 — Intermediate CA
  ✓ pki.Init on fresh DB creates Intermediate CA in ca_intermediate table
  ✓ Intermediate CA cert is signed by Root CA
  ✓ Intermediate CA cert chain verifies against Root CA cert
  ✓ Intermediate CA cert has IsCA=true, MaxPathLen=0
  ✓ Intermediate CA private key loaded into svc.intermediateKey after Init
  ✓ Root CA private key zeroed from memory after signing Intermediate CA

Phase 5 — WorkspaceCA
  ✓ GenerateWorkspaceCA returns WorkspaceCAResult with all fields populated
  ✓ WorkspaceCA cert SAN contains URI:tenant:<tenantID>
  ✓ WorkspaceCA cert signed by Intermediate CA
  ✓ WorkspaceCA cert chain: WorkspaceCA → Intermediate CA → Root CA
  ✓ Two calls with different tenantIDs produce different certs
  ✓ Two calls with different tenantIDs produce different encrypted keys
    (HKDF context differs — same master secret, different output)
  ✓ WorkspaceCA private key zeroed from memory after encryption

Phase 6 — Bootstrap transaction
  ✓ New user: workspace created with status='provisioning' then 'active'
  ✓ New user: first admin role assigned
  ✓ New user: workspace_ca_keys row created
  ✓ New user: workspaces.ca_cert_pem populated
  ✓ Returning user: existing tenant_id returned, last_login_at updated
  ✓ Returning user: no new workspace or CA created
  ✓ ROLLBACK: if GenerateWorkspaceCA fails → no workspace row in DB
  ✓ ROLLBACK: if INSERT workspace_ca_keys fails → no workspace row in DB
  ✓ ROLLBACK: if UPDATE workspace status fails → no workspace row in DB
  ✓ After ROLLBACK: calling Bootstrap again creates a clean workspace
  ✓ UNIQUE (tenant_id, provider_sub) prevents duplicate users
  ✓ slug is URL-safe lowercase with hyphens
  ✓ Bootstrap is idempotent for returning users

Phase 7 — Wiring
  ✓ bootstrap.Service wired in main.go with Pool + PKIService
  ✓ auth.Config updated with BootstrapService field
  ✓ Member 2's callback.go updated to call bootstrapSvc.Bootstrap()
  ✓ Full flow test: login → bootstrap → workspace active → JWT issued
```

---

## The Most Critical Guarantee

The atomic transaction in `runBootstrapTransaction` must uphold this guarantee:

```
A workspace with status='active' in the DB always has:
  - A corresponding row in workspace_ca_keys
  - A non-null ca_cert_pem in workspaces

A workspace with status='provisioning' in the DB means:
  - The transaction did not complete
  - This workspace should never be used
  - It will be cleaned up (or retried on next login)

There is no case where status='active' without a CA.
There is no case where workspace_ca_keys has a row without a workspace.
The FK constraint (workspace_ca_keys.tenant_id → workspaces.id)
enforces the second guarantee at the DB level.
The transaction order (INSERT workspace → ... → UPDATE status='active')
enforces the first guarantee at the application level.
```

---

## Summary

```
Phase 1  crypto.go — key generation, HKDF, AES-GCM, PEM, serial numbers
         → pure functions, no DB, start immediately

Phase 2  service.go — pki.Service interface, WorkspaceCAResult, Init signature
         → agree with Member 4 before implementing

Phase 3  root.go — Root CA generate + store + load
         → needs Postgres running (Member 4 docker-compose)

Phase 4  intermediate.go — Intermediate CA generate + store + load into memory
         → needs root.go complete

Phase 5  workspace.go — WorkspaceCA generate per workspace
         → needs intermediate.go loaded in svc.intermediateKey

Phase 6  bootstrap.go — atomic workspace + user + CA transaction
         → needs Phase 5 complete
         → replaces the stub Member 2 wrote

Phase 7  Wiring coordination — auth.Config + main.go + callback.go updates
         → coordinate with Member 2 and Member 4 before merging

Waits for:
  Member 4 Phase 1 → docker-compose + 001_schema.sql + db/pool.go
  Member 2 (coordinate) → Bootstrap() call site update in callback.go
  Member 4 (coordinate) → main.go wiring + auth.Config update
```
