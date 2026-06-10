---
type: decision
status: accepted
date: 2026-06-03
related:
  - "[[CodeStudy/02-Protect-Resource-Flow]]"
  - "[[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]"
  - "[[Sprint8/path]]"
tags:
  - adr
  - shield
  - controller
  - reconciliation
  - desired-state
  - firewall
  - reliability
---

# ADR-004 — Resource Enforcement Is Desired-State Reconciled, Not Fire-and-Forget

## Context

The protect-resource flow (see [[CodeStudy/02-Protect-Resource-Flow]]) delivers firewall
intent from the Controller (source of truth) to the Shield (which writes nftables rules)
as **incremental instructions**: `apply this` / `remove this`. There is no reconciliation.

Code study of Pieces 1–3 surfaced two failure modes that share one root cause:

### Bug 1 — Zombie firewall rules (Finding 5)
`DeleteResource` hard-`DELETE`s the DB row but never guarantees the shield drops its rule:
- `SoftDelete` (`store.go:318`) guards only `status != 'protecting'`, so it **allows deleting a
  `protected` resource** — yet its error message says "must be unprotected before deleting".
- Hard delete sends no `remove`; the shield rebuilds its chain from its **own in-memory
  `active` set**, so the rule keeps enforcing. Later shield re-verify acks hit
  `RecordAck`'s `WHERE … deleted_at IS NULL` → 0 rows, silent no-op. No console path to clean up.
- Subtlety: a `failed` resource may **also** hold a rule — `handle_apply`
  (`shield/src/resources.rs`) sets `failed` with the rule still applied in the
  "port not listening" case. So blocking only `protected` deletes is insufficient.

### Bug 2 — Fail-open after reboot (new finding)
Shield reboot wipes in-kernel nftables and the in-memory `active` list
(`network::setup()` deletes the whole `zecurity` table on startup; `SharedResourceState::new()`
starts empty). The controller only re-pushes `status='protecting'` on reconnect
(`GetPendingForShield` → `store.go:176`), **never `protected`**. Result: previously-protected
resources silently become **unprotected** and are never restored. A security fail-open.

### Root cause
Incremental-only delivery with **no reconciliation**. If a single message is lost, missed
during a transient disconnect, or wiped by a reboot, `controller state ≠ shield state` **forever**
(drift), with no mechanism to detect or correct it.

### Rejected first idea
"Delete the row immediately, rely on reconnect reconciliation." Rejected because hard-deleting
the row **destroys the reconciliation anchor** — reconciliation-by-absence cannot distinguish
"should be removed" from "never existed", so any single failure in the snapshot path becomes
**permanent and invisible**. Worse than a visible stuck state.

The "shield offline → delete now" variant is also unsafe: a **transient disconnect with the
shield process alive** retains the rule in kernel + memory and is indistinguishable, from the
controller's view, from a dead process. Only reboot/decommission are truly clean.

## Decision

Move resource enforcement from fire-and-forget to **desired-state reconciliation**, governed by
one invariant:

> **Never destroy the record of an intent until its effect is observably confirmed.
> Observe actual state — do not assume a push landed.**

Three mechanisms:

1. **Tombstone deletion.** Delete marks the row `deleting` (a tombstone) instead of removing it.
   The row — the reconciliation anchor — survives disconnects, crashes, and reboots until cleanup
   is confirmed.
2. **Shield reports actual state.** On each heartbeat the shield reports its current active
   resource IDs + a monotonic `generation` + a `fingerprint`. The controller **observes** reality
   instead of trusting acks.
3. **Closed-loop reconciliation.** The controller continuously compares `desired` vs reported
   `actual` and converges: orphan (reported∉desired) → `remove`; missing (desired∉reported)
   → `apply`; `deleting` tombstone confirmed absent → reap (hard-delete).

### Roles
- **Controller** = authoritative source of *desired* truth.
- **Shield** = disposable executor + authority on *observed* truth. It owns no durable truth;
  reconciliation converges desired→actual.

