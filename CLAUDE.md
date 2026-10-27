# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Relay (Rust), Client (Rust), Admin UI (React), Provider Console (React, new in Sprint 21).

**Sprint 22 is the active sprint: Provider Dashboard Phase 3, "Super-Admin TOTP and Relay Management".** Make the provider console a safe place to manage the relay fleet. Order: **T → A → {E, X} → D** (V alongside):
- **T — super-admin TOTP** (D-28, D-33; **merged in PR #138**): an operator requires TOTP if they are a super-admin or have TOTP enabled; `IssueSession` refuses a full session without `otp` for them and `RequireProvider` enforces it per request from the DB row; login and password change end in a confined MFA-stage token (`mfa: verify|enroll`); reset-totp (reason required); `zecurity-controller provider recover-admin` break-glass CLI; console `/mfa` page. **Live authenticator acceptance (AT-T) is pending (Sathiya).**
- **A — relay admin API hardening** (D-17, D-31): JSON error codes, mandatory reason + typed relay-name confirmation (server-checked) for revoke/delete, revoke/delete super-admin only;
- **E — relay enrolment from the console** (D-30, D-20): one-time provisioning token plus a complete install command;
- **X — relay revoke/delete in the console** (D-30, D-31, D-17), super-admin only;
- **D — relay drain and resume** (D-32): a drained relay leaves the published relay list; connectors migrate with **no connector or proto change**.
- **Not in Sprint 22:** tenant suspension (separate epic, D-34), relay pools/regions (D-03), recovery codes and refresh tokens (D-28), voluntary relay-ops TOTP (OQ-2), external IdPs (D-29).

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → **Decision record 2026-09-23** (D-01 … D-23), **amendment 2026-09-26** (D-24 … D-29) and **amendment 2026-10-26** (D-30 … D-34). The plan is in `.zecurity-obs/Sprint22/path.md` (decided open questions OQ-1 … OQ-3, checklist, test gates). This sprint introduces no architectural decisions beyond D-30 … D-34.

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, run by Postgres only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change, recreate the local DB: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members only.** **M1 = Sathiya** (testing: Sprint 22 acceptance runs per phase, AT-T … AT-D, on the lab). **M2 = Barath** (all Sprint 22 development: phases T, A, E, X, D in `Sprint22/Member2-Go-Rust/*`; see `.zecurity-obs/Sprint22/path.md` → Team Assignments).

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint22/path.md` — constraints, decided open questions, dependency graph, conflict zones, checklist and test gates
3. Read the phase file for their **first unchecked phase** where all `depends_on` items are done (Barath: `Sprint22/Member2-Go-Rust/Phase{1..5}-*.md` in the order T → A → {E, X} → D; Sathiya: the matching cases in `Sprint22/Acceptance-Test-Plan.md`, starting with AT-T)
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint22/path.md` | Constraints, decided open questions, dependency graph, conflict zones, checklist, test gates |
| `.zecurity-obs/Sprint22/Member2-Go-Rust/Phase*.md` | Detailed spec per phase (T, A, E, X, D) |
| `.zecurity-obs/Sprint22/Acceptance-Test-Plan.md` | Sprint acceptance cases (AT-CORE, AT-T, AT-A, AT-E, AT-X, AT-D) |
| `.zecurity-obs/Sprint21/path.md` | Previous sprint record (read-only provider console) |
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
cd controller && go tool gqlgen generate --config graph/gqlgen.yml   # GraphQL codegen (go generate ./graph/... does nothing: no directive)
cd admin && npm run codegen                                  # Frontend TS hooks
cd provider-console && npm run build                         # Provider console (npm test, npm run lint too)
cd controller && docker compose down -v && docker compose up -d   # Recreate local DB after any schema change
```

---

## Rules (non-negotiable)

Sprint 22 specific:
- Build gate passes before proceeding to the next phase (DB-backed tests must run, not skip; a skipped DB test is a failed case).
- The Decision Record (2026-09-23, amendments 2026-09-26 and 2026-10-26) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code.
- **Order:** T → A → {E, X} → D. Don't start a phase before its dependencies are merged. No console relay mutation ships before TOTP (D-33; T is merged).
- **No migration framework.** A schema change is a new numbered SQL file in `controller/migrations/` (next: `043_`, reserved for Phase D `043_relay_drain.sql`); never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- **Console writes (D-30):** auth calls (incl. the TOTP steps), operator management (incl. reset-totp), and relay create / revoke / delete / drain / resume. Everything else stays read-only. Every new write goes into `api-surface.test.ts` on purpose. Don't modify `admin/`.
- **Relay roles (D-31):** relay-ops may create, drain and resume relays; **revoke and delete are super-admin only**, enforced server-side in `decide()`. Console visibility is never the authorization control.
- **D-17:** relay revoke/delete need a mandatory reason (in the body, never the URL; stored in the audit) and a typed relay-name confirmation, checked by the server.
- **Drain (D-32):** drain = exclusion from the published relay list. Keep the last-eligible-relay guard (race-safe), audit, unchanged connector migration and the 120 s connector drain timeout. **No connector, relay or proto change.** Draining reduces relay availability; the console says so before confirming.
- **TOTP (Phase T):** never add a path that mints a full session outside `IssueSession`, and never let a super-admin (or an operator with TOTP enabled) act without `otp` in `amr`. TOTP secrets are sealed with `pki.EncryptSecret` and never logged, audited or returned after enrolment. No grace periods, no self-service recovery bypass; recovery is reset-totp (another super-admin, reason required) or `recover-admin` on the controller host.
- Provider tokens use the provider key and issuer only; a tenant JWT must never authenticate a provider route.
- Operator management must stay lock-out-proof: no self-disable/demote/reset (password or TOTP), never zero active super-admins. Bootstrap is **create-only**.
- Provider Identity is an **internal controller module** (`controller/internal/provider`, ADR-029), not a separate service.
- Never log, audit or return passwords (incl. temporary ones), `password_hash`, TOTP secrets or codes. Provider login and TOTP verification stay rate-limited (fail closed) and must not reveal whether an account exists.
- Provider read APIs never return secrets (`encrypted_*`, keys, tokens, `enrollment_token_jti`, CA PEM bodies). Query services in `internal/providerquery/` don't import `net/http` (D-04). A provisioning token is returned once (`Cache-Control: no-store`) and never stored in the console.
- Proto changes are additive only; never renumber fields (none are planned this sprint).
- Respect the `Sprint22/path.md` conflict-zone order in `main.go`: T → A → E → D.

Sprint 20 (invariants still in force):
- Single controller instance (D-02): process-local notify, caches and registry are acceptable. Don't build cross-replica machinery.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal: no status write path may leave it.
- Don't change `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers, enrollment); tenant suspension (D-07/D-08) is a separate epic (D-34), not Sprint 22.
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
