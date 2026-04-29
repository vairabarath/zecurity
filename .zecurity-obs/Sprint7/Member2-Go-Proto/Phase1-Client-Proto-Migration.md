---
type: phase
status: done
sprint: 7
member: M2
phase: Phase1-Client-Proto-Migration
depends_on: []
tags:
  - proto
  - migration
  - graphql
  - day1
---

# M2 Phase 1 — ClientService Proto + DB Migration + GraphQL

> **Day 1 work — no dependencies. Land this before anyone else can start.**

---

## What You're Building

1. A new proto file `proto/client/v1/client.proto` defining `ClientService` — 3 RPCs used by the Rust CLI.
2. DB migration `011_client.sql` adding `invitations` and `client_devices` tables.
3. GraphQL schema `client.graphqls` for invitation mutations and device queries.

---

## Files to Touch

### 1. `proto/client/v1/client.proto` (NEW — create directory first)

```protobuf
syntax = "proto3";
package client.v1;
option go_package = "github.com/zecurity/controller/proto/client/v1;clientv1";

service ClientService {
  // GetAuthConfig — public, no auth. CLI calls this first to get Google OAuth config.
  rpc GetAuthConfig(GetAuthConfigRequest) returns (GetAuthConfigResponse);

  // TokenExchange — exchange OAuth code for Zecurity JWT + refresh token.
  // If invite_token is set, accepts the invitation and adds user to workspace as MEMBER.
  rpc TokenExchange(TokenExchangeRequest) returns (TokenExchangeResponse);

  // EnrollDevice — issue mTLS certificate for the client device.
  // Requires valid access_token in request (not in gRPC metadata).
  rpc EnrollDevice(EnrollDeviceRequest) returns (EnrollDeviceResponse);
}

// ── GetAuthConfig ──────────────────────────────────────────────────────────

message GetAuthConfigRequest {
  string workspace_slug = 1;
}

message GetAuthConfigResponse {
  string google_client_id = 1;
  string auth_endpoint    = 2;  // "https://accounts.google.com/o/oauth2/v2/auth"
  string token_endpoint   = 3;  // "https://oauth2.googleapis.com/token"
  string controller_host  = 4;  // e.g. "controller.example.com" — CLI uses to build redirect_uri
}

// ── TokenExchange ──────────────────────────────────────────────────────────

message TokenExchangeRequest {
  string workspace_slug = 1;
  string code           = 2;  // OAuth authorization code from Google callback
  string code_verifier  = 3;  // PKCE verifier (plain text, 32 bytes base64url)
  string redirect_uri   = 4;  // Must match what was sent to Google
  string invite_token   = 5;  // Optional: hex token from invitation email link
}

message TokenExchangeResponse {
  string access_token  = 1;  // Zecurity JWT (15 min TTL)
  string refresh_token = 2;  // Opaque token (7 day TTL, stored in Redis)
  int64  expires_in    = 3;  // seconds until access_token expires
  string email         = 4;  // user's Google email
}

// ── EnrollDevice ───────────────────────────────────────────────────────────

message EnrollDeviceRequest {
  string access_token = 1;  // Zecurity JWT from TokenExchange
  string csr_pem      = 2;  // PEM-encoded PKCS#10 CSR, P-384 ECDSA key
  string device_name  = 3;  // e.g. hostname of the machine
  string os           = 4;  // "linux" | "darwin" | "windows"
}

message EnrollDeviceResponse {
  string certificate_pem     = 1;  // signed device certificate (PEM)
  string workspace_ca_pem    = 2;  // workspace CA cert for trust chain
  string intermediate_ca_pem = 3;  // intermediate CA cert
  string spiffe_id           = 4;  // e.g. "spiffe://ws-myworkspace.zecurity.in/client/uuid"
}
```

**After creating the file**, update `buf.gen.yaml` if the new package path needs adding (check if `proto/client/v1` is covered by existing glob — if `buf.gen.yaml` uses `proto/**`, no change needed).

Run from repo root:
```bash
buf generate
```
Verify no errors. New Go package appears at `controller/proto/client/v1/`.

---

### 2. `controller/migrations/011_client.sql` (NEW)

