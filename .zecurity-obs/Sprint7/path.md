---
type: planning
status: active
sprint: 7
tags:
  - sprint7
  - dependencies
  - execution-path
  - team-coordination
  - client-app
---

# Sprint 7 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

**Client Application (Phase 1)** — End users can be invited to a workspace by an admin, log in via Google OAuth from a Rust CLI, enroll their device with an mTLS certificate issued by the Controller's existing CA, and check their connection status. The Admin UI gains role-based routing (admin → dashboard, member → client install page) and a web invitation acceptance flow.

> **Prerequisite:** Sprint 6 must be merged.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **ClientService transport** | gRPC on same port 9090 as ConnectorService — no new ports. Plain TLS (not mTLS) since the user has no cert yet. JWT Bearer in gRPC metadata for authenticated RPCs. |
| **OAuth for CLI** | CLI does full PKCE locally: spins up a local HTTP server, opens browser to Google, captures redirect code. Calls `GetAuthConfig` on Controller to get `google_client_id` + endpoints. |
| **Device cert** | Reuses existing `pki.Service.SignCSR()` — same P-384 ECDSA / SPIFFE pattern as connectors. SPIFFE ID: `spiffe://ws-{slug}.zecurity.in/client/{device_id}` |
| **Invitation token** | 32 random bytes → lowercase hex (crypto/rand). Single-use. 7-day TTL. |
| **Role routing** | After login, Admin UI reads user role from JWT/GraphQL Me. ADMIN → `/dashboard`. MEMBER/VIEWER → `/client-install`. |
| **Invite acceptance** | Web page `/invite/:token`. User signs in with Google (existing OAuth). Controller marks invitation accepted, adds user to workspace as MEMBER. |
| **Email sending** | SMTP via env vars (`SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM`, `SMTP_PASSWORD`). If not configured: log invite link to stdout (dev mode). |
| **DB migration** | `011_client.sql` — adds `invitations` and `client_devices` tables. (010 was Sprint 6 discovery.) |
| **CLI language** | Rust — lives in `client/` workspace at repo root. Binary: `zecurity-client`. |
| **CLI storage — disk** | ONE file only: `/etc/zecurity/client.conf` (user fallback: `~/.config/zecurity-client/client.conf`). Contains: `workspace`, and optionally `controller_address`/`connector_address`/`http_base_url` for dev. In prod these are compiled-in constants (`appmeta.rs` via `option_env!`). |
| **CLI storage — runtime** | Session tokens, device cert, private key, user info — **in memory only** (`RuntimeState` struct). Never serialized, never written to disk. Process exit clears everything. |
| **CLI IPC** | Not needed in Sprint 7. Session is in-memory only during login. Sprint 8 handles IPC for connected state. |
| **CLI command** | `zecurity-client login` — one-shot OAuth + enroll cert + store in RuntimeState + print result + exit. No daemon, no loop. |
| **Private key lifecycle** | Generated fresh (P-384 ECDSA) on every `login` run. Lives in `RuntimeState.device.private_key_pem`. Used in-process to build `rustls::ClientConfig` for mTLS (Sprint 8) — never hits disk. |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Role routing, `/invite/:token` page, `/client-install` page, admin install button |
| **M2** | Go (Proto + DB + GraphQL) | `proto/client/v1/client.proto`, migration 011, `client.graphqls` |
| **M3** | Go (Controller) | ClientService gRPC impl, invitation HTTP API + email, role middleware |
| **M4** | Rust (Client CLI) | `client/` workspace — commands: setup, login, status, logout, invite |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/client/v1/client.proto` | M2 creates it | M2 commits first — everyone waits for `buf generate` |
| `controller/graph/client.graphqls` | M2 creates it | M2 commits first — everyone waits for `go generate` + `npm run codegen` |
| `cmd/server/main.go` | M3 registers ClientService | M3 only — do not touch ConnectorService/ShieldService registrations |
| `admin/src/App.tsx` | M1 adds role routing | M1 only |
| `controller/internal/auth/middleware.go` | M3 adds RequireRole | M3 only |

---

## Execution Timeline

### DAY 1 — Unblocking Work (Must land before anyone fans out)

- [x] **M2-D1-A** `proto/client/v1/client.proto` — Create ClientService with 3 RPCs: `GetAuthConfig`, `TokenExchange`, `EnrollDevice`. See [[Sprint7/Member2-Go-Proto/Phase1-Client-Proto-Migration]].
- [x] **M2-D1-B** `controller/migrations/011_client.sql` — `invitations` + `client_devices` tables.
- [x] **M2-D1-C** `controller/graph/client.graphqls` — Invitation + ClientDevice types, queries, `createInvitation` mutation. Add to `gqlgen.yml`.
- [x] **TEAM** Run `buf generate` from repo root → Go stubs updated
- [x] **TEAM** Run `cd controller && go generate ./graph/...` → gqlgen regenerates `generated.go`
- [x] **TEAM** Run `cd admin && npm run codegen`

> After Day 1: M3 can start ClientService impl; M1 can start page scaffolds; M4 can start CLI scaffold.

---

### PHASE A — M2 Proto + DB + GraphQL (Day 1 = Phase A)

> See [[Sprint7/Member2-Go-Proto/Phase1-Client-Proto-Migration]] for full specs.

- [x] **M2-A1** `proto/client/v1/client.proto`
- [x] **M2-A2** `controller/migrations/011_client.sql`
- [x] **M2-A3** `controller/graph/client.graphqls` + `gqlgen.yml` update

> Build check: `buf generate` clean + `cd controller && go build ./...` passes.

---

### PHASE B — M3 ClientService gRPC (Depends on: Day 1 done)

- [x] **M3-B1** `controller/internal/client/service.go` — Implement `GetAuthConfig`, `TokenExchange`, `EnrollDevice`
- [x] **M3-B2** `controller/internal/client/store.go` — DB queries for client_devices insert + lookup
- [x] **M3-B3** `cmd/server/main.go` — Register `clientv1.RegisterClientServiceServer`

> Build check: `cd controller && go build ./...` passes.

---

### PHASE C — M3 Invitation HTTP API + Email (Depends on: Day 1 done)

- [x] **M3-C1** `controller/internal/invitation/handler.go` — `POST /api/invitations`, `GET /api/invitations/{token}`, `POST /api/invitations/{token}/accept`
- [x] **M3-C2** `controller/internal/invitation/store.go` — DB queries for invitations
- [x] **M3-C3** `controller/internal/invitation/email.go` — SMTP send + dev-mode stdout fallback
- [x] **M3-C4** `cmd/server/main.go` — Wire invitation routes
- [x] **M3-C5** `controller/graph/resolvers/` — `createInvitation` + `invitation` + `myDevices` resolvers

> Build check: `cd controller && go build ./...` passes.

---

### PHASE D — M3 Role Enforcement (Depends on: M3-B + M3-C done)

- [x] **M3-D1** `controller/internal/middleware/role.go` — `RequireRole(roles ...string)` HTTP middleware
- [x] Apply to `POST /api/invitations` (admin only)
- [x] GraphQL `createInvitation` resolver — role check in resolver context

> Build check: `cd controller && go build ./...` passes.

---

### PHASE E — M1 Frontend (Depends on: Day 1 codegen done + M3-C done)

- [x] **M1-E1** `admin/src/App.tsx` — Role-based redirect after auth (ADMIN → /dashboard, MEMBER/VIEWER → /client-install)
- [x] **M1-E2** `admin/src/pages/InviteAccept.tsx` — NEW: `/invite/:token` page with Google sign-in
- [x] **M1-E3** `admin/src/pages/ClientInstall.tsx` — NEW: download links + install instructions
- [x] **M1-E4** Sidebar/Header — "Install Client" button for ADMIN users

> Build check: `cd admin && npm run build` passes.

---

### PHASE F — M4 Rust Client CLI

#### F1 — Scaffold + setup/status/logout (No dependencies)

- [x] **M4-F1** `client/Cargo.toml` — workspace + dependencies
- [x] **M4-F2** `client/src/main.rs` — clap CLI with subcommands
- [x] **M4-F3** `client/src/appmeta.rs` — compile-time controller/connector constants via `option_env!`
- [x] **M4-F4** `client/src/config.rs` — reads `/etc/zecurity/client.conf` (TOML, workspace + optional dev overrides only)
- [x] **M4-F5** `client/src/runtime.rs` — `RuntimeState` in-memory struct (never serialized)
- [x] **M4-F6** `client/src/error.rs` — error types
- [x] `setup` (writes conf), `status` (placeholder), `logout` (placeholder) commands compile and run

> Build check: `cd client && cargo build` passes.

#### F2 — Login Flow (Depends on: M3-B done + F1 done)

- [x] **M4-F7** `client/build.rs` — tonic-build proto compilation
- [x] **M4-F8** `client/src/grpc.rs` — tonic ClientService client
- [x] **M4-F9** `client/src/login.rs` — library module (not a command): PKCE, local callback, GetAuthConfig, TokenExchange, EnrollDevice; returns `LoginResult` with all data in memory

> Build check: `cd client && cargo build` passes.

#### F3 — Invite Command (Depends on: M3-C done + F2 done)

- [x] **M4-F10** `client/src/cmd/invite.rs` — HTTP POST /api/invitations; gets access token from running daemon via `ipc::query_daemon_token()`

> Build check: `cd client && cargo build` passes.

#### F4 — Login One-Shot (Depends on: F2 done)

> **Architecture: Option B** — `login` is one-shot (auth + print + exit). No daemon, no IPC socket, no systemd unit.

- [x] **M4-F11** ~~`client/src/ipc.rs`~~ — **REMOVED** (no daemon IPC)
- [x] **M4-F12** `client/src/cmd/login.rs` — rewrite: load config → `login::run()` → print result → exit
- [x] **M4-F13** `client/src/cmd/status.rs` — rewrite: reads config file only, prints workspace + "Not connected"
- [x] **M4-F14** `client/src/cmd/logout.rs` — rewrite: no-op, prints "No active session to clear"
- [x] **M4-F15** ~~`client/zecurity-client.service`~~ — **REMOVED** (no systemd daemon)
- [x] `client/src/cmd/invite.rs` — rewrite: runs own `login::run()` to get token, then POST /api/invitations
- [x] Delete `client/src/ipc.rs` and `client/zecurity-client.service`; remove `libc` from Cargo.toml if unused

> Build check: `cd client && cargo build` passes.

#### F5 — TUN Mode (Depends on: F4 done + Sprint 8 Connector `:9092` listener live)

- [ ] **M4-F16** `client/src/tun_mode.rs` — reads cert + key from `RuntimeState` (in memory), builds `rustls::ClientConfig`, creates TUN interface, IP packet forwarding loop to Connector `:9092`
- [ ] Wire `TunTunnel::run()` into `connect.rs` replacing `tunnel_placeholder()`
- [ ] Systemd unit: add `AmbientCapabilities=CAP_NET_ADMIN` for TUN device access

> Build check: `cd client && cargo build` passes.

---

## Dependency Graph (Visual)

```
M2-D1-A/B/C (proto + migration + graphql)
        │
        ▼
