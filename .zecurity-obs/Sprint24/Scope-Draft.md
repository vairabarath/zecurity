---
type: sprint-scope-draft
sprint: 24
title: Provider Dashboard — monitoring and alerting (F23, F26; F27 stretch)
status: SIGNED OFF by Barath 2026-11-20 — superseded by Sprint24/path.md and the Decision Record amendment 2026-11-20 (D-48 … D-54); kept as the scope discussion record
baseline: main 81aabc82 (Sprint 23 closed; Must have 18 / 18; Can have 3 / 22)
---

# Sprint 24 — scope draft

> **Status: signed off 2026-11-20.** The binding plan is `Sprint24/path.md` and the phase files; the decisions are in the Decision Record (amendment 2026-11-20, D-48 … D-54). This file is the discussion record.

## Goal

The Must-have list is done (18 / 18). Sprint 24 starts on the **Can-have** list with the two features that make the fleet operable day to day:
- **F26** alert rules with email and webhook delivery;
- **F23** active-session timeseries.

Today an operator has to keep the console open to notice a problem: a relay going offline or over capacity, a pool without an eligible relay, or a degraded control plane. The only proactive message is the daily certificate-expiry email (F14). F26 closes that gap. F23 adds history, so "how busy was this relay last week" and "when did this tenant's sessions drop" have answers. Both also lay groundwork: F27 (incident timeline) needs F26's alert history, and F40 (SLA tracking) needs F23's stored series.

One part needs an agent change: **per-tenant relay bandwidth** (in scope, Barath 2026-11-20). The relay reports bytes and streams per workspace in its heartbeat. This is an **additive** proto field; no field is renumbered. Everything else is controller and console only.

| Feature | Today | Proposed phases | Size |
|---|---|---|---|
| **F23** Active session timeseries | ⬜ Live gauges only (Prometheus, PENDING-10; relay heartbeat `connection_count`; B2 60 s bandwidth samples). Nothing is stored over time. | **R1** per-workspace relay counters (relay + proto), **M1** store, **M2** console charts | 🔴 Large (relay change) |
| **F26** Alert rules + webhook/email | ⬜ Only F14's fixed daily email | **A1** rules and evaluator, **A2** delivery, **A3** console | 🟡 Medium |
| **F27** Incident timeline (stretch) | ⬜ | **T1** timeline view | 🟡 Medium, only if A1–A3 land early |
| **Sprint 23 follow-ups** | Found in the Sprint 23 acceptance run | **X1** | 🟢 Small |

## What already exists (reuse, don't rebuild)

| Building block | Where | Used by |
|---|---|---|
| Relay heartbeat stores `connection_count`, `max_connections`, `bytes_to_client_per_sec`, `bytes_to_connector_per_sec` (B2, 60 s window in the lab) | `controller/internal/relay/store.go`, migrations 023, 063 | M1 samples, A1 relay rules |
| Relay link capacity and headroom (D-42) | `relays.link_capacity_bps`, `providerquery` headroom | A1 "near / over capacity" rule |
| Relay pool state (`no_eligible_relays`, failed over) | `relaypool.EffectivePool`, F11-1 | A1 pool rule |
| Control-plane health | `controller/internal/health`, `GET /provider/health` (F25) | A1 health rule |
| Client device sessions (`session_idle_until`, `session_max_until`) | `controller/internal/devicesession` | M1 per-tenant active sessions, controller-side |
| Daily super-admin email with a Valkey day marker, audited | `controller/internal/certalert` (F14, D-36) | A2 email; F14 could later become a built-in rule |
| Webhook delivery with HMAC-SHA256 signing (`X-Zecurity-Signature`) for the tenant SIEM export | `controller/internal/auditexport` (PENDING-11) | A2 webhook (same signing scheme) |
| Outbox with retry and backoff | `controller/internal/outbox` | A2 delivery retries |
| Append-only provider audit (F17) | `provider_audit_logs` | Rule and channel changes; T1 timeline |
| Quotas with `over` per key (S3) | `providerquery` quotas | A1 "tenant over quota" rule |

## Proposed phases

