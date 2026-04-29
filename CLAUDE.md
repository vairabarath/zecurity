# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Admin UI (React).

**Sprint 7 is the active sprint.** Building Client Application (Phase 1) — Admin invites users via email, client CLI login with Google OAuth, device enrollment with mTLS certificate, status command, role-based routing in Admin UI (admin → dashboard, member → client-install).

---

## Your First Step

When a team member starts a session, they will tell you their member number (M1, M2, M3, or M4). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint7/path.md` — dependency map and progress checkboxes
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are checked
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't give you a member number, ask: *"Which team member are you? (M1 Frontend / M2 Go / M3 Go+Rust / M4 Rust)"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint7/path.md` | Dependency map + progress tracker (checkboxes) |
| `.zecurity-obs/Sprint7/Member{N}-*/Phase*.md` | Detailed spec per phase |
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

Sprint 7 specific:
- Build gate passes before proceeding to next phase
- ClientService uses plain TLS + JWT Bearer (no mTLS for client yet)
- Reuse existing PKI (`pki.Service.SignCSR()`) and OAuth (`auth/exchange.go`)
- CLI state in memory only — tokens, cert, key never written to disk

Sprint 6 and earlier:
- Build gate passes before proceeding to next phase
- Never change proto field numbers
- Check `Sprint5/path.md` conflict zone table before editing shared files
- `appmeta` constants must be identical in Go and Rust
- Shield heartbeats to Connector `:9091` only — never directly to Controller
- Shield validates `resource.host == detect_lan_ip()` before applying nftables
- nftables `chain resource_protect` always flushed + rebuilt atomically — never appended
- Resource instructions delivered via heartbeat piggyback only — no new RPCs