```sql
-- Sprint 7: Client application tables

-- Invitations sent by admins to new users
CREATE TABLE invitations (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email        TEXT        NOT NULL,
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    invited_by   UUID        NOT NULL REFERENCES users(id),
    token        TEXT        NOT NULL UNIQUE,  -- 32-byte random hex, used in email link
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending','accepted','expired')),
    expires_at   TIMESTAMPTZ NOT NULL DEFAULT NOW() + INTERVAL '7 days',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON invitations(token);
CREATE INDEX ON invitations(email, workspace_id);

-- Client devices enrolled by end users via CLI
CREATE TABLE client_devices (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,   -- device hostname
    os             TEXT        NOT NULL,   -- linux / darwin / windows
    cert_serial    TEXT,
    cert_not_after TIMESTAMPTZ,
    spiffe_id      TEXT,
    last_seen_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON client_devices(user_id, workspace_id);
```

---

### 3. `controller/graph/client.graphqls` (NEW)

```graphql
type Invitation {
  id        ID!
  email     String!
  status    String!
  expiresAt Time!
  createdAt Time!
}

type ClientDevice {
  id           ID!
  name         String!
  os           String!
  spiffeId     String
  certNotAfter Time
  lastSeenAt   Time
  createdAt    Time!
}

extend type Query {
  # Returns the caller's enrolled client devices (any authenticated user)
  myDevices: [ClientDevice!]!

  # Returns invitation details by token (public — no auth required)
  invitation(token: String!): Invitation
}

extend type Mutation {
  # Admin only — creates an invitation and sends email to the given address
  createInvitation(email: String!): Invitation!
}
```

### 4. `controller/gqlgen.yml` (MODIFY)

Under `models:` section, add mappings if gqlgen cannot auto-resolve:

```yaml
models:
  Invitation:
    model: github.com/zecurity/controller/graph/model.Invitation
  ClientDevice:
    model: github.com/zecurity/controller/graph/model.ClientDevice
```

---

## After Writing All Files

Run in this order:

```bash
# 1. Regenerate proto stubs
buf generate

# 2. Regenerate GraphQL
cd controller && go generate ./graph/...

# 3. Regenerate frontend TS hooks
cd admin && npm run codegen
```

---

## Build Check

```bash
cd controller && go build ./...
```

Must pass with no errors before checking this phase complete.

---

## Post-Phase Fixes

### Fix: proto `go_package` must include `proto/` segment
**Issue:** The phase doc specified `option go_package = "github.com/zecurity/controller/proto/client/v1;clientv1"`. That module path does not exist in this repo (`go.mod` is `github.com/yourorg/ztna/controller`). Even after fixing the org, omitting the `proto/` segment is wrong because `paths=source_relative` writes generated files to `controller/gen/go/proto/client/v1/`, and consumers import them as `github.com/yourorg/ztna/controller/gen/go/proto/client/v1`.

**Fix Applied:**
```proto
// AFTER:
option go_package = "github.com/yourorg/ztna/controller/gen/go/proto/client/v1;clientv1";
```
Matches `proto/shield/v1/shield.proto`. (Note: `proto/connector/v1/connector.proto` uses the older non-`proto/` form; do not copy that — `shield.proto` is the correct pattern.)

### Fix: GraphQL fields need colons; no `Time` scalar exists
**Issue 1:** Phase doc renders fields as `id        ID!` (column-aligned, no colon). gqlgen rejected this with `Expected :, found Name`.
**Issue 2:** Phase doc used `Time!` for timestamp fields; the repo has no `scalar Time` declaration. Every other graphqls file types timestamps as `String` / `String!`.

**Fix Applied (`controller/graph/client.graphqls`):**
```graphql
type Invitation {
  id:        ID!
  email:     String!
  status:    String!
  expiresAt: String!
  createdAt: String!
}
```
And `ClientDevice` similarly — `certNotAfter`, `lastSeenAt` as `String`, `createdAt` as `String!`.

### Fix: Skip the `gqlgen.yml` `models:` block from the phase doc
**Issue:** Phase doc instructed adding `Invitation` / `ClientDevice` model mappings pointing at `github.com/zecurity/controller/graph/model.{Invitation,ClientDevice}` — that package does not exist in this repo and would break `go generate`.

**Fix Applied:** Only added `- graph/client.graphqls` to the `schema:` list. No `models:` entries — gqlgen auto-generates the structs into `graph/models_gen.go`, matching how `Connector`, `Shield`, `Resource`, and `DiscoveredService` are already handled.