### X1 — Sprint 23 follow-ups (small, first)
1. **SCIM at the users limit:** an admin can't turn SCIM on at the limit, because the mapping probe provisions a user and the quota refuses it. Proposal: the probe doesn't take a quota slot (it is deleted in the same flow), or the error explains the cause. *Needs Barath's choice.*
2. **Client quota message:** the client prints the device-quota refusal as a raw gRPC dump (`status: ResourceExhausted, message: "…", metadata: MetadataMap {…}`). Print the message only, as the suspended message already does.
3. **SCIM while suspended** (open since S1): SCIM provisioning isn't refused while a tenant is suspended. *Needs Barath's call: refuse (D-45's "configuration mutations") or allow (the IdP keeps the directory in sync).*
4. **D-45/D-46 amendment** from S4 (tenant-private export, two download routes, links valid until the purge starts): confirm and write into the Decision Record.

### R1 — per-workspace relay counters (relay, proto, controller)
- **Proto (additive):** `HeartbeatRequest` gets a new `repeated WorkspaceTraffic workspace_traffic = 13;` with `string trust_domain = 1; uint64 bytes_client_to_connector = 2; uint64 bytes_connector_to_client = 3; uint32 active_streams = 4;`. The counters are cumulative since the relay started, like fields 9 and 10, so a lost heartbeat loses no bytes and a restart is detected (a counter goes down).
- **Relay:** the session bridge already knows each stream's workspace (`identity.trust_domain` on the mTLS identity, `relay/src/session.rs`). Add per-trust-domain atomic counters next to the existing totals; report every workspace with traffic or open streams. Bounded: one entry per workspace the relay serves; entries for workspaces no longer allowed are dropped.
- **Controller:** map `trust_domain` → workspace (unknown or revoked trust domains are ignored and logged, never stored); compute per-workspace rates over the same window as B2; feed M1.
- **Privacy (D-40):** relay-ops see relay totals only; per-tenant relay series are super-admin only.
- **Compatibility:** an older relay sends no field 13 → per-tenant relay series read "Not reported" for that relay (as B2 does for bandwidth), never 0.

### M1 — F23 metric store (controller, migration)
- A `provider_metric_samples` table (or one per series family), written by a controller job every minute:
  - per relay: connections, bytes to clients, bytes to connectors (from the heartbeat values the controller already stores);
  - per tenant: active client sessions (devices with a live session), connectors online, shields online;
  - fleet totals.
- Downsampling (1 min → 1 h) and retention, per decision D-49.
- A provider read API: `GET /provider/metrics/{series}?scope=…&from=…&to=…&step=…`, read-only, super-admin and relay-ops (relay series) per D-06/D-31; no tenant names to relay-ops (as relay pools, D-40).
- Per tenant also: **relay bytes to clients and to connectors, and active relay streams**, from R1. The per-tenant values on a relay add up to the relay's totals (checked in the live run).
- The F23 series for relays and fleet totals need no agent change; the per-tenant relay series come from R1.

### M2 — F23 console
- Relay detail: connections and bandwidth charts (1 h / 24 h / 7 d / 30 d).
- Tenant detail: active sessions and agents-online charts.
- Overview: fleet sessions sparkline.

### A1 — F26 rules and evaluator
- **Two alert scopes, separate in the schema from the start (Barath 2026-11-20):**
  - **Platform alerts:** about the provider's infrastructure; never shown to a tenant. Examples: relay offline, relay over capacity, pool with no eligible relay, control plane degraded (DB latency, PKI), platform certificate expiry.
  - **Tenant alerts:** something one tenant can act on; always carry a `tenant_id`. Examples: the tenant's connector or shield offline, the tenant over a quota, the tenant with no relay path. Sprint 24 delivers them to the **provider** only; tenant-facing delivery (admin UI, the tenant's own webhook) is a later feature, but the data model already supports it.
  - Each rule kind belongs to exactly one scope; `scope` is a column on rules, states and events (`platform` | `tenant`), and a tenant alert row without `tenant_id` is refused by a CHECK constraint. Tenant-scoped reads always filter by `tenant_id`, so a later tenant view can't leak another tenant's or the platform's alerts.
