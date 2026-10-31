---
type: phase
member: M2
person: Barath
sprint: 22
phase: 5
execution: D
title: Relay Drain and Resume
status: implemented
depends_on: [2, 4]   # A (errors, audit helpers) and X (danger-zone card, dialog)
schema_change: true   # 043_relay_drain.sql
tags:
  - go
  - react
  - provider
  - relay
  - drain
  - provider-dashboard
---

# Phase 5 (D) — Relay Drain and Resume

> **Decision Record:** **D-32** (drain in scope; last-eligible-relay guard, audit, unchanged connector migration, 120 s connector drain timeout; amends D-21), **D-31** (relay-ops may drain/resume), **D-02** (single controller), **D-23** (existing telemetry only), ADR-017 (connectivity changes notify the transport plane, never the ACL).
>
> **Design review (2026-10-29):** problem statements P1–P10 reviewed by Barath with an external review (ChatGPT); decisions recorded below under **Design decisions**. Scope stays controller, migration, API and console: **no relay, connector or proto change.**

## How it works (verified 2026-10-26, re-checked 2026-10-29)

- Connectors place onto relays from the list the controller publishes (`BuildLabelledRelayList`, `relay/store.go`): active, capacity label high/medium, routable address. The list `version` is a content hash (`relayListVersion`): removing a relay changes it; adding the same relay back restores it.
- The list reaches connectors at connect time (`connector/control_stream.go`) and through `broadcastRelayList` (`cmd/server/main.go`).
- When a connector's active relay **leaves the list**, it migrates immediately: it registers with another relay, reports the switch at once (`ConnectorRelayState`, reason `switched`), then keeps the old session until its streams reach zero or `relay_drain_timeout_secs` (default **120**) passes, and aborts it (`connector/src/relay_selector.rs:601-631`).
- The switch updates `connector_relay_placement` and fires `NotifyTopologyChange` for that connector, so client transport snapshots follow. The snapshot keeps using a relay while it is `status='active'` (`policy/store.go:625`), so a draining relay keeps serving existing clients until their connector moves.
- So **drain = leave the list**. No connector, relay or proto change.

### Telemetry facts (verified 2026-10-29)

- `connection_count` = the relay's `ACTIVE_STREAMS`: client↔connector streams it is bridging right now (+1/−1 around `pipe_streams`, `relay/src/session.rs:277-279`). Not connector registrations, not idle client sessions. Streams on a migrating connector's old session stay counted until they end or the connector aborts the old session at 120 s.
- The relay sends it every **30 s**. The controller persists it to Postgres only when a DB write is due: **every 5 min** (`RELAY_HEARTBEAT_DB_WRITE_INTERVAL`) or when certificate/version/hostname/address metadata changes. A count change alone does not trigger a write. **The stored count can be ~5 min old.**
- No timestamp says when the stored count was measured. `last_heartbeat_at` is not one: the expiry loop also advances it from the Valkey liveness key (`RefreshLastHeartbeat`).
- `attached_connectors` (placement rows) drops when a connector **switches**, before its old session closes. Zero attached connectors alone does not mean zero traffic.
- The `ACTIVE_STREAMS` decrement is not drop-guarded: if a bridging task were cancelled mid-stream the count would stay above zero. Not confirmed reachable; it errs towards "Draining", never towards "Drained".

## Design decisions (2026-10-29)

