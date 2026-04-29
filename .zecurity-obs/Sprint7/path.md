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
| **CLI IPC** | Running `connect` daemon exposes a Unix socket (`/run/zecurity-client.sock` or `/tmp/zecurity-client-{uid}.sock`). `status`, `invite`, `logout` subcommands query it via newline-delimited JSON. |
| **Private key lifecycle** | Generated fresh (P-384 ECDSA) on every `connect` run. Lives in `RuntimeState.device.private_key_pem`. Used in-process to build `rustls::ClientConfig` for TUN mTLS — never hits disk. |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Role routing, `/invite/:token` page, `/client-install` page, admin install button |
| **M2** | Go (Proto + DB + GraphQL) | `proto/client/v1/client.proto`, migration 011, `client.graphqls` |
| **M3** | Go (Controller) | ClientService gRPC impl, invitation HTTP API + email, role middleware |
| **M4** | Rust (Client CLI) | `client/` workspace — all 5 commands: setup, login, status, logout, invite |

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

- [ ] **M3-D1** `controller/internal/auth/middleware.go` — `RequireRole(roles ...string)` HTTP middleware
- [ ] Apply to `POST /api/invitations` (admin only)
- [ ] GraphQL `createInvitation` resolver — role check in resolver context

> Build check: `cd controller && go build ./...` passes.

---

### PHASE E — M1 Frontend (Depends on: Day 1 codegen done + M3-C done)

- [ ] **M1-E1** `admin/src/App.tsx` — Role-based redirect after auth (ADMIN → /dashboard, MEMBER/VIEWER → /client-install)
- [ ] **M1-E2** `admin/src/pages/InviteAccept.tsx` — NEW: `/invite/:token` page with Google sign-in
- [ ] **M1-E3** `admin/src/pages/ClientInstall.tsx` — NEW: download links + install instructions
- [ ] **M1-E4** Sidebar/Header — "Install Client" button for ADMIN users

> Build check: `cd admin && npm run build` passes.

---

### PHASE F — M4 Rust Client CLI

#### F1 — Scaffold + setup/status/logout (No dependencies)

- [ ] **M4-F1** `client/Cargo.toml` — workspace + dependencies
- [ ] **M4-F2** `client/src/main.rs` — clap CLI with subcommands
- [ ] **M4-F3** `client/src/appmeta.rs` — compile-time controller/connector constants via `option_env!`
- [ ] **M4-F4** `client/src/config.rs` — reads `/etc/zecurity/client.conf` (TOML, workspace + optional dev overrides only)
- [ ] **M4-F5** `client/src/runtime.rs` — `RuntimeState` in-memory struct (never serialized)
- [ ] **M4-F6** `client/src/error.rs` — error types
- [ ] `setup` (writes conf), `status` (placeholder), `logout` (placeholder) commands compile and run

> Build check: `cd client && cargo build` passes.

#### F2 — Login Flow (Depends on: M3-B done + F1 done)

- [ ] **M4-F7** `client/build.rs` — tonic-build proto compilation
- [ ] **M4-F8** `client/src/grpc.rs` — tonic ClientService client
- [ ] **M4-F9** `client/src/login.rs` — library module (not a command): PKCE, local callback, GetAuthConfig, TokenExchange, EnrollDevice; returns `LoginResult` with all data in memory

> Build check: `cd client && cargo build` passes.

#### F3 — Invite Command (Depends on: M3-C done + F2 done)

- [ ] **M4-F10** `client/src/cmd/invite.rs` — HTTP POST /api/invitations; gets access token from running daemon via `ipc::query_daemon_token()`

> Build check: `cd client && cargo build` passes.

#### F4 — Systemd Daemon + IPC (Depends on: F2 done)

- [ ] **M4-F11** `client/src/ipc.rs` — Unix socket server (inside daemon) + client helpers (`query_daemon_status`, `query_daemon_token`, `signal_daemon_logout`)
- [ ] **M4-F12** `client/src/cmd/connect.rs` — `connect` subcommand: calls `login::run()`, populates `SharedState`, spawns IPC server, reconnect loop, sd_notify READY + WATCHDOG
- [ ] **M4-F13** `client/src/cmd/status.rs` — updated: queries IPC socket for live status
- [ ] **M4-F14** `client/src/cmd/logout.rs` — updated: sends logout command via IPC socket
- [ ] **M4-F15** `client/zecurity-client.service` — systemd unit file (Type=notify, Restart=on-failure, WatchdogSec=90)

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
- [ ] `zecurity-client connect` opens browser, completes OAuth, populates RuntimeState in memory, prints "Connected as user@example.com"
- [ ] `zecurity-client status` queries running daemon via Unix socket, prints all fields
- [ ] `zecurity-client status` with no daemon running prints "Not connected"
- [ ] `zecurity-client logout` signals daemon via Unix socket to clear in-memory session
- [ ] `zecurity-client invite --email user@example.com` gets access token from daemon via IPC, calls HTTP API
- [ ] `zecurity-client invite` with no daemon running prints "Not connected" error
- [ ] Admin UI: ADMIN login → /dashboard; MEMBER login → /client-install
- [ ] `/invite/:token` page shows workspace + inviter info + Google sign-in button
- [ ] After invite acceptance: user added to workspace as MEMBER, redirect to /client-install
- [ ] ADMIN user sees "Install Client" button in sidebar/header
- [ ] `GET /api/invitations/:token` returns 404 for expired/unknown tokens
- [ ] `POST /api/invitations` returns 403 for non-admin JWT
- [ ] `systemctl start zecurity-client` starts daemon; `journalctl` shows READY=1 and WATCHDOG=1
- [ ] `zecurity-client connect` (TUN mode, requires Sprint 8 Connector) creates `tun0` (100.64.0.2/24), routes packets through Connector `:9092`
- [ ] mTLS to Connector uses cert + key from RuntimeState — no disk file involved
- [ ] Connector rejects revoked cert → client logs error, clears RuntimeState, retries login

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Confirm dependency checkboxes before touching any file.
2. **Reuse existing PKI.** `pki.Service.SignCSR()` handles cert issuance — do not reimplement.
3. **Reuse existing OAuth code.** `auth/exchange.go` and `auth/idtoken.go` — TokenExchange gRPC handler calls these directly.
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
