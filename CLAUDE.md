# Zecurity — Claude Code Context

> This file is auto-loaded by Claude Code at session start.

---

## Project

**Zecurity** — ZTNA platform. Controller (Go), Connector (Rust), Shield (Rust), Relay (Rust), Client (Rust), Admin UI (React), Provider Console (React, new in Sprint 21).

**Sprint 26 is the active sprint: Provider Dashboard, billing plans and invoicing (F34).** Decisions **D-62 … D-70** are made (Decision Record amendment **2026-11-23, billing**). The Sprint 26 documents were **signed off on 2026-11-23 by Sathiya, who instructed that this override the product-owner sign-off step; Barath reviews them in PR #249**. Sprint 25 (F32, F33, usage alerts) is **merged** (2026-11-25, #244 → #248 plus the live-run fixes #253; `main` `1bf7e225`) and live-checked; F32 and F33 are ticked (#255). Sprint 26 **prices, invoices and emails invoices**: no payment collection, no enforcement, no agent change.
- **P1a — billing schema (F34)**, Barath, merged first (the contract with P3/P4):
  - `073_plan_cards`: `billing_settings` (country, currency, tax, retention, email), `plan_prices`, `plans.revision`, `plan_card_revisions`, `tenant_billing_plans`, `usage_tenants.plan_id`;
  - `074_invoices`: `invoices`, `invoice_lines`, `invoice_number_counters`, `invoice_emails`, immutability triggers, `prune_invoices()`.
- **P1b — plan cards + billing settings API (D-63, D-64, D-65, D-69)**, Barath:
  - cards are created and edited any time (price, included quantities, limits; revision history);
  - the currency is editable, default USD (e.g. INR); a change switches every card;
  - plan assignment records the month-start plan; `PlanForMonth` is the only billing-plan rule.
- **P2 — invoice generator + lifecycle (D-66, D-67)**, Barath:
  - month close plus a 6 h settling delay; drafts from U2's reader (counts only);
  - regenerate, issue (gap-free `INV-<year>-<n>`), mark paid, void;
  - editable retention (default 8 years); invoices survive the purge (amends D-46).
- **X3 — Sprint 25 follow-ups**, Sathiya: **done** — the acceptance run's findings were fixed in #253 (F32-1 relay usage on every heartbeat, "Not metered" months, counted share); F32, F33 ticked.
- **P3 — invoice API + tenant email (D-68, D-70)**, Sathiya, after P1a (writes after P2):
  - `GET/POST /provider/invoices…`; `invoice.read` / `invoice.write` (super-admin only; relay-ops 403);
  - issued invoices are emailed to the tenant's active admins (editable, default on), with a void notice and resend.
- **P4 — billing console**, Sathiya, after P1b and P3: Billing settings, Plans as cards (create, edit, duplicate, history), Invoices page and detail, tenant Invoices, export, print view.
- **Not in Sprint 26:** payment collection, any enforcement, tenant-facing billing pages, tax IDs or per-country tax rules, multi-currency at once or amount conversion, proration, discounts or credits, direct-path metering, F19 (billing role), F20, F22, F28/F29, F31, F35, F37, F39, F40.

Scope source of truth: `docs/provider-dashboard-architecture-decisions.md` → decision record 2026-09-23 (D-01 … D-23) and amendments 2026-09-26 (D-24 … D-29), 2026-10-26 (D-30 … D-34), 2026-11-11 (D-35, D-36), 2026-11-12 (D-37 … D-41), 2026-11-17 (D-42 … D-47; amends D-07, D-11, D-17, D-30, D-31; preserves D-08) **2026-11-20 (D-48 … D-54; amends D-30, extends D-40)** , **2026-11-21 (D-55: relay-ops see relay and pool operation audit rows in the incident timeline; extends D-40)** , **2026-11-21, usage (D-56 … D-61; extends D-40 and D-50, amends D-46)** and **2026-11-23, billing (D-62 … D-70; extends D-43, D-40/D-60, amends D-30, D-46)**. The plan is `.zecurity-obs/Sprint26/path.md` (decisions summary, order, conflict zones, rules, test gates, acceptance).

**Database (pre-production rule):**
- Schema changes are new numbered SQL files in `controller/migrations/`, run by Postgres only when its volume is first created.
- There is **no migration framework** and nothing upgrades an existing database. Don't add one.
- After any schema change, recreate the local DB: `cd controller && docker compose down -v && docker compose up -d`. Local data is disposable.
- Details: `docs/database-development.md` (Sprint 21 DEV-1).