| # | Problem | Decision |
|---|---------|----------|
| P1 | The eligibility rule is split between the SQL and the Go loop (the loop drops rows whose address comes out empty). A guard written from the SQL alone could count a relay that is never published and allow a drain that leaves **zero** relays. | **One shared SQL predicate** in `relay/store.go`, used by `BuildLabelledRelayList`, the drain guard and the read API: `status = 'active' AND capacity_label IN ('high','medium') AND drain_started_at IS NULL AND (NULLIF(public_addr,'') IS NOT NULL OR (address_scope = 'public' AND observed_ip IS NOT NULL))`. The Go empty-address skip stays as a defensive check only. |
| P2 | Two concurrent drains of the last two eligible relays must not both succeed. | **Row locks, fixed order.** One transaction: `SELECT id FROM relays WHERE id = $target OR (<eligible>) ORDER BY id FOR UPDATE` (target and eligible set in one statement, one lock order); re-read the target; refuse if it is eligible and the only eligible relay; else set `drain_started_at`; audit; commit. Under READ COMMITTED the waiting transaction re-checks `drain_started_at IS NULL` on the committed row, so the count is right. |
| P3 | The guard covers drain only; revoke, delete, capacity demotion and liveness can still empty the list. | **Revoke and delete stay unguarded** (security actions). The console **warns** (never blocks) when revoke/delete targets the only relay in the list, from server-computed fields (below). Documented: the guard stops an operator draining the last eligible relay; it does not guarantee the list is never empty. |
| P4 | "Drained" must not be shown while streams may still run, and the stored count can be stale. | Server-computed **`drain_state`** with a measured-at timestamp for the count and fresh writes while draining (rules below). Console text "Drained (as of the last heartbeat)", never an absolute guarantee. |
| P5 | `ConfirmDangerDialog` always requires a reason and the typed name (D-17); drain is reversible and not D-17. | Two props, defaults unchanged (revoke/delete untouched): `requireConfirmText` and `reasonRequired`. Drain passes `false` for both and states that draining reduces relay availability. Resume: plain button, no reason. |
| P6 | A draining relay later revoked/deleted keeps `drain_started_at`. | Revoke/delete leave the column as is; the read API returns `draining_since = null` and `drain_state = null` for `revoked`/`deleted`. |
| P7 | Audit and broadcast order. | State change + audit row in **one transaction** (audit failure rolls back). Broadcast **once, after commit, only on a real change**. No revocation-checker refresh, no direct transport-snapshot invalidation (placement changes already notify; ADR-017). |
| P8 | A failed post-commit broadcast leaves connected connectors on the old list. | **No new mechanism in Phase D.** Log it with the relay id; never roll back a committed drain. Reconnecting connectors get the current list at connect time. Periodic reconciliation is a follow-up (FU-6). |
| P9 | Resume must restore the pre-drain version. | Holds by the content hash when address and label didn't change during the drain; the test pins that case. |
| P10 | The relay itself doesn't know it is drained. | Drain is an **availability** tool, not a security control; revoke is the control. Said in help text and here. No certificate or ACL change. |

### P4: `drain_state` rules

`connection_count_at` = when the stored count was **measured**: the controller's receive time, captured at the top of `Service.Heartbeat` before any cache or DB work and written with the count by `RecordHeartbeat`. The proto has no relay-side timestamp; the relay reads its counter while building the request, so the gap is one RPC's transit (no proto change).

