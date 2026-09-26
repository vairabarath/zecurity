# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Relay (Rust), Client (Rust), Admin UI (React), Provider Console (React, new in Sprint 21).

**Sprint 21 is the active sprint: Provider Dashboard Phase 2, "Dedicated Provider Console: Login, Roles, Read-Only Fleet View".** A dedicated provider console with hardened login and a working role system, then read-only views of the state Sprint 20 made true. Order: H → C → U → R → P (K, V alongside):
- provider identity hardening (D-16): dedicated provider signing key and issuer, Google `sub` + `hd` binding, `session_generation`, logout;
- a dedicated provider console, `provider-console/` (Vite + React, D-18): Google login, logout, role-aware access (`super-admin` / `relay-ops`);
- operator management: super-admins add operators, change roles, disable and re-enable (audited, lock-out-proof; bootstrap accounts pinned);
- provider read APIs under `/provider/*` (D-04/D-05/D-06/D-15): relays, tenants, provider audit, certificate expiry;
- read-only console pages for relays, tenants, audit and certificate health;
- Sprint 20 cleanup: KI-1 (relay renewal scheduling), KI-2 (shield renewal window), KI-3 (dev redirect URI), JWTs removed from `.env.example`;
- Sprint 20 live verification (`docs/sprint20-live-acceptance-runbook.md`).

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"** (D-01 … D-23). The plan is in `.zecurity-obs/Sprint21/path.md`. This sprint introduces **no** architectural decisions.

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, run by Postgres only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change, recreate the local DB: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members only.** **M1 = Sathiya** (Go + React: Sprint 20 live verification, provider console foundation + Provider users page + read pages, provider read actions and read APIs). **M2 = Barath** (Go + Rust: provider identity hardening, operator management API, KI-1, KI-2, KI-3, `.env.example` JWT cleanup, database development guide).

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint21/path.md` — development rule, dependency map, conflict zones, open questions and progress checkboxes
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are checked (`Sprint21/Member1-Go/*` for Sathiya, `Sprint21/Member2-Go-Rust/*` for Barath)
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint21/path.md` | Development rule, dependency map, conflict zones, open questions, progress tracker (checkboxes) |
| `.zecurity-obs/Sprint21/Member{N}-*/Phase*.md` | Detailed spec per phase |
| `.zecurity-obs/Sprint21/Acceptance-Test-Plan.md` | Sprint acceptance cases (AT-CORE, AT-H, AT-C, AT-U, AT-R, AT-P, AT-K, AT-DEV, AT-V) |
| `docs/database-development.md` | Local database workflow: schema files, reset rule (lands with Sprint 21 DEV-1) |
| `docs/sprint20-live-acceptance-runbook.md` + `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md` | Sprint 20 live verification (Sprint 21 Phase V) |
| `.zecurity-obs/Sprint20/path.md` | Sprint 20 record, incl. **Known Issues** KI-1 … KI-3 |
| `docs/provider-dashboard-architecture-decisions.md` | Provider dashboard Decision Record (binding) |
| `docs/provider-dashboard-architecture-discovery.md` | Current-architecture discovery report |
| `.zecurity-obs/Planning/Session Log.md` | Append a session entry when done |

---

## Post-Sprint Fixes

After completing a sprint, fixes may be merged from main branch. **Always check for "Post-Phase Fixes" sections** in:
- The sprint's `path.md` (overview of all fixes)
- Individual phase files (specific fixes for that phase)

These sections document bugs discovered during testing and their resolutions. Apply these fixes when working on related code.

---

## How to Document Fixes

When you fix a bug during development:

1. **Add fix to the correct phase file** — If the bug is in code that was implemented in a specific phase, add the fix details to that phase file's "Post-Phase Fixes" section.

2. **Include in path.md** — Also add a summary to the sprint's `path.md` "Post-Sprint Fixes" section for overview.

3. **Document the fix with:**
   - File name and location
   - Issue description
   - Root cause (if known)
   - Fix applied (code snippet or description)
   - Related files also fixed

Example fix format:
```markdown
### Fix: <Bug Name>
**Issue:** <What was wrong>