- **Rule types (built-in kinds with parameters; no free-form query language):**
  - *platform:* relay offline for more than N minutes;
  - *platform:* relay near or over capacity (B2 headroom; threshold %, for N minutes);
  - *platform:* relay pool with no eligible relay;
  - *platform:* control plane degraded (F25 checks, incl. DB latency);
  - *platform:* platform certificate expiring (F14's scope; could replace the fixed daily email later, decision D-51);
  - *tenant:* connector offline for more than N minutes; shield offline for more than N minutes;
  - *tenant:* tenant with no relay path (F11-1);
  - *tenant:* tenant over quota (S3), informational.
- An evaluator job in the controller (single instance, D-02) with **firing → resolved** state per (rule, target), hysteresis (a "for N minutes" condition) and no repeat while firing (re-notify interval per rule).
- Storage: `alert_rules`, `alert_states`, `alert_events` (append-only history: fired, resolved, delivered, failed). The history is what F27 shows later.

### A2 — F26 delivery
- **One generic JSON webhook, no vendor integrations (Barath 2026-11-20).** Slack, PagerDuty, Opsgenie and others are wired by the operator from the standard payload (a Slack workflow or a PagerDuty Events webhook transform), so we maintain no vendor SDKs. A stable, versioned payload:
  ```json
  {
    "version": 1,
    "event_id": "uuid",
    "event_type": "relay.offline | relay.over_capacity | connector.offline | …",
    "status": "firing | resolved",
    "severity": "critical | warning | info",
    "scope": "platform | tenant",
    "tenant_id": "uuid or null",
    "target": {"type": "relay | pool | connector | shield | tenant | controller | certificate", "id": "…", "name": "…"},
    "summary": "one human-readable line",
    "started_at": "RFC 3339",
    "timestamp": "RFC 3339",
    "console_url": "link to the alert in the provider console"
  }
  ```
  No secrets, tokens or tenant content in the payload; tenant names only where the channel is allowed to see them (D-50).
- **Signing:** HMAC-SHA256 over timestamp + body, like the SIEM export's `X-Zecurity-Signature`, with a replay window; the secret is sealed (`pki.EncryptSecret`) and never returned after creation; SSRF guard on the URL (no private ranges in production; dev flag as for relays).
- **Email** to super-admins, or to an explicit recipient list (D-50), reusing the SMTP path of F14.
- Retries through the outbox with backoff; each delivery attempt recorded in `alert_events`.
- A "send test" action per channel.

### A3 — F26 console
- An **Alerts** page: active alerts, history, rules (create, edit, enable/disable, reason), channels (create, test, rotate secret).
- An Overview badge with the count of firing alerts.
- New console writes (amends D-30): alert rule and channel management. Each one goes into `api-surface.test.ts` on purpose.

### T1 — F27 incident timeline (stretch)
- One time-ordered view that merges alert events (A1), provider audit rows (F17) and relay/pool state changes, filterable by relay, pool or tenant.
- Read-only; no new data beyond what A1 and F17 already record.

## Decisions to make first (Phase 0, proposed D-48 … D-53)

| Proposed ID | Question | Recommendation |
|---|---|---|
| **D-48** | Scope of Sprint 24 | F23 incl. per-tenant relay bandwidth (R1, relay + additive proto change) + F26 (+ X1); F27 only as a stretch; F22, F32/F33, F28/F29 later |
| **D-49** | Metric resolution and retention | 1-min samples kept 7 days; hourly roll-ups kept 400 days; stored in Postgres (no new time-series database) |
| **D-50** | Alert scopes and recipients | Platform and tenant alerts separate in the schema (`scope`, `tenant_id` required for tenant alerts); Sprint 24 delivers both to the provider only (super-admins by default; optional email lists and webhooks per channel); a channel can be limited to one scope; relay-ops see platform relay alerts only, without tenant names |
| **D-51** | F14 daily email | Keep it as it is this sprint; fold it into F26 as a built-in rule later |
| **D-52** | Webhook format and security | One generic, versioned JSON payload (fields above), no vendor-specific integrations; HMAC-SHA256 over timestamp + body, replay window, sealed secret, HTTPS only outside development, SSRF guard |
| **D-53** | Who manages rules and channels | Super-admin only (D-06 pattern), every change audited with before/after and a reason (F17, D-17 style) |
| **D-54** | Per-tenant relay bandwidth | Relay reports cumulative bytes and active streams per trust domain in the heartbeat (additive proto field 13); per-tenant relay series super-admin only (D-40); older relays read "Not reported" |

**Answered by Barath (2026-11-20):**
- Per-tenant relay bandwidth is **needed now** → R1 (relay + additive proto change), D-54.
- Tenant visibility needs a clean boundary: platform alerts (relay, certificates, DB latency) never mix with tenant-actionable alerts (their connector is down). The scopes are separate in the schema now, so tenant-facing notifications later need no refactor → D-50.
- Paging and chat tools through **one generic JSON webhook** (severity, timestamp, tenant_id, event type), no vendor SDKs → D-52.

- Tenant-facing alerts (later sprint) start **in the admin UI only**; the tenant's own webhook is added only if customers ask for it.
- Retention in D-49 is **approved as proposed**: 1-minute samples kept 7 days, hourly roll-ups kept 400 days.

**Signed off by Barath on 2026-11-20.** Still open (each blocks only its X1 item): X1.1, X1.3, X1.4.

## Suggested split (two members, as in Sprint 23)

| Member | Phases | Order |
|---|---|---|
| **Barath (M2)** | R1, M1, M2 (F23) | R1 and M1 in parallel → M2 |
| **Sathiya (M1)** | X1, A1, A2, A3 (F26); T1 if time allows | X1 → A1 → A2 → A3 → (T1) |

M1 and A1 are independent and can run in parallel. A1's capacity rule reads the same relay values as M1, so the two agree on one reader (no duplicated query). R1 touches the relay and the proto, so it needs the relay build and an additive-proto review; the controller side of R1 lands with M1.

**Migrations (to reserve):** `068_metric_samples` (M1), `069_alerts` (A1/A2). Check `ls controller/migrations | tail -3` before adding; no migration framework; `[schema: reset DB]`.

**Conflict zones:** `proto/relay/v1/relay.proto` (R1 only; additive), the relay heartbeat handler in the controller (R1 and B2's bandwidth code), `controller/cmd/server/main.go` (new jobs and routes), `controller/internal/provider/authz.go` (new actions), `provider-console/src/api/api-surface.test.ts`, the Overview page (`provider-console/src/pages/Home.tsx`), and the provider navigation.

## Not in Sprint 24 (proposed)
- F22 tunnel success rate (needs relay and connector success/failure counters; R1 adds byte and stream counters only).
- Tenant-facing alert delivery: the schema is ready (D-50), the feature is later — admin UI first; tenant webhooks only on customer demand.
- Vendor-specific integrations (Slack app, PagerDuty SDK): covered by the generic webhook (D-52).
- F32/F33 metering and usage report; F34 billing.
- F28/F29 relay config push, version pinning.
- F19 more provider roles, F31 impersonation, F35 API keys, F37 reseller hierarchy (each needs a decision first).
- F39 custom dashboards, F40 SLA tracking (after F22, F23, F25).

## Test gates (as in Sprint 23)
- Build, vet and the full Go suite with every DB-backed test running (a skipped DB test is a failed case); console and admin tests, lint and build.
- **Live acceptance on the lab:**
  - F23: drive relay traffic for two tenants at once; per-tenant relay rates add up to the relay total (B2) and match each client's measured download; an older relay reads "Not reported"; stored samples match the live values; roll-up, retention and charts.
  - F26: stop a relay, push it over capacity, empty a pool → platform alert fires once, email and webhook arrive (signature verified, payload fields as D-52), resolves when fixed; stop a tenant's connector → a tenant alert with that `tenant_id`; a channel limited to platform alerts receives no tenant alert; webhook endpoint down → retries recorded; relay-ops can't manage rules and see no tenant alerts.

## Next steps
1. Barath reviews this draft and decides the scope and D-48 … D-53.
2. On sign-off: write `Sprint24/path.md` (decisions, order, conflict zones, rules, gates, acceptance) and the two phase files, and amend the Decision Record.
3. X1 can start as soon as Barath answers the two X1 questions; it doesn't depend on the rest.
