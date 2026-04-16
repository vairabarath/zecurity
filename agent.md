# Agent Coordination — Zecurity

> Entry point for any AI agent working on this project.
> Read this before doing anything.

---

## Project

**Zecurity** — a Zero Trust Network Access (ZTNA) platform. Admins create remote networks, deploy connectors on Linux servers, and those connectors maintain secure mTLS tunnels back to the controller using SPIFFE X.509 identities.

- **Controller:** Go — `controller/` — HTTP :8080 (GraphQL) + gRPC :9090 (connector RPCs)
- **Connector:** Rust — `connector/` — Linux binary, enrollment + heartbeat + cert renewal + auto-update
- **Admin UI:** React + Vite + Apollo — `admin/`
- **Database:** PostgreSQL (pgx/v5) + Redis (sessions + JTI burn)
- **PKI:** 3-tier CA (Root → Intermediate → Workspace CA → Connector cert)
- **Identity:** SPIFFE — `spiffe://<trust_domain>/connector/<id>`
- **Releases:** GitHub Actions (`.github/workflows/connector-release.yml`) — triggered by `connector-v*` tags

**What's complete:**
- Sprint 1: Auth (Google OAuth + JWT), workspace management, admin UI
- Sprint 2: PKI, connector enrollment, mTLS heartbeat, SPIFFE interceptor, auto-update
- Sprint 3: Automatic cert renewal (RenewCert RPC, proof-of-possession CSR, channel rebuild)

**What's next:**
- Phase 6: end-to-end renewal test (see [[Planning/Roadmap]])
- Sprint 4: traffic proxying (WireGuard / tun)

---

## Agent Hierarchy

```
Claude Code (Lead)
  │
  ├── Manages architecture decisions and Obsidian knowledge base
  ├── Reviews all implementation work
  ├── Final say on design direction
  │
  └── Codex / OpenCode / Other models (Specialists)
        ├── Execute implementation tasks
        ├── Follow conventions defined here
        └── Log their work in the session log
```

**Claude Code is the lead agent.** When uncertain about architecture, design, or structure — defer to Claude Code's prior decisions documented in the vault.

---

## Workflow Split

| Layer | Tool | What Goes Here |
|-------|------|----------------|
| **Knowledge** | Obsidian (`.zecurity-obs/`) | Architecture diagrams, service docs, planning, session logs |
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
    PKI.md                      — 3-tier CA, key encryption, cert renewal flow
    Auth.md                     — Google OAuth, JWT, enrollment tokens
  Planning/
    Roadmap.md                  — sprint status, current priorities, what's next
    Session Log.md              — running log of all work sessions
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

---

## Session Protocol

### Before Starting Work

1. Read `agent.md` (this file)
2. Read `.zecurity-obs/Planning/Session Log.md` for recent context
3. Read `.zecurity-obs/Planning/Roadmap.md` for current priorities
4. Read relevant service note(s) if touching a specific subsystem

### During Work

- Follow existing code patterns (read the file you're modifying first)
- Controller: `go build ./...` must pass before committing
- Connector: `cargo build` must pass (warnings OK, errors not)
- If making an architecture decision, document it or flag it for Claude Code

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

### Rust (Connector)
- `anyhow::Result` for all fallible functions
- Async with `tokio` — `#[tokio::main]`
- Structured logging with `tracing` (`info!`, `error!`, `warn!`)
- Config via `figment` (env + TOML file)
- All file I/O async via `tokio::fs`

### Proto (connector.proto)

**Source of truth:** `controller/proto/connector/v1/connector.proto`

- Single source file with versioned package `connector.v1`
- Go generated code: `controller/gen/go/proto/connector/v1/` (via Buf)
- Rust builds reference controller proto directly: `../controller/proto/connector/v1/connector.proto`
- Generate Go code: `make generate-proto` or `cd controller && buf generate`
- Build: `go build ./...` (Go) / `cargo build` (Rust)

---

## Releases

Connector binaries are built by GitHub Actions:
- Trigger: push a tag matching `connector-v*`
- Produces: `connector-linux-amd64` + `connector-linux-arm64` (musl static)
- Also packages: install script + systemd units

```bash
git tag connector-v0.x.x
git push origin connector-v0.x.x
```

---

## Quick Reference

| Task | Command |
|------|---------|
| Build controller | `cd controller && go build ./...` |
| Build connector (dev) | `cd connector && cargo build` |
| Build connector (release) | `cd connector && cargo build --release` |
| Generate proto | `make generate-proto` (from repo root) |
| Test renewal (short TTL) | Set `CONNECTOR_CERT_TTL=3m CONNECTOR_RENEWAL_WINDOW=2m CONNECTOR_HEARTBEAT_INTERVAL=5s` in `.env` |
| Release connector binary | `git tag connector-vX.Y.Z && git push origin connector-vX.Y.Z` |
| Open vault | Open `.zecurity-obs/` in Obsidian |
