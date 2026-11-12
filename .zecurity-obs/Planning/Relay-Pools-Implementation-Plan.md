---
type: plan
title: Relay pools, failover and regions — implementation plan
status: plan approved 2026-11-12 (not started)
created: 2026-11-12
decisions: D-37, D-38, D-39, D-40, D-41 (docs/provider-dashboard-architecture-decisions.md, amendment 2026-11-12)
baseline: main c79a75e1
features: [F4, F9, F10, F11, F30]
tags:
  - provider-dashboard
  - relay
  - plan
---

# Relay pools, failover and regions: implementation plan

Three independently reviewable PRs. Options and rationale: [[D-03-Relay-Pools-Options]]. Binding decisions: D-37 … D-41.

| PR | Scope | Schema | Proto | Connector | Relay |
|---|---|---|---|---|---|
| 1 | Pools, tenant primary pool, per-pool lists, per-pool guard, residency placement validation, API + console | `056_relay_pools.sql` | — | — | — |
| 2 | Failover pool, reconciler with hold-down, audit, API + console | `057_relay_pool_failover.sql` | — | — | — |
| 3 | Relay-side residency enforcement: policy version, lease, acknowledgement, immediate revocation | `058_relay_pool_policy.sql` | additive fields in `relay.v1` heartbeat | — | yes |

Each PR is labelled `[schema: reset DB]` where it adds a migration (pre-production rule: recreate the local DB).

## Existing code this changes (checked on `main` `c79a75e1`)

| Assumption today | Where | Contradiction with D-37 … D-41 |
|---|---|---|
| One global list | `relay.Store.BuildLabelledRelayList(ctx)` (`internal/relay/store.go:735`), `LabelledRelayListSource` interface (`internal/connector/control_stream.go:44`) | Needs a workspace (or pool) parameter |
| One broadcast to every connector | `ConnectorRegistry.BroadcastRelayList` (`control_stream.go:548`), `publishRelayList` (`cmd/server/main.go:~340`) | Per-pool fan-out by workspace |
| Global drain guard | `DrainRelay` locks every eligible relay (`internal/relay/drain.go:31-88`) | Guard per pool, serialised on the pool row |
| Global eligible count | `providerquery` `EligibleRelayCount` (`internal/providerquery/relays.go:143-147`) used by the console drain warning | Per pool |
| Placement accepted as reported | `UpsertPlacement` (`store.go:448`), called from heartbeat (`control_stream.go:1097`) and relay events (`:1160`) | Residency tenants: validate before recording |
| Relays accept every workspace | relay multi-workspace listener; `crl.rs` checks per `workspace_id` | Residency pools: allowlist (PR 3) |
| `relay.*` prefix is relay-ops | `decide()` + `superAdminOnlyRelayActions` (`internal/provider/authz.go`) | Pool region/residency is super-admin; tenant assignment is `tenant.*` (super-admin, D-06) |
| App role grants are per table at creation | `053_audit_chain.sql:327` grants on existing tables only, no default privileges | New tables must be granted to `ztna_app` explicitly |

Kept as is: the connector only dials relays in its current list (`connector/src/relay_selector.rs` `build_migration_candidates`, failover path), clients follow the connector's relay through the transport snapshot (`internal/transport/compiler.go:84`), and eviction runs every 60 s with a 90 s expiry (`cmd/server/main.go:798`).

## PR 1: relay pools and tenant primary pool

### Schema `056_relay_pools.sql`

```sql
CREATE TABLE relay_pools (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 64),
    region      TEXT CHECK (char_length(region) <= 32 AND region ~ '^[a-z][a-z0-9]*(-[a-z0-9]+)*$'),
    residency   BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (NOT residency OR region IS NOT NULL)
);
-- Built-in default pool: fixed id, no region, never residency.
INSERT INTO relay_pools (id, name) VALUES ('00000000-0000-0000-0000-000000000001', 'default');
ALTER TABLE relays     ADD COLUMN pool_id       UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES relay_pools(id) ON DELETE RESTRICT;
ALTER TABLE workspaces ADD COLUMN relay_pool_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001' REFERENCES relay_pools(id) ON DELETE RESTRICT,
                       -- graceful migration (D-40): the previous pool stays authorized until relay_pool_previous_until
                       ADD COLUMN relay_pool_previous_id    UUID REFERENCES relay_pools(id) ON DELETE RESTRICT,
                       ADD COLUMN relay_pool_previous_until TIMESTAMPTZ,
                       ADD CONSTRAINT relay_pool_previous_pair CHECK ((relay_pool_previous_id IS NULL) = (relay_pool_previous_until IS NULL));
CREATE INDEX relays_pool_id_idx ON relays (pool_id);
CREATE INDEX workspaces_relay_pool_id_idx ON workspaces (relay_pool_id);
-- The default pool can't become residency or get a region (CHECK via trigger or constraint on id).
GRANT SELECT, INSERT, UPDATE, DELETE ON relay_pools TO ztna_app;   -- 053 grants only existing tables
```