buf generate + go generate + npm codegen
        │
   ┌────┼──────────────┬──────────────┐
   ▼    ▼              ▼              ▼
M3-B  M3-C           M1-E          M4-F1
(gRPC) (Invite API)  (UI pages)   (CLI scaffold)
  │      │                            │
  └──────┤                        M4-F2 (login)
         ▼                            │
       M3-D                       M4-F3 (invite)
   (role middleware)                   │
                                   M4-F4 (connect daemon + systemd)
                                       │
                                   M4-F5 (TUN mode) ←── Sprint 8 Connector :9092
```

---

## Final Verification Checklist

- [ ] `buf generate` — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd client && cargo build` — clean (warnings OK)
- [ ] `cd admin && npm run build` — clean
- [ ] `zecurity-client setup --workspace myworkspace` writes `/etc/zecurity/client.conf` (workspace only)
- [ ] No session/cert/key data is ever written to disk at any point
- [ ] `zecurity-client login` opens browser, completes OAuth, prints "Logged in as user@example.com", exits
- [ ] `zecurity-client status` prints workspace from config + "Not connected (run login to authenticate)"
- [ ] `zecurity-client status` with no config prints "Not configured"
- [ ] `zecurity-client logout` prints "No active session to clear"
- [ ] `zecurity-client invite --email user@example.com` runs login to authenticate via OAuth, then calls HTTP API
- [ ] Admin UI: ADMIN login → /dashboard; MEMBER login → /client-install
- [ ] `/invite/:token` page shows workspace + inviter info + Google sign-in button
- [ ] After invite acceptance: user added to workspace as MEMBER, redirect to /client-install
- [ ] ADMIN user sees "Install Client" button in sidebar/header
- [ ] `GET /api/invitations/:token` returns 404 for expired/unknown tokens
- [ ] `POST /api/invitations` returns 403 for non-admin JWT
- [ ] `zecurity-client login` (TUN mode, requires Sprint 8 Connector) creates `tun0` (100.64.0.2/24), routes packets through Connector `:9092`
- [ ] mTLS to Connector uses cert + key from RuntimeState — no disk file involved
- [ ] Connector rejects revoked cert → client logs error, clears RuntimeState, retries login

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Confirm dependency checkboxes before touching any file.
2. **Reuse existing PKI.** `pki.Service.SignCSR()` handles cert issuance — do not reimplement.
3. **CLI uses its own Google OAuth app.** `ClientService` does NOT call `authSvc.ExchangeCode` — it has its own private `exchangeCode()` method using `CLIENT_GOOGLE_CLIENT_ID`/`CLIENT_GOOGLE_CLIENT_SECRET`. The admin web app and CLI must be registered as separate OAuth clients in Google Console. See Phase 1 Post-Phase Fixes.
4. **ClientService = no mTLS.** Unlike ConnectorService, ClientService does not require client certificates on the gRPC connection. JWT Bearer in metadata is the auth mechanism.
5. **DB migration is 011.** Migrations 001–010 are taken. Do not reuse or skip numbers.
6. **SPIFFE format for client devices:** `spiffe://ws-{workspace_slug}.zecurity.in/client/{device_id}`
7. **Invitation token:** `crypto/rand` → 32 bytes → hex.EncodeToString — never UUID, never sequential.
8. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.

