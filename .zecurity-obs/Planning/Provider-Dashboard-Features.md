---
type: tracker
title: Provider Dashboard — Feature Tracker
updated: 2026-11-23
owners: [Barath (M2), Sathiya (M1)]
source: Feature list from Barath, 2026-10-26 (Phase 1 / 2 / 3)
baseline: main c79a75e1 (Sprint 22 T, A, E, X, D merged; FU-1 … FU-19 done except FU-11, mitigated; quick wins #190, F17 #192, F5 #193, F14 #195 merged and live-checked 2026-11-12)
tags:
  - provider-dashboard
  - tracker
---

# Provider Dashboard — Feature Tracker

> **How to use:** tick a box when a feature is **merged and live-checked**. Put the owner's name in the table below when someone picks a feature up. "Must have" = Phase 1 (F1–F18): *"you can't run the product without it."* "Can have" = Phase 2 and 3 (F19–F40): operability and business features.
>
> **Status key:** ✅ done · 🧪 merged, live check pending (`Pending-Live-Tests.md`) · ◐ partly done (what's missing is in the notes) · 🚧 in progress · ⬜ not started
> **Effort key:** 🟢 **Quick** = controller API + console only (no schema, no agent change, no new decision) · 🟡 **Medium** = needs a migration or a background job · 🔴 **Large** = needs connector / relay / shield / client changes or new infrastructure · ⚖️ **Decision** = needs a Decision Record decision first (don't decide it in code)

## Tracking

**Progress:** Must have **18 / 18** done · Can have 3 / 22 done · **Total 21 / 40** (ticked = merged **and** live-checked)

**Live checks:** all merged items passed on 2026-11-12 (F5, F8, F14, F15, F16, F17, F25, F38), 2026-11-17 (relay pools: F4, F9, F10, F11) and 2026-11-19 (Sprint 23 on `main` `4d1faf50`: B1 #222, B2 #226, S1 #223, S2 recheck #225, S3 #224, cross-feature checks; [[Pending-Live-Tests]]). S4 tenant deletion (#229) was live-checked on its branch (two runs, PASS); the merged S4 code is identical to the tested code (only B2 #226 and docs were merged in afterwards).

## Status snapshot (2026-11-12)

Checked against `main` `c79a75e1`. "Quick" = controller read API + console view only (🟢 in the effort key).

### Must have (Phase 1): 18 of 18 done

| ID | Feature | Status | What's left |
|---|---|---|---|
| F1 | Relay enrolment with a one-time token | ✅ Done | — |
| F2 | Relay certificates from the provider CA | ✅ Done | — |
| F3 | Relay revoke and decommission | ✅ Done | — |
| F4 | Relay list: status, version, region, heartbeat | ✅ Done | — (region via relay pools, PR #200; live PASS 2026-11-17) |
| F5 | Capacity view: sessions, bandwidth, headroom | ✅ Done | — (bandwidth PR #193, live PASS 2026-11-12; headroom B2 #226, live PASS 2026-11-19) |
| F6 | Drain mode | ✅ Done | — (Phase D, PR #156; AT-D live PASS) |
| F7 | Tenant create / suspend / delete | ✅ Done | — (create S2 #225 and suspend/resume S1 #223 live PASS 2026-11-19 on `main`; delete S4 #229 live PASS on the branch 2026-11-18/19, same S4 code as merged) |
| F8 | Tenant detail page | ✅ Done | — (policy counts PR #190, live PASS 2026-11-12; plan B1 #222, live PASS 2026-11-19) |
| F9 | Tenant → relay pool assignment | ✅ Done | — (PR #200; live PASS 2026-11-17) |
| F10 | Region / residency constraint | ✅ Done | — (routing PR #200, relay-side enforcement PR #202, connector recovery fix PR #210; live PASS 2026-11-17) |
| F11 | Failover pool per tenant | ✅ Done | — (PRs #201, #213, #215; live PASS 2026-11-17 run 2) |
| F12 | Per-tenant quotas | ✅ Done | — (S3 #224, six keys, live PASS 2026-11-19) |
| F13 | Certificate inventory by expiry | ✅ Done | — |
| F14 | Expiry alerting | ✅ Done | — (banner/filter; daily super-admin email D-36, PR #195; live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197)) |
| F15 | Revocation + CRL/OCSP status | ✅ Done | — (CRL-based by design, D-35; CRL status PR #190; live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197)) |
| F16 | CA hierarchy view | ✅ Done | — (PR #190; live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197)) |
| F17 | Append-only provider action log | ✅ Done | — (PR #192; live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197)) |
| F18 | Audit search and filter | ✅ Done | — |

### Can have (Phase 2 and 3): 3 of 22 done

| ID | Feature | Status | What's left |
|---|---|---|---|
| F19 | Provider admin roles | ◐ Partly | Two roles today; more need a decision |
| F20 | Break-glass account | ◐ Partly | `recover-admin` CLI exists; decide whether it counts |
| F21 | MFA on the provider console | ✅ Done | — |
| F22 | Tunnel success rate per relay/tenant | ⬜ Not started | Needs relay/connector counters (large) |
| F23 | Active-session timeseries | 🧪 Merged | Sprint 24 M1 #240, M2 #242 (charts, per-tenant relay bandwidth, R1); Sprint 24 acceptance run pending |
| F24 | Connector online/offline roster | ✅ Done | — |
| F25 | Control-plane health | ✅ Done | — (Overview card over `GET /provider/health`, PR #190; live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197)) |
| F26 | Alert rules + webhook/email | 🧪 Merged | Sprint 24 A1 #234, A2 #235, A3 #236; usage alerts U4 #248 (Sprint 25, open); Sprint 24 acceptance run pending |
| F27 | Incident timeline | 🧪 Merged | Sprint 24 T1 #238 (D-55 relay-ops rows); Sprint 24 acceptance run pending |
| F28 | Relay config push, staged rollout | ⬜ Not started | Large |
| F29 | Relay version pinning and rollback | ⬜ Not started | Large |
| F30 | Tenant migration between pools | ⬜ Not started | Decided (D-38): an assignment change with make-before-break (PR 1); no bulk/scheduled migration |
| F31 | Read-only support view (impersonation) | ◐ Partly | Audited read-only view exists; impersonation needs a decision |
| F32 | Usage collection | 🚧 In review | Sprint 25 U1 #245 (ledger `071_usage` + collector, D-57 … D-59); live check after merge |
| F33 | Per-tenant usage report | 🚧 In review | Sprint 25 U2 #246 (API), U3 #247 (console + export); live check after merge |
| F34 | Billing plans and invoicing | ⬜ Not started | Sprint 26 (signed off 2026-11-23, D-62 … D-70): editable plan cards, editable currency (default USD), month-start plan billing, invoices emailed to tenants, optional tax; `Sprint26/path.md` |
| F35 | Provider API + API keys | ◐ Partly | REST API exists; API keys need a decision |
| F36 | Tenant self-signup / provisioning | ◐ Partly | First sign-in creates a workspace; no provider-side workflow |
| F37 | Multi-provider / reseller hierarchy | ⬜ Not started | Needs a decision |
| F38 | Compliance export | ◐ Partly | Audit export CSV/JSON (client-side, ≤ 10,000 rows, PR #190) live check PASS 2026-11-12 ([[Pending-Live-Tests]], PR #197); full SOC 2 bundle is larger |
| F39 | Custom dashboards and saved views | ⬜ Not started | Large |
| F40 | SLA tracking per tenant | ⬜ Not started | Large, after F22, F23 and F25 |

**Quick wins** (controller read API + console view, no decision needed): F8, F14, F15, F16 (Must have); F25, F38 (Can have).
All merged in PR #190 (F14's quick part was already in place). Boxes are ticked only after the live check.

### Remaining Must have: decisions (D-03 decided 2026-11-12)

| Decision | Unblocks | Recommendation |
|---|---|---|
| ~~**D-03** relay pools / regions~~ | F4 (region), F9, F10, F11 (and F30) | **Decided D-37 … D-41** (2026-11-12): tenant relay pools with controller-driven failover; region = routing, residency needs relay-side enforcement. Plan: [[Relay-Pools-Implementation-Plan]] (3 PRs) |
| **D-34** tenant suspension epic + deletion semantics (Q4) | F7 | Separate epic |
| Quota model | F12 | Which limits (connectors, users, sessions), where enforced |
| Billing model (F34) | F8 (plan) | Wait for F34 |
| ~~Alert channel and recipients~~ | F14 (sending) | **Decided D-36** (2026-11-11): daily email to super-admins |
| ~~OCSP needed?~~ | F15 (OCSP) | **Decided D-35** (2026-11-11): no OCSP, CRL-based by design |

## Checklist

### Must have (Phase 1)

*Relay fleet*
- [x] **F1** Relay enrollment via one-time bootstrap token ✅
- [x] **F2** Cert issuance to relay from provider CA ✅
- [x] **F3** Relay decommission / revoke ✅
- [x] **F4** Relay list with status, version, region, last heartbeat ✅ (region via relay pools PR #200; live PASS 2026-11-17)
- [x] **F5** Capacity view — active sessions, bandwidth, headroom ✅ (bandwidth PR #193, live PASS 2026-11-12; headroom B2 #226, live PASS 2026-11-19)
- [x] **F6** Drain mode (no new placements, bleed off existing) ✅ (Phase D, PR #156; AT-D live PASS)

*Tenants*
- [x] **F7** Tenant create / suspend / delete ✅ (create #225, suspend/resume #223 live PASS 2026-11-19; delete S4 #229 live PASS on the branch, same code as merged)
- [x] **F8** Tenant detail page — connectors, users, policies count, plan ✅ (policy counts PR #190; plan B1 #222, live PASS 2026-11-19)
- [x] **F9** Tenant → relay pool assignment ✅ (PR #200; live PASS 2026-11-17)
- [x] **F10** Region / residency constraint on placement ✅ (PRs #200, #202, #210; live PASS 2026-11-17)
- [x] **F11** Failover pool per tenant ✅ (PRs #201, #213, #215; live PASS 2026-11-17, run 2 on a reset DB)
- [x] **F12** Per-tenant quotas ✅ (S3 #224: users, resources, remote networks, connectors, shields, client devices; live PASS 2026-11-19)

*PKI*
- [x] **F13** Certificate inventory with expiry, sorted by days remaining ✅
- [x] **F14** Expiry alerting (threshold-based) ✅ (daily super-admin email D-36, PR #195; live PASS 2026-11-12)
- [x] **F15** Revocation action + CRL/OCSP status ✅ (CRL status PR #190; OCSP closed by D-35; live PASS 2026-11-12)
- [x] **F16** CA hierarchy view ✅ (PR #190; live PASS 2026-11-12)

*Audit*
- [x] **F17** Append-only provider action log (actor, action, target, before/after, timestamp) ✅ (PR #192; live PASS 2026-11-12)
- [x] **F18** Audit search and filter ✅

### Can have (Phase 2)

*Access control*
- [ ] **F19** Provider admin roles (owner / operator / read-only / support) ◐ (two roles today)
- [ ] **F20** Break-glass account with forced audit entry ◐ (`recover-admin` CLI, audited)
- [x] **F21** MFA enforcement on provider console ✅

*Monitoring*
- [ ] **F22** Tunnel establishment success rate per relay and per tenant ⬜
- [ ] **F23** Active session count timeseries ⬜ (some data exported to Prometheus, PENDING-10; not stored or shown)
- [x] **F24** Connector online/offline roster per tenant ✅
- [x] **F25** Control-plane health (API, PKI engine, policy store, DB) ✅ (Overview card PR #190; live PASS 2026-11-12)
- [ ] **F26** Alert rules + webhook/email dispatch ⬜
- [ ] **F27** Incident timeline view ⬜

*Operations*
- [ ] **F28** Relay config push with staged rollout (canary → batch → fleet) ⬜
- [ ] **F29** Relay version pinning and rollback ⬜
- [ ] **F30** Tenant migration between relay pools ⬜
- [ ] **F31** Read-only tenant support view (impersonation, audited) ◐ (audited read-only detail, no impersonation)

*Metering*
- [ ] **F32** Usage collection — session-seconds, bytes relayed, connector-hours ⬜
- [ ] **F33** Per-tenant usage report ⬜

### Can have (Phase 3)

- [ ] **F34** Billing plans and invoicing ⬜
- [ ] **F35** Provider API + API keys ◐ (REST API exists, no API keys)
- [ ] **F36** Tenant self-signup / provisioning workflow ◐ (first login creates a workspace)
- [ ] **F37** Multi-provider / reseller hierarchy ⬜
- [ ] **F38** Compliance export (SOC 2 evidence, audit bundles) ◐ (audit CSV/JSON export PR #190, live PASS 2026-11-12; no SOC 2 bundle)
- [ ] **F39** Custom dashboards and saved views ⬜
- [ ] **F40** SLA tracking per tenant ⬜

---

## Quick-win queue (API-only)

Features — or the quick part of a feature — that fit "controller API + console only": no migration, no connector/relay change, no new architectural decision. Each new console read goes into `api-surface.test.ts` (`ALLOWED_READS`) on purpose; console writes stay limited to D-30.

| # | Feature | Quick part | Owner |
|---|---------|-----------|-------|
| 1 | **F8** Tenant detail | Add policy counts (groups, access rules, resources) to the tenant detail API and page | |
| 2 | **F16** CA hierarchy view | Read API returning Root → Intermediate → workspace CAs (subject, serial, validity, issuer link); tree view in Certificates | |
| 3 | **F15** CRL status | Read API for the relay CRL and each workspace CRL: last generated, next update, number of entries; shown in Certificates | |
| 4 | **F25** Control-plane health | Provider read over the existing PENDING-10 health checks (DB, Valkey, PKI, policy compiler); status card on Overview | |
| 5 | **F38** Compliance export (lite) | Export the provider audit log (filtered, JSON/CSV) as a read; full SOC 2 bundle stays ⬜ | |
| 6 | **F14** Expiry alerting (lite) | Overview banner and Certificates filter for "expiring within N days"; sending alerts waits for F26 | |

All six done: #1–#5 merged in PR #190; #6 was already in place.

---

## Feature details

### Must have (Phase 1)

| ID | Feature | Status | Effort for what's left | Evidence / what's missing | Owner |
|----|---------|--------|------------------------|---------------------------|-------|
| F1 | Relay enrollment via one-time bootstrap token | ✅ | — | Sprint 20 provisioning (single-use token, atomic burn); console Add relay with install command, Sprint 22 Phase E (PR #151); token lifecycle tests (expired, concurrent). | |
| F2 | Cert issuance to relay from provider CA | ✅ | — | Relay certificates signed by the platform Intermediate, SANs limited to the stored allowlist; renewal with key continuity (Sprint 20/21). | |
| F3 | Relay decommission / revoke | ✅ | — | API hardened in Phase A (PR #142: reason, typed name, super-admin only); console revoke/delete in Phase X (PR #154); CRL + connector migration live-checked. | |
| F4 | Relay list with status, version, region, last heartbeat | ✅ | — | Status, version, last heartbeat, address scope, capacity tier (Sprint 21 Relays page); pool and region per relay (relay pools PR #200, D-37). Live PASS 2026-11-17. | |
| F5 | Capacity view — active sessions, bandwidth, headroom | ◐ | ⚖️ (headroom) | Connections / max and capacity tier shown; `relay_connections_total` and `relay_capacity_utilization_ratio` exported to Prometheus (PENDING-10). **Bandwidth** merged (PR #193): relay counts bytes per direction as they flow and reports cumulative counters in heartbeat fields 9/10 (`optional`, additive); migration `052_relay_bandwidth.sql`; `RecordHeartbeat` derives the rate from the previous stored sample (late heartbeats ignored; counter decrease or uptime < gap = restart, no rate); `GET /provider/relays[/{id}]` returns `bandwidth` (null = not reported / revoked; null rates = measuring); console Bandwidth column and detail card. Rate is an average over the heartbeat DB-write window (5 min default). Not in scope: a bandwidth ceiling ("bandwidth headroom" needs a configured link capacity), Prometheus throughput metric. | |
| F6 | Drain mode | ✅ | — | Sprint 22 **Phase D** (D-32), PR #156: drained relay leaves the published list, last-eligible-relay guard, `043_relay_drain.sql`; AT-D live PASS (incl. AT-D6 client data plane); follow-ups FU-6, FU-7, FU-14. | |
| F7 | Tenant create / suspend / delete | ⬜ | ⚖️ 🔴 | Tenants are created by first sign-in only. Suspension is a separate epic (D-34); deletion semantics are open (decisions doc Q4). | |
| F8 | Tenant detail page — connectors, users, policies count, plan | ◐ | ⚖️ (plan) | Done: connectors, shields, remote networks, users, devices with counts (Sprint 21, audited `tenant.read`); **policy counts** merged (PR #190, live PASS 2026-11-12). Missing: **plan** (needs F34). | |
| F9 | Tenant → relay pool assignment | ✅ | — | Relay pools with per-pool relay lists, super-admin assignment with graceful/immediate migration, per-pool last-relay guard (PR #200, D-37/D-38/D-41). Live PASS 2026-11-17. | |
| F10 | Region / residency constraint on placement | ✅ | — | Region routing (PR #200); relay-side residency enforcement with versioned, leased workspace policy (PR #202, D-40); connector retries a refused migration (PR #210, live finding F10-1). Live PASS 2026-11-17. Residency covers relay traffic only. | |
| F11 | Failover pool per tenant | ✅ | — | Controller-driven failover with a 5-minute hold-down, state in Postgres (PR #201, D-39); F11-1 "no relay path" display and audit (PR #213); F11-2 D-39 refinement: never held on an empty failover pool while the primary has an eligible relay (PR #215). Live PASS 2026-11-17 (run 2: reset DB, `main` 883fed1c, migration 061, traffic probes). | |
| F12 | Per-tenant quotas | ⬜ | ⚖️ 🟡 | Needs quota entities and enforcement points (connector/user/session creation). | |
| F13 | Certificate inventory with expiry, sorted by days remaining | ✅ | — | Certificates page: root, intermediate, workspace CAs and agent certificates, ordered by `not_after`, expiry buckets (Sprint 21). | |
| F14 | Expiry alerting (threshold-based) | ✅ | 🟡 | Expiry buckets (<24h, <7d, <30d), Overview "Needs attention" and the Certificates window filter are in place. **Daily email** (D-36), PR #195, live PASS 2026-11-12: `internal/certalert` job (checked a minute after start, then hourly; one email per UTC day via a Valkey marker; retried if every send fails); recipients = active super-admins; platform CAs from 30 days, relays and the controller gRPC certificate within 24 h or expired; tenant agent certificates excluded (short-lived, self-renewing); audited `cert_expiry.alert_sent`; `CERT_EXPIRY_ALERTS=false` disables. Rules and other channels: F26. | |
| F15 | Revocation action + CRL/OCSP status | ✅ | — | Revoke actions exist (relays: console; connectors/shields: admin UI); workspace and relay CRLs are published; **CRL status per signer** merged (`GET /provider/pki`, PR #190). **OCSP closed by D-35** (2026-11-11): CRL-based by design; clients, connectors and relays check CRLs. | |
| F16 | CA hierarchy view | ✅ | — | Root → Intermediate → workspace CA tree merged (`GET /provider/pki`, PR #190): parents linked by SKI/AKI, paginated workspace CAs, unlinked CAs listed separately. | |
| F17 | Append-only provider action log | ✅ | — | `provider_audit_logs` has actor, action, target, details, IP, timestamp; mutations audited (transactionally for revoke/delete/create). Merged (PR #192): migration `051_provider_audit_append_only.sql` refuses UPDATE, DELETE and TRUNCATE (incl. TRUNCATE ... CASCADE) with SQLSTATE 42501; `provider.WithChange` adds `details.before` / `details.after` (state fields only, never credentials) to operator create/bootstrap, role change, disable/enable, password reset/change, TOTP enrol/reset, recover-admin, relay create/revoke/delete/drain/resume. Events (login, logout, MFA verify, rate limits, tenant.read) carry no before/after. Limit: the table owner or a superuser can still disable the triggers (needs a non-owner app role, out of scope). | |
| F18 | Audit search and filter | ✅ | — | Audit page with filters and pagination (Sprint 21). | |

### Can have (Phase 2)

| ID | Feature | Status | Effort for what's left | Evidence / what's missing | Owner |
|----|---------|--------|------------------------|---------------------------|-------|
| F19 | Provider admin roles (owner / operator / read-only / support) | ◐ | ⚖️ 🟡 | Two roles today: super-admin and relay-ops (D-05, D-31), enforced server-side in `decide()`. More roles need a decision on the action matrix. | |
| F20 | Break-glass account with forced audit entry | ◐ | ⚖️ 🟢 | `zecurity-controller provider recover-admin` (Phase T) on the controller host, audited as `provider_user.recover_admin`. Decide whether this counts, or a standing break-glass account is wanted. | |
| F21 | MFA enforcement on provider console | ✅ | — | Super-admin TOTP, Phase T (PR #138); live AT-T PASS (PR #147). | |
| F22 | Tunnel establishment success rate per relay and per tenant | ⬜ | 🔴 | Needs success/failure counters reported by relays/connectors. | |
| F23 | Active session count timeseries | ⬜ | 🟡 / 🔴 | Relay connection count is in each heartbeat; `connector_active_streams` and relay connection gauges are exported to Prometheus (PENDING-10), but not stored or shown in the console; per-tenant sessions need agent data. | |
| F24 | Connector online/offline roster per tenant | ✅ | — | Tenant detail lists connectors with status and last heartbeat (Sprint 21). | |
| F25 | Control-plane health | ✅ | — | Health subsystem with `/livez`, `/readyz`, `/healthz`, `/health` and internal diagnostics (PENDING-10); Overview **Control plane** card over `GET /provider/health` (super-admin, sanitized messages) merged (PR #190). | |
| F26 | Alert rules + webhook/email dispatch | ⬜ | 🟡 | Needs rule storage, an evaluator and dispatch (SMTP config exists for invites). | |
| F27 | Incident timeline view | ⬜ | 🟡 | Could combine provider audit, relay/connector events and alerts (after F26). | |
| F28 | Relay config push with staged rollout | ⬜ | 🔴 | No relay config/command channel or release workflow. | |
| F29 | Relay version pinning and rollback | ⬜ | 🔴 | Relay version is reported; no pinning or update control. | |
| F30 | Tenant migration between relay pools | ⬜ | ⚖️ 🔴 | Depends on F9 (pools). | |
| F31 | Read-only tenant support view (impersonation, audited) | ◐ | ⚖️ 🔴 | Read-only tenant detail exists and every view is audited. Impersonation needs the support-access decision (decisions doc Q9). | |
| F32 | Usage collection — session-seconds, bytes relayed, connector-hours | ⬜ | 🔴 | Nothing is metered today. | |
| F33 | Per-tenant usage report | ⬜ | 🔴 | Depends on F32. | |

### Can have (Phase 3)

| ID | Feature | Status | Effort for what's left | Evidence / what's missing | Owner |
|----|---------|--------|------------------------|---------------------------|-------|
| F34 | Billing plans and invoicing | ⬜ | ⚖️ 🔴 | No plan/billing model. | |
| F35 | Provider API + API keys | ◐ | ⚖️ 🟡 | Provider REST API exists (session tokens). API keys need an auth decision (must not mint sessions outside `IssueSession`). | |
| F36 | Tenant self-signup / provisioning workflow | ◐ | 🟡 | First sign-in creates a workspace with its admin and CA. No provider-side provisioning workflow. | |
| F37 | Multi-provider / reseller hierarchy | ⬜ | ⚖️ 🔴 | Single provider today (decisions doc Q1). | |
| F38 | Compliance export (SOC 2 evidence, audit bundles) | ◐ | 🟡 | **Audit export merged** (PR #190, live PASS 2026-11-12): CSV/JSON from `GET /provider/audit` in the browser, applied filters and order kept, keyset pages without repeats, ≤ 10,000 rows with an explicit limit message, CSV formula cells neutralised. Missing: SOC 2 evidence bundle. | |
| F39 | Custom dashboards and saved views | ⬜ | 🔴 | — | |
| F40 | SLA tracking per tenant | ⬜ | 🔴 | Depends on monitoring (F22, F23, F25). | |

---

## Rules that apply to every feature

From `CLAUDE.md` and the Decision Record (binding):
- A feature that needs a new architectural choice (⚖️) stops for a decision first; don't decide it in code.
- Schema changes are new numbered files in `controller/migrations/` (next: `062_` as of 2026-11-17; check with `ls controller/migrations | tail -3`); label the PR `[schema: reset DB]`.
- Console writes are limited to D-30 (auth, operator management, relay create/revoke/delete/drain/resume); every new console read or write goes into `api-surface.test.ts` on purpose.
- Provider read APIs never return secrets; query services in `internal/providerquery/` don't import `net/http`.
- Tests first; DB-backed tests must run (a skipped DB test is a failure); live check before ticking a box.

## Changelog

- 2026-10-29: created from the 2026-10-26 feature list; statuses checked against `fixed-pendings` `4cc7ffcc`.
- 2026-11-06: status snapshot tables added (Must have / Can have); statuses checked against `main` `520851d4`. F6 done (Phase D, PR #156, AT-D live PASS). F25 partly done and F5/F23 notes updated (PENDING-10 health checks and Prometheus metrics). Progress 8 / 40.
- 2026-11-06: quick-win batch implemented (branch `feat/provider-quick-wins`, one PR): F8 policy counts, F15/F16 `GET /provider/pki` (CA tree by key identifiers + CRL status per signer), F25 `GET /provider/health` (super-admin, sanitized), F38 audit export (CSV/JSON, client-side). F14's quick part was already in place (corrected). Follow-up: `resources` has no `tenant_id` index; the F8 count uses one grouped aggregate per statement instead (an index needs a migration).
- 2026-11-11: refreshed against `main` `37a66bb9`. Merged: quick wins (PR #190: F8 policy counts, F15 CRL status, F16 CA tree, F25 health card, F38 audit export), F17 append-only audit + before/after (PR #192, migration `051_`), F5 relay bandwidth (PR #193, migration `052_`, heartbeat fields 9/10). New status 🧪 = merged, live check pending ([[Pending-Live-Tests]]). Every remaining Must-have item now needs a decision (table under the snapshot). Progress unchanged at 8 / 40 until the live session.
- 2026-11-11: decisions D-35 (F15: no OCSP, CRL-based by design) and D-36 (F14: daily super-admin expiry email) recorded in `docs/provider-dashboard-architecture-decisions.md`. F15 → 🧪 (live check of the CRL status view). F14 email implemented on `feat/f14-expiry-email`, PR pending.
- 2026-11-12: live session on the lab ([[Pending-Live-Tests]], PR #197): F5 (bandwidth), F8 (policy counts), F14, F15, F16, F17, F25, F38 (audit export) all PASS. Ticked F14, F15, F16, F17 (Must have) and F25 (Can have). F5 stays ◐: bandwidth passed, headroom (link-capacity ceiling) still open. Progress 13 / 40 (Must have 10 / 18). Remaining Must-have items all need a decision: D-03 (F4 region, F9, F10, F11), D-34 (F7), quota model (F12), billing (F8 plan); D-03 options drafted in [[D-03-Relay-Pools-Options]].
- 2026-11-12: D-03 reopened and decided as D-37 … D-41 (tenant relay pools, controller-driven failover, region = routing, residency = relay-side enforcement). F4, F9, F10, F11, F30 unblocked; plan [[Relay-Pools-Implementation-Plan]] (PR 1 pools, PR 2 failover, PR 3 relay residency enforcement). F10 closes only after PR 3.
- 2026-11-17: relay pools live run on the lab ([[Pending-Live-Tests]]): F4/F9 (PR #200) and F11 (PR #201) PASS; F10 (PR #202) failed at step 1 on a connector recovery defect (F10-1: a refused migration was never retried), fixed in PR #210 and retested live (recovered on its own, no restart). Ticked F4, F9, F10. F11 ◐: failover passed, but F11-1 (console/audit when both pools are down) is open; tick it with that fix. Progress 16 / 40 (Must have 13 / 18). Remaining Must-have items: F5 headroom, F7 (D-34), F8 plan, F12 quotas.
- 2026-11-17: F11 completed: F11-1 (PR #213, "no relay path" display and audit) and F11-2 (PR #215, D-39 refinement: never held on an empty failover pool while the primary has an eligible relay) merged; full live re-run on a reset DB (`main` 883fed1c, migration 061) passed with traffic probes. Ticked F11. Progress 17 / 40 (Must have 14 / 18). Remaining Must-have items: F5 headroom, F7 (D-34), F8 plan, F12 quotas.
- 2026-11-18: Sprint 23 merges: B1 plans #222 (F8), B2 headroom #226 (F5), S1 suspend/resume #223 and S2 tenant creation #225 (F7; S2 handed from Barath to Sathiya), S3 quotas #224 (F12). Snapshot rows set to 🧪 / ◐; nothing ticked until live-checked. S4 deletion remains.
- 2026-11-23: snapshot refreshed. Sprint 24 merged: F23 (M1 #240, M2 #242), F26 (A1–A3 #234–#236), F27 (T1 #238) → 🧪, ticked after the Sprint 24 acceptance run. Sprint 25 code complete as a stacked set (#244 docs, #245 U1, #246 U2, #247 U3, #248 U4): F32, F33 → 🚧. Sprint 26 theme chosen: **F34 billing** (scope draft `Sprint26/Scope-Draft.md`, D-62 … D-68 proposed). Progress unchanged at 21 / 40.
- 2026-11-23: Sprint 26 (F34) signed off (by Sathiya, overriding the product-owner sign-off step; Barath reviews in PR #249). Decision Record amendment 2026-11-23, billing (D-62 … D-70); plan `Sprint26/path.md`.