- `NOT NULL` with a fixed default instead of "NULL means default": no NULL semantics in queries, and every existing insert path (`bootstrap.go:154`, relay create) keeps working unchanged.
- The protected `workspaces.status='active'` predicates are not touched.
- Final names checked against conventions: plural tables, `*_id` foreign keys.
- Region and residency can only change while no workspace uses the pool (D-37): enforced in the pool store under the pool row lock, with a test.
- **Authorized pools** of a workspace = primary, plus failover (PR 2), plus the previous pool while `now < relay_pool_previous_until`. One SQL helper (like `eligible.Where`) used by placement validation (PR 1) and the relay policy (PR 3).

### Controller

1. **Pool store** (`internal/relay/pools.go`): create, rename, set region/residency, delete (only when no relays and no tenants; never `default`), move relay, assign tenant primary pool.
   - Every mutation runs in one transaction: lock the pool row(s) `FOR UPDATE` (always in id order), then the relays, then audit through the existing `inTx` hook pattern.
2. **Per-pool guard** (`drain.go`): `DrainRelay` and `MoveRelay` lock the source pool row, then that pool's eligible relays, and refuse with `last_eligible_relay_in_pool` if the pool is in use (any tenant's primary, or `default`) and would have no eligible relay left.
   - Tenant assignment refuses a target pool with no eligible relay (`pool_has_no_eligible_relay`).
   - Pool deletion refuses `pool_not_empty`.
3. **Lists**:
   - Replace `BuildLabelledRelayList(ctx)` with `BuildLabelledRelayLists(ctx) (map[poolID]*List, error)` (one query, `eligible.Where`, grouped by `pool_id`) and `ListForWorkspace(ctx, wsID)`.
   - The fingerprint stays content-based (`relayListVersion`).
4. **Delivery**:
   - Connect-time push uses `ListForWorkspace(client.tenantID)`.
   - `publishRelayList` builds all lists plus a `workspace → pool` map and calls `ConnectorRegistry.PublishRelayLists(lists, wsPool)`. That sends each client its pool's list through the existing `sendRelayList(list, onlyIfBehind)`, so unchanged pools send nothing.
   - The FU-6 resend path uses the same function.
   - `BroadcastRelayList` is removed.
   - Triggers: today's (eviction, tier/address change, drain/resume, create/revoke/delete) plus relay move and tenant assignment.
5. **Placement validation** (residency tenants only): in both `UpsertPlacement` call sites, if the connector's workspace is in a residency pool and the reported relay's pool is not one of the tenant's **authorized pools** (primary, previous pool during a graceful migration window), then:
   - don't record the placement, and delete the existing one so the transport snapshot drops the relay path;
   - notify topology;
   - audit `relay_placement.rejected` (once per connector and relay transition, deduplicated in memory per D-02);
   - increment a metric.
   Non-residency tenants: unchanged (R1 only).
6. **Authz** (`authz.go`):
   - New actions `relay.pool.create`, `relay.pool.update` (name), `relay.pool.delete`, `relay.move` (relay-ops through the `relay.` prefix).
   - `relay.pool.set_residency` (region/residency) goes into `superAdminOnlyRelayActions`.
   - `tenant.relay_pool.assign` is super-admin through D-06.
   - Matrix test extended.
7. **Provider API** (D-04 REST, JSON error codes as in Phase A, `Cache-Control: no-store` on writes):
   - `GET /provider/relay-pools`: id, name, region, residency, relay count, eligible count, tenant count.
   - `GET /provider/relay-pools/{id}`: relays, plus tenants for super-admin only.
   - `POST /provider/relay-pools`, `PATCH /provider/relay-pools/{id}`, `DELETE /provider/relay-pools/{id}`.
   - `POST /provider/relays/{id}/pool {pool_id, reason}`; optional `pool_id` on `POST /provider/relays`.
   - `PUT /provider/tenants/{id}/relay-pool {pool_id, reason, migration: "graceful"|"immediate"}` (reason 1–500 runes, required; `graceful` by default sets the previous pool and `relay_pool_previous_until = now + RELAY_POOL_MIGRATION_WINDOW`, default `5m`, refused at startup if shorter than the connector drain timeout of 120 s; `immediate` clears it).
   - Reads: relays carry `pool {id, name, region}`; the tenant detail carries `relay_pool` and its eligible count; `EligibleRelayCount` per pool.
