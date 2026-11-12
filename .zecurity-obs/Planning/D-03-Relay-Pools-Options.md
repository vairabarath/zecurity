---
type: decision-draft
title: D-03 relay pools and regions — options
status: decided 2026-11-12 (Option C, D-37 … D-41)
created: 2026-11-12
baseline: main c79a75e1
unblocks: [F4 (region), F9, F10, F11, F30]
tags:
  - provider-dashboard
  - decision
---

# D-03 relay pools and regions: options

> **Decided 2026-11-12: Option C**, recorded as D-37 … D-41 in `docs/provider-dashboard-architecture-decisions.md`. Plan: [[Relay-Pools-Implementation-Plan]] (three PRs). Two corrections to this draft were made in the decision: (1) placement validation does **not** enforce residency on its own, because relays accept every workspace; residency (R2) also needs relay-side enforcement (PR 3, additive relay proto fields), and F10 stays partly done until then; (2) failover takes about 1.5–2.5 minutes (90 s eviction expiry, swept every 60 s), not ≈ 90 s. Refined in review the same day: removing a workspace from a residency relay's list ends its sessions as soon as the relay applies the policy (planned moves get a controller-side migration window instead), and relay policies carry a monotonic version, an acknowledgement and a 10-minute lease (D-40). The text below is kept as the options record.

D-03 (Decision Record 2026-09-23) says: *"Not in the first release. Global relay fleet only. Q5–Q7 are deferred."* Four of the eight remaining Must-have items wait on it: **F4** (region column), **F9** (tenant → pool), **F10** (region / residency), **F11** (failover pool per tenant). F30 (tenant migration between pools, Can have) too.

Reopening D-03 means answering three linked questions from `docs/provider-dashboard-architecture-decisions.md`:
- **Q5 pool semantics:** hard constraint (P1), preferred + failover (P2), ordered set (P3)?
- **Q6 granularity:** tenant, remote network or connector?
- **Q7 region meaning:** latency (R1), relay-traffic residency (R2) or full infrastructure residency (R3)?

This draft bundles the coherent combinations into five options, with a recommendation. Nothing here is decided until it is recorded as an amendment to the Decision Record.

## Facts today (checked against `main` `c79a75e1`)

- **One global relay list.**
  - `relay.Store.BuildLabelledRelayList` (`controller/internal/relay/store.go:735`) selects every relay matching `eligible.Where` (`internal/relay/eligible/eligible.go`: active, capacity high/medium, not draining, routable address) and fingerprints it.
  - `ConnectorRegistry.BroadcastRelayList` (`internal/connector/control_stream.go:548`) sends that list to every connector (`cmd/server/main.go:349`); connect-time push sends the same list.
- **Per-tenant delivery is already possible:** `ClientsForWorkspace` (`control_stream.go:363`) and per-connector `get` (`:349`) exist.
- **The connector chooses:** `connector/src/relay_selector.rs` ranks HIGH, then MEDIUM, then RTT; migrates make-before-break when the active relay leaves the list. It chooses from whatever list it receives.
- **Proto:** `LabelledRelayInfo{relay_id, relay_addr, spiffe_id, capacity label}` has no pool, priority or region field.
- **Relays serve every workspace:** the relay runs a multi-workspace mTLS listener and checks each peer against its workspace CA's CRL (`relay/src/crl.rs`). It has no tenant allowlist.
- **Placement is not validated:** `UpsertPlacement` (`store.go:448`) stores whatever relay the connector reports.
- **Clients follow the connector:** the transport snapshot gives clients each connector's current relay (`internal/transport/compiler.go:84`), so restricting connectors restricts client relay traffic too.
- **Drain guard is global:** D-32's last-eligible-relay guard counts `eligible.Where` over the whole fleet.
- **No region concept anywhere:** `remote_networks.location` is descriptive only.
- **Single controller (D-02):** Postgres, Valkey and the controller are all single-instance and single-region.

## Options

### Option 0: keep D-03 deferred

Global fleet stays. F4, F9, F10, F11 stay open and the Must-have list cannot be completed.
- **Effort:** none.
- **When it fits:** one region, no residency customers.

### Option A: region label only (R1-lite, no placement change)

An operator-set `region` on each relay, shown in the relay list and detail (F4) and filterable. It does **not** change which relays a connector is offered.
- **Unblocks:** F4 only.
- **Change:** migration (`relays.region`), provider API + console edit field (relay-ops), audit. No proto, connector or relay change.
- **Effort:** 🟢/🟡 small. Fully compatible with every later option (a pool can later be tied to a region).

### Option B: hard tenant pools (P1 + tenant granularity; region = pool attribute)

- **Data:** `relay_pools(id, name, region, residency bool)`; each relay belongs to **exactly one** pool; `workspaces.relay_pool_id` (nullable = the default pool).
- **List:** built per pool from `eligible.Where AND pool = …`; sent per workspace via `ClientsForWorkspace`; version fingerprint per pool.
- **Failover:** only inside the pool. An exhausted pool leaves connectors in backoff (no relay path; the direct client→connector path is unaffected).
- **Residency (R2, relay traffic only):** a pool with `residency = true` is a hard boundary for the list. The controller also rejects (logs, alerts and does not record) a reported placement outside the tenant's pool. *(Corrected in the decision: this is supplementary; enforcement needs the relay-side check, D-40.)*
- **Drain guard:** becomes per pool (the last eligible relay of a pool with assigned tenants).
- **Unblocks:** F4 (region via pool), F9, F10 (R1 + R2). Not F11.
- **Change:** migration, controller list build/fan-out, provider API + console (pools, membership, tenant assignment), audit. **No proto, connector or relay change**; the connector's make-before-break migration handles pool moves.
- **Effort:** 🟡 medium.