### Design refinements (mandatory — not optional polish)
- **Generation + fingerprint** (clone the existing `DiscoveryReport` idiom): cheap reports,
  stale/out-of-order rejection, no-op fast path.
- **Hysteresis / grace:** act on orphan/missing only after **N consecutive consistent reports**
  (or report generation past the last instruction) so the reconciler never fights in-flight ops.
- **Tenant/shield scoping (security):** a shield's report may only affect resources whose
  `shield_id` == the reporting shield. Enforce in the reconciler.
- **Idempotency:** `apply_nftables`/`handle_remove` rebuild from the full set — re-sends are safe.
- **Break-glass:** an audit-logged admin `forceDeleteResource` for genuinely decommissioned
  shields, so a permanently-offline tombstone isn't immortal.

### Reuse existing templates
- `DiscoveryReport` (`proto/shield/v1/shield.proto`) — shield→controller diff/full-sync with
  `seq` + `fingerprint` + `full_sync`. Template for the **state report**.
- `ACLSnapshot` (`client.v1`, pushed via `ConnectorControlMessage`) — controller→edge
  desired-state push. Template for the **snapshot**.

## Guardrails (per CLAUDE.md)
- Never change existing proto field numbers — only add new ones.
- State reports ride the **existing Control stream** (heartbeat piggyback) — no new RPCs.
- `resource_protect` chain stays **flush + rebuild atomic** — reuse `apply_nftables`.
- Build gate each phase:
  `cd controller && go build ./... && go vet ./...` ·
  `cd connector && cargo build` · `cargo build --manifest-path shield/Cargo.toml` ·
  `buf generate` (Go stubs) + `cargo build` (Rust stubs) ·
  `cd admin && npm run codegen && npx tsc --noEmit`.

---

## Implementation plan (4 phases, manual, step-by-step)

Each phase builds, passes its gate, and leaves the system safer than before. Phase 1 alone fixes
the delete-orphan bug (Finding 5). Do not start a phase until the prior gate is green.

### PHASE 1 — Tombstone delete (closed-loop via the existing remove-ack)
**Fixes:** zombie-on-delete (online + offline-then-reconnect), guard/message mismatch,
`failed`-with-rule orphan, stale index (Finding 9). **Not yet:** reboot fail-open, non-delete drift.

- **1.1 Migration** `controller/migrations/015_resource_deleting_state.sql`
  - Drop + re-add `resources_status_check` with `'deleting'` in the IN-list.
  - Drop `idx_resources_managing`; create
    `idx_resources_pending ON resources(shield_id, status) WHERE status IN ('protecting','deleting')`.
- **1.2 Store** `controller/internal/resource/store.go`
  - `MarkDeleting(tenantID, id)`: `UPDATE … SET status='deleting', pending_action='remove',
    updated_at=NOW() WHERE id=$1 AND tenant_id=$2 AND status IN ('protected','failed') RETURNING id`,
    then `GetByID` (row carries shield_id + connector_id for the push).
  - Rename `SoftDelete` → `DeleteRow`; guard `status IN ('pending','unprotected')`; fix error text.
  - `GetPendingForShield`: `status IN ('protecting','deleting')` (replay remove on reconnect).
  - `RecordAck`: if current row `status='deleting'` and ack status `'unprotected'` → `DELETE` row
    (reap on confirmation). Keep existing stale-ack guards.
- **1.3 Resolver** `controller/graph/resolvers/resource.resolvers.go` `DeleteResource`
  - Load row → branch: `protecting` → reject; `pending`/`unprotected` → `DeleteRow`;
    `protected`/`failed` → `MarkDeleting` + `ConnectorRegistry.PushInstruction(row)` (remove).
- **1.4 Frontend** `admin/src/pages/ResourceDetail.tsx`
  - Add `'deleting'` to the transitional set (spinner banner + 3s polling); disable Delete during
    `protecting`/`deleting`; label "Deleting…".
- **Gate 1:** protect→delete (online): rule drops, row reaped on `unprotected` ack. Delete while
  connector/shield offline: row stays `deleting`; reconnect → remove replays → reaped. Builds + tsc clean.

