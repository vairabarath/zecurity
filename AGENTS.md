# Zecurity — Agent Context (Codex / OpenCode / Kilo / Gemini CLI)

> Load this file at session start. It is the entry point for all AI agents.

---

## Project Summary

**Zecurity** — Zero Trust Network Access platform.

| Component | Lang | Location | Port |
|-----------|------|----------|------|
| Controller | Go | `controller/` | HTTP :8080, gRPC :9090 |
| Connector | Rust | `connector/` | heartbeat to :9090, Shield server :9091 |
| Shield | Rust | `shield/` | heartbeats to Connector :9091 |
| Relay | Rust | `relay/` | QUIC :9093; heartbeat to :9090 |
| Client | Rust | `client/` | CLI + daemon; gRPC to :9090 |
| Admin UI | React | `admin/` | dev :5173 |
| Provider Console | React | `provider-console/` (Sprint 21) | dev :5174 |

**Sprint 21 is active: Provider Dashboard Phase 2, "Dedicated Provider Console: Login, Roles, Read-Only Fleet View".** A dedicated provider console with hardened login and a working role system, then read-only views of the state Sprint 20 made true. Order: H → C → U → R → P (K, V alongside):
- provider identity foundation (Decision Record amendment 2026-09-26, D-24…D-29): a Zecurity-owned Provider Identity Service (pluggable `Authenticator`, single token-minting path, `amr` claim); first method is email + password with Argon2id, rate-limited login, forced first password change, create-only bootstrap; dedicated provider signing key and issuer, `session_generation`, logout (D-25). **no direct Google OAuth for providers** (external IdPs may plug in later, D-29); tenant Google/OIDC is unchanged. **Mandatory TOTP for super-admin before Sprint 22** (D-28);
- a dedicated provider console, `provider-console/` (Vite + React, D-18): email + password login, change password, logout, role-aware access (`super-admin` / `relay-ops`);
- operator management (D-27): super-admins add operators, change roles, disable, re-enable and reset passwords (one-time temporary passwords; audited; lock-out-proof);
- provider read APIs under `/provider/*` (D-04/D-05/D-06/D-15): relays, tenants, provider audit, certificate expiry;
- read-only console pages for relays, tenants, audit and certificate health;
- Sprint 20 cleanup: KI-1, KI-2, KI-3, JWTs removed from `.env.example`;
- Sprint 20 live verification (`docs/sprint20-live-acceptance-runbook.md`).

