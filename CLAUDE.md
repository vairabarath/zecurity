# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Relay (Rust), Client (Rust), Admin UI (React), Provider Console (React, new in Sprint 21).

**Sprint 24 is the active sprint: Provider Dashboard, monitoring and alerting (F23, F26; F27 stretch).** Starts the Can-have list (Must have is 18 / 18 since Sprint 23). Decisions **D-48 … D-54** are made (Decision Record amendment **2026-11-20**); the Sprint 24 documents were **signed off by the product owner (Barath) on 2026-11-20**; implementation may start in the phase order below.
- **R1 — per-workspace relay counters (F23, D-54)**, Barath: the relay reports cumulative bytes per direction and active streams **per trust domain** in the heartbeat (**additive** proto field 13, `WorkspaceTraffic`); the controller maps trust domain → workspace (unknown/revoked ignored, never stored); older relays read "Not reported".
- **M1 — metric store + read API (F23, D-49)**, Barath: Postgres, 1-min samples kept 7 days, hourly roll-ups kept 400 days, gaps never zero-filled; `GET /provider/metrics/{series}`; per-tenant series super-admin only (D-40 extended).
- **M2 — console charts (F23)**, Barath, after M1: relay detail, tenant detail, Overview sparkline (1 h / 24 h / 7 d / 30 d).
- **X1 — Sprint 23 follow-ups**, Sathiya: client quota message; SCIM mapping probe exempt from the users quota (one probe at a time, no enrollment rights; real creates get a clear `quota_exceeded` SCIM error); SCIM refused with 403 while suspended; the S4 export amendment is confirmed (D-46 refinement, 2026-11-20).
- **A1 — alert rules + evaluator (F26, D-50, D-53)**, Sathiya: **platform** vs **tenant** scope separate in the schema (tenant rows need `tenant_id`, CHECK); built-in rule kinds only; firing → resolved with `for` and re-notify; read errors never resolve.
- **A2 — delivery (F26, D-52)**, Sathiya, after A1: one generic versioned JSON webhook (no vendor SDKs), HMAC over timestamp + body (`X-Zecurity-Signature`), sealed secret shown once, HTTPS outside dev, SSRF guard at connect time; email via the F14 sender; outbox retries.
- **A3 — Alerts console (F26)**, Sathiya, after A2. **T1 — incident timeline (F27)**, stretch.
- **Not in Sprint 24:** F22 tunnel success rate, tenant-facing alert delivery (later: admin UI first, tenant webhooks only on demand), vendor integrations, F32/F33 metering, F34 billing, F28/F29, F19, F31, F35, F37, F39, F40.

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → decision record 2026-09-23 (D-01 … D-23) and amendments 2026-09-26 (D-24 … D-29), 2026-10-26 (D-30 … D-34), 2026-11-11 (D-35, D-36), 2026-11-12 (D-37 … D-41), 2026-11-17 (D-42 … D-47; amends D-07, D-11, D-17, D-30, D-31; preserves D-08) and **2026-11-20 (D-48 … D-54; amends D-30, extends D-40)**. The plan is `.zecurity-obs/Sprint24/path.md` (decisions summary, order, conflict zones, rules, test gates, acceptance).

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, run by Postgres only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change, recreate the local DB: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members only, both developing in Sprint 24.** **M1 = Sathiya** (Person 2): X1 follow-ups, A1 rules + evaluator, A2 delivery, A3 console, T1 (stretch) — `Sprint24/Member2-Sathiya/Phases-X1-A1-A3-Alerts.md`. **M2 = Barath** (Person 1, product owner): R1 relay counters, M1 metric store, M2 charts — `Sprint24/Member1-Barath/Phases-R1-M1-M2-Timeseries.md`. Both review each other's PRs and run the sprint acceptance on the lab.

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint24/path.md` — decisions (D-48 … D-54), order, conflict zones, rules, test gates and acceptance
3. Read their phase file and pick the **first unchecked phase** whose dependencies are merged (Barath: R1 and M1 in parallel → M2; Sathiya: X1 → A1 → A2 → A3 → T1 stretch). If the documents aren't signed off yet (`path.md` checklist), don't start implementation
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint24/path.md` | Active sprint: decisions summary, phases and order, conflict zones, rules, test gates, acceptance, checklist |
| `.zecurity-obs/Sprint24/Member1-Barath/Phases-R1-M1-M2-Timeseries.md` | Barath's phases R1, M1, M2 (F23) |
| `.zecurity-obs/Sprint24/Member2-Sathiya/Phases-X1-A1-A3-Alerts.md` | Sathiya's phases X1, A1, A2, A3, T1 (F26, F27) |
| `.zecurity-obs/Sprint24/Scope-Draft.md` | Sprint 24 scope discussion (signed off 2026-11-20) |
| `.zecurity-obs/Sprint23/path.md` | Previous sprint record (plans, headroom, tenant lifecycle, quotas; Post-Sprint Fixes) |
| `.zecurity-obs/Planning/Provider-Dashboard-Features.md` | Feature tracker (tick only when merged **and** live-checked) |
| `.zecurity-obs/Planning/Pending-Live-Tests.md` | Live-check steps and results log for merged features |
| `.zecurity-obs/Planning/Relay-Pools-Implementation-Plan.md` | Relay pools record (D-37 … D-41, F11-1/F11-2 fixes) |
| `.zecurity-obs/Sprint22/path.md` | Previous sprint record (TOTP, relay management, drain) |
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