> **STATUS: ✅ IMPLEMENTED + VERIFIED (2026-06-03)** against the live stack (controller + connector +
> remote shield). Scenario 1 (online delete): `protected → deleting → reaped` <2s, nftables 5173
> rule dropped. Scenario 2 (offline delete): row **held at `deleting`** 40s+ while shield stopped
> (no orphan, intent preserved), **auto-reaped on shield reconnect** via the connector's buffered
> `remove`. Scenario 4 (pending delete): immediate `DeleteRow`, no shield round-trip. Migration 015
> + index swap (Finding 9) live. One bug found+fixed during review: a missing closing quote in the
> `RecordAck` reap SQL. Phase 1 not yet committed at time of writing.

### PHASE 2 — Desired-state snapshot on (re)connect (open-loop push)
**Fixes:** reboot fail-open (protected re-applied), reconnect drift (missed removes dropped).

- **2.1 Proto** (model on `DiscoveryReport`/`ACLSnapshot`)
  - `proto/shield/v1/shield.proto`:
    `message ResourceSnapshot { repeated ResourceInstruction resources = 1; uint64 generation = 2; }`
    + oneof `ResourceSnapshot resource_snapshot = 12;` in `ShieldControlMessage`.
  - `proto/connector/v1/connector.proto`:
    `message ResourceSnapshotBatch { map<string, ResourceSnapshot> shield_snapshots = 1; }`
    + oneof `ResourceSnapshotBatch resource_snapshots = 13;` in `ConnectorControlMessage`.
  - Regenerate: `buf generate` + `cargo build` (connector + shield).
- **2.2 Controller** `store.go` + `internal/connector/control_stream.go`
  - `GetDesiredForShield(shieldID)`: `status='protected' OR (status='protecting' AND pending_action='apply')`.
  - Rename `pushPendingInstructions` → `pushDesiredSnapshots`: build a `ResourceSnapshotBatch` per
    shield from `GetDesiredForShield` and send it on connect (replaces protecting-only deltas).
- **2.3 Connector** `connector/src/control_stream.rs` + `agent_server.rs`
  - Handle `ConnectorControlMessage::ResourceSnapshots` → forward each shield's `ResourceSnapshot`
    over its Control stream (new arm beside `push_instructions`).
- **2.4 Shield** `shield/src/control_stream.rs` + `resources.rs`
  - Handle `ShieldControlMessage::ResourceSnapshot`: validate host per resource, **replace**
    `state.active` with the snapshot, bump generation, `apply_nftables(&active)` once (atomic).
- **Gate 2:** protect 2 → reboot shield → both rules reappear. Protect, unprotect while connector
  briefly down (missed remove) → reconnect snapshot omits it → rule dropped.

> **STATUS: ✅ IMPLEMENTED + VERIFIED (2026-06-04)** on the live stack (controller local; connector
> on "Archer" 192.168.1.87; shield on 192.168.1.164). Observed in logs: snapshot built on
> connector-connect (generation = unix-millis), cached, **"replayed cached resource snapshot on
> connect"** on shield (re)connect, shield **"resource snapshot applied — chain rebuilt"** with the
> matching generation; after a shield process restart the same cached snapshot re-applied (fresh
> process starts at generation 0 — correct). Protect showed the **dual delivery**: incremental apply
> + live snapshot rebuild ~40ms apart (idempotent by design). Shield restart with a protected
> resource recovered seamlessly — DB `last_verified_at` never exceeded its normal 15s sawtooth.
> Deployed via locally-built binaries (update timers stopped during test); release after merge.

### PHASE 3 — Closed-loop: shield state report + controller reconciler
**Fixes:** transient-disconnect zombies, partial applies, lost messages. Enables confirmation-gated reaping.