See individual member phase files for detailed specs:
- [[Sprint7/Member2-Go-Proto/Phase1-Client-Proto-Migration]]
- [[Sprint7/Member3-Go-Controller/Phase1-ClientService-gRPC]]
- [[Sprint7/Member3-Go-Controller/Phase2-Invitation-API]]
- [[Sprint7/Member3-Go-Controller/Phase3-Role-Enforcement]]
- [[Sprint7/Member1-Frontend/Phase1-Role-Routing-Invite-Pages]]
- [[Sprint7/Member1-Frontend/Phase2-Admin-User-Detection]]
- [[Sprint7/Member4-Rust-Client/Phase1-CLI-Scaffold]]
- [[Sprint7/Member4-Rust-Client/Phase2-Login-Flow]]
- [[Sprint7/Member4-Rust-Client/Phase3-Invite-Command]]
- [[Sprint7/Member4-Rust-Client/Phase4-Systemd-Daemon]]
- [[Sprint7/Member4-Rust-Client/Phase5-TUN-Mode]]

**M4 TUN mode note:** Phase 5 depends on Sprint 8's Connector `device_tunnel.rs` being live on `:9092`. F4 (daemon structure) can be completed and tested without it. F5 requires the Connector listener to be running.

---

## Post-Sprint Fixes