Computed in Go in `providerquery` from the row and an injected `now` (tests use explicit timestamps, never sleeps). `T_drain = 120 s` (the connector's default `relay_drain_timeout_secs`), `T_fresh = 90 s` (three heartbeats, same as relay expiry). Evaluated in this order:

| Order | Condition | `drain_state` | Console |
|-------|-----------|---------------|---------|
| — | not draining, or status `revoked`/`deleted` | `null` | — |
| 1 | `attached_connectors > 0` | `migrating` | "N connectors still attached" |
| 2 | `connection_count_at` missing, or `now − connection_count_at > T_fresh` | `unverified` | "No recent heartbeat: drain not confirmed yet" |
| 3 | `connection_count > 0` | `streams_open` | "M connections still open; they end within 120 s of their connector moving" |
| 4 | `connection_count_at < drain_started_at + T_drain` | `unverified` | "Waiting for the 120-second drain window" |
| 5 | otherwise (0 attached, count 0, measured ≥ 120 s after the drain started, ≤ 90 s old) | `drained` | "Drained (as of the last heartbeat): safe to revoke or delete" |

Known limits (documented, accepted):
- A connector that misses the broadcast keeps `attached_connectors > 0` (safe) until it switches; once it switches late, its old session can live up to 120 s after the measurement that produced `drained`, because `T_drain` runs from `drain_started_at`.
- A connector configured with `relay_drain_timeout_secs` above 120 is not covered by `T_drain`.
- Streams opened between heartbeats (≤ 30 s) are only seen at the next heartbeat; hence "as of the last heartbeat".

### P4: fresh count writes while draining (Valkey flag)

Postgres stays the source of truth for whether a relay is draining; the flag only **speeds up persistence**.
- Key `relay:heartbeat:draining:<id>` (next to the other `relay:heartbeat:*` keys), TTL 10 min. Set after a drain commits; deleted after a resume commits.
- **Reconcile:** a 60 s loop (`relay.RunDrainFlagLoop`, also once at start-up) sets the flag, with a fresh TTL, for every relay with `drain_started_at IS NOT NULL` and status `active`/`inactive`. A failed set after drain, a controller restart or an expired/evicted flag is repaired within one sweep; a failed delete after resume expires within 10 min (only extra writes).
- `cacheRelayHeartbeat`: a write is due when the throttle marker is absent **or** the draining flag is present; a Valkey error reading the flag counts as due (the existing conservative rule).
- **Fail-safe invariant:** every failure mode only makes counts *older* → `unverified`, never `drained` early. A heartbeat racing a drain/resume is harmless: a count measured before `drain_started_at + 120 s` can't produce `drained`.

### P3: revoke/delete warning fields

`RelayDetail` gains `in_relay_list` (bool) and `eligible_relay_count` (int), both computed with the shared P1 predicate (`id IN (SELECT id FROM relays WHERE <eligible>)`, `SELECT count(*) …`). The revoke and delete dialogs show "No eligible relays will remain. New connections may fail until another relay is eligible." when `in_relay_list && eligible_relay_count == 1`. Advisory only, valid at load time; the server never blocks revoke/delete on it.

## Schema — `043_relay_drain.sql` `[schema: reset DB]`

```sql
ALTER TABLE relays ADD COLUMN drain_started_at    TIMESTAMPTZ;
ALTER TABLE relays ADD COLUMN connection_count_at TIMESTAMPTZ;
```

## Contract

| Route | Allowed | Body | Success | Errors |
|-------|---------|------|---------|--------|
| `POST /provider/relays/{id}/drain` | super-admin, relay-ops (`relay.drain`) | `{reason?}` (optional, trimmed, ≤ 500 code points) | **204**; audit `relay.drain` (actor, reason, client IP); flag set; broadcast | 400 `invalid_request` / `reason_too_long`; 404 `not_found`; 409 `invalid_state` (pending), `already_revoked`, `already_deleted`, `last_eligible_relay`; already draining → 204 no-op, not audited, no broadcast |
| `POST /provider/relays/{id}/resume` | super-admin, relay-ops (`relay.resume`) | none (empty or `{}`) | **204**; audit `relay.resume` (actor, client IP, `drained_since`); flag cleared; broadcast | 404; 409 `already_revoked` / `already_deleted`; not draining → 204 no-op, not audited, no broadcast |

- Drain is allowed on `active` and `inactive` relays. A non-eligible target (inactive, low tier, no routable address) may always be drained.
- Errors use the Phase A JSON helpers; 500 bodies are exactly `{"error":"server_error"}`.
- **Read API (additive):** `RelaySummary.draining_since` and `drain_state` (both nullable); `RelayDetail.in_relay_list`, `eligible_relay_count`.

## Console

- Relays list: **Draining** badge next to the status.
- Relay detail: **Drain relay** (both roles; dialog with an optional reason, the availability warning and the 120-second behaviour) and **Resume relay** (both roles; no dialog); drain progress from `drain_state` (table above), refreshed on Refresh.
- Revoke/delete dialogs: the advisory last-relay warning (P3).
- Overview "Needs attention": draining relays with their `drain_state`.
- `api-surface.test.ts`: drain and resume pinned as writes; `relay-errors.ts`: `last_eligible_relay`, `invalid_state`.

## Tests (write first)

**Go (DB-backed, real Valkey where Valkey is involved):**
- Shared predicate: empty-string and NULL addresses, public scope without observed IP, low tier, inactive, draining → never in the list **and** never counted by the guard or `eligible_relay_count`; `in_relay_list` matches the published list.
- Drain removes the relay from `BuildLabelledRelayList` and changes `version`; resume restores both.
- Guard: only eligible relay → 409; two eligible relays drained **concurrently** (goroutines released together) → exactly one 204, the other 409 `last_eligible_relay`, and the **published list** still has one relay; non-eligible target → allowed.
- State rules: pending → `invalid_state`; revoked/deleted → `already_*`; repeat drain/resume → 204, no audit, no broadcast.
- Authz: relay-ops allowed for drain/resume, still 403 for revoke/delete.
- Audit rows with actor, reason, client IP; rollback (no drain) when the audit write fails.
- Broadcast and flag hooks called exactly once per real change, after commit; never on no-op or error; broadcast failure is logged and the drain stays committed.
- Heartbeat: draining flag forces a DB write; `connection_count_at` = receive time; flag read error forces a write; reconcile restores a missing flag and leaves non-draining relays alone.
- `drain_state` table above with explicit timestamps: zero count measured before `drain_started_at + 120 s` → not `drained`; zero count older than 90 s → `unverified`; fresh zero after the window with 0 attached → `drained`; non-zero → `streams_open`; stale zero can't hide a newer non-zero (the newest stored count wins; `unverified` while stale); attached > 0 wins over everything; revoked/deleted → null.
- Reconnecting connector receives the current list (connect-time push excludes a draining relay).

**Console:** badge; buttons per role and state; dialog props (drain: no typed name, optional reason; revoke/delete unchanged); each `drain_state` text; last-relay warning shown/hidden and never disabling confirm; Overview item; API-surface pin.

**Regression:** `cd connector && cargo test relay_selector` unchanged and passing.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-D1 … AT-D12** (AT-D6 is the live drain on the lab fleet).

## Test gate

Go gate, console gate, schema gate (`043_`) and connector regression from [[Sprint22/path]].

## Implementation record (2026-10-29, branch `sprint22/phase-d-relay-drain`)

**Controller**
- `internal/relay/eligible/eligible.go` (new package, no `net/http`): `eligible.Where`, the one eligibility predicate (P1). Used by `BuildLabelledRelayList`, `Store.DrainRelay` and `providerquery.GetRelay`.
- `internal/relay/drain.go` (new): `Store.DrainRelay` (target + eligible rows in one `ORDER BY id FOR UPDATE` statement; refuses the last eligible relay; state rules; in-tx audit hook), `Store.ResumeRelay`, `Store.ListDrainingRelayIDs`; `Service.MarkDraining` / `ClearDraining`; `ReconcileDrainFlags` / `RunDrainFlagLoop`.
- `internal/relay/admin_handler.go`: `Drain` and `Resume` handlers; `authorizeRelayAction` shared with revoke/delete (actor, `decide()`, UUID check); `decodeOptionalJSON` (empty body allowed, otherwise the bounded strict decoder); `OnRelayDrainChanged` post-commit hook (only on a real change).
- `internal/relay/heartbeat.go`: `receivedAt` taken at the top of `Heartbeat` and stored as `connection_count_at`; the draining flag (or a failed flag read) makes the DB write due.
- `internal/relay/store.go`: list uses `eligible.Where`; `RecordHeartbeat(..., measuredAt)` writes `connection_count_at`; an orphaned doc comment above `UpsertPlacement` removed.
- `internal/provider/authz.go`: `ActionRelayDrain` / `ActionRelayResume`, `CanDrainRelay` / `CanResumeRelay` (relay-ops through the `relay.` prefix; revoke/delete stay in `superAdminOnlyRelayActions`).
- `internal/providerquery/relays.go`: `draining_since`, `drain_state` (`drainState()`, constants `DrainWindow` 120 s, `DrainFreshness` 90 s), `in_relay_list`, `eligible_relay_count` (still 3 statements per detail).
- `cmd/server/main.go`: routes `POST /provider/relays/{id}/drain` and `/resume` behind `RequireProvider`; `OnRelayDrainChanged` sets/clears the flag (logged on failure) and calls `broadcastRelayList`; drain-flag loop goroutine.
- `migrations/043_relay_drain.sql`: `drain_started_at`, `connection_count_at`.

**Console**
- `components/RelayDrainCard.tsx` (new): Drain (dialog, optional reason, availability warning) and Resume (one click) for both roles on active/inactive relays; progress text from `drain_state` only; super-admins are told a drained relay can be revoked or deleted.
- `components/ConfirmDangerDialog.tsx`: `reasonRequired`, `requireConfirmText` (defaults unchanged) and an advisory `warning` note.
- `components/RelayDangerZone.tsx`: last-relay warning when `in_relay_list && eligible_relay_count === 1`.
- `pages/RelayDetail.tsx`, `pages/Relays.tsx`: Draining badge; `pages/Home.tsx`: "Needs attention" item per draining relay with its state.
- `api/relays.ts` `drainRelay` / `resumeRelay`; `api/types.ts` fields and codes; `lib/relay-errors.ts` `drainActionFailure`; `api-surface.test.ts` pins both writes.

**Tests**
- Go (DB-backed, throwaway databases, isolated Valkey): `drain_db_test.go` (shared predicate, drain/resume + version, guard, 10-round concurrent race, state rules and no-ops, relay-ops authz, body validation, audit rollback, draining list, measured time), `drain_flag_test.go` (flag forces writes, flag read error forces writes, reconcile, no-cache), `providerquery/relays_drain_test.go` (`drainState` table with explicit timestamps, read API fields), authz matrix, DTO surface, route coexistence.
- Mutation checks: removing `FOR UPDATE` makes the race test fail (both drains succeed, list empty); a guard on the old weaker predicate lets the last published relay be drained and fails the predicate test; dropping `in_relay_list` from the warning condition fails the console test.
- Gate 2026-10-29 (after the review fixes below): Go 1520 pass / 0 fail / 2 expected skips (`TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`); `go vet ./...` clean; console 507 pass, lint and build clean; `cargo test relay_selector` 27 pass (no connector change). Schema: every DB-backed test applies all migrations, `043_` included, to a fresh database; the dev database was not reset.

**Live (pending, M1):** AT-D6 on the lab fleet (≥ 2 relays) with a long download; AT-D1…D5, D7, D11 through the console.

## Post-Phase Fixes

### Fix: independent review findings (2026-10-29, before commit)
An independent read-only review (verdict: ready with follow-ups; no Critical or High) confirmed the drain guard, lock ordering, audit atomicity, shared predicate, authz and flag fail-safety, and found:

**1. Post-commit hooks ran on the request context (Medium).** `OnRelayDrainChanged` and `OnRelayRevoked` got the request context with its 5 s budget. A client disconnecting right after the commit (or a budget spent on lock waits) cancelled the flag write and the relay-list broadcast: the change stayed committed but connectors kept the old list until an unrelated broadcast. Inherited from the revoke path (Phase A).

**Fix (`internal/relay/admin_handler.go`):**
```go
// BEFORE: h.OnRelayDrainChanged(ctx, relayID, draining)   // ctx = WithTimeout(r.Context(), 5s)
// AFTER:
hookCtx, cancelHook := fanoutContext(r) // WithTimeout(context.WithoutCancel(r.Context()), 10s)
defer cancelHook()
h.OnRelayDrainChanged(hookCtx, relayID, draining)
```
Same for `OnRelayRevoked` (revoke and delete).

**2. Broadcast failure logged without the relay id (Low, AT-D12).** The fan-out moved from `main.go` to `relay.DrainFanout(flags, publish)`; `main.go` splits `publishRelayList` (returns the error) from `broadcastRelayList` (logs it). DrainFanout logs flag and publish failures with the relay id and never undoes the change.

**3. `unverified` text on an offline relay (Low).** The card asked to "refresh in a minute" even for an inactive relay that can't confirm. `RelayDrainCard.tsx` now says the relay is inactive and confirms when its heartbeats return.

**Tests added:** `TestDrainDB_HooksRunAfterCommitOnDetachedContext` (drain, resume and revoke hooks see committed data and a live context after the request is cancelled; mutation-checked: on the request context it fails), `TestDrainDB_BroadcastFailureKeepsDrain` (AT-D12: drain committed and audited, failure logged with the relay id, connect-time list excludes the relay), `TestDrainFanout`, console "unverified on an inactive relay".

**Accepted follow-ups (not fixed, low impact):** *(all fixed 2026-10-30 except the out-of-order broadcasts, which FU-6 now corrects within a minute; see the next two entries)*
- Drain vs `EvictRelays` (multi-row `UPDATE … WHERE id = ANY`, no lock order) can deadlock; Postgres aborts one: the drain gets a 500 and can be retried, eviction retries next tick.
- `RecordHeartbeat` has no guard against an older heartbeat overwriting a newer count (`connection_count_at <= $12`); rare, since the relay's 10 s RPC timeout cancels the server-side write.
- `drain_started_at` (DB clock, transaction start) vs `connection_count_at` (controller clock): skew or a lock wait shortens the 120 s window slightly.
- Out-of-order relay-list broadcasts (existing; corrected by the next broadcast) — FU-6.
- `policy.GetActiveRelay` can pick a draining relay for the snapshot's top-level `RelayAddr`; clients use only the per-connector relay address today.
- `eligible.Where` evaluates to NULL for `address_scope IS NULL AND observed_ip IS NOT NULL` (no write path produces it); a `COALESCE(…, false)` would make it defensive.
- Untested: drain racing revoke/eviction/resume (reasoned safe: single-row lockers re-checked by EvalPlanQual).

### Fix: FU-7 review residuals (2026-10-30, branch `sprint22/fu6-fu7-relay-drain-residuals`)
**1. Drain vs eviction deadlock.** `EvictRelays` updated several rows in no fixed order while `DrainRelay` locks in id order; Postgres aborted one (drain → 500, eviction → retried next tick). Reproduced: the new race test fails with `deadlock detected (SQLSTATE 40P01)` on the old statement.
```sql
-- BEFORE: UPDATE relays SET status = 'inactive' ... WHERE id = ANY($1) AND status = 'active' AND last_heartbeat_at < $2
-- AFTER (internal/relay/store.go):
WITH locked AS (SELECT id FROM relays WHERE id = ANY($1::uuid[]) AND status = 'active'
                  AND last_heartbeat_at < $2 ORDER BY id FOR UPDATE)
UPDATE relays r SET status = 'inactive', updated_at = NOW() FROM locked WHERE r.id = locked.id
```
**2. Older heartbeat overwriting a newer count.** `RecordHeartbeat` now writes `connection_count` only when `connection_count_at IS NULL OR connection_count_at <= $12`, and `connection_count_at = GREATEST(connection_count_at, $12)`. The rest of the heartbeat (status, addresses, `last_heartbeat_at`) is written as before.

**3. One clock for the 120 s window.** `DrainRelay` set `drain_started_at = NOW()` (database clock, transaction start, before any lock wait) while `connection_count_at` is the controller's receive time. It now sets `drain_started_at = $2` with `time.Now().UTC()` taken after the row locks are held.

**4. `policy.GetActiveRelay` ignored drain.** Adds `AND drain_started_at IS NULL`. The snapshot's top-level `RelayAddr` is recompiled with the ACL, not on drain (ADR-017), so an already compiled snapshot keeps its address until its next compile; clients use the per-connector relay address today.

**5. `eligible.Where` could be NULL** (`address_scope IS NULL AND observed_ip IS NOT NULL`; `DrainRelay` scans it into a Go bool, so that relay's drain was a 500). Now `COALESCE((…), false)`. `capacity_label` is `NOT NULL` in the schema; `address_scope` is the only nullable input.

**Tests (`internal/relay/drain_residuals_db_test.go`, `internal/policy/active_relay_drain_test.go`):** `TestDrainDB_OlderHeartbeatKeepsNewerCount`, `TestDrainDB_EligibleWhereNeverNull`, `TestDrainDB_DrainStartIsAfterLockWait`, `TestDrainDB_DrainRacesEviction` (20 rounds, eviction in descending id order), `TestDrainDB_DrainRacesRevoke`, `TestDrainDB_DrainRacesResume`, `TestGetActiveRelay_SkipsDrainingRelay`. Each of fixes 1–5 was reverted once and its test failed.

### Fix: FU-6 relay list reconciliation (2026-10-30, same branch)
See `Sprint22/path.md` → Post-Sprint Fixes → FU-6. Per-stream record of the last relay list version sent; a 60 s sync sends the current list only to streams that are behind (the connector re-probes on every push, so a blind re-broadcast would cost every connector a probe sweep per minute).

### Finding: FU-14 running streams aren't cut at the 120 s drain timeout (AT-D6 lab run, 2026-10-30; open)
**Issue:** in the live AT-D6 run a 1 GiB download through the drained relay kept running after the connector's "old relay drain timed out with active streams still open" (+120.0 s) and completed at +442 s. The console correctly stayed at `streams_open` until it ended, then showed "Drained".

**Root Cause:** `connector/src/relay_handler.rs:167` runs each relay stream as a detached `tokio::spawn` task holding its own QUIC stream. On timeout `relay_selector.rs:631` calls `old.handle.abort()`, which ends only the session task (the accept loop); the stream tasks keep the old QUIC connection alive.

**Fix (not applied):** a connector change, outside Sprint 22 (D-32: no connector change). Close the old relay's QUIC connection, or abort the tracked stream tasks, when the drain timeout fires; add a selector test with a stream that outlives the timeout. Until then the console's "up to 120 seconds" overstates the cut. Tracked as FU-14 in `Sprint22/path.md`.

### Fix: console no longer promises a 120 s cut (2026-10-30, branch `sprint22/drain-text-fu14`)
**Issue:** the drain card, the drain dialog and the `streams_open` progress line said open connections end "for up to 120 seconds" / "within 120 seconds of their connector moving". The live AT-D6 run showed they run until they end (FU-14).

**Fix Applied (`provider-console/src/components/RelayDrainCard.tsx`):**
```
// BEFORE: "connections already on it keep running until they end, for up to 120 seconds."
// AFTER:  "connections already on it keep running until they end, which for a long transfer can take a while."
// BEFORE (streams_open): "They end within 120 seconds of their connector moving."
// AFTER:  "They keep running until they end, so a long transfer can keep it here; it reads as drained once they finish."
```
The `unverified` line ("a heartbeat measured at least 120 seconds after the drain started") is the server's measurement window and is unchanged. Tests: `RelayDetail.test.tsx` asserts the dialog says "keep running until they end" and doesn't mention 120 seconds, and checks the new `streams_open` line. Console 507 pass, lint and build clean. When FU-14 is fixed in the connector, the text can name the timeout again.

### Fix: FU-14 drain timeout now cuts (2026-10-31, branch `fix/connector-drain-timeout-cut`)
`RelayDrainTracker::cut()` closes the old relay connection (code `DRNT` 0x44524E54); `relay_selector::drain_old_relay` calls it when `drain_timeout` expires with streams still open. Before, only the session task was aborted, and detached stream tasks kept the connection. Details and tests: `Sprint22/path.md` → Post-Sprint Fixes → FU-14. The console's drain text (#160) can name the 120 s limit again once connectors run this fix.