### Option C: Option B + controller-driven failover pool (P2a)  ← recommended

Everything in B, plus `workspaces.failover_pool_id` (nullable).
- **Rule:** the controller sends the primary pool's list; when it has **no eligible relay**, it sends the failover pool's list instead, and switches back when the primary recovers. Connectors migrate with today's behaviour.
- **Residency:** if the primary pool is a residency pool, the failover pool must be a residency pool in the same region (checked on assignment), so failover never crosses the boundary.
- **Failover latency:** the existing eviction timing (90 s expiry, swept every 60 s: about 1.5–2.5 minutes) plus a broadcast. Faster failover needs Option D.
- **Unblocks:** F4, F9, F10, F11. F30 (tenant migration between pools) is then an assignment change plus the existing migration.
- **Change:** B + failover selection in the list builder and an audit of each switch. Still **no proto, connector or relay change**.
- **Effort:** 🟡 medium (about B + one small PR).

### Option D: ordered pools, connector-driven (P3 / P2b)

The controller sends all assigned pools with a priority; the connector fails over across priorities immediately on session failure.
- **Change:** additive proto field (priority on `LabelledRelayInfo`); connector selector changes (priority → tier → RTT; fail-back rule, because today's >15 %/>10 ms rule is RTT-based). Connector rollout to every tenant.
- **Benefit:** fastest failover (seconds, not ≈ 90 s).
- **Effort:** 🔴 large. Can be added later on top of C; the data model is the same.

### Not proposed now

- **Network- or connector-level assignment (P4):** a later override on `remote_networks` (nullable, inheriting the tenant's pool) fits on top of B/C without redesign. Add when a multi-country tenant needs it.
- **R3 full infrastructure residency:** regional controllers, Postgres, Valkey and PKI. It is PENDING-12 Option B (XL) and needs HA first (D-02). Stays out.
- **Relay-side tenant allowlist:** enforcing pools inside the relay needs a relay change and a trust-distribution channel. Controller-side list filtering plus placement validation is enough for R2 on relay traffic.

## Comparison

| | 0 | A | B | C | D |
|---|---|---|---|---|---|
| Unblocks | — | F4 | F4, F9, F10 | **F4, F9, F10, F11** (+ F30) | F4, F9, F10, F11 |
| Schema | — | 1 column | pools + assignment | B + failover column | as C |
| Proto / connector / relay change | — | none | none | **none** | proto + connector |
| Failover | global | global | inside pool only | ≈ 1.5–2.5 min to failover pool | seconds |
| Residency (relay traffic) | no | no | list only (enforced with relay-side check) | list only (enforced with relay-side check) | same |
| Effort | — | 🟢 | 🟡 | 🟡 | 🔴 |

## Recommendation

**Option C, delivered in two PRs** (B first, then the failover pool), with:
1. **Granularity:** tenant (workspace). Network-level override later, if needed.
2. **Region:** an attribute of a pool. "Region" means R1 (latency) and, for pools marked residency, R2 for relay traffic only. R3 is explicitly out of scope.
3. **Membership:** a relay is in exactly one pool (clean region semantics; no double counting in capacity).
4. **Default pool:** every existing relay starts in a `default` pool, and tenants without an assignment use it, so nothing changes for anyone until a provider assigns a pool.
5. **Empty pool:** fail closed for the relay path (backoff; direct path unaffected), never a silent fallback to another pool. Failover happens only through an explicit failover pool.
6. **Roles (extends D-31):** relay-ops manage pools and relay membership; tenant pool assignment (F9/F11) is super-admin only, because it changes a tenant's traffic path. Every change is audited with before/after (F17).
7. **Drain guard (D-32):** per pool. Draining or moving the last eligible relay of a pool that has tenants is refused, exactly like today's global guard.
8. **Placement validation:** reported placements outside the tenant's allowed pools are refused and audited (supplementary; R2 enforcement is the relay-side check, D-40).
9. **No connector, relay or proto change.** This keeps ADR-016 ("controller filters and labels, connector chooses").

Option A alone is the right choice only if pools are not wanted this year: it closes F4 and leaves F9–F11 open.

## Questions for you

1. Option C (recommended), B, A, D or keep deferred?
2. Pool membership: exactly one pool per relay (recommended) or many?
3. Empty pool: fail closed (recommended) or fall back to the default pool?
4. Tenant assignment by super-admin only (recommended), or relay-ops too?
5. Is R2 for relay traffic only (recommended) enough for the residency promise, given that the controller and its data stay in one region?

Once decided, record it as an amendment to `docs/provider-dashboard-architecture-decisions.md` (D-03 superseded, new D-37 …), then plan the PRs.
