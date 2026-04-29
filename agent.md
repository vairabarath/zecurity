# Agent Coordination — Zecurity

> Entry point for any AI agent working on this project.
> Read this before doing anything.

---

## Quick Start (For AI Agents — Read This First)

A team member will tell you their member number. When they do, execute this sequence immediately:

```
1. Read this file (agent.md) fully
2. Read .zecurity-obs/Sprint7/path.md
3. Find the first unchecked phase for your member where all depends_on are checked
4. Read that phase file
5. Tell the member: what they're building, which files to touch, the build check command
```

If no member number given, ask: *"Which member are you? M1 (Frontend), M2 (Go), M3 (Go+Rust), or M4 (Rust)?"*

**Team workflow guide (human-readable):** `.zecurity-obs/Sprint7/team-workflow.md`  
**Active sprint plan:** `.zecurity-obs/Sprint7/path.md`

---

## Project

**Zecurity** — a Zero Trust Network Access (ZTNA) platform. Admins create remote networks, deploy connectors on Linux servers, and those connectors maintain secure mTLS tunnels back to the controller using SPIFFE X.509 identities. Shields are deployed on resource hosts and heartbeat through Connectors.

- **Controller:** Go — `controller/` — HTTP :8080 (GraphQL) + gRPC :9090 (Connector + Shield RPCs)
- **Connector:** Rust — `connector/` — Linux binary, enrollment + heartbeat + cert renewal + auto-update + Shield-facing gRPC :9091
- **Shield:** Rust — `shield/` — Linux binary, enrollment + heartbeat via Connector + zecurity0 + nftables *(Sprint 4)*
- **Admin UI:** React + Vite + Apollo — `admin/`
- **Client:** Rust CLI — `client/` *(Sprint 7)*
- **Database:** PostgreSQL (pgx/v5) + Redis (sessions + JTI burn)
- **PKI:** 3-tier CA (Root → Intermediate → Workspace CA → Connector cert / Shield cert)
- **Identity:** SPIFFE — `spiffe://<trust_domain>/connector/<id>` and `spiffe://<trust_domain>/shield/<id>`
- **Releases:** GitHub Actions — `connector-v*` tags (connector), `shield-v*` tags (shield)

**What's complete:**
- Sprint 1: Auth (Google OAuth + JWT), workspace management, admin UI
- Sprint 2: PKI, connector enrollment, mTLS heartbeat, SPIFFE interceptor, auto-update
- Sprint 3: Automatic cert renewal (RenewCert RPC, proof-of-possession CSR, channel rebuild)

**What's complete:**
- Sprint 4: Shield deployment — zecurity0 TUN, nftables base table, heartbeat via Connector, SPIFFE identity

**What's complete:**
- Sprint 5: Resource protection — Shield applies nftables rules per resource, lifecycle `pending → managing → protecting → protected` via heartbeat piggyback

**What's active:**
- Sprint 7: Client Application — Admin invites users, client CLI login with mTLS enrollment, status command, role-based routing. See `.zecurity-obs/Sprint7/path.md`

---

## Agent Hierarchy

```
Claude Code (Lead)
  │
  ├── Manages architecture decisions and Obsidian knowledge base
  ├── Reviews all implementation work
  ├── Final say on design direction
  ├── Updates Sprint7/path.md checkboxes as phases complete
  │
  └── Codex / OpenCode / Other models (Specialists)
        ├── Execute implementation tasks assigned per member role
        ├── Follow conventions defined here
        ├── Check Sprint7/path.md BEFORE touching any file
        └── Log their work in the session log
```

**Claude Code is the lead agent.** When uncertain about architecture, design, or structure — defer to Claude Code's prior decisions documented in the vault.

---

## Workflow Split

| Layer | Tool | What Goes Here |
|-------|------|----------------|
| **Knowledge** | Obsidian (`.zecurity-obs/`) | Architecture diagrams, service docs, planning, session logs, sprint plans |
| **Code** | VSCode / Neovim / Claude Code | Go + Rust source, tests, scripts, git |
| **Coordination** | This file (`agent.md`) | Shared conventions, agent roles, vault structure |

---

## Obsidian Vault

**Location:** `.zecurity-obs/`

The shared brain. All agents should read relevant notes before working on a subsystem.

