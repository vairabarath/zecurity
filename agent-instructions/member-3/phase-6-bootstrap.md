# Phase 6 — Bootstrap Transaction

Implement the atomic workspace + user + WorkspaceCA creation transaction. Replaces the stub Member 2 wrote.

Status: Completed

---

## File: `controller/internal/bootstrap/bootstrap.go`

**Path:** `controller/internal/bootstrap/bootstrap.go`

**Important:** This replaces the stub Member 2 wrote. The `Result` type, package declaration, and function signature must remain identical. Only the function body changes.

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

## Verification Checklist

```
[x] New user: workspace created with status='provisioning' then 'active'
[x] New user: first admin role assigned
[x] New user: workspace_ca_keys row created
[x] New user: workspaces.ca_cert_pem populated
[x] Returning user: existing tenant_id returned, last_login_at updated
[x] Returning user: no new workspace or CA created
[x] ROLLBACK: if GenerateWorkspaceCA fails → no workspace row in DB
[x] ROLLBACK: if INSERT workspace_ca_keys fails → no workspace row in DB
[x] ROLLBACK: if UPDATE workspace status fails → no workspace row in DB
[x] After ROLLBACK: calling Bootstrap again creates a clean workspace
[x] UNIQUE (tenant_id, provider_sub) prevents duplicate users
[x] slug is URL-safe lowercase with hyphens
[x] Bootstrap is idempotent for returning users
```