Sprint 24 specific:
- Build gate passes before proceeding to the next phase (DB-backed tests must run, not skip; a skipped DB test is a failed case).
- The Decision Record (all amendments through **2026-11-20**) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code.
- No phase before its dependencies are merged (M1 before M2; A1 before A2 before A3).
- **No commit, push, PR or merge without the product owner's explicit go** for that step.
- **Proto (R1, D-54):** additive only — `repeated WorkspaceTraffic workspace_traffic = 13`; never renumber; an older relay must keep working and read "Not reported" (never 0).
- **Alert scopes (D-50):** platform and tenant alerts are separate in the schema; tenant rows always carry `tenant_id` (DB CHECK); tenant-scoped reads always filter by it. Relay-ops never see tenant names, tenant alerts or per-tenant series (D-40 extended).
- **Webhooks (D-52):** one generic versioned JSON payload, no vendor SDKs; HMAC over timestamp + body; secret sealed, shown once, never logged/audited/returned; HTTPS outside dev; SSRF guard checked at connect time; no secrets or tenant content in the payload.
- **Rules and channels (D-53):** super-admin only, reason + before/after audit; built-in rule kinds, no query language.
- **Metrics (D-49):** Postgres, 1-min 7 days, hourly 400 days; an unknown value is a gap, never 0. Evaluator read errors never resolve an alert.
- **No migration framework.** Reserved: `068_metric_samples` (M1), `069_alerts` (A1/A2). Earlier: `062_` … `067_` (Sprint 23); check the latest with `ls controller/migrations | tail -3` before adding one. Never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- **Console writes (D-30, amended by D-42 … D-47 and D-53):** auth calls (incl. TOTP), operator management, relay create / revoke / delete / drain / resume / move / pools, tenant pool assignment, plans and plan assignment, relay link capacity, tenant create / suspend / resume / delete / cancel delete / legal hold, quota overrides, **alert rules and alert channels (create, update, enable/disable, delete, test, rotate secret)**. Every new write goes into `api-surface.test.ts` on purpose. `admin/` may change only where a phase file says so.
- **Tenant actions are super-admin only (D-06)**, plans too; relay link capacity is relay-ops or super-admin (D-31 amended). Enforced server-side in `decide()`; console visibility is never the authorization control.
- **D-17 (extended):** relay revoke/delete and **tenant suspend/delete** need a mandatory reason (in the body, never the URL; stored in the audit) and a typed name/slug confirmation, checked by the server. Tenant-affecting writes (plan assignment, resume, cancel, quota override, legal hold) need a reason. Audit with before/after (F17).
- **D-08 holds:** suspension never refuses or invalidates an existing connector's or shield's authentication; suspended agents get a deny-all ACL and resume without re-enrollment.
- **Quota checks (D-47)** are server-side on every creation path and atomic with the creation (same transaction, serialised per workspace); lookup errors fail closed. Lowered limits never delete resources.
- **Relay pools (D-37 … D-41, F11-2):** the effective pool has one definition (`relaypool.EffectivePool`) used by both relay-list delivery paths and the tenant read; never re-derive it.
- **Drain (D-32):** drain = exclusion from the published relay list; keep the last-eligible-relay guard (race-safe), audit, unchanged connector migration and the 120 s connector drain timeout.
- **TOTP:** never add a path that mints a full session outside `IssueSession`, and never let a super-admin (or an operator with TOTP enabled) act without `otp` in `amr`. TOTP secrets are sealed with `pki.EncryptSecret` and never logged, audited or returned after enrolment. Recovery is reset-totp (another super-admin, reason required) or `recover-admin` on the controller host.
- Provider tokens use the provider key and issuer only; a tenant JWT must never authenticate a provider route.
- Operator management must stay lock-out-proof: no self-disable/demote/reset (password or TOTP), never zero active super-admins. Bootstrap is **create-only**.
- Provider Identity is an **internal controller module** (`controller/internal/provider`, ADR-029), not a separate service.
- Never log, audit or return passwords (incl. temporary ones), `password_hash`, TOTP secrets or codes, invitation tokens or key material. Provider login and TOTP verification stay rate-limited (fail closed) and must not reveal whether an account exists.
- Provider read APIs never return secrets (`encrypted_*`, keys, tokens, `enrollment_token_jti`, CA PEM bodies). Query services in `internal/providerquery/` don't import `net/http` (D-04). A provisioning token is returned once (`Cache-Control: no-store`) and never stored in the console.
- Proto changes are additive only; never renumber fields (Sprint 24: R1 field 13 only).
- Respect the `Sprint24/path.md` conflict zones (`relay.proto`, relay heartbeat handler, shared relay reader for M1/A1, `main.go`, `authz.go`, `api-surface.test.ts`, `Home.tsx`).

Sprint 20 (invariants still in force):
- Single controller instance (D-02): process-local notify, caches and registry are acceptable. Don't build cross-replica machinery.
- `revoked` (and connector `revoked_at IS NOT NULL`) is terminal: no status write path may leave it.
- Don't change `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers, enrollment, `WorkspaceGuard`'s status query). Tenant suspension (Sprint 23, D-45) is represented by `suspended_at` precisely so these stay unchanged (D-34).
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