```
.zecurity-obs/
  Architecture/
    System Overview.canvas      — services, databases, connections
    Connector Lifecycle.canvas  — enrollment → heartbeat → cert renewal flow
  Services/
    Controller.md               — Go backend: HTTP + gRPC, internal services
    Connector.md                — Rust agent: lifecycle, state files, config
    Shield.md                   — Rust resource agent: lifecycle, network setup (Sprint 4)
    PKI.md                      — 3-tier CA, key encryption, cert renewal flow
    Auth.md                     — Google OAuth, JWT, enrollment tokens
  Planning/
    Roadmap.md                  — sprint status, current priorities, what's next
    Session Log.md              — running log of all work sessions
  Sprint1/ (complete) - Sprint5/ (complete)
  Sprint6/ (complete) - Discovery
  Sprint7/ (ACTIVE)  - Client Application
    path.md                     — MASTER dependency map + ordered checklist (read first!)
    Member1-Frontend/           — M1 phase files
    Member2-Go-Proto/           — M2 phase files
    Member3-Go-Controller/     — M3 phase files
    Member4-Rust-Client/       — M4 phase files (client CLI)
  Sprint8/ (planned) - RDE (Connector :9092 tunnel)
  Decisions/
  Research/
```

### Conventions

- Use `[[wikilinks]]` to connect related notes
- Every service note links to the services it depends on
- Canvas files are the visual truth — update them when architecture changes
- Tag notes with frontmatter: `type`, `status`, `language`, `related`
- Sprint phase files use frontmatter: `type: task`, `status: pending/in-progress/done`, `member`, `phase`, `depends_on`, `unlocks`

---

## Session Protocol

### Before Starting Work

1. Read `agent.md` (this file)
2. Read `.zecurity-obs/Planning/Session Log.md` for recent context
3. Read `.zecurity-obs/Planning/Roadmap.md` for current priorities
4. **If Sprint 7:** Read `.zecurity-obs/Sprint7/path.md` — check which phases are unchecked, confirm all dependencies for your phase are met
5. Read relevant service note(s) if touching a specific subsystem

### During Work

