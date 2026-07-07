---
type: phase
member: M1
sprint: 11.2
phase: 2
title: Client — Background ACL Sync Scheduler
status: completed
commit: 4168cf3
depends_on:
  - Sprint11.2/Member1-Client/Phase1-MultiTransport
---

# Phase 2 — Client: Background ACL Sync Scheduler

## Finding (from Relay E2E Security Review, F3)

**The client's ACL sync was action-triggered only — an idle client never converged.**

`ACL_REFRESH_TTL_SECS = 60` looked like a sync interval but was only a
**staleness gate**: `refresh_acl_if_needed` checked "is the cached snapshot
older than 60s?" and was called exclusively from IPC handlers. The actual
fetch triggers were:

| Trigger | Fetch behavior |
|---|---|
| Daemon startup (stored state present) | One-shot background fetch |
| Login (`PostLoginState`) | Always |
| `zecurity-client up` | Only if cache older than 60s |
| `zecurity-client resources` | Only if cache older than 60s |
| `zecurity-client sync` | Always |
| **Time passing, tunnel up, no commands** | **Never** |

There was no timer, no interval task, no background loop. A headless client
with a long-lived tunnel kept its snapshot forever. Consequences:

- Revoked access stayed usable until the next user action (policy enforcement gap).
- New resources never appeared.
- A connector going offline (controller recompiles ACL, commit `bf66530`)
  never reached the client — it kept trying the dead connector first and
  relied on Sprint 11.2 multi-connector failover to mask it. Single-connector
  remote networks had no mask and failed until a manual `sync`.

## Fix (commit `4168cf3`)

Added `run_acl_sync_scheduler` to `client/src/daemon.rs` — a daemon-lifetime
tokio task spawned in `run()` after `tun_slot` creation:

```rust
async fn run_acl_sync_scheduler(state: SharedState, conf: ClientConf, tun_slot: TunSlot) {
    let mut ticker = tokio::time::interval(Duration::from_secs(ACL_REFRESH_TTL_SECS as u64));
    ticker.set_missed_tick_behavior(MissedTickBehavior::Delay);
    ticker.tick().await; // consume immediate first tick — startup already fetched

    loop {
        ticker.tick().await;
        // skip when not logged in (pre-login / post-logout)
        if no session or device in state { continue; }

        match sync_acl_now(&state, &conf).await {
            Ok(result) if result.changed => {
                // same path as the `sync` IPC handler
                restart_tunnel_if_running(&state, &conf, &tun_slot).await;
            }
            Ok(_) => {}   // unchanged — quiet
            Err(e) => warn!("background ACL sync failed — keeping cached snapshot"),
        }
    }
}
```

Design points:

- **Reuses the existing path** — `sync_acl_now` + `restart_tunnel_if_running`,
  identical semantics to the `sync` IPC handler. Version-change detection and
  the down/up restart live in one code path.
- **Token expiry handled transparently** — `sync_acl_now` goes through
  `fetch_acl_snapshot_with_refresh`, so the 401-refresh single-flight path
  covers the scheduler too.
- **Fail-open on staleness** — transient fetch failure keeps the cached
  snapshot (consistent with `refresh_acl_if_needed`).
- **Idle-safe** — skips ticks with no session/device; picks up again after login.

## Result

An idle client now converges on any policy change within one
`ACL_REFRESH_TTL_SECS` (60s) interval with no user action: revocations take
effect, new resources appear, and connector-offline ACL recompiles reorder
the client's connector list automatically.

## Implementation Checklist

- [x] **M1-F1** `client/src/daemon.rs` — `run_acl_sync_scheduler`: 60s interval task; identity gate; `sync_acl_now` + `restart_tunnel_if_running` on version change
- [x] **M1-F2** `client/src/daemon.rs` — spawn wired in `run()` after `tun_slot` creation (needs the slot for restart)
- [x] **Build gate:** `cd client && cargo build` passes

## Build Check

```bash
cd client && cargo build
```