**Team: two members only, both developing in Sprint 26.** **M1 = Sathiya** (Person 2): X3 follow-ups, P3 invoice API + tenant email, P4 billing console — `Sprint26/Member2-Sathiya/Phases-X3-P3-P4-Billing-Console.md`. **M2 = Barath** (Person 1, product owner): P1a billing schema, P1b plan cards + settings API, P2 invoice generator — `Sprint26/Member1-Barath/Phases-P1-P2-Prices-Invoices.md`. Both review each other's PRs and run the sprint acceptance on the lab.

---

## Your First Step

When a team member starts a session, they will tell you who they are (Sathiya / M1 or Barath / M2). When they do:

1. Read `agent.md` (project root) — full conventions, code style, build commands
2. Read `.zecurity-obs/Sprint26/path.md` — decisions (D-62 … D-70), order, conflict zones, rules, test gates and acceptance
3. Read their phase file and pick the **first unchecked phase** whose dependencies are merged (Barath: P1a → P1b and P2; Sathiya: X3 → P3 (after P1a; writes after P2) → P4). If the documents aren't signed off yet (`path.md` checklist), don't start implementation
4. **Check for "Post-Phase Fixes" section** in the phase file — apply any fixes listed there
5. Brief them: what they're building, which files to touch, and the build check command

If they don't say who they are, ask: *"Are you Sathiya (M1) or Barath (M2)?"*

---

## Key Files

| File | Purpose |
|------|---------|
| `agent.md` | Full conventions, build commands, code style |
| `.zecurity-obs/Sprint26/path.md` | Active sprint: decisions summary, phases and order, conflict zones, rules, test gates, acceptance, checklist |
| `.zecurity-obs/Sprint26/Member1-Barath/Phases-P1-P2-Prices-Invoices.md` | Barath's phases P1a, P1b, P2 (F34) |
| `.zecurity-obs/Sprint26/Member2-Sathiya/Phases-X3-P3-P4-Billing-Console.md` | Sathiya's phases X3, P3, P4 (F34) |
| `.zecurity-obs/Sprint26/Scope-Draft.md` | Sprint 26 scope discussion (signed off 2026-11-23) |
| `.zecurity-obs/Sprint25/path.md` | Previous sprint record (usage ledger F32, usage report F33, usage alerts; stack #244 → #248, acceptance run pending) |
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

Sprint 26 specific:
- Build gate passes before proceeding to the next phase (DB-backed tests must run, not skip; a skipped DB test is a failed case).
- The Decision Record (all amendments through **2026-11-23, billing**) is binding. If a phase seems to need a new architectural choice, stop and ask. Don't decide it in code.
- No phase before its dependencies are merged (Sprint 25 is merged; P1a before P1b, P2 and P3; P2 before P3's writes; P1b and P3 before P4).
- **No commit, push, PR or merge without the product owner's explicit go** for that step.
- **Money is integers (D-64):** minor units only; no floats. Overage is rounded up to whole units, and tax half up. Overflow refuses.
- **Billing reads, never estimates:** quantities come only through U2's `ReadMonthUsage` (D-59); the billing plan only through `PlanForMonth` (month-start plan, D-65).
- **Issued invoices are immutable (D-66):** DB triggers. Numbers come from a gap-free counter (no `SEQUENCE`). One non-void invoice per tenant per month. Only `prune_invoices()` deletes issued invoices (editable retention, default 8 years, D-67); the S4 purge never does.
- **Billing is tenant data (D-70):** super-admin only (`billing.settings.write`, `invoice.read`, `invoice.write`; cards use the plan actions); relay-ops 403. Invoice email goes only to the tenant's own active admins; addresses are never audited or logged (D-68).
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
- **No migration framework.** Reserved: `073_plan_cards`, `074_invoices` (P1a). Earlier: `071_`, `072_` (Sprint 25), `068_` … `070_` (Sprint 24), `062_` … `067_` (Sprint 23); check the latest with `ls controller/migrations | tail -3` before adding one. Never edit an existing file; label the PR `[schema: reset DB]`; everyone recreates their local DB.
- **Console writes (D-30, amended by D-42 … D-47 and D-53):** auth calls (incl. TOTP), operator management, relay create / revoke / delete / drain / resume / move / pools, tenant pool assignment, plans and plan assignment, relay link capacity, tenant create / suspend / resume / delete / cancel delete / legal hold, quota overrides, **alert rules and alert channels (create, update, enable/disable, delete, test, rotate secret)**, **plan card prices, billing settings, and invoice issue / mark paid / void / regenerate / resend (D-70)**. Every new write goes into `api-surface.test.ts` on purpose. `admin/` may change only where a phase file says so.
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
- Respect the `Sprint26/path.md` conflict zones (U2 usage reader, U1 `usage_tenants` snapshot, B1 plan create/update/assign, S4 purge, SMTP sender, `main.go`, `authz.go`, `api-surface.test.ts`, `TenantDetail.tsx`, Plans page, navigation).

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
