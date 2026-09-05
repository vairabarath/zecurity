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
| Admin UI | React | `admin/` | dev :5173 |

**Sprint 17 is active:** SCIM Directory Synchronization (ADR-025) — **solo M1 sprint** building the SCIM identity engine (schema, SCIM token auth, break-glass permission, provider profiles + mapping, Users provision/update/deprovision, Groups, identity-conflict workflow, connection lifecycle + health + sync). **Sprint 18 (Durable Outbox, PENDING-15) is MERGED** into `fixed-pendings` (`internal/outbox/*` + `migrations/033_outbox_events.sql`); SCIM consumes it via `outbox.Enqueue` — it is not built in Sprint 17.

---

## First Action Every Session

The human will tell you their member number. Do this immediately:

```
Step 1: Read agent.md             → full project conventions
Step 2: Read .zecurity-obs/Sprint17/path.md  → dependency map + checkboxes (reconciled: solo M1, outbox = Sprint 18)
Step 3: Find first unchecked phase for this member where all depends_on are ✅
Step 4: Read that phase file      → exact spec, files, build check
Step 5: Check for "Post-Phase Fixes" section in the phase file → apply any fixes listed there
Step 6: Brief the human: "Here's what you're building today..."
```

---

## Authoritative Files

- **`agent.md`** — conventions, code style, env vars, release process
- **`.zecurity-obs/Sprint17/path.md`** — ordered execution with checkboxes (source of truth for what's done; reconciled solo-M1 plan, outbox already merged as Sprint 18)
- **`.zecurity-obs/Sprint17/Member1-Go/Phase*.md`** — per-phase implementation specs (M1 only; `Member2-Go/*` retained as historical pointers to Sprint 18)
- **`.zecurity-obs/Services/*.md`** — service documentation (read before touching a subsystem)

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

Two proto files exist (both at repo root under `proto/`):
- `proto/connector/v1/connector.proto` — Connector ↔ Controller
- `proto/shield/v1/shield.proto` — Shield ↔ Connector + Shield ↔ Controller

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
- Migration numbering follows the merged tree: SCIM = `034` (next free slot after `030`/`031`/`032`/`033`).
  Never author `outbox_events` in a SCIM migration — it lives in `033_outbox_events.sql` (Sprint 18).

---

## End of Session

Before ending, always:
1. Mark completed phase checkboxes in `.zecurity-obs/Sprint17/path.md`
2. Update the phase file frontmatter `status: done`
3. Append entry to `.zecurity-obs/Planning/Session Log.md`
