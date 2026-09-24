---
type: phase
member: M2
person: Barath
sprint: 20
phase: 1
execution: A
title: Relay Liveness Truth
status: implemented   # code + tests done; live acceptance PENDING
depends_on: []
tags:
  - go
  - relay
  - valkey
  - liveness
  - provider-dashboard
---

# Phase 1 (A) — Relay Liveness Truth

> Day-1 work, independent. Decision Record prerequisite #1 (Q15) and D-23 ("existing data only,
> **after the relay-liveness fix**").

## Problem (verified)

```text
Relay ──Heartbeat every 30s──▶ Service.Heartbeat (internal/relay/heartbeat.go:54)
                                  │
                                  ├─ Valkey  relay:heartbeat:last:<id>      ← written EVERY heartbeat (true liveness)
                                  └─ Postgres relays.last_heartbeat_at      ← written only if metadata changed
                                                                              or the 5-min db-write marker expired
                                                                              (cacheRelayHeartbeat, heartbeat.go:178-209)

RunExpiryLoop (expiry.go, main.go:579: interval 60s, expiry 90s)
  └─ EvictExpiredRelays (store.go:408): active AND last_heartbeat_at < now-90s → 'inactive'
```

A healthy relay with unchanged metadata is marked `inactive` about 90–150 s after each DB write. It
returns to `active` at the next DB write, up to 5 min later.

**Second defect (recovery lag).** `RecordHeartbeat` (store.go:611) re-activates a relay only on a
**DB-written** heartbeat. After eviction, if the relay's Valkey `metadata` key and `db-write` marker
are still alive (TTL ≥ 5 min), the next heartbeats skip the DB. A relay that really came back stays
`inactive` for up to 5 min.

## Goal

1. Never evict a relay whose real heartbeat (Valkey liveness key) is fresh.
2. Keep the Sprint 10.3 DB-write throttle (`RELAY_HEARTBEAT_DB_WRITE_INTERVAL`).
3. A relay that is evicted and then heartbeats again is `active` again on its **first** heartbeat.

## Files

| File | Change |
|------|--------|
| `controller/internal/relay/expiry.go` | Liveness-aware `runEviction`; new `livenessSource` interface; clear throttle markers after eviction |
| `controller/internal/relay/store.go` | `ListEvictionCandidates`, `RefreshLastHeartbeat`, guarded `EvictRelays` (replaces/wraps `EvictExpiredRelays`) |
| `controller/internal/relay/heartbeat.go` | `Service.LastHeartbeat(ctx, relayID)` reads `relay:heartbeat:last:<id>`; `Service.ClearHeartbeatThrottle(ctx, relayID)` deletes `db-write` + `metadata` keys |
| `controller/cmd/server/main.go` (~579) | Pass `relaySvc` as the liveness source to `RunExpiryLoop` |
| `controller/internal/relay/expiry_test.go` | New cases (below) |
| `controller/internal/relay/heartbeat_test.go` | Re-activation after eviction |

## Design

### Liveness source

```go
// expiry.go
type livenessSource interface {
    // LastHeartbeat returns the most recent heartbeat time recorded in Valkey.
    // ok=false when the key is absent (expired or never written).
    LastHeartbeat(ctx context.Context, relayID string) (t time.Time, ok bool, err error)
    // ClearHeartbeatThrottle deletes relay:heartbeat:db-write:<id> and
    // relay:heartbeat:metadata:<id> so the next heartbeat writes Postgres.
    ClearHeartbeatThrottle(ctx context.Context, relayID string) error
}
```

`Service` implements it (it already owns `redis` and the key prefixes in heartbeat.go:27-31). The
value stored under `relay:heartbeat:last:<id>` is Unix seconds (`strconv.FormatInt(now.Unix(), 10)`,
heartbeat.go:185). With `redis == nil`, `LastHeartbeat` returns `ok=false, err=nil`; that path writes
Postgres on every heartbeat, so the DB is authoritative.

### Sweep algorithm (`runEviction`)

```text
threshold := now - expiry
candidates := store.ListEvictionCandidates(threshold)      // SELECT id WHERE status='active' AND last_heartbeat_at < threshold

for each id in candidates:
    t, ok, err := liveness.LastHeartbeat(id)
    switch:
      err != nil  → treat as "no liveness signal": evict per DB    (see invariant I3)
      ok && t >= threshold → alive: store.RefreshLastHeartbeat(id, t)   // guarded, no notify
      otherwise   → add to evict set

evicted := store.EvictRelays(evictSet, threshold)          // guarded UPDATE … RETURNING id
for each id in evicted:
    liveness.ClearHeartbeatThrottle(id)                    // best-effort, log on error
    NotifyTopologyChange per workspace (unchanged)
if len(evicted) > 0 → onPoolChange (unchanged)
```

### Store methods

```sql
-- ListEvictionCandidates
SELECT id::text FROM relays WHERE status = 'active' AND last_heartbeat_at < $1;

-- RefreshLastHeartbeat (never moves the timestamp backwards, never touches non-active rows)
UPDATE relays
   SET last_heartbeat_at = $2, updated_at = NOW()
 WHERE id = $1 AND status = 'active' AND last_heartbeat_at < $2;

-- EvictRelays (re-checks the predicate so a heartbeat DB write between SELECT and UPDATE wins)
UPDATE relays
   SET status = 'inactive', updated_at = NOW()
 WHERE id = ANY($1::uuid[]) AND status = 'active' AND last_heartbeat_at < $2
RETURNING id::text;
```

Keep `EvictExpiredRelays` only if other callers need it (grep first). Otherwise replace it.

## Invariants