8. **Audit:** `relay_pool.created/updated/deleted`, `relay.moved`, `tenant.relay_pool_assigned`, `relay_placement.rejected`, all with `provider.WithChange` before/after (F17).

### Console (provider-console)

- **Relays:** Pool and Region columns and a filter; "Move to pool" action (relay-ops and super-admin) with the guard error explained; pool picker on Add relay (default preselected).
- **New Pools page** (nav for both roles):
  - list with region, residency badge ("routing only" until PR 3), relays, eligible relays, tenants;
  - detail with members, plus tenant names for super-admin only;
  - create, rename and delete;
  - region/residency edit shown to super-admin only.
- **Tenant detail:** Relay pool section with the pool, region, residency, eligible relays and a warning when 0. "Change pool" (super-admin) with a reason field and the D-32-style availability warning.
- Every new write is added to `api-surface.test.ts` on purpose (CLAUDE.md rule).

### PR 1 tests

- **DB-backed (`PKI_TEST_DATABASE_URL`, no skips):**
  - migration: default pool exists; existing and new relays and workspaces get `default`;
  - residency without region rejected; default can't be made residency or deleted.
- **List isolation:**
  - two pools, two tenants, each gets only its pool's relays;
  - moving a relay changes only that pool's tenants' lists (spy registry: other clients receive nothing);
  - connect-time and broadcast lists are identical for the same workspace.
- **Guards:**
  - drain or move of the last eligible relay of a used pool → 409;
  - assignment to an empty pool → 409; deleting a non-empty pool → 409;
  - concurrent drain + move of the last two relays: exactly one succeeds (two goroutines, same pattern as the existing drain race test).
- **Placement (residency tenant):**
  - in-pool report recorded; previous-pool report recorded during the migration window and rejected after it; `migration: immediate` rejects it at once;
  - out-of-pool report not recorded, existing placement removed, audit row written once, transport notify fired;
  - non-residency tenant: unchanged behaviour.
- **Authz:** relay-ops can't assign tenants or set residency (403, nothing written, no audit); super-admin can.
- **Audit:** one row per success with before/after; none on 4xx.
- **Console:** unit tests per page and role; `api-surface.test.ts`.
- **Live (lab):**
  - two pools, a tenant moved between them: its connector migrates make-before-break, and another tenant's connector receives no list change;
  - empty pool: the relay path is gone and the direct path still works.

## PR 2: controller-driven failover

### Schema `057_relay_pool_failover.sql`

```sql
ALTER TABLE workspaces  ADD COLUMN relay_failover_pool_id UUID REFERENCES relay_pools(id) ON DELETE RESTRICT,
                        ADD CONSTRAINT relay_failover_differs CHECK (relay_failover_pool_id IS DISTINCT FROM relay_pool_id);
ALTER TABLE relay_pools ADD COLUMN exhausted_at         TIMESTAMPTZ,   -- set: primary has had no eligible relay since
                        ADD COLUMN eligible_since       TIMESTAMPTZ;   -- set while recovering: eligible continuously since
```

### Rules

- **Effective pool** = `failover` if the primary has `exhausted_at IS NOT NULL` and a failover pool is set, else the primary.
- **Reconciler** `relay.ReconcilePools(ctx, now)`, run before every list publish and on the existing 60 s eviction tick:
  - for each pool, read its eligible count under a pool row lock;
  - 0 eligible and not exhausted → set `exhausted_at = now`, clear `eligible_since`, audit `relay_pool.failover` (actor `system:relay-pool-failover`, details: pool, affected tenant count);
  - exhausted and > 0 eligible → set `eligible_since` if NULL; once `now - eligible_since ≥ hold-down` → clear both, audit `relay_pool.failback`;
  - exhausted and 0 again while recovering → clear `eligible_since` (the hold-down restarts).
  - It is idempotent: running it twice at the same `now` changes nothing. State is in Postgres, so a restart resumes mid-failover. One controller (D-02).
