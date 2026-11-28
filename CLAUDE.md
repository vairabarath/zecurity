# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Relay (Rust), Client (Rust), Admin UI (React), Provider Console (React, new in Sprint 21).

**Sprint 27 is the active sprint: provider access and API (F19, F20, F35).** Started 2026-11-27. Decisions **D-71 … D-76** are made (Decision Record amendment **2026-11-23, provider access**). The documents were signed off on 2026-11-23 by Sathiya (as for Sprint 26); Barath reviews them in PR #252. Sprint 26 (F34) is **merged and live-checked** (acceptance PASS 2026-11-26, #260 `7ee717bc`); F34 is ticked. Sprint 27 adds roles, loud break-glass and read-only API keys: **no agent change, no external IdP, no impersonation, no keys that write.** **Every phase is merged (2026-11-28, #262 → #267, `main` `8db309bf`) at Sathiya's go, before Barath's review. Acceptance run PASS 2026-11-28 (`main` `b6521928`); F19 ticked. Two decisions from the run (Decision Record amendment 2026-11-28): **D-77** every break-glass use raises the critical alert even if acknowledged at once (the ack resolves it) and **D-78** disabling or demoting a super-admin permanently revokes the API keys they created. Left: fixes **X5** (D-77) and **X6** (D-78) with live rechecks, then tick F20 and F35; Barath's review of the open points in `Sprint27/path.md`. Migrations `075_` … `077_` are on `main`: recreate your local DB.**
- **X4 — Sprint 26 follow-ups**, Sathiya: **done** — the acceptance run's one finding (a purged tenant's final invoice) was fixed in #260.
- **O1 — role matrix + roles (D-72, D-73, D-74)**, Barath (**built by Sathiya, takeover**; live PASS; **merged #262**), merged first (the contract with K1 and O2):
  - `decide()` becomes an explicit role → actions table; existing roles keep exactly today's permissions; an action with no entry is denied to every role except super-admin;
  - new roles `auditor` (all reads), `billing` (cards, billing settings, invoices, usage, tenant list, plan assignment), `support` (tenant reads, the tenant's alerts and timeline, health); TOTP mandatory for all three; `075_provider_roles`.
- **G1 — loud break-glass (D-75)**, Barath (**built by Sathiya, takeover**; live PASS; **merged #263**), after O1: `recover-admin --reason` on the controller host is the only path; each use raises a critical `provider.break_glass_used` alert, emails every super-admin and shows a console banner until acknowledged; `077_provider_alert_kinds` (also holds K1's `api_key.expiring`).
- **K1 — provider API keys (D-76)**, Sathiya (live PASS; **merged #264**), after O1 (expiry alert after `077_`): `zpk_<id>_<secret>`, SHA-256 at rest, shown once; one role per key (never super-admin); read-only; expires ≤ 365 days; optional CIDR allowlist and rate limit; never a session; `076_provider_api_keys`.
- **K2 — keys console**, Sathiya (live PASS; **merged #266**), after K1: create (show once), list (prefix only), revoke with a reason.
- **O2 — console per role**, Sathiya (live PASS; **merged #267**, with the break-glass banner), after O1: navigation and write buttons follow the matrix (UX only; the server decides).
- **Not in Sprint 27:** agent changes, external IdPs (D-29), impersonation or support grants (F31), partner scoping (F37), several roles per operator, step-up beyond today's TOTP, keys that write, OAuth/mTLS for the API, F22, F28, F29, F36, F38, F39, F40.

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → decision record 2026-09-23 (D-01 … D-23) and amendments 2026-09-26 (D-24 … D-29), 2026-10-26 (D-30 … D-34), 2026-11-11 (D-35, D-36), 2026-11-12 (D-37 … D-41), 2026-11-17 (D-42 … D-47; amends D-07, D-11, D-17, D-30, D-31; preserves D-08) **2026-11-20 (D-48 … D-54; amends D-30, extends D-40)** , **2026-11-21 (D-55: relay-ops see relay and pool operation audit rows in the incident timeline; extends D-40)** , **2026-11-21, usage (D-56 … D-61; extends D-40 and D-50, amends D-46)** **2026-11-23, billing (D-62 … D-70; extends D-43, D-40/D-60, amends D-30, D-46)** and **2026-11-23, provider access (D-71 … D-76; extends D-26, D-28, D-50; amends D-05, D-06, D-30, D-31, D-68/D-70)** and **2026-11-28, break-glass alert and API key creators (D-77, D-78; clarifies D-75, extends D-76)**. The plan is `.zecurity-obs/Sprint27/path.md` (decisions summary, order, conflict zones, rules, test gates, acceptance).

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, run by Postgres only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change, recreate the local DB: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members only, both developing in Sprint 27.** **M1 = Sathiya** (Person 2): X4 follow-ups (done), K1 API keys, K2 keys console, O2 console per role — `Sprint27/Member2-Sathiya/Phases-X4-K1-K2-O2-API-Keys.md`. **M2 = Barath** (Person 1, product owner): O1 role matrix + roles, G1 loud break-glass — `Sprint27/Member1-Barath/Phases-O1-G1-Roles-Break-Glass.md`. Both review each other's PRs and run the sprint acceptance on the lab.

---

## Sync with `origin/main` before building (every agent, every session)

Before you build, fix or change anything, bring the checkout up to date with `origin/main`. Nobody may start a feature on a stale `main`.

1. **Fetch:** `git fetch origin --prune`.
2. **Check whether `main` moved:** `git rev-list --left-right --count main...origin/main`. The second number is how many commits `origin/main` is ahead. If it's 0, go to step 5.
3. **Say what moved** before pulling. Tell the human in a few lines:
   - the merged PRs: `git log --oneline --first-parent main..origin/main`;
   - the areas they touched: `git diff --stat main origin/main`.
   
   Call out especially:
   - **new files in `controller/migrations/`**: recreate the local DB (`cd controller && docker compose down -v && docker compose up -d`);
   - **`CLAUDE.md`, `AGENTS.md`, `agent.md`, the active sprint's `path.md` or the Decision Record**: read them again before going on; the rules or the active sprint may have changed;
   - **`proto/` or `controller/graph/*.graphqls`**: regenerate (`buf generate`, gqlgen, `npm run codegen`).
4. **Update local `main`:** `git switch main && git pull --ff-only`. If it can't fast-forward (local commits on `main`), stop and ask; never reset `main`.
5. **Update the working branch** (if you're on, or about to use, a feature branch):
   - **uncommitted changes:** commit them or `git stash push -m "<why>"` first, and say so. Never discard someone's work;
   - **a pushed or shared branch:** `git merge origin/main`; never rewrite pushed history;
   - **a local-only branch:** `git rebase origin/main` is fine;
   - **a stacked branch** (based on another open PR's branch): update its base branch first, then merge the base into it;
   - **conflicts:** resolve only where the intent of both sides is clear (e.g. keep both table rows); otherwise stop and ask. Re-run the build and tests after resolving.
6. **New work starts from the updated `main`:** `git switch -c <branch> main`.

Check again before opening or updating a PR (`git fetch origin` plus step 2): if `main` moved meanwhile, merge it in and re-run the gates, so the PR is tested against what it will merge into. Pushing, PRs and merges still need the product owner's explicit go.

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

0. **Sync with `origin/main`** (section above): fetch, say what moved, fast-forward `main`, update the working branch.
1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint27/path.md` — decisions (D-71 … D-76), order, conflict zones, rules, test gates and acceptance
3. Read their phase file and pick the **first unchecked phase** whose dependencies are merged (all Sprint 27 phases are merged and the acceptance passed; next are fixes X5 (D-77) and X6 (D-78) in `Sprint27/path.md` → Post-Sprint Fixes). If the documents aren't signed off yet (`path.md` checklist), don't start implementation
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint27/path.md` | Active sprint: decisions summary, phases and order, conflict zones, rules, test gates, acceptance, checklist |
| `.zecurity-obs/Sprint27/Member1-Barath/Phases-O1-G1-Roles-Break-Glass.md` | Barath's phases O1, G1 (F19, F20) |
| `.zecurity-obs/Sprint27/Member2-Sathiya/Phases-X4-K1-K2-O2-API-Keys.md` | Sathiya's phases X4, K1, K2, O2 (F35, F19) |
| `.zecurity-obs/Sprint27/Scope-Draft.md` | Sprint 27 scope discussion (signed off 2026-11-23) |
| `.zecurity-obs/Sprint26/path.md` | Previous sprint record (billing plans and invoicing F34; acceptance PASS 2026-11-26) |
| `.zecurity-obs/Sprint25/path.md` | Previous sprint record (usage ledger F32, usage report F33, usage alerts; acceptance PASS 2026-11-24) |
| `.zecurity-obs/Sprint24/path.md` | Previous sprint record (metrics F23, alerts F26, incident timeline F27; acceptance run pending) |
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

Sprint 27 specific:
- Build gate passes before proceeding to the next phase (DB-backed tests must run, not skip; a skipped DB test is a failed case).
- The Decision Record (all amendments through **2026-11-28, break-glass alert and API key creators**) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code.
- No phase before its dependencies are merged (O1 before G1, K1 and O2; `077_` before K1's expiry alert; K1 before K2).
- **No commit, push, PR or merge without the product owner's explicit go** for that step.
- **Deny by default (D-72):** an action with no matrix entry is denied to every role except super-admin; a test fails when an action constant has no entry. Existing roles keep exactly today's permissions (full action × role table test, incl. D-31, D-41, D-55).
- **New roles (D-73):** `auditor` reads only; `billing` never gets relay, pool, tenant lifecycle, quota, alert or operator actions; `support` never sees invoices, usage or the provider audit log, and never writes. Tenant reads by any role are audited. `relay-ops` still never sees tenant data (D-40).
- **TOTP (D-74)** is mandatory for `super-admin`, `auditor`, `billing`, `support`; never mint a session without `otp` in `amr` for them.
- **API keys (D-76, D-78):** disabling or demoting a key's creator permanently revokes their keys (same transaction, audited; re-enabling never restores them); read-only (`decide()` refuses every write for an `api_key` actor, not handlers); never `super-admin`; expire ≤ 365 days; never go through `IssueSession` or yield a session; refused (403) on auth, TOTP, operator and key routes; the secret is shown once (`no-store`) and never logged, audited or returned again; rate limit and CIDR checks fail closed; a generic 401 on any failure.
- **Break-glass (D-75, D-77):** only `recover-admin --reason` on the controller host; no standing account, no console or API route; every use alerts (even if acknowledged before the evaluator ticks; the ack resolves the alert), emails super-admins and shows a banner; the temporary password stays stdout-only; no secrets in the alert.
- Migrations reserved: `075_provider_roles` (O1), `076_provider_api_keys` (K1), `077_provider_alert_kinds` (G1, both new platform kinds; never edit `069_` or `072_`).
- Respect the `Sprint27/path.md` conflict zones (`authz.go`, `RequireProvider` / `identity.go`, `totp.go`, operator handlers, `recover_admin.go`, `internal/alerting` kinds, `main.go`, `roles.ts`, `Layout.tsx`, `api-surface.test.ts`, `static-guards.test.ts`).

Sprint 26 (still in force):
- **Money is integers (D-64):** minor units only; no floats. Overage is rounded up to whole units, and tax half up. Overflow refuses.
- **Billing reads, never estimates:** quantities come only through U2's `ReadMonthUsage` (D-59); the billing plan only through `PlanForMonth` (month-start plan, D-65).
- **Issued invoices are immutable (D-66):** DB triggers. Numbers come from a gap-free counter (no `SEQUENCE`). One non-void invoice per tenant per month. Only `prune_invoices()` deletes issued invoices (editable retention, default 8 years, D-67); the S4 purge never does.
- **Billing is tenant data (D-70, opened to the `billing` role by D-73):** super-admin only (`billing.settings.write`, `invoice.read`, `invoice.write`; cards use the plan actions); relay-ops 403. Invoice email goes only to the tenant's own active admins; addresses are never audited or logged (D-68).
- **No enforcement (D-62):** no code path uses invoices, prices or tax to refuse, slow, suspend or limit anything. No payment data.

Sprint 25 (still in force):
- **Usage is counted, never estimated (D-59):** exact relay bytes from R1's cumulative counters (never integrated rates), minutes counted per minute; a gap stays unmetered and marks the hour incomplete; never back-filled from M1. Every ledger write is idempotent (never counted twice); a relay restart never produces a negative or doubled delta.
- **Usage is tenant data (D-60):** super-admin only (`tenant.usage.read`); relay-ops get 403 and never see usage or usage alerts. A purged tenant keeps only its usage, its `usage_tenants` snapshot (id, name, slug, plan name) and monthly active-user counts, never user ids (D-58).
- **Usage alerts warn only (D-61):** no code path uses usage to refuse, slow or limit anything.

Sprint 24 (still in force):
- **Proto (R1, D-54):** additive only — `repeated WorkspaceTraffic workspace_traffic = 13`; never renumber; an older relay must keep working; a relay that isn't reporting reads "Not reported" (never 0).
- **Alert scopes (D-50):** platform and tenant alerts are separate in the schema; tenant rows always carry `tenant_id` (DB CHECK); tenant-scoped reads always filter by it. Relay-ops never see tenant names, tenant alerts or per-tenant series (D-40 extended). In the incident timeline relay-ops also see the allowlisted relay and pool operation audit rows (actor and reason, no details; `timeline.relay_audit`, D-55), never the full audit log.
- **Webhooks (D-52):** one generic versioned JSON payload, no vendor SDKs; HMAC over timestamp + body; secret sealed, shown once, never logged/audited/returned; HTTPS outside dev; SSRF guard checked at connect time; no secrets or tenant content in the payload.
- **Rules and channels (D-53):** super-admin only, reason + before/after audit; built-in rule kinds, no query language.
- **Metrics (D-49):** Postgres, 1-min 7 days, hourly 400 days; an unknown value is a gap, never 0. Evaluator read errors never resolve an alert.
- **No migration framework.** Reserved: `075_` … `077_` (Sprint 27). Earlier: `073_`, `074_` (Sprint 26), `071_`, `072_` (Sprint 25), `068_` … `070_` (Sprint 24), `062_` … `067_` (Sprint 23); check the latest with `ls controller/migrations | tail -3` before adding one. Never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- **Console writes (D-30, amended by D-42 … D-47 and D-53):** auth calls (incl. TOTP), operator management, relay create / revoke / delete / drain / resume / move / pools, tenant pool assignment, plans and plan assignment, relay link capacity, tenant create / suspend / resume / delete / cancel delete / legal hold, quota overrides, **alert rules and alert channels (create, update, enable/disable, delete, test, rotate secret)**, **plan card prices, billing settings, and invoice issue / mark paid / void / regenerate / resend (D-70)**, **API key create / revoke (D-76) and the break-glass banner acknowledge (D-75)**. Every new write goes into `api-surface.test.ts` on purpose. `admin/` may change only where a phase file says so.
- **Tenant actions are super-admin only (D-06, amended by D-73:** `auditor` and `support` read tenants, `billing` reads the tenant list and assigns plans**)**, plans too; relay link capacity is relay-ops or super-admin (D-31 amended). Enforced server-side in `decide()`; console visibility is never the authorization control.
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