- **3.1 Proto** (mirror `DiscoveryReport` + `ShieldDiscoveryReport`)
  - `shield.proto`: `message ResourceStateReport { string shield_id = 1; uint64 generation = 2;
    repeated string active_resource_ids = 3; uint64 fingerprint = 4; }`
    + oneof `ResourceStateReport resource_state = 13;`
  - `connector.proto`: `message ResourceStateBatch { repeated ResourceStateReport reports = 1; }`
    + oneof `ResourceStateBatch resource_state = 14;`
  - Regenerate (buf + cargo).
- **3.2 Shield** `resources.rs` + `control_stream.rs`
  - `generation: AtomicU64` in `SharedResourceState`, bump on every apply/remove/snapshot.
  - On each heartbeat, emit `ResourceStateReport` (active IDs + fingerprint + generation).
- **3.3 Connector** forward `ResourceStateReport` → `ResourceStateBatch` to controller (mirror discovery forwarding).
- **3.4 Controller reconciler** (new `internal/resource/reconcile.go`, invoked on each `ResourceStateBatch`)
  - Scope to `shield_id == report.shield_id` (security).
  - `desired = GetDesiredForShield`; `reported = report.active_resource_ids`.
    - reported∉desired → orphan → send `remove`.
    - desired∉reported → missing → send `apply`.
    - `deleting` tombstone id∉reported for **≥N consecutive reports** → reap (`DELETE`).
  - Hysteresis: track "inconsistent since gen/time"; act only past the grace threshold. Use
    `generation`/`fingerprint` to skip unchanged reports.
- **Gate 3:** inject drift (stray nft rule / killed remove) → controller re-issues correction.
  Delete while offline → reconnect snapshot drops it → next report confirms absent → tombstone
  auto-reaped. Verify no thrash on a freshly-protected resource (hysteresis).

> **STATUS: ✅ IMPLEMENTED + VERIFIED (2026-06-08) — all of 3.1–3.5 done.**
> Gate 3 passed on the live stack (controller local `go run`; connector on Archer 192.168.1.87;
> shield on inkyank-01 192.168.1.164, binaries hand-deployed from the branch).
> - **Test 1 (no-thrash):** protected resource steady ~90s, zero `reconcile:` chatter. ✅
> - **Organic orphan:** a stale connector-cached snapshot replayed a `failed` resource onto the
>   shield; controller logged `ORPHAN ×2 → drift persisted 2 reports → re-pushing snapshot`, next
>   report clean. ✅ (Hysteresis confirmed: acted only on the 2nd consecutive drift report.)
> - **Test 2 finding:** injecting `status='unprotected'` on a legitimately-enforced+reachable
>   resource did NOT create a stable orphan — `RecordAck`'s periodic re-verify ack healed it back
>   to `protected` within one heartbeat. Good property: the shield's truth overrides DB corruption.
>   The `AND status != 'deleting'` guard is what makes `deleting` immune to this (so reap is stable).
> - **Test 3 (tombstone reap — headline):** injected `status='deleting'` directly (no resolver →
>   no remove instruction → no ack). Reaped purely by the reconciler in ~76s (≈5 heartbeats:
>   ~2 to resync the orphan, ~3 to confirm absent). `nft list chain inet zecurity resource_protect`
>   on the host confirmed the 5173 rule gone (empty chain). ✅
>
> **Open follow-ups (not blockers):**
> - **`failed`-in-desired decision (UNRESOLVED):** `GetDesiredForShield` excludes `failed`, so a
>   port-not-listening resource (which the shield DID apply a rule for) gets its rule stripped by
>   the reconciler. Fail-closed alternative: include `failed` in the desired set so a temporarily
>   down service keeps enforcement. One-line SQL change; product call pending.
> - **`RenewCert not implemented`** in the dev `go run` controller — shield cert renewal fails;
>   will expire the 7-day cert and cause an mTLS lockout (same failure mode as 2026-06-03). Fix
>   the controller build before long-running deployments. Unrelated to ADR-004.
>
> _Original in-progress detail below (kept for history)._

