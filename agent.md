# Agent Coordination — Zecurity

> Entry point for any AI agent working on this project.
> Read this before doing anything.

---

## Quick Start (For AI Agents — Read This First)

A team member will tell you their member number. When they do, execute this sequence immediately:

```
1. Read this file (agent.md) fully
2. Read .zecurity-obs/Sprint4/path.md
3. Find the first unchecked phase for this member where all depends_on are checked
4. Read that phase file
5. Tell the member: what they're building, which files to touch, the build check command
```

If no member number given, ask: *"Which member are you? M1 (Frontend), M2 (Go), M3 (Go+Rust), or M4 (Rust)?"*

**Team workflow guide (human-readable):** `.zecurity-obs/Sprint4/team-workflow.md`

---

## Project

**Zecurity** — a Zero Trust Network Access (ZTNA) platform. Admins create remote networks, deploy connectors on Linux servers, and those connectors maintain secure mTLS tunnels back to the controller using SPIFFE X.509 identities. Shields are deployed on resource hosts and heartbeat through Connectors.

- **Controller:** Go — `controller/` — HTTP :8080 (GraphQL) + gRPC :9090 (Connector + Shield RPCs)
- **Connector:** Rust — `connector/` — Linux binary, enrollment + heartbeat + cert renewal + auto-update + Shield-facing gRPC :9091
- **Shield:** Rust — `shield/` — Linux binary, enrollment + heartbeat via Connector + zecurity0 + nftables *(Sprint 4)*
- **Admin UI:** React + Vite + Apollo — `admin/`
- **Database:** PostgreSQL (pgx/v5) + Redis (sessions + JTI burn)
- **PKI:** 3-tier CA (Root → Intermediate → Workspace CA → Connector cert / Shield cert)
- **Identity:** SPIFFE — `spiffe://<trust_domain>/connector/<id>` and `spiffe://<trust_domain>/shield/<id>`
- **Releases:** GitHub Actions — `connector-v*` tags (connector), `shield-v*` tags (shield)

**What's complete:**
- Sprint 1: Auth (Google OAuth + JWT), workspace management, admin UI
- Sprint 2: PKI, connector enrollment, mTLS heartbeat, SPIFFE interceptor, auto-update
- Sprint 3: Automatic cert renewal (RenewCert RPC, proof-of-possession CSR, channel rebuild)

**What's active:**
- Sprint 4: Shield deployment — see `.zecurity-obs/Sprint4/path.md` for the full execution plan

**What's next:**
- Sprint 5: Resource discovery (RDE, Connector scans network, per-resource nftables rules)

---

## Agent Hierarchy

```
Claude Code (Lead)
  │
  ├── Manages architecture decisions and Obsidian knowledge base
  ├── Reviews all implementation work
  ├── Final say on design direction
  ├── Updates Sprint4/path.md checkboxes as phases complete
  │
  └── Codex / OpenCode / Other models (Specialists)
        ├── Execute implementation tasks assigned per member role
        ├── Follow conventions defined here
        ├── Check Sprint4/path.md BEFORE touching any file
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
  Sprint4/                      — Sprint 4 execution plan and phase files
    path.md                     — MASTER dependency map + ordered checklist (read first!)
    Member1-Frontend/           — M1 phase files (React + GraphQL)
    Member2-Go-Proto-Shield/    — M2 phase files (Proto + Shield service + PKI)
    Member3-Go-DB-GraphQL/      — M3 phase files (DB + resolvers + Connector improvements)
    Member4-Rust-Shield-CI/     — M4 phase files (Shield crate + CI)
  Decisions/
    (architecture decision records — why X over Y)
  Research/
    (protocol notes, external references)
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
4. **If Sprint 4:** Read `.zecurity-obs/Sprint4/path.md` — check which phases are unchecked, confirm all dependencies for your phase are met
5. Read relevant service note(s) if touching a specific subsystem

### During Work

- Follow existing code patterns (read the file you're modifying first)
- Controller: `go build ./...` must pass before committing
- Connector: `cargo build` must pass (warnings OK, errors not)
- Shield: `cargo build --manifest-path shield/Cargo.toml` must pass
- **Sprint 4:** After completing a phase, check its box in `Sprint4/path.md` and update the phase file `status:` frontmatter to `done`
- If making an architecture decision, document it or flag it for Claude Code
- Do not touch files owned by other members — see conflict zone table in `Sprint4/path.md`

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
| Generate proto (all) | `make generate-proto` (from repo root) |
| Regenerate GraphQL | `cd controller && go generate ./graph/...` |
| Frontend codegen | `cd admin && npm run codegen` |
| Test renewal (short TTL) | Set `CONNECTOR_CERT_TTL=3m CONNECTOR_RENEWAL_WINDOW=2m CONNECTOR_HEARTBEAT_INTERVAL=5s` in `.env` |
| Release connector binary | `git tag connector-vX.Y.Z && git push origin connector-vX.Y.Z` |
| Release shield binary | `git tag shield-vX.Y.Z && git push origin shield-vX.Y.Z` |
| Open vault | Open `.zecurity-obs/` in Obsidian |
| Sprint 4 dependency map | Read `.zecurity-obs/Sprint4/path.md` |

---

## Sprint 4 Quick Rules (for any AI agent)

1. **Read `Sprint4/path.md` first.** Find your member's phases. Confirm all `depends_on` are checked.
2. **Build gates are mandatory.** Every phase file has a "Build Check" section. Do not proceed until it passes.
3. **Conflict zones.** Files multiple members touch are listed in `path.md`. Coordinate before editing them.
4. **Proto is immutable once published.** Never change field numbers. Never remove fields.
5. **appmeta constants must match exactly.** Go `identity.go` and Rust `appmeta.rs` strings must be identical.
6. **Shield heartbeats to Connector :9091 only.** Never to Controller directly (post-enrollment).
7. **Enrollment token is single-use.** Redis JTI burn is atomic GET+DEL — race conditions are not possible.