Bugs discovered during TL review and Phase 1 implementation. All applied before M1 Phase E work began.

### Fix: `CONTROLLER_HOST` missing from env files
**Phase:** M3 Phase 1 | **File:** `controller/cmd/server/main.go`, `controller/.env`, `controller/.env.example`
`mustEnv("CONTROLLER_HOST")` was called but var never added to env files — server crashed on startup. Added `CONTROLLER_HOST=localhost:9090` to both files.
See full details → [[Sprint7/Member3-Go-Controller/Phase1-ClientService-gRPC]]

---

### Fix: CLI OAuth must use separate Google credentials
**Phase:** M3 Phase 1 | **File:** `controller/internal/client/service.go`, `controller/cmd/server/main.go`
`TokenExchange` was calling `authSvc.ExchangeCode` / `authSvc.VerifyIDToken` (admin web app credentials). Google rejects a code exchange when the client ID doesn't match. Added private `exchangeCode()` to `ClientService`, new env vars `CLIENT_GOOGLE_CLIENT_ID` + `CLIENT_GOOGLE_CLIENT_SECRET`.
See full details → [[Sprint7/Member3-Go-Controller/Phase1-ClientService-gRPC]]

---

### Fix: `workspace_name` missing from GET invitation response
**Phase:** M3 Phase 2 | **File:** `controller/internal/invitation/handler.go`
`GET /api/invitations/{token}` response omitted `workspace_name`. Frontend `InviteAccept` needs it to call `InitiateAuth(workspaceName)`. `GetByToken` already JOINed it — just wasn't serialized. Added `WorkspaceName` to `invitationResponse`.
See full details → [[Sprint7/Member3-Go-Controller/Phase2-Invitation-API]]

---

### Fix: `AcceptInvitation` did not add user as MEMBER
**Phase:** M3 Phase 2 | **File:** `controller/internal/invitation/store.go`, `controller/internal/invitation/handler.go`
`AcceptInvitation` only updated `invitations.status`. The `INSERT INTO workspace_users` step from the plan spec was missing — invited users had no workspace membership after accepting. Fixed store + handler to pass and use `userID`.
See full details → [[Sprint7/Member3-Go-Controller/Phase2-Invitation-API]]

---

### Fix: `CreateInvitation` GraphQL resolver sent blank workspace name in email
**Phase:** M3 Phase 2 | **File:** `controller/graph/resolvers/client.resolvers.go`
Resolver passed `""` to `SendInvitation`. Fixed by querying workspace name from DB before calling emailer.
See full details → [[Sprint7/Member3-Go-Controller/Phase2-Invitation-API]]

---

### Fix: `client.graphqls` missing from admin codegen + queries not added
**Phase:** M3 Phase 2 | **File:** `admin/codegen.yml`, `admin/src/graphql/queries.graphql`, `admin/src/graphql/mutations.graphql`
`Invitation`/`ClientDevice` types and `createInvitation`/`myDevices`/`invitation` operations were absent from generated TypeScript. Added schema entry to codegen config, added queries + mutation to graphql files, re-ran `make codegen`. Also created `controller/graph/resolvers/client_helpers.go` to house `invitationToGQL()` helper so gqlgen doesn't evict it.
See full details → [[Sprint7/Member3-Go-Controller/Phase2-Invitation-API]]