- **Hold-down:** `RELAY_POOL_FAILBACK_HOLDDOWN` (default `5m`, minimum `1m`).
- **Failover itself is not debounced further:** eviction already requires 90 s without a heartbeat.
- **Assignment validation:**
  - failover ≠ primary;
  - same region (both NULL counts as same only for non-residency pools);
  - residency primary requires a residency failover with the same region;
  - failover pool must have an eligible relay at assignment time.
- **Guards:**
  - a pool that is any tenant's failover counts as "in use" for the per-pool guard;
  - draining the last eligible relay of a primary stays refused even when a failover exists (an operator reassigns first). Open question Q2 below.
- **Placement validation:** allowed pools for a residency tenant = primary ∪ failover (same region by rule), so failover and failback migrations are never rejected.

### API and console

- `PUT /provider/tenants/{id}/relay-pool` takes `{pool_id, failover_pool_id|null, reason}`.
- Pool reads add `state: healthy | failed_over | recovering`, `exhausted_at`, `eligible_since`.
- Console:
  - Pools page shows the state;
  - Tenant detail shows "Using failover pool since …";
  - Overview "Needs attention" lists failed-over pools.

### PR 2 tests

- **Deterministic reconciler table test (fake clock):**
  - healthy → exhausted on 0 eligible;
  - recovery shorter than the hold-down → still exhausted;
  - a dip during recovery restarts the hold-down;
  - failback after the hold-down;
  - idempotent repeat;
  - no failover pool → empty list (fail closed);
  - both exhausted → empty list.
- **Restart:** a persisted `exhausted_at` survives a new reconciler instance; lists stay on failover.
- **Region rules:** incompatible combinations rejected with exact codes.
- **Lists:** tenants of an exhausted primary receive the failover list; other tenants receive nothing.
- **Audit:** one failover row and one failback row per transition.
- **Live:** stop the only relay of the primary → connectors move to failover within about 2.5 minutes; start it → failback after the hold-down; flap the relay inside the hold-down → no failback.

## PR 3: relay-side residency enforcement

### Schema `058_relay_pool_policy.sql`

```sql
ALTER TABLE relay_pools ADD COLUMN policy_version BIGINT NOT NULL DEFAULT 1;   -- bumped in the same tx as any change to the pool's authorized workspace set
ALTER TABLE relays      ADD COLUMN policy_version_acked BIGINT,              -- last version the relay reported applying
                        ADD COLUMN policy_acked_at      TIMESTAMPTZ;
```

- Every change to a pool's authorized set bumps `policy_version` in the same transaction: assignment, migration start, failover set or cleared, a migration window expiring (the 60 s tick closes expired windows and bumps the old pool), tenant removal.
- Monotonic per pool, so the relay can reject an older or replayed version.

### Proto (additive, `proto/relay/v1/relay.proto`)

- `HeartbeatRequest`:
  - `bool workspace_policy_supported` (the relay can enforce);
  - `uint64 workspace_policy_version_applied`.
- `HeartbeatResponse`:
  - `bool workspace_policy_enforced = 4`: false for non-residency pools, so nothing changes there;
  - `repeated string allowed_workspace_ids = 5`;
  - `uint64 workspace_policy_version = 6`;
  - `uint32 workspace_policy_lease_seconds = 7` (`RELAY_POLICY_LEASE`, default `10m`, refused if under four heartbeat intervals).

### Controller

- The relay heartbeat handler looks up the relay's pool.
- For a residency pool, it returns the workspaces whose **authorized pools** include that pool, with the pool's `policy_version` and the lease.
- It records `policy_version_acked` and `policy_acked_at` from the request.
- The console shows each relay as current, behind (acked < version) or not acknowledging.
- Pools whose relays don't all report `workspace_policy_supported` are shown as "residency not enforced" in the console and API. The console never shows "enforced" without it.

### Relay

- **Policy store:** version, allowed set, lease expiry (`applied_at + lease`), persisted in `RELAY_STATE_DIR`. An older version than the one held is ignored.
- **Checks:** at connector registration and client session setup, after the SPIFFE/CRL check (the relay already has `workspace_id`), refuse the peer if the policy is enforced and the workspace is not allowed.
- **Revocation:** a newly applied policy that drops a workspace **immediately ends** that workspace's connector registrations and client sessions on the relay. Graceful moves are handled by the controller keeping the old pool authorized during the migration window (D-40), not by a relay-side delay.
- **No policy yet** (first boot, no file): refuse all workspace peers until the first heartbeat response (seconds).
- **Controller unreachable:** keep using the policy while the lease is valid; each successful heartbeat renews it. **At lease expiry:** refuse new workspace sessions and end existing ones, until a fresh policy arrives.
- **Not enforced** (non-residency pool): no checks, no lease effect.
- **Logs and metrics** for refusals, terminations, lease expiry and the applied version.

