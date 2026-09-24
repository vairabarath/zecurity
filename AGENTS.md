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

**Sprint 20 is active: Provider Dashboard Phase 1, "Backend Truth".** The goal is that controller state is true before any provider dashboard reads it:
- relay liveness (no false `inactive`);
- terminal connector/shield revocation;
- relay SAN allowlist enforcement;
- the disconnect watcher notifies the transport plane;
- controller gRPC cert rotation;
- relay in-band `RenewCert` (D-19);
- connector renewal trigger plus cert hot-swap.

The source of truth is `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"**. The sprint introduces no architectural decisions and no migrations.

**Team: two members.** **M1 = Sathiya** (Go), **M2 = Barath** (Go + Rust). There is no M3/M4.

---

## First Action Every Session

The human will tell you who they are (Sathiya / M1 or Barath / M2). Do this immediately:

```
Step 1: Read agent.md             → full project conventions
Step 2: Read .zecurity-obs/Sprint20/path.md  → dependency map, conflict zones, checkboxes
Step 3: Find first unchecked phase for this member where all depends_on are ✅
        (Sathiya → Sprint20/Member1-Go/*, Barath → Sprint20/Member2-Go-Rust/*)
Step 4: Read that phase file      → exact spec, files, invariants, tests, build check
Step 5: Check for "Post-Phase Fixes" section in the phase file → apply any fixes listed there
Step 6: Brief the human: "Here's what you're building today..."
```

---

## Authoritative Files

- **`agent.md`** — conventions, code style, env vars, release process
- **`docs/provider-dashboard-architecture-decisions.md`** — provider dashboard Decision Record (D-01 … D-23); binding for Sprint 20 and later provider-dashboard work
- **`docs/provider-dashboard-architecture-discovery.md`** — factual current-architecture report (evidence and line references)
- **`.zecurity-obs/Sprint20/path.md`** — ordered execution with checkboxes (source of truth for what's done)
- **`.zecurity-obs/Sprint20/Member{1-Go,2-Go-Rust}/Phase*.md`** — per-phase implementation specs
- **`.zecurity-obs/Sprint20/Acceptance-Test-Plan.md`** — acceptance cases; a skipped DB test counts as failed
- **`.zecurity-obs/Services/*.md`** — service documentation (read before touching a subsystem)

## Sprint 20 Rules

- If a phase seems to need a new architectural choice or a migration, **stop and ask**.
- Single controller instance (D-02): process-local notifications/caches are acceptable.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal. No status write may leave it.
- Don't touch `workspaces.status='active'` predicates; suspension (D-07/D-08) is a later sprint.
- Relay/connectivity changes notify the transport plane, never the ACL (ADR-017).
- Conflict-zone order (`path.md`): M2-A → M1-D → M1-E in `main.go`; M1-B before M2-G in `control_stream.go`.

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
- `proto/relay/v1/relay.proto` — Relay ↔ Controller (Sprint 20 adds `RenewCert`, additive only)
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
- Migration numbering follows the merged tree. The latest migration is `036_client_device_pubkey_fingerprint.sql`,
  so the next free number is `037`. Sprint 20 adds **no** migrations.

---

## End of Session

Before ending, always:
1. Mark completed phase checkboxes in `.zecurity-obs/Sprint20/path.md` (and in the phase file)
2. Update the phase file frontmatter `status: done`
3. Append entry to `.zecurity-obs/Planning/Session Log.md`