- Follow existing code patterns (read the file you're modifying first)
- Controller: `go build ./...` must pass before committing
- Connector: `cargo build` must pass (warnings OK, errors not)
- Shield: `cargo build --manifest-path shield/Cargo.toml` must pass
- Client: `cargo build --manifest-path client/Cargo.toml` must pass
- **Sprint 7:** After completing a phase, check its box in `Sprint7/path.md` and update the phase file `status:` frontmatter to `done`
- If making an architecture decision, document it or flag it for Claude Code
- Do not touch files owned by other members — see conflict zone table in `Sprint6/path.md`

### After Work

Append a session entry to `.zecurity-obs/Planning/Session Log.md`:

```markdown
## YYYY-MM-DD — [Agent Name]

**What was done:**
- bullet points of changes

**Key decisions:**
- choices made and why

**What's next:**
- what the next session should pick up
```

---

## Code Conventions

### Go (Controller)
- Standard `gofmt` formatting
- Errors wrapped with `fmt.Errorf("context: %w", err)`
- gRPC handlers return `status.Error(codes.X, "message")` — never raw errors
- DB queries use `pgx/v5` directly (no ORM)
- All env vars parsed in `cmd/server/main.go` via `mustEnv` / `mustDuration` / `envOr`

### Rust (Connector + Shield)
- `anyhow::Result` for all fallible functions
- Async with `tokio` — `#[tokio::main]`
- Structured logging with `tracing` (`info!`, `error!`, `warn!`)
- Config via `figment` (env + TOML file)
- All file I/O async via `tokio::fs`
- Mirror constants between `connector/src/appmeta.rs` and `shield/src/appmeta.rs` — they must be identical

### Proto (connector.proto + shield.proto)

**Source of truth:**
- `proto/connector/v1/connector.proto` — Connector ↔ Controller RPCs
- `proto/shield/v1/shield.proto` — Shield ↔ Connector + Shield ↔ Controller RPCs

**Rules:**
- Single source file per service with versioned package
- Go generated code: `controller/gen/go/proto/<service>/v1/` (via Buf)
- Rust `build.rs` references: `../proto/<service>/v1/<service>.proto`
- Buf configs (`buf.yaml`, `buf.gen.yaml`) live at repo root
- Generate Go code: `make generate-proto` or `buf generate` (from repo root)
- **Never change or reorder existing proto field numbers** — they are permanent
- Build: `go build ./...` (Go) / `cargo build` (Rust) / `cargo build --manifest-path shield/Cargo.toml` (Shield)

---

## Releases

### Connector
```bash
git tag connector-v0.x.x
git push origin connector-v0.x.x
```
Produces: `connector-linux-amd64` + `connector-linux-arm64` (musl static)

### Shield (Sprint 4)
```bash
git tag shield-v0.x.x
git push origin shield-v0.x.x
```
Produces: `shield-linux-amd64` + `shield-linux-arm64` (musl static)

---

## Quick Reference

| Task | Command |
|------|---------|
| Build controller | `cd controller && go build ./...` |
| Build connector (dev) | `cd connector && cargo build` |
| Build connector (release) | `cd connector && cargo build --release` |
| Build shield (dev) | `cargo build --manifest-path shield/Cargo.toml` |
| Build shield (release) | `cargo build --manifest-path shield/Cargo.toml --release` |
| Build client (dev) | `cd client && cargo build` |
| Generate proto (all) | `make generate-proto` (from repo root) |
| Regenerate GraphQL | `cd controller && go generate ./graph/...` |
| Frontend codegen | `cd admin && npm run codegen` |
| Test renewal (short TTL) | Set `CONNECTOR_CERT_TTL=3m CONNECTOR_RENEWAL_WINDOW=2m CONNECTOR_HEARTBEAT_INTERVAL=5s` in `.env` |
| Release connector binary | `git tag connector-vX.Y.Z && git push origin connector-vX.Y.Z` |
| Release shield binary | `git tag shield-vX.Y.Z && git push origin shield-vX.Y.Z` |
| Open vault | Open `.zecurity-obs/` in Obsidian |
| Sprint 7 dependency map | Read `.zecurity-obs/Sprint7/path.md` |

---

## Sprint 6 Quick Rules (for any AI agent)

1. **Read `Sprint6/path.md` first.** Find your member's phases. Confirm all `depends_on` are checked.
2. **Build gates are mandatory.** Every phase file has a "Build Check" section. Do not proceed until it passes.
3. **Conflict zones.** Files multiple members touch are listed in `path.md`. Coordinate before editing them.
4. **Proto is immutable once published.** Never change field numbers. Never remove fields. Current maxes: ShieldControlMessage field 6 (pong), ConnectorControlMessage field 7 (pong).
5. **appmeta constants must match exactly.** Go `identity.go` and Rust `appmeta.rs` strings must be identical.
6. **Shield heartbeats to Connector :9091 only.** Never to Controller directly (post-enrollment).
7. **Discovery rides existing Control streams.** No new RPCs — DiscoveryReport on Shield Control stream; ShieldDiscoveryBatch/ScanCommand/ScanReport on Connector Control stream.
8. **Shield scans only its own host.** Read `/proc/net/tcp` — no network scanning from Shield.
9. **Connector scanner has hard limits.** Max 512 targets, 16 ports, 32 concurrent probes — enforced in scope.rs and scan.rs.

---

## Sprint 7 Quick Rules (for any AI agent)

1. **Read `Sprint7/path.md` first.** Find your member's phases. Confirm all `depends_on` are checked.
2. **Build gates are mandatory.** Every phase file has a "Build Check" section. Do not proceed until it passes.
3. **Conflict zones.** Files multiple members touch are listed in `path.md`. Coordinate before editing them.
4. **Proto is immutable once published.** Never change field numbers. Current: ClientService RPCs (GetAuthConfig, TokenExchange, EnrollDevice).
5. **ClientService = no mTLS.** Uses plain TLS + JWT Bearer. Client does not have cert yet.
6. **Reuse existing PKI.** `pki.Service.SignCSR()` handles cert issuance.
7. **Reuse existing OAuth.** `auth/exchange.go` for token exchange.
8. **CLI state in memory only.** Tokens, cert, key stored in RuntimeState. Never written to disk.
9. **SPIFFE format:** `spiffe://ws-{workspace_slug}.zecurity.in/client/{device_id}`