- **I1** A relay whose Valkey liveness timestamp is ≥ threshold is never evicted.
- **I2** Eviction still notifies the **transport** plane only (`NotifyTopologyChange`), never the ACL (ADR-017 / Track B), and still calls `onPoolChange` once per sweep that evicted ≥ 1 relay.
- **I3** If Valkey is unavailable, behaviour falls back to DB-only eviction (today's behaviour). This is safe: on a cache error `Heartbeat` already forces a DB write (`shouldWriteDB = true`, heartbeat.go:88-92), so the DB is fresh while Valkey is down.
- **I4** `revoked` / `deleted` relays are never touched by the sweep or by `RefreshLastHeartbeat`.
- **I5** The DB-write throttle, its default (5 min), the 60 s sweep and the 90 s expiry are unchanged.
- **I6** After eviction, the relay's next heartbeat writes Postgres (markers cleared), so `RecordHeartbeat` sets `status='active'`.
- **I7** `relays.last_heartbeat_at` for a live relay is never older than `expiry + sweep interval` (≤ 150 s). This bounds what a dashboard shows until provider read APIs read Valkey directly (Dashboard Phase 2).

## Tests

`expiry_test.go` (fake store + fake liveness source):
- DB-stale, Valkey fresh → **not** evicted; `RefreshLastHeartbeat` called with the Valkey time; no topology notify; `onPoolChange` not called.
- DB-stale, Valkey key absent → evicted; topology notified per workspace; `onPoolChange` called once; `ClearHeartbeatThrottle` called.
- DB-stale, Valkey error → evicted (fallback I3).
- `ClearHeartbeatThrottle` error → eviction still reported and notified (best-effort).
- Mixed batch (one alive, one dead) → only the dead one evicted; `onPoolChange` once.

Store integration (same DB env as `store_revoke_integration_test.go`):
- `EvictRelays` does not evict a row whose `last_heartbeat_at` was advanced after candidate selection.
- `RefreshLastHeartbeat` never moves the timestamp backwards and ignores non-active rows.

`heartbeat_test.go`:
- After `ClearHeartbeatThrottle`, the next `Heartbeat` with unchanged metadata calls `RecordHeartbeat` (DB write) and the relay becomes `active`.

## Acceptance Criteria

> **Status: PENDING / NOT YET RUN.** These are live checks against a running relay. The behaviour
> is covered by unit/integration tests (see Implementation Checklist), but the live run has not
> been done yet.

- [ ] **PENDING** — With default config, a relay heartbeating every 30 s with unchanged metadata stays `active` for ≥ 10 min (manual or integration run; watch `relays.status`).
- [ ] **PENDING** — Stopping the relay process → `inactive` within ≤ 150 s; connectors receive a relay list without it.
- [ ] **PENDING** — Restarting it within 5 min → `active` on its first heartbeat (no 5-min lag).
- [ ] **PENDING** — No change in DB write rate for a healthy relay beyond one `RefreshLastHeartbeat` per sweep when stale.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/...
```

## Implementation Checklist

- [x] **M2-A1** `expiry.go` — `livenessSource`; liveness-aware `runEviction`
- [x] **M2-A2** `store.go` — `ListEvictionCandidates`, `RefreshLastHeartbeat`, guarded `EvictRelays` (replaces `EvictExpiredRelays`, which had no other callers)
- [x] **M2-A3** `heartbeat.go` — `LastHeartbeat`, `ClearHeartbeatThrottle` on `Service`
- [x] **M2-A4** Clear throttle markers after eviction (`db-write` + `metadata`; the liveness key is never deleted)
- [x] **M2-A5** `main.go` — wire `relaySvc` into `RunExpiryLoop`
- [x] **M2-A6** Tests:
  - `expiry_test.go`: `TestRunEviction_FreshLivenessRefreshesInsteadOfEvicting`, `TestRunEviction_StaleEverywhereEvictsAndClearsThrottle`, `TestRunEviction_LivenessErrorFallsBackToPostgres`; the 4 existing tests were updated to the new signature
  - `heartbeat_test.go`: `TestHeartbeat_FirstHeartbeatAfterEvictionPersists` (miniredis)
  - `store_evict_integration_test.go`: `TestEvictRelaysIntegration_HeartbeatWinsRace`, which ran against Postgres (`PKI_TEST_DATABASE_URL`), not skipped
- [x] **Build gate:** `cd controller && go build ./...` and `go test ./internal/relay/...` pass (0 skips with the DB env set)
- [ ] **Live acceptance — PENDING / NOT YET RUN** (see Acceptance Criteria)

## Post-Phase Fixes

### Fix: `ClearHeartbeatThrottle` multi-key DEL panic
**Issue:** The first implementation deleted both throttle markers with one `DEL`. valkey-go panicked (`multi key command with different key slots are not allowed`), which would have crashed the expiry sweep on the first eviction.

**Root Cause:** valkey-go checks cluster slots on the client side and rejects multi-key commands whose keys hash to different slots. `relay:heartbeat:db-write:<id>` and `relay:heartbeat:metadata:<id>` hash to different slots.

**Fix Applied (`controller/internal/relay/heartbeat.go`, `ClearHeartbeatThrottle`):**
```go
// BEFORE:
s.redis.Del(ctx, relayHeartbeatDBWritePrefix+relayID, relayHeartbeatMetadataPrefix+relayID)

// AFTER: one DEL per key
for _, key := range []string{relayHeartbeatDBWritePrefix + relayID, relayHeartbeatMetadataPrefix + relayID} {
    if err := s.redis.Del(ctx, key).Err(); err != nil { ... }
}
```
Caught by `TestHeartbeat_FirstHeartbeatAfterEvictionPersists` before commit. No other files affected.
