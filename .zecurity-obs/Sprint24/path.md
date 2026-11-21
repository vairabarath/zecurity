---
type: sprint
sprint: 24
title: Provider Dashboard — monitoring and alerting (F23, F26; F27 stretch)
status: decided and signed off (2026-11-20); implementation may start
created: 2026-11-20
team:
  - Person 1 = Barath (M2): F23 — R1 per-workspace relay counters, M1 metric store, M2 console charts
  - Person 2 = Sathiya (M1): X1 Sprint 23 follow-ups; F26 — A1 rules and evaluator, A2 delivery, A3 console; T1 (F27) stretch
baseline: main 81aabc82 (Sprint 23 closed; Must have 18 / 18; Can have 3 / 22)
---

# Sprint 24 — Monitoring and alerting

## Goal

The Must-have list is done (18 / 18). Sprint 24 starts on the **Can-have** list with the two features that make the fleet operable day to day:

| Feature | Today | Phases (owner) |
|---|---|---|
| **F23** Active session timeseries | ⬜ live gauges only; nothing stored over time | **R1** per-workspace relay counters, **M1** store, **M2** charts (Barath) |
| **F26** Alert rules + webhook/email | ⬜ only F14's fixed daily email | **A1** rules and evaluator, **A2** delivery, **A3** console (Sathiya) |
| **F27** Incident timeline (stretch) | ⬜ | **T1** (Sathiya, only if A1–A3 land early) |
| Sprint 23 follow-ups | found in the Sprint 23 acceptance run | **X1** (Sathiya) |

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions:** `docs/provider-dashboard-architecture-decisions.md` → **amendment 2026-11-20 (D-48 … D-54)**, on top of D-01 … D-47. Scope discussion: `Sprint24/Scope-Draft.md` (PR #232).

---

## Decisions (Phase 0 — done 2026-11-20)

Decided by the product owner (Barath) after review of the scope draft; full text in the Decision Record.

| ID | Decision | Phase |
|---|---|---|
| **D-48** | Scope: F23 incl. per-tenant relay bandwidth + F26 + X1; F27 stretch only. | all |
| **D-49** | Postgres, no new TSDB; **1-min samples 7 days, hourly roll-ups 400 days**; gaps stay gaps (never zero-filled). | M1 |
| **D-50** | **Platform** vs **tenant** alert scopes separate in the schema (`scope`; tenant rows need `tenant_id`, CHECK); both delivered to the provider only this sprint; channels may be limited to one scope; relay-ops see platform relay alerts only, no tenant names. Tenant-facing delivery later: **admin UI first**, tenant webhooks only on customer demand. | A1, A2, A3 |
| **D-51** | F14 daily email unchanged (D-36). | — |
| **D-52** | **One generic versioned JSON webhook**, no vendor SDKs; HMAC-SHA256 over timestamp + body with replay window; sealed secret, never returned; HTTPS outside dev; SSRF guard (re-checked at connect); outbox retries. | A2 |
| **D-53** | Rules and channels: **super-admin only**, reason + before/after audit, never the secret; built-in rule kinds only. | A1, A2, A3 |
| **D-54** | Relay reports cumulative bytes and active streams **per trust domain** (additive proto field 13); unknown/revoked trust domains ignored and logged; per-tenant relay series super-admin only; older relays "Not reported". | R1, M1 |

**Amended:** D-30 (new console writes). **Extended:** D-40 (per-tenant relay data hidden from relay-ops). **Unchanged:** D-02, D-06, D-12, D-14, D-17, D-36.

**X1 answers (Barath, 2026-11-20):**
- X1.1 SCIM at the users limit: the **mapping probe doesn't take a users slot** (one probe at a time per workspace, no enrollment or login rights); real SCIM creates at the limit get a clear `quota_exceeded` SCIM error.
- X1.3 SCIM while suspended: **refused with 403**; the IdP reconciles after resume.

- X1.4 S4 export amendment: **confirmed**; written into the Decision Record (amendment 2026-11-20, D-46 refinement).

---

## Team assignments

**Person 1 — Barath (M2):** R1 → M1 → M2. Phase file: `Member1-Barath/Phases-R1-M1-M2-Timeseries.md`.
**Person 2 — Sathiya (M1):** X1 → A1 → A2 → A3 → (T1). Phase file: `Member2-Sathiya/Phases-X1-A1-A3-Alerts.md`.

Both: review each other's PRs; the sprint acceptance run (live, on a reset lab DB, current `main`).

### Phases and order

| Phase | Owner | Feature | Depends on | Migration |
|---|---|---|---|---|
| **X1** Sprint 23 follow-ups | Sathiya | F7, F12 | all items decided (2026-11-20) | none expected |
| **R1** Per-workspace relay counters | Barath | F23 | D-54 | none (proto + relay; controller side lands with M1) |
| **M1** Metric store + read API | Barath | F23 | D-49; per-tenant relay series need R1 | `068_metric_samples.sql` |
| **M2** Console charts | Barath | F23 | M1 merged | none |
| **A1** Alert rules + evaluator | Sathiya | F26 | D-50, D-53 | `069_alerts.sql` |
| **A2** Delivery (webhook, email) | Sathiya | F26 | A1 merged, D-52 | in `069_` (channels, deliveries) + `070_alert_delivery_queue` (D-52 refined: own queue, not the outbox) |
| **A3** Alerts console | Sathiya | F26 | A2 merged | none |
| **T1** Incident timeline (stretch) | Sathiya | F27 | A1 merged | none |

```
Barath:  R1 (relay + proto) ─┐
         M1 (store, API) ────┴──► M2 (charts)
Sathiya: X1 ──► A1 (rules, evaluator) ──► A2 (delivery) ──► A3 (console) ──► (T1)
```

- **M1 and A1 are independent** and run in parallel. A1's relay capacity rule reads the same relay values as M1's sampler: **one shared reader** (in `internal/providerquery` or `internal/relay`), agreed in the first of the two PRs; the second reuses it.
- R1 and M1 can be built in parallel; M1's per-tenant relay series stay "Not reported" until R1 is merged.
- `069_alerts.sql` holds A1 and A2 tables in one file (rules, states, events, channels, deliveries). If A2 needs a schema change after `069_` is merged, use the next free number; never edit a merged migration.
- Migration numbers checked against `main` (latest `067_tenant_first_admin_invite.sql`).

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `proto/relay/v1/relay.proto` | Barath (R1) | **Additive only**: field 13 and the `WorkspaceTraffic` message; `buf generate`; never renumber. |
| `controller/internal/relay/heartbeat.go` (B2 bandwidth code) | Barath (R1, M1) | Per-workspace rates next to `bandwidthSample`; same window and restart detection as B2. |
| Relay state readers (connections, bandwidth, headroom) | Barath (M1), Sathiya (A1) | One reader, shared: **`providerquery.RelayLoads`** (added by A1). M1's sampler reuses it; don't add a second relay-load query. |
| `controller/cmd/server/main.go` | both | Own block per phase (metric job, evaluator, delivery worker, routes); rebase before merge. |
| `controller/internal/provider/authz.go` (`decide()`) | both | New actions: `metrics.read` (M1; relay series relay-ops + super-admin, tenant series super-admin); `alert.read`, `alert.rule.*`, `alert.channel.*` (A1–A3; super-admin; relay-ops `alert.read` platform relay alerts only). |
| `provider-console/src/api/api-surface.test.ts` | both | Every new console write added on purpose (D-30 amended). |
| `provider-console/src/pages/Home.tsx` (Overview), navigation | both | M2 sparkline; A3 firing-alerts badge and Alerts nav entry. One section each. |
| Relay detail / tenant detail pages | Barath (M2), Sathiya (A3) | M2 adds chart sections; A3 may link active alerts. Separate sections. |
| `controller/migrations/` | both | Reserved `068_` (M1), `069_` (A1/A2, merged), `070_` (A2 delivery queue); `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-48 … D-54 are binding**, with D-01 … D-47. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **Proto changes are additive only** (R1: field 13). Older relays must keep working against the new controller and read "Not reported".
- **Tenant boundary (D-50):** tenant alert rows always carry `tenant_id`; every tenant-scoped read filters by it; platform data never reaches a tenant-scoped query. Relay-ops never see tenant names or per-tenant series (D-40).
- **Every new console write**: server-side authorization in `decide()`; a **reason**; audit with before/after (F17); an entry in `api-surface.test.ts`. Never log, audit or return webhook secrets, SMTP credentials or signing keys.
- **Webhooks (D-52):** sealed secret, HMAC over timestamp + body, HTTPS outside dev, SSRF guard checked at connect time (DNS rebinding), bounded response read, short timeouts; the payload carries no secrets or tenant content.
- **Single controller (D-02):** the metric job, evaluator and delivery worker are process-local; no cross-replica locking.
- **Fail safe:** a metric read error produces a gap, never a zero; an evaluator read error leaves alert state unchanged (no false "resolved") and is logged; a delivery failure is retried and recorded.
- **Bounded growth:** retention pruning (D-49) runs with the sampler; `alert_events` is append-only, with a retention of its own only if a later decision says so.
- Schema: new numbered files only, no migration framework, recreate the local DB (`docs/database-development.md`).
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` before its tracker box is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env: 0 failures, and only the two known hard-coded skips (a skipped DB test is a failed case).
- **Relay (R1):** `cargo build`, `cargo test`, `cargo clippy` in `relay/`; `buf generate` with no diff besides the new field.
- **Provider console:** `npm test`, `npm run lint`, `npm run build`.
- **Mutation checks** for each new guard:
  - tenant alert without `tenant_id` refused by the DB;
  - relay-ops reading a tenant series or a tenant alert → 403;
  - unknown trust domain in field 13 not stored;
  - webhook to a private address refused (and a DNS name resolving to one);
  - evaluator read error doesn't resolve a firing alert;
  - counter decrease (relay restart) doesn't produce a negative or huge rate.
- Phase-specific tests are in the phase files.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and latest migration, use traffic probes, keep the logs.

- **F23:**
  - drive relay traffic for **two tenants at once**: per-tenant relay rates add up to the relay total (B2) and match each client's measured download;
  - a relay built before R1 reads "Not reported" for per-tenant series;
  - stored 1-minute samples match the live values; the hourly roll-up is correct; samples older than the retention are pruned (test retention setting);
  - relay detail, tenant detail and Overview charts show 1 h / 24 h / 7 d / 30 d;
  - relay-ops see relay series but no per-tenant series or tenant names.
- **F26:**
  - stop a relay, push one over capacity, empty a pool → each **platform** alert fires once, email and webhook arrive (signature verified, payload fields as D-52), no repeat while firing, **resolved** when fixed;
  - stop a tenant's connector → a **tenant** alert carrying that `tenant_id`;
  - a channel limited to platform alerts receives no tenant alert;
  - webhook endpoint down → retries recorded, delivered when it is back;
  - a webhook URL to a private address is refused (outside dev mode);
  - relay-ops can't manage rules or channels and see no tenant alerts;
  - every rule and channel change is audited with a reason, never the secret.
- **X1:** each answered item checked live (SCIM at the limit, client quota message, SCIM while suspended).
- **F27 (if built):** the timeline shows alert events, audit rows and relay/pool changes in order, filterable.

Tracker: F23 and F26 (and F27 if built) ticked only after their live checks.

## Checklist

- [x] Phase 0: D-48 … D-54 decided and written into the Decision Record (2026-11-20)
- [x] Documents reviewed (scope draft, Sprint 24 files, Decision Record amendment) — signed off by Barath 2026-11-20; implementation may start
- [x] X1 — Sprint 23 follow-ups (Sathiya) — merged (#233, `53e5d01e`) — live-checked 2026-11-20 (PASS)
- [ ] R1 — Per-workspace relay counters (Barath)
- [ ] M1 — Metric store + read API (Barath)
- [ ] M2 — Console charts (Barath)
- [x] A1 — Alert rules + evaluator (Sathiya) — merged (#234) — live-checked 2026-11-20 (PASS)
- [x] A2 — Delivery: webhook, email (Sathiya) — merged (#235) — live-checked 2026-11-20 (PASS)
- [x] A3 — Alerts console (Sathiya) — merged (#236) — live-checked 2026-11-20 (PASS)
- [ ] T1 — Incident timeline (Sathiya, stretch)
- [ ] Sprint acceptance run (both); tracker F23, F26

## Post-Sprint Fixes

_None yet._

## Out of scope

F22 tunnel success rate; tenant-facing alert delivery (admin UI first, later; tenant webhooks only on demand); vendor-specific integrations (Slack app, PagerDuty SDK); F32/F33 metering, F34 billing; F28/F29 relay config push and version pinning; F19, F31, F35, F37; F39 custom dashboards, F40 SLA tracking.