### PR 3 tests

- **Relay unit tests (fake clock):**
  - enforced + allowed → accepted; enforced + other workspace → refused (connector and client);
  - not enforced → accepted;
  - no policy at boot → refused;
  - removal → that workspace's sessions end at once, others untouched;
  - older version ignored;
  - heartbeats missed within the lease → still serving;
  - lease expired → new sessions refused and existing ones ended; fresh policy → serving again;
  - persisted policy survives restart with its original lease expiry.
- **Controller:**
  - heartbeat responses per pool type;
  - every authorized-set change bumps the version exactly once;
  - a graceful migration keeps the old pool until the window and then bumps it; an immediate migration bumps it at once;
  - acknowledgement recorded;
  - the capability flag drives "enforced".
- **Live:**
  - a connector of tenant B pointed at a residency relay of tenant A's pool is refused at the relay;
  - removing tenant A (immediate) ends its sessions within one heartbeat;
  - a graceful move keeps traffic until the window ends;
  - blocking the relay→controller path ends sessions at lease expiry, and they recover when it's restored;
  - mixed relay versions show "not enforced".

## Tracker impact

| Feature | After PR 1 | After PR 2 | After PR 3 |
|---|---|---|---|
| F4 region on relay list | implemented (region via pool), live check → ✅ | — | — |
| F9 tenant → pool | implemented, live check → ✅ | — | — |
| F10 region / residency | routing supported (R1) → ◐ | ◐ | residency enforced and live-checked → ✅ |
| F11 failover pool | — | implemented, live check → ✅ | — |
| F30 migration between pools (Can have) | assignment change + make-before-break → ◐ (no bulk/scheduled migration) | — | — |

Ticked only after merge **and** live check ([[Pending-Live-Tests]]).

## Risks

- **Migration storms:** moving a large tenant, or a pool failing over, migrates all its connectors at once. make-before-break bounds the impact, but there is no jitter today. Measure in the live check; add jitter only if needed.
- **Failover time:** 1.5–2.5 minutes from the last heartbeat. It must not be sold as instant; faster needs P3 (connector change).
- **Capacity fragmentation:** small pools hit "no eligible relay" sooner (capacity LOW excludes a relay). The console must show eligible counts per pool.
- **Residency wording:** until PR 3 is live-checked, the UI says "routing only". Residency covers relay traffic only; control-plane data (controller, Postgres, Valkey, logs) stays in one region (D-02, R3 out of scope).
- **Old relays in residency pools:** without PR 3 support they can't enforce, so the pool shows "not enforced".
- **App role grants:** new tables need explicit grants (`053` grants only existing tables). Missing grants break the controller only when it runs as `ztna_app`, so test with `SET ROLE ztna_app`.
- **Audit volume:** placement rejections must be deduplicated (heartbeats repeat).

## Decisions taken (2026-11-12)

1. Failback hold-down: **5 minutes**; any loss of eligibility during it restarts the timer.
2. Last eligible relay of a primary: **never drained or moved while tenants use the pool**, even with a failover pool (reassign first).
3. Removal from a residency allowlist: **sessions end as soon as the relay applies the policy** (≤ one heartbeat, 30 s). Planned moves use the controller-side migration window (default 5 minutes, `migration: graceful`); `migration: immediate` and every other loss of authorization take effect at once.
4. Region codes: **validated free-form** (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, ≤ 32); region and residency can't change while a tenant uses the pool.
5. relay-ops see **tenant counts only**.
6. Policy freshness: **monotonic version per pool, relay acknowledgement, 10-minute lease**; at lease expiry an enforcing relay ends existing sessions and refuses new ones.

## Still open

- Lease length vs outage tolerance: 10 minutes means a controller outage longer than that stops relay traffic for **residency** tenants (non-residency pools are unaffected). Confirm after the PR 3 live check.
- Behaviour of the graceful window when the destination pool is in another region: allowed (the old region stays authorized only for the window), but the console should warn on a cross-region move of a residency tenant.