> **(historical) IN PROGRESS (2026-06-05) — steps 3.1–3.3 implemented, 3.4–3.5 pending.**
> - **3.1 protos ✅** — `ResourceStateReport` (shield oneof field 13: shield_id, generation,
>   sorted active_resource_ids, fingerprint) + `ResourceStateBatch` (connector oneof field 14).
>   buf + both cargo regens clean; controller builds.
> - **3.2 shield ✅** — `state_seq: Mutex<u64>` in `SharedResourceState`; `bump_state_seq()` at
>   ALL FOUR active-set mutation points (apply upsert, apply Err-rollback retain, remove retain,
>   snapshot replace); `build_state_report()` (sorted ids → DefaultHasher fingerprint); report
>   emitted on every heartbeat tick after the ack drain (`control_stream.rs`).
> - **3.3 connector ✅** — `pending_state` latest-wins map in `ShieldMaps`; inbound
>   `Body::ResourceState` arm buffers per shield; `drain_state_batch()` (exact mirror of
>   `drain_discovery_batch`); flushed upstream on the health tick after the ShieldStatus send.
> - **3.4 controller (NEXT)** — store helpers `GetDeletingForShield` + `ReapTombstone`;
>   new `internal/connector/reconcile.go` (security scope: shield must belong to reporting
>   connector+tenant; drift → 2-consecutive-report hysteresis → `buildSnapshotMsg` re-push;
>   tombstone absent 3 consecutive reports → reap); `Recon reconcileState` field on
>   `EnrollmentHandler` (zero-value-ready, lazy maps); `ConnectorControlMessage_ResourceState`
>   case in the control-stream recv loop. Full code in the Phase 3 chat guide.
> - **3.5 gate (PENDING)** — Runtime A: SQL-delete a protected row → orphan detected → snapshot
>   resync drops rule. Runtime B (showcase): delete-while-shield-down + connector restart (buffer
>   lost) → tombstone reaped via report-confirmed absence ≈45s. Runtime C: no reconciler chatter
>   during normal operations.
> - Reports reflect the shield's in-memory intent state, NOT raw kernel nftables (manual nft
>   tamper invisible — documented limitation). Hysteresis counters are controller-memory.

### PHASE 4 — UX, break-glass, observability, cleanup
- **Frontend: finalize `deleting` UX (list + detail). ✅ DONE (2026-06-10).**
  - `Resources.tsx`: fixed `transitionalStates` — was `["managing","protecting","removing"]` (legacy
    states renamed in mig 009, MISSING `deleting`) → `["protecting","deleting"]`. A `deleting` row now
    polls at 3s, so it disappears promptly after the reconciler reaps it instead of lingering for the
    30s slow interval (which read as a hung delete).
  - `ResourceDetail.tsx`: during `deleting`, `isProtected` is false, which previously rendered the
    misleading "No shield is enforcing / Install a shield" hero AND a "Protect this resource" button.
    Added a dedicated deletion hero + a "Removing" row in the Protection panel; suppressed the
    unprotected CTA. Copy states the row persists until the shield confirms removal (not a hung
    delete) and points to Force delete (4.1) if the shield is gone. Transitional banner scoped to
    `protecting` to avoid duplicating the deletion hero.
- **Break-glass: admin-only `forceDeleteResource(id)` (`@hasRole([ADMIN])`), audit-logged. ✅ DONE (2026-06-10).**
  - `internal/resource.ForceDeleteRow` — tenant-scoped hard `DELETE` in ANY state, bypassing the
    confirmation-gated tombstone path; for resources stuck because their shield is gone.
  - Resolver `ForceDeleteResource`: snapshot row → force-delete → audit-log → best-effort
    `PushSnapshotForShield` (a still-connected shield drops the now-removed rule via replace-semantics).
  - Durable audit: migration `016_audit_logs.sql` (append-only `audit_logs`) + `internal/audit`
    package `Record()` (write-and-log; never fails the already-completed action). Action key
    `resource.force_delete`. First consumer of a reusable audit table.
  - Frontend: guarded "Force delete" button on `ResourceDetail`, shown only when the resource is
    transitional (where normal Delete is disabled), behind a stern break-glass confirm.
