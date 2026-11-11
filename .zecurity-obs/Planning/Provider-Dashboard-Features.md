---
type: tracker
title: Provider Dashboard — Feature Tracker
updated: 2026-11-11
owners: [Barath (M2), Sathiya (M1)]
source: Feature list from Barath, 2026-10-26 (Phase 1 / 2 / 3)
baseline: main 37a66bb9 (Sprint 22 T, A, E, X, D merged; FU-1 … FU-19 done except FU-11, mitigated; quick wins #190, F17 #192, F5 #193 merged)
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

**Progress:** Must have 6 / 18 done · Can have 2 / 22 done · **Total 8 / 40** (ticked = merged **and** live-checked)

**Merged, live check pending:** F5, F16, F17 (Must have); F25 (Can have); plus the merged parts of F8, F15 and F38. Checklist: [[Pending-Live-Tests]].

## Status snapshot (2026-11-11)

Checked against `main` `37a66bb9`. "Quick" = controller read API + console view only (🟢 in the effort key).

### Must have (Phase 1): 6 of 18 done, 3 more merged and waiting for the live check

| ID | Feature | Status | What's left |
|---|---|---|---|
| F1 | Relay enrolment with a one-time token | ✅ Done | — |
| F2 | Relay certificates from the provider CA | ✅ Done | — |
| F3 | Relay revoke and decommission | ✅ Done | — |
| F4 | Relay list: status, version, region, heartbeat | ◐ Partly | Region needs the pool/region decision (D-03) |
| F5 | Capacity view: sessions, bandwidth, headroom | 🧪 Merged | Bandwidth merged (PR #193, migration `052_`); live check pending. Bandwidth *headroom* (a link-capacity ceiling) deferred |
| F6 | Drain mode | ✅ Done | — (Phase D, PR #156; AT-D live PASS) |
| F7 | Tenant create / suspend / delete | ⬜ Not started | Needs a decision (D-34 epic) |
| F8 | Tenant detail page | ◐ Partly | Policy counts merged (PR #190, live check pending); **plan** needs billing (F34) |
| F9 | Tenant → relay pool assignment | ⬜ Not started | Needs D-03 |
| F10 | Region / residency constraint | ⬜ Not started | Needs D-03 |
| F11 | Failover pool per tenant | ⬜ Not started | Needs D-03 (after F9) |
| F12 | Per-tenant quotas | ⬜ Not started | Needs a decision |
| F13 | Certificate inventory by expiry | ✅ Done | — |
| F14 | Expiry alerting | ◐ Partly | Banner / filter in place (Overview "Needs attention" + certificate tile, Certificates window 24 h – 365 d); **sending** alerts needs a channel/recipients decision (or F26) |
| F15 | Revocation + CRL/OCSP status | ◐ Partly | CRL status merged (`GET /provider/pki`, PR #190, live check pending); **OCSP**: decide whether it is needed (agents use CRLs) |
| F16 | CA hierarchy view | 🧪 Merged | Root → Intermediate → workspace tree merged (`GET /provider/pki`, PR #190); live check pending |
| F17 | Append-only provider action log | 🧪 Merged | DB-enforced append-only + before/after merged (PR #192, migration `051_`); live check pending |
| F18 | Audit search and filter | ✅ Done | — |

### Can have (Phase 2 and 3): 2 of 22 done

| ID | Feature | Status | What's left |
|---|---|---|---|
| F19 | Provider admin roles | ◐ Partly | Two roles today; more need a decision |
| F20 | Break-glass account | ◐ Partly | `recover-admin` CLI exists; decide whether it counts |
| F21 | MFA on the provider console | ✅ Done | — |
| F22 | Tunnel success rate per relay/tenant | ⬜ Not started | Needs relay/connector counters (large) |
| F23 | Active-session timeseries | ⬜ Not started | Prometheus exports some of the data (PENDING-10); nothing stores or shows it over time |
| F24 | Connector online/offline roster | ✅ Done | — |
| F25 | Control-plane health | 🧪 Merged | Overview card over the PENDING-10 checks (`GET /provider/health`, super-admin, sanitized) merged (PR #190); live check pending |
| F26 | Alert rules + webhook/email | ⬜ Not started | Medium: rules, evaluator, dispatch |
| F27 | Incident timeline | ⬜ Not started | Medium, after F26 |
| F28 | Relay config push, staged rollout | ⬜ Not started | Large |
| F29 | Relay version pinning and rollback | ⬜ Not started | Large |
| F30 | Tenant migration between pools | ⬜ Not started | Needs D-03 |
| F31 | Read-only support view (impersonation) | ◐ Partly | Audited read-only view exists; impersonation needs a decision |
| F32 | Usage collection | ⬜ Not started | Large: nothing is metered |
| F33 | Per-tenant usage report | ⬜ Not started | After F32 |
| F34 | Billing plans and invoicing | ⬜ Not started | Needs a decision |
| F35 | Provider API + API keys | ◐ Partly | REST API exists; API keys need a decision |
| F36 | Tenant self-signup / provisioning | ◐ Partly | First sign-in creates a workspace; no provider-side workflow |
| F37 | Multi-provider / reseller hierarchy | ⬜ Not started | Needs a decision |
| F38 | Compliance export | ◐ Partly | Audit export CSV/JSON merged (client-side, ≤ 10,000 rows, PR #190, live check pending); full SOC 2 bundle is larger |
| F39 | Custom dashboards and saved views | ⬜ Not started | Large |
| F40 | SLA tracking per tenant | ⬜ Not started | Large, after F22, F23 and F25 |

**Quick wins** (controller read API + console view, no decision needed): F8, F14, F15, F16 (Must have); F25, F38 (Can have).
All merged in PR #190 (F14's quick part was already in place). Boxes are ticked only after the live check.

### Remaining Must have: every item needs a decision

| Decision | Unblocks | Recommendation |
|---|---|---|
| **D-03** relay pools / regions | F4 (region), F9, F10, F11 (and F30) | The highest-leverage decision |
| **D-34** tenant suspension epic + deletion semantics (Q4) | F7 | Separate epic |
| Quota model | F12 | Which limits (connectors, users, sessions), where enforced |
| Billing model (F34) | F8 (plan) | Wait for F34 |
| Alert channel and recipients | F14 (sending) | Daily email to super-admins via existing SMTP, or fold into F26 |
| OCSP needed? | F15 (OCSP) | Close as CRL-based by design |

## Checklist

### Must have (Phase 1)

*Relay fleet*
- [x] **F1** Relay enrollment via one-time bootstrap token ✅
- [x] **F2** Cert issuance to relay from provider CA ✅
- [x] **F3** Relay decommission / revoke ✅
- [ ] **F4** Relay list with status, version, region, last heartbeat ◐ (region missing)
- [ ] **F5** Capacity view — active sessions, bandwidth, headroom 🧪 (bandwidth merged, PR #193; live check pending)
- [x] **F6** Drain mode (no new placements, bleed off existing) ✅ (Phase D, PR #156; AT-D live PASS)

*Tenants*
- [ ] **F7** Tenant create / suspend / delete ⬜
- [ ] **F8** Tenant detail page — connectors, users, policies count, plan ◐ (policy counts merged, PR #190; plan missing)
- [ ] **F9** Tenant → relay pool assignment ⬜
- [ ] **F10** Region / residency constraint on placement ⬜
- [ ] **F11** Failover pool per tenant ⬜
- [ ] **F12** Per-tenant quotas (connectors, users, sessions) ⬜

*PKI*
- [x] **F13** Certificate inventory with expiry, sorted by days remaining ✅
- [ ] **F14** Expiry alerting (threshold-based) ◐ (banner/filter in place; no alerts sent)
- [ ] **F15** Revocation action + CRL/OCSP status ◐ (CRL status merged, PR #190; no OCSP)
- [ ] **F16** CA hierarchy view 🧪 (merged, PR #190; live check pending)

*Audit*
- [ ] **F17** Append-only provider action log (actor, action, target, before/after, timestamp) 🧪 (merged, PR #192; live check pending)
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
- [ ] **F25** Control-plane health (API, PKI engine, policy store, DB) 🧪 (Overview card merged, PR #190; live check pending)
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
- [ ] **F38** Compliance export (SOC 2 evidence, audit bundles) ◐ (audit CSV/JSON export merged, PR #190; no SOC 2 bundle)
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
| F4 | Relay list with status, version, region, last heartbeat | ◐ | ⚖️ 🔴 | Status, version, last heartbeat, address scope, capacity tier: done (Sprint 21 Relays page). **Region** needs the region/pool model (D-03, not in Sprint 22). | |
| F5 | Capacity view — active sessions, bandwidth, headroom | 🧪 | — (live check) | Connections / max and capacity tier shown; `relay_connections_total` and `relay_capacity_utilization_ratio` exported to Prometheus (PENDING-10). **Bandwidth** merged (PR #193): relay counts bytes per direction as they flow and reports cumulative counters in heartbeat fields 9/10 (`optional`, additive); migration `052_relay_bandwidth.sql`; `RecordHeartbeat` derives the rate from the previous stored sample (late heartbeats ignored; counter decrease or uptime < gap = restart, no rate); `GET /provider/relays[/{id}]` returns `bandwidth` (null = not reported / revoked; null rates = measuring); console Bandwidth column and detail card. Rate is an average over the heartbeat DB-write window (5 min default). Not in scope: a bandwidth ceiling ("bandwidth headroom" needs a configured link capacity), Prometheus throughput metric. | |
| F6 | Drain mode | ✅ | — | Sprint 22 **Phase D** (D-32), PR #156: drained relay leaves the published list, last-eligible-relay guard, `043_relay_drain.sql`; AT-D live PASS (incl. AT-D6 client data plane); follow-ups FU-6, FU-7, FU-14. | |
| F7 | Tenant create / suspend / delete | ⬜ | ⚖️ 🔴 | Tenants are created by first sign-in only. Suspension is a separate epic (D-34); deletion semantics are open (decisions doc Q4). | |
| F8 | Tenant detail page — connectors, users, policies count, plan | ◐ | ⚖️ (plan) | Done: connectors, shields, remote networks, users, devices with counts (Sprint 21, audited `tenant.read`); **policy counts** merged (PR #190, live check pending). Missing: **plan** (needs F34). | |
| F9 | Tenant → relay pool assignment | ⬜ | ⚖️ 🔴 | Needs pool entities and a per-tenant relay list (D-03). | |
| F10 | Region / residency constraint on placement | ⬜ | ⚖️ 🔴 | Needs regions on relays/tenants and a filter in the relay list builder (D-03). | |
| F11 | Failover pool per tenant | ⬜ | ⚖️ 🔴 | Connector-side failover between relays exists; no designated secondary pool (depends on F9). | |
| F12 | Per-tenant quotas | ⬜ | ⚖️ 🟡 | Needs quota entities and enforcement points (connector/user/session creation). | |
| F13 | Certificate inventory with expiry, sorted by days remaining | ✅ | — | Certificates page: root, intermediate, workspace CAs and agent certificates, ordered by `not_after`, expiry buckets (Sprint 21). | |
| F14 | Expiry alerting (threshold-based) | ◐ | ⚖️ 🟡 | Expiry buckets (<24h, <7d, <30d), Overview "Needs attention" and the Certificates window filter are in place. No alert is sent: needs a channel/recipient decision (a minimal daily super-admin email over the existing SMTP config) or F26. | |
| F15 | Revocation action + CRL/OCSP status | ◐ | ⚖️ (OCSP) | Revoke actions exist (relays: console; connectors/shields: admin UI); workspace and relay CRLs are published; **CRL status per signer** merged (`GET /provider/pki`, PR #190, live check pending). Missing: **OCSP**. Clients, connectors and relays check CRLs; nothing would query an OCSP responder without agent changes. Recommendation: close as CRL-based by design. | |
| F16 | CA hierarchy view | 🧪 | — (live check) | Root → Intermediate → workspace CA tree merged (`GET /provider/pki`, PR #190): parents linked by SKI/AKI, paginated workspace CAs, unlinked CAs listed separately. | |
| F17 | Append-only provider action log | 🧪 | — (live check) | `provider_audit_logs` has actor, action, target, details, IP, timestamp; mutations audited (transactionally for revoke/delete/create). Merged (PR #192): migration `051_provider_audit_append_only.sql` refuses UPDATE, DELETE and TRUNCATE (incl. TRUNCATE ... CASCADE) with SQLSTATE 42501; `provider.WithChange` adds `details.before` / `details.after` (state fields only, never credentials) to operator create/bootstrap, role change, disable/enable, password reset/change, TOTP enrol/reset, recover-admin, relay create/revoke/delete/drain/resume. Events (login, logout, MFA verify, rate limits, tenant.read) carry no before/after. Limit: the table owner or a superuser can still disable the triggers (needs a non-owner app role, out of scope). | |
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
| F25 | Control-plane health | 🧪 | — (live check) | Health subsystem with `/livez`, `/readyz`, `/healthz`, `/health` and internal diagnostics (PENDING-10); Overview **Control plane** card over `GET /provider/health` (super-admin, sanitized messages) merged (PR #190). | |
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
| F38 | Compliance export (SOC 2 evidence, audit bundles) | ◐ | 🟡 | **Audit export merged** (PR #190, live check pending): CSV/JSON from `GET /provider/audit` in the browser, applied filters and order kept, keyset pages without repeats, ≤ 10,000 rows with an explicit limit message, CSV formula cells neutralised. Missing: SOC 2 evidence bundle. | |
| F39 | Custom dashboards and saved views | ⬜ | 🔴 | — | |
| F40 | SLA tracking per tenant | ⬜ | 🔴 | Depends on monitoring (F22, F23, F25). | |

---

## Rules that apply to every feature

From `CLAUDE.md` and the Decision Record (binding):
- A feature that needs a new architectural choice (⚖️) stops for a decision first; don't decide it in code.
- Schema changes are new numbered files in `controller/migrations/` (next: `053_`); label the PR `[schema: reset DB]`.
- Console writes are limited to D-30 (auth, operator management, relay create/revoke/delete/drain/resume); every new console read or write goes into `api-surface.test.ts` on purpose.
- Provider read APIs never return secrets; query services in `internal/providerquery/` don't import `net/http`.
- Tests first; DB-backed tests must run (a skipped DB test is a failure); live check before ticking a box.

## Changelog

- 2026-10-29: created from the 2026-10-26 feature list; statuses checked against `fixed-pendings` `4cc7ffcc`.
- 2026-11-06: status snapshot tables added (Must have / Can have); statuses checked against `main` `520851d4`. F6 done (Phase D, PR #156, AT-D live PASS). F25 partly done and F5/F23 notes updated (PENDING-10 health checks and Prometheus metrics). Progress 8 / 40.
- 2026-11-06: quick-win batch implemented (branch `feat/provider-quick-wins`, one PR): F8 policy counts, F15/F16 `GET /provider/pki` (CA tree by key identifiers + CRL status per signer), F25 `GET /provider/health` (super-admin, sanitized), F38 audit export (CSV/JSON, client-side). F14's quick part was already in place (corrected). Follow-up: `resources` has no `tenant_id` index; the F8 count uses one grouped aggregate per statement instead (an index needs a migration).
- 2026-11-11: refreshed against `main` `37a66bb9`. Merged: quick wins (PR #190: F8 policy counts, F15 CRL status, F16 CA tree, F25 health card, F38 audit export), F17 append-only audit + before/after (PR #192, migration `051_`), F5 relay bandwidth (PR #193, migration `052_`, heartbeat fields 9/10). New status 🧪 = merged, live check pending ([[Pending-Live-Tests]]). Every remaining Must-have item now needs a decision (table under the snapshot). Progress unchanged at 8 / 40 until the live session.