**Root Cause:** <Why it happened>

**Fix Applied (line ~XX):**
// BEFORE:
<old code>

// AFTER:
<new code>
```

**Important:** Add fixes to the phase file where the original implementation was done, not just to path.md. This ensures the phase file contains all knowledge about that implementation.

---

## Build Commands (memorize these)

```bash
cd controller && go build ./...                              # Go controller
cd connector && cargo build                                  # Rust connector
cargo build --manifest-path shield/Cargo.toml               # Rust shield
cd client && cargo build                                   # Rust client CLI
buf generate                                                 # Proto → Go stubs (from repo root)
cd controller && go generate ./graph/...                     # GraphQL codegen
cd admin && npm run codegen                                  # Frontend TS hooks
cd provider-console && npm run build                         # Provider console (Sprint 21)
cd controller && docker compose down -v && docker compose up -d   # Recreate local DB after any schema change
```

---

## Rules (non-negotiable)

Sprint 21 specific:
- Build gate passes before proceeding to next phase (DB-backed tests must run, not skip)
- The Decision Record (`docs/provider-dashboard-architecture-decisions.md`, 2026-09-23) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code. Open questions OQ-1/OQ-2 in `path.md` must be answered before the tenant read endpoints.
- **No migration framework.** A schema change is a new numbered SQL file in `controller/migrations/` (next: `037_`); never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- Provider tokens use the provider key and issuer only; a tenant JWT must never authenticate a provider route.
- The provider console is **read-only except** logout and operator management (super-admin). Don't modify `admin/`.
- Operator management must stay lock-out-proof: no self-disable/demote, never zero active super-admins, bootstrap-pinned accounts can't be disabled or demoted.
- Provider read APIs never return secrets (`encrypted_*`, keys, tokens, `enrollment_token_jti`, CA PEM bodies). Query services in `internal/providerquery/` don't import `net/http` (D-04).
- Proto changes (KI-2 option A only) are additive; never renumber fields.
- Respect the `path.md` conflict-zone order in `main.go`: M2-H → M2-U → M1-R3.

Sprint 20 (invariants still in force):
- Single controller instance (D-02): process-local notify, caches and registry are acceptable. Don't build cross-replica machinery.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal: no status write path may leave it.
- Don't change `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers, enrollment); suspension (D-07/D-08) is a later sprint.
- Relay/connectivity changes notify the transport plane, never the ACL (ADR-017).

Sprint 8 specific:
- Build gate passes before proceeding to next phase
- Policy mutations must invalidate the per-workspace ACL snapshot cache via `NotifyPolicyChange(workspace_id)`
- Missing ACL snapshot/resource/SPIFFE means default-deny
- Connector receives ACL snapshots via heartbeat piggyback; Controller is not in the tunnel hot path
- Client durable state is encrypted at rest in `state_store.rs`; decrypted private key and active access token live only in process/daemon memory during active use. See `.zecurity-obs/Decisions/ADR-002-Client-Daemon-Required.md`.

Sprint 6 and earlier:
- Build gate passes before proceeding to next phase
- Never change proto field numbers
- Check `Sprint5/path.md` conflict zone table before editing shared files
- `appmeta` constants must be identical in Go and Rust
- Shield heartbeats to Connector `:9091` only — never directly to Controller
- Shield validates `resource.host == detect_lan_ip()` before applying nftables
- nftables `chain resource_protect` always flushed + rebuilt atomically — never appended
- Resource instructions delivered via heartbeat piggyback only — no new RPCs

---

## Tool Usage

For any file search or grep in the current git-indexed directory, use the `fff` MCP tools (`ffgrep`, `fffind`, `fff-multi-grep`). They are faster, more token-efficient, and provide frecency-ranked results with git status annotations.