- **Finding 8 cleanup ✅ DONE (2026-06-10) — Option A (drop the dead scaffolding).**
  Resources are hard-deleted and the tombstone is `deleting` + ack-gated reap, so the soft-delete
  leftovers were pure confusion. Repurposing `deleted_at` as a tombstone timestamp (Option B) was
  rejected: `updated_at` already records when `MarkDeleting` fired, and a `deleted_at` on a
  `deleting` (not deleted) row is misleading. Changes:
  - migration `017_resources_drop_soft_delete.sql`: drop indexes → `DROP COLUMN deleted_at` →
    recreate `idx_resources_shield` / `idx_resources_pending` without the `deleted_at IS NULL`
    predicate → swap `resources_status_check` to drop the unreachable `'deleted'` value
    (enum is now `pending|protecting|protected|unprotected|failed|deleting`).
  - `internal/resource/store.go`: removed all seven `deleted_at IS NULL` filters.
  - frontend: `resourceTone` in `Resources.tsx` + `ResourceDetail.tsx` aligned to the real enum —
    dropped dead `'deleted'`/`'managing'`/`'removing'`, and FIXED a latent bug where `Resources.tsx`
    had no tone for the real `deleting` state (fell through to `info`).
  - SCOPE NOTE: only the `resources` table touched. The `'deleted'` status on workspaces / users /
    connectors / shields / remote_networks is real in-use soft-delete and was left untouched.
- **Observability ✅ DONE (2026-06-10) — Prometheus metrics on the reconciler.**
  Backend: `github.com/prometheus/client_golang` with a private registry; exposed on a SEPARATE
  internal listener (`METRICS_ADDR`, default `127.0.0.1:9102`) — never the public mux, since metrics
  leak operational data. New `internal/metrics` package owns the collectors + typed helpers; wired
  into `internal/connector/reconcile.go`.
  - `reconcile_reports_total` (counter) — shield reports processed (denominator).
  - `reconcile_drift_detected_total{kind="orphan"|"missing"}` (counter) — drifting resources seen.
  - `reconcile_resyncs_total` (counter) — corrective snapshot re-pushes after hysteresis.
  - `reconcile_tombstones_reaped_total` (counter) — confirmed-absent tombstones reaped.
  - `reconcile_shields_drifting` (gauge) — shields drifting right now.
  - `reconcile_tombstones_pending` (gauge) — tombstones awaiting reap; a sustained non-zero value =
    a gone shield = a break-glass (Phase 4.1 `forceDeleteResource`) candidate.
  - NAMING: the original wishlist `orphans_removed`/`missing_reapplied` were renamed. Removal happens
    on the shield via snapshot replace-semantics — the controller never gets a per-orphan removal
    confirmation, only the orphan's absence in the NEXT report. So the honest controller-side signals
    are `drift_detected{kind}` (seen) + `resyncs_total` (corrective action); "removed" is observable
    as `drift_detected{orphan}` going quiet after a resync.
  - CARDINALITY RULE: no `shield_id`/`tenant_id`/`resource_id` labels (unbounded → series explosion).
    Only `kind` (orphan|missing). Per-entity detail stays in the logs.

## Sequencing
- **Phase 1 is independently shippable** — fixes Finding 5 safely; ship first.
- **Phases 2–3 are the real architecture** — also resolve Findings 6 & 7 and reboot fail-open; they
  overlap **Piece 6** (connector↔shield delivery) in the code study.
- Do not ship Phase 3 with hysteresis disabled — that's the one part that can misbehave; test the
  grace window deliberately.

## Consequences
**Positive:** eliminates zombie rules, reboot fail-open, missed apply/remove, transient-disconnect
drift, partial failures, lost messages; offline-shield recovery; eventual consistency. Aligns the
data plane with the snapshot direction of ADR-001 (ACL snapshots) and Sprint 9 RDE.

**Negative / cost:** a new shield→controller state report + a reconcile loop + generation/fingerprint
bookkeeping; tombstones add a state to the machine and a reaping path; the reconciler needs careful
hysteresis to avoid thrash. Larger than a `SoftDelete` patch — it's a subsystem.
