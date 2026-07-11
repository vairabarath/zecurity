---
type: phase
member: M1
sprint: 12
phase: 1
title: Provider Data Model + Authorization Core
status: planned
depends_on: []
tags:
  - go
  - db
  - provider
  - authz
  - rbac
  - pending-07a
---

# Phase 1 — Provider Data Model + Authorization Core

> Day-1 work. This is the load-bearing boundary for the whole provider plane. M1 Phase 2 and
> M2 Phase 2 (the `/provider` re-home) are blocked until the store + `RequireProvider` exist.
> Implements the alpha slice of PENDING-07a.

## Goal

Stand up the **separate provider identity tier** and the **single authorization chokepoint**:

1. A `provider_users` store, completely independent of the tenant `users` table.
2. A dedicated `provider_audit_logs` table (the tenant `audit_logs.tenant_id` is `NOT NULL` and
   cannot hold provider actions).
3. One `Authz.decide(actor, action, target)` policy function, wrapped by typed `CanX` methods whose
   signatures already carry `actor` + `target` so partner-scoping later is a policy change, not a
   signature change.
4. Bootstrap super-admin #0 from environment config (no self-registration).

## Files

| File | Change |
|------|--------|
| `controller/migrations/025_provider_users.sql` (new) | `provider_users` table |
| `controller/migrations/026_provider_audit_logs.sql` (new) | append-only provider audit table |
| `controller/internal/provider/store.go` (new) | `ProviderStore` — user CRUD + audit insert |
| `controller/internal/provider/authz.go` (new) | `Authz` chokepoint + typed `CanX` wrappers |
| `controller/cmd/server/main.go` | bootstrap seed from `PROVIDER_BOOTSTRAP_EMAILS` |

## 025_provider_users.sql

```sql
CREATE TABLE provider_users (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT        NOT NULL UNIQUE,          -- corp Google email; join key. Stored lowercase (mirror ADR-005).
    role        TEXT        NOT NULL DEFAULT 'relay-ops',
    disabled_at TIMESTAMPTZ,                          -- soft-disable keeps the audit FK stable
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

## 026_provider_audit_logs.sql

```sql
CREATE TABLE provider_audit_logs (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_user_id UUID        REFERENCES provider_users(id),  -- nullable: system actions
    provider_email   TEXT        NOT NULL,                       -- readable even if the user is later renamed/removed
    action           TEXT        NOT NULL,   -- dotted verb, e.g. 'relay.create'
    target_type      TEXT        NOT NULL,   -- e.g. 'relay'
    target_id        TEXT        NOT NULL,
    details          JSONB,                  -- context snapshot at action time (name/TTL/SANs, granted role, …)
    ip_address       TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Append-only by convention (no UPDATE/DELETE in app code).
CREATE INDEX idx_provider_audit_created ON provider_audit_logs (created_at DESC);
CREATE INDEX idx_provider_audit_target  ON provider_audit_logs (target_type, target_id);
```

## store.go

```go
package provider

type ProviderUser struct {
    ID         string
    Email      string
    Role       string     // "super-admin" | "relay-ops"
    DisabledAt *time.Time
}

type AuditEntry struct {
    ProviderUserID *string
    ProviderEmail  string
    Action         string // dotted verb, e.g. "relay.create"
    TargetType     string
    TargetID       string
    Details        map[string]any
    IPAddress      string
}

type ProviderStore struct { pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *ProviderStore

// GetByEmail returns the active provider user or ErrProviderUserNotFound
// (also returns not-found when disabled_at is set).
func (s *ProviderStore) GetByEmail(ctx context.Context, email string) (*ProviderUser, error)
func (s *ProviderStore) List(ctx context.Context) ([]ProviderUser, error)
func (s *ProviderStore) Create(ctx context.Context, email, role string) (*ProviderUser, error)
func (s *ProviderStore) Disable(ctx context.Context, id string) error
func (s *ProviderStore) InsertAudit(ctx context.Context, e AuditEntry) error

// Upsert used by the bootstrap seed (idempotent).
func (s *ProviderStore) UpsertSuperAdmin(ctx context.Context, email string) error
```

- Normalize `email` to lowercase in Go before every query (mirror ADR-005 email normalization).

## authz.go

```go
package provider

// Actor is the verified provider identity from RequireProvider.
type Actor struct { UserID, Email, Role string }

// Target identifies what is being acted on. Fields unused in alpha but carried
// now so partner-scoping (target.ProviderOrgID) is a decide() change later,
// not a signature change across every call site.
type Target struct { Type, ID string }

type Authz interface {
    CanCreateRelay(actor Actor, target Target) error
    CanIssueProvisioningToken(actor Actor, target Target) error
    CanDeleteRelay(actor Actor, target Target) error
    CanManageProviderUser(actor Actor, target Target) error
    CanViewProviderAudit(actor Actor) error
}

// decide is the ONE chokepoint. Alpha role matrix:
//   super-admin → allow all
//   relay-ops   → allow "relay.*" only
// Returns a typed authz error (mapped to 403 by the handler).
func decide(actor Actor, action string, target Target) error
```

- Every `CanX` wrapper is a one-liner delegating to `decide(actor, "<verb>", target)`.
- Keep the role→allowed-actions mapping in a single place inside `decide` (a `map[string][]string`
  or switch). **Do not** add per-user permission rows in alpha.

## main.go (bootstrap seed)

- On startup, read `PROVIDER_BOOTSTRAP_EMAILS` (comma-separated). For each, call
  `providerStore.UpsertSuperAdmin(ctx, email)` (idempotent). Log which emails were seeded.
- No public self-registration route for provider users — bootstrap is the only creation path until
  M1 Phase 2's admin surface (or a later CLI) exists.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/provider/...
```

## Implementation Checklist

- [ ] **M1-A1** `controller/migrations/025_provider_users.sql`
- [ ] **M1-A2** `controller/migrations/026_provider_audit_logs.sql`
- [ ] **M1-A3** `controller/internal/provider/store.go`
- [ ] **M1-A4** `controller/internal/provider/authz.go`
- [ ] **M1-A5** `controller/cmd/server/main.go` — `PROVIDER_BOOTSTRAP_EMAILS` seed
- [ ] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
