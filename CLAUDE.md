# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Admin UI (React).

**Sprint 20 is the active sprint: Provider Dashboard Phase 1, "Backend Truth".** Before any provider dashboard reads controller state, make that state true:
- relay liveness (no false `inactive`);
- connector/shield revocation is terminal;
- relay SAN allowlist enforcement;
- the disconnect watcher notifies the transport plane;
- controller gRPC cert rotation;
- relay in-band `RenewCert` (D-19);
- connector renewal trigger plus cert hot-swap.

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"** (D-01 … D-23). Evidence: `docs/provider-dashboard-architecture-discovery.md`. This sprint introduces **no** architectural decisions and **no** migrations.

**Team: two members only.** **M1 = Sathiya** (Go: lifecycle guards, disconnect watcher, controller cert rotation). **M2 = Barath** (Go + Rust: relay liveness, SAN allowlist, relay renewal, connector renewal).

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint20/path.md` — dependency map, conflict zones and progress checkboxes
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are checked (`Sprint20/Member1-Go/*` for Sathiya, `Sprint20/Member2-Go-Rust/*` for Barath)
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint20/path.md` | Dependency map + conflict zones + progress tracker (checkboxes) |
| `.zecurity-obs/Sprint20/Member{N}-*/Phase*.md` | Detailed spec per phase |
| `.zecurity-obs/Sprint20/Acceptance-Test-Plan.md` | Sprint acceptance cases (AT-CORE, AT-A … AT-G) |
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
```

---

## Rules (non-negotiable)

Sprint 20 specific:
- Build gate passes before proceeding to next phase (DB-backed tests must run, not skip)
- The Decision Record (`docs/provider-dashboard-architecture-decisions.md`, 2026-09-23) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code.
- Single controller instance (D-02): process-local notify, caches and registry are acceptable. Don't build cross-replica machinery.
- No migrations this sprint. Proto change is additive only (`RelayService.RenewCert`) plus comment fixes; never renumber fields.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal: no status write path may leave it.
- Don't change `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers, enrollment); suspension (D-07/D-08) is a later sprint.
- Relay/connectivity changes notify the transport plane, never the ACL (ADR-017).
- Respect the `path.md` conflict-zone order: M2-A before M1-D before M1-E in `main.go`; M1-B before M2-G in `control_stream.go`.

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
