---
type: tracker
title: Provider Dashboard — Feature Tracker
updated: 2026-10-29
owners: [Barath (M2), Sathiya (M1)]
source: Feature list from Barath, 2026-10-26 (Phase 1 / 2 / 3)
baseline: fixed-pendings 4cc7ffcc (Sprint 22 T, A, E, X merged; D next)
tags:
  - provider-dashboard
  - tracker
---

# Provider Dashboard — Feature Tracker

> **How to use:** tick a box when a feature is **merged and live-checked**. Put the owner's name in the table below when someone picks a feature up. "Must have" = Phase 1 (F1–F18): *"you can't run the product without it."* "Can have" = Phase 2 and 3 (F19–F40): operability and business features.
>
> **Status key:** ✅ done · ◐ partly done (what's missing is in the notes) · 🚧 in progress · ⬜ not started
> **Effort key:** 🟢 **Quick** = controller API + console only (no schema, no agent change, no new decision) · 🟡 **Medium** = needs a migration or a background job · 🔴 **Large** = needs connector / relay / shield / client changes or new infrastructure · ⚖️ **Decision** = needs a Decision Record decision first (don't decide it in code)

## Tracking

**Progress:** Must have 5 / 18 done · Can have 2 / 22 done · **Total 7 / 40**

### Must have (Phase 1)

*Relay fleet*
- [x] **F1** Relay enrollment via one-time bootstrap token ✅
- [x] **F2** Cert issuance to relay from provider CA ✅
- [x] **F3** Relay decommission / revoke ✅
- [ ] **F4** Relay list with status, version, region, last heartbeat ◐ (region missing)
- [ ] **F5** Capacity view — active sessions, bandwidth, headroom ◐ (bandwidth missing)
- [ ] **F6** Drain mode (no new placements, bleed off existing) 🚧 Sprint 22 Phase D

*Tenants*
- [ ] **F7** Tenant create / suspend / delete ⬜
- [ ] **F8** Tenant detail page — connectors, users, policies count, plan ◐ (policies count, plan missing)
- [ ] **F9** Tenant → relay pool assignment ⬜
- [ ] **F10** Region / residency constraint on placement ⬜
- [ ] **F11** Failover pool per tenant ⬜
- [ ] **F12** Per-tenant quotas (connectors, users, sessions) ⬜

*PKI*
- [x] **F13** Certificate inventory with expiry, sorted by days remaining ✅
- [ ] **F14** Expiry alerting (threshold-based) ◐ (buckets shown, no alerts sent)
- [ ] **F15** Revocation action + CRL/OCSP status ◐ (no CRL status view, no OCSP)
- [ ] **F16** CA hierarchy view ◐ (CAs listed, no hierarchy view)

*Audit*
- [ ] **F17** Append-only provider action log (actor, action, target, before/after, timestamp) ◐ (not DB-enforced, no before/after)
- [x] **F18** Audit search and filter ✅

### Can have (Phase 2)

*Access control*
- [ ] **F19** Provider admin roles (owner / operator / read-only / support) ◐ (two roles today)
- [ ] **F20** Break-glass account with forced audit entry ◐ (`recover-admin` CLI, audited)
- [x] **F21** MFA enforcement on provider console ✅

*Monitoring*
- [ ] **F22** Tunnel establishment success rate per relay and per tenant ⬜
- [ ] **F23** Active session count timeseries ⬜
- [x] **F24** Connector online/offline roster per tenant ✅
- [ ] **F25** Control-plane health (API, PKI engine, policy store, DB) ⬜
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
- [ ] **F38** Compliance export (SOC 2 evidence, audit bundles) ⬜
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
| 4 | **F25** Control-plane health | Read API checking DB, Valkey, PKI (Intermediate loaded, controller cert expiry) and policy compiler; status card on Overview | |
| 5 | **F38** Compliance export (lite) | Export the provider audit log (filtered, JSON/CSV) as a read; full SOC 2 bundle stays ⬜ | |
| 6 | **F14** Expiry alerting (lite) | Overview banner and Certificates filter for "expiring within N days"; sending alerts waits for F26 | |

---

## Feature details

### Must have (Phase 1)

| ID | Feature | Status | Effort for what's left | Evidence / what's missing | Owner |
|----|---------|--------|------------------------|---------------------------|-------|
| F1 | Relay enrollment via one-time bootstrap token | ✅ | — | Sprint 20 provisioning (single-use token, atomic burn); console Add relay with install command, Sprint 22 Phase E (PR #151); token lifecycle tests (expired, concurrent). | |
| F2 | Cert issuance to relay from provider CA | ✅ | — | Relay certificates signed by the platform Intermediate, SANs limited to the stored allowlist; renewal with key continuity (Sprint 20/21). | |
| F3 | Relay decommission / revoke | ✅ | — | API hardened in Phase A (PR #142: reason, typed name, super-admin only); console revoke/delete in Phase X (PR #154); CRL + connector migration live-checked. | |
| F4 | Relay list with status, version, region, last heartbeat | ◐ | ⚖️ 🔴 | Status, version, last heartbeat, address scope, capacity tier: done (Sprint 21 Relays page). **Region** needs the region/pool model (D-03, not in Sprint 22). | |
| F5 | Capacity view — active sessions, bandwidth, headroom | ◐ | 🔴 | Connections / max and capacity tier shown. **Bandwidth** isn't measured anywhere: needs relay heartbeat fields (additive proto + relay change). | |
| F6 | Drain mode | 🚧 | 🟡 | Sprint 22 **Phase D** (D-32): drained relay leaves the published list, last-eligible-relay guard, `043_relay_drain.sql`, no connector/relay/proto change. | |
| F7 | Tenant create / suspend / delete | ⬜ | ⚖️ 🔴 | Tenants are created by first sign-in only. Suspension is a separate epic (D-34); deletion semantics are open (decisions doc Q4). | |
| F8 | Tenant detail page — connectors, users, policies count, plan | ◐ | 🟢 (policies) / ⚖️ (plan) | Done: connectors, shields, remote networks, users, devices with counts (Sprint 21, audited `tenant.read`). Missing: **policy counts** (quick), **plan** (needs F34). | |
| F9 | Tenant → relay pool assignment | ⬜ | ⚖️ 🔴 | Needs pool entities and a per-tenant relay list (D-03). | |
| F10 | Region / residency constraint on placement | ⬜ | ⚖️ 🔴 | Needs regions on relays/tenants and a filter in the relay list builder (D-03). | |
| F11 | Failover pool per tenant | ⬜ | ⚖️ 🔴 | Connector-side failover between relays exists; no designated secondary pool (depends on F9). | |
| F12 | Per-tenant quotas | ⬜ | ⚖️ 🟡 | Needs quota entities and enforcement points (connector/user/session creation). | |
| F13 | Certificate inventory with expiry, sorted by days remaining | ✅ | — | Certificates page: root, intermediate, workspace CAs and agent certificates, ordered by `not_after`, expiry buckets (Sprint 21). | |
| F14 | Expiry alerting (threshold-based) | ◐ | 🟢 (lite) / 🟡 | Expiry buckets (<24h, <7d, <30d) visible in the console. No alert is sent; dispatch depends on F26. | |
| F15 | Revocation action + CRL/OCSP status | ◐ | 🟢 (CRL status) / 🔴 (OCSP) | Revoke actions exist (relays: console; connectors/shields: admin UI); workspace and relay CRLs are published. Missing: **CRL status view**, **OCSP**. | |
| F16 | CA hierarchy view | ◐ | 🟢 | CAs appear in the certificate inventory; no Root → Intermediate → workspace hierarchy view. | |
| F17 | Append-only provider action log | ◐ | 🟡 | `provider_audit_logs` has actor, action, target, details, IP, timestamp; mutations audited (transactionally for revoke/delete/create). Missing: **DB-enforced append-only** (trigger or privileges) and **before/after** fields. | |
| F18 | Audit search and filter | ✅ | — | Audit page with filters and pagination (Sprint 21). | |

### Can have (Phase 2)

| ID | Feature | Status | Effort for what's left | Evidence / what's missing | Owner |
|----|---------|--------|------------------------|---------------------------|-------|
| F19 | Provider admin roles (owner / operator / read-only / support) | ◐ | ⚖️ 🟡 | Two roles today: super-admin and relay-ops (D-05, D-31), enforced server-side in `decide()`. More roles need a decision on the action matrix. | |
| F20 | Break-glass account with forced audit entry | ◐ | ⚖️ 🟢 | `zecurity-controller provider recover-admin` (Phase T) on the controller host, audited as `provider_user.recover_admin`. Decide whether this counts, or a standing break-glass account is wanted. | |
| F21 | MFA enforcement on provider console | ✅ | — | Super-admin TOTP, Phase T (PR #138); live AT-T PASS (PR #147). | |
| F22 | Tunnel establishment success rate per relay and per tenant | ⬜ | 🔴 | Needs success/failure counters reported by relays/connectors. | |
| F23 | Active session count timeseries | ⬜ | 🟡 / 🔴 | Relay connection count is in each heartbeat (not stored over time); per-tenant sessions need agent data. | |
| F24 | Connector online/offline roster per tenant | ✅ | — | Tenant detail lists connectors with status and last heartbeat (Sprint 21). | |
| F25 | Control-plane health | ⬜ | 🟢 | Only a basic `/health` endpoint today. | |
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
| F38 | Compliance export (SOC 2 evidence, audit bundles) | ⬜ | 🟢 (audit export) / 🟡 | Audit data exists; no export. | |
| F39 | Custom dashboards and saved views | ⬜ | 🔴 | — | |
| F40 | SLA tracking per tenant | ⬜ | 🔴 | Depends on monitoring (F22, F23, F25). | |

---

## Rules that apply to every feature

From `CLAUDE.md` and the Decision Record (binding):
- A feature that needs a new architectural choice (⚖️) stops for a decision first; don't decide it in code.
- Schema changes are new numbered files in `controller/migrations/` (`043_` is reserved for Phase D); label the PR `[schema: reset DB]`.
- Console writes are limited to D-30 (auth, operator management, relay create/revoke/delete/drain/resume); every new console read or write goes into `api-surface.test.ts` on purpose.
- Provider read APIs never return secrets; query services in `internal/providerquery/` don't import `net/http`.
- Tests first; DB-backed tests must run (a skipped DB test is a failure); live check before ticking a box.

## Changelog

- 2026-10-29: created from the 2026-10-26 feature list; statuses checked against `fixed-pendings` `4cc7ffcc`.