The source of truth is `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"**. The plan is `.zecurity-obs/Sprint21/path.md`. The sprint introduces no architectural decisions.

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, which Postgres runs only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members.** **M1 = Sathiya** (Go + React: live verification, provider console foundation + Provider users page + read pages, provider read APIs), **M2 = Barath** (Go + Rust: provider identity foundation, operator management API, Sprint 20 cleanup, database development guide). There is no M3/M4.

---

## First Action Every Session

The human will tell you who they are (Sathiya / M1 or Barath / M2). Do this immediately:

```
Step 1: Read agent.md             → full project conventions
Step 2: Read .zecurity-obs/Sprint21/path.md  → development rule, dependency map, conflict zones, open questions, checkboxes
Step 3: Find first unchecked phase for this member where all depends_on are ✅
        (Sathiya → Sprint21/Member1-Go/*, Barath → Sprint21/Member2-Go-Rust/*)
Step 4: Read that phase file      → exact spec, files, invariants, tests, build check
Step 5: Check for "Post-Phase Fixes" section in the phase file → apply any fixes listed there
Step 6: Brief the human: "Here's what you're building today..."
```

---

## Authoritative Files

- **`agent.md`** — conventions, code style, env vars, release process
- **`docs/provider-dashboard-architecture-decisions.md`** — provider dashboard Decision Record (D-01 … D-23); binding for Sprint 20 and later provider-dashboard work
- **`docs/provider-dashboard-architecture-discovery.md`** — factual current-architecture report (evidence and line references)
- **`.zecurity-obs/Sprint21/path.md`** — ordered execution with checkboxes (source of truth for what's done)
- **`.zecurity-obs/Sprint21/Member{1-Go,2-Go-Rust}/Phase*.md`** — per-phase implementation specs
- **`.zecurity-obs/Sprint21/Acceptance-Test-Plan.md`** — acceptance cases; a skipped DB test counts as failed
- **`docs/database-development.md`** — local database workflow (schema files, reset rule); lands with Sprint 21 DEV-1
- **`docs/sprint20-live-acceptance-runbook.md`**, **`.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md`** — Sprint 20 live verification (Sprint 21 Phase V)
- **`.zecurity-obs/Sprint20/path.md`** — Sprint 20 record, incl. **Known Issues** KI-1 … KI-3
- **`.zecurity-obs/Services/*.md`** — service documentation (read before touching a subsystem)

## Sprint 21 Rules

- If a phase seems to need a new architectural choice, **stop and ask**. Answer OQ-1/OQ-2 (`path.md`) before the tenant read endpoints.
- **No migration framework.** A schema change is a new numbered SQL file (next: `037_`); never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- Provider tokens use the provider key and issuer only; a tenant JWT must never authenticate a provider route.
- The provider console is **read-only except** logout and operator management (super-admin). Don't modify `admin/`.
- Operator management must stay lock-out-proof: no self-disable/demote/reset, never zero active super-admins. Break-glass recovery is `PROVIDER_BOOTSTRAP_RESET` (Phase H).
- Never log, audit or return passwords (incl. temporary ones) or `password_hash`. Provider login must stay rate-limited and must not reveal whether an account exists.
- Provider read APIs never return secrets (`encrypted_*`, keys, tokens, `enrollment_token_jti`, CA PEM bodies); `internal/providerquery/` doesn't import `net/http` (D-04).
- Conflict-zone order in `main.go` (`path.md`): M2-H → M2-U → M1-R3.

## Sprint 20 invariants (still in force)

- Single controller instance (D-02): process-local notifications/caches are acceptable.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal. No status write may leave it.
- Don't touch `workspaces.status='active'` predicates; suspension (D-07/D-08) is a later sprint.
- Relay/connectivity changes notify the transport plane, never the ACL (ADR-017).

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
```rust
// BEFORE:
<old code>

// AFTER:
<new code>
```
```

**Important:** Add fixes to the phase file where the original implementation was done, not just to path.md. This ensures the phase file contains all knowledge about that implementation.

---

## Proto Convention

Four proto files exist (all at repo root under `proto/`):
- `proto/connector/v1/connector.proto` — Connector ↔ Controller
- `proto/shield/v1/shield.proto` — Shield ↔ Connector + Shield ↔ Controller
- `proto/relay/v1/relay.proto` — Relay ↔ Controller (includes `RenewCert`, added in Sprint 20)
- `proto/client/v1/client.proto` — Client ↔ Controller

Never renumber or reuse proto fields.

**Run from repo root:** `buf generate` → Go stubs land in `controller/gen/go/proto/`

Rust stubs are generated automatically via `build.rs` in each crate.

---

## Branch Workflow

- **`fixed-pendings`** is the integration branch where finished sprints land and where cross-sprint
  reconciliations are committed (e.g. ca84341 reconciled Sprint 17 to the already-merged durable outbox).
  Treat it as the reconciled source of truth for sprint docs.
- Feature branches (e.g. `mnemosyne`) are **fast-forwarded** onto `origin/fixed-pendings` once it moves
  ahead — they carry no unique commits of their own, so a plain `git merge --ff-only origin/fixed-pendings`
  is the correct "rebase" (no commit replay, no rebase of unique history).
- Before fast-forwarding, stash/shelve in-flight work. **Keep implementation code** (e.g. `internal/scim/`,
  `migrations/034_scim_directory_sync.sql`) but **discard stale sprint-plan doc edits** that the
  reconciliation already supersedes.
- Schema file numbering follows the merged tree. The latest file in `controller/migrations/` is
  `036_client_device_pubkey_fingerprint.sql`, so the next free number is `037` (Sprint 21 Phase H uses it). These are
  plain SQL files run by Postgres on first volume creation. There is no migration framework; see `docs/database-development.md`.

---

## End of Session

Before ending, always:
1. Mark completed phase checkboxes in `.zecurity-obs/Sprint21/path.md` (and in the phase file)
2. Update the phase file frontmatter `status: done`
3. Append entry to `.zecurity-obs/Planning/Session Log.md`
