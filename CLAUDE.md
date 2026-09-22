# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Admin UI (React).

**Sprint 19 is the active sprint.** Two independent tracks share the number:

- **Track A — PENDING-16, Resource Policy → Device Profile Binding.** Solo, Member02. Introduces `Resource → Resource Policy → Device Profile(s)` as the authorization path, replacing the direct `resource_profile_bindings` model and retiring `device_profiles.mode` from authorization. **Phases 1–7 complete; Phase 8 (Linux end-to-end) is next.** Plan: `.zecurity-obs/Sprint19/path.md` (Track A), phases in `Sprint19/Member02/`.
- **Track B — PENDING-13, Client Device Lifecycle.** Owner Member2-Go, phases in `Sprint19/Member2-Go/`. Tracks 1–3 done.

> **Branch note.** Track A lives on `pending-16`, not `fixed-pendings`. Sprints 1–18 are complete; **do not** trust the sprint-by-sprint tables in `.zecurity-obs/Home.md` or `Planning/Roadmap.md` — both are frozen at Sprint 6. The accurate status index is `.zecurity-obs/pending/README.md`.

---

## Your First Step

When a team member starts a session, they will tell you their member number (M1, M2, M3, or M4). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint19/path.md` — it carries **both** Sprint 19 tracks; Track A (PENDING-16) is Member02's, Track B (PENDING-13) is Member2-Go's
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are checked
4. **Check for "Post-Phase Fixes" / "Implementation" sections** in the phase file — completed phases record what was built, the decisions taken, and any gaps deliberately left
5. Brief them: what they're building, which files to touch, and the build check command

If they don't give you a member number, ask: *"Which team member are you? (M1 Frontend / M2 Go / M3 Go+Rust / M4 Rust)"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint19/path.md` | Dependency map + progress tracker, both tracks |
| `.zecurity-obs/Sprint19/Member02/Phase*.md` | Track A spec per phase, with an Implementation record once done |
| `.zecurity-obs/pending/README.md` | **The accurate status index** — which PENDING items are built |
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
```

---

## Rules (non-negotiable)

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
