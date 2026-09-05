---
type: phase
member: M2
sprint: 15
phase: 2
title: Daemon Collection Scheduler
status: planned
depends_on: [Phase1-Proto-and-Linux-Collectors]
tags: [rust, client, posture, daemon, pending-08]
---

# Phase 2 — Daemon Collection Scheduler

> Depends on Phase 1 (collectors + generated RPC stub).

## Goal

Wire posture collection into the daemon's existing lifecycle: run at startup and every
5 minutes, and submit over the same authenticated control-plane path already used for
ACL-snapshot fetch.

## Files

| File | Change |
|------|--------|
| `client/src/daemon.rs` | new collection scheduler + submission call |

## Scheduling

- Run collection once at daemon startup, then on a 5-minute interval — a sibling loop to
  the existing `run_refresh_scheduler` (`daemon.rs:963-1000`), not merged into it (posture
  cadence and token-refresh cadence are different concerns).
- **Handle "not logged in yet" explicitly.** At startup, the daemon may not yet have a
  valid access token (no completed login). The scheduler must not fail/panic in that
  state — defer the first collection attempt until login completes, then trigger it
  immediately (not wait for the next 5-minute tick) so a freshly-logged-in device
  reports posture promptly rather than being silently skipped for up to 5 minutes.
- Assemble the `DevicePostureReport` from all collectors in Phase 1's `posture.rs`. A
  single collector timing out, erroring, or **panicking** (caught via its own task's
  `JoinError`, per Phase 1) **must not** prevent the rest of the report from being
  submitted — populate that check as `ERROR` and continue.
- Submit `ReportDevicePostureRequest{access_token, device_id, report}`, authenticated
  with the existing refreshed access token — reuse the same auth-attach +
  401-triggered-refresh pattern as `fetch_acl_snapshot_with_refresh`
  (`daemon.rs:844-928`), not a new auth path. `device_id` comes from the daemon's own
  known device identity (the same value already used elsewhere in the daemon), not from
  the token — the token carries no device claim.
- Generate one fresh `report_id` (UUID) per **collection cycle**; on a transient submission
  failure, retry that report with the **same** `report_id` so the server-side duplicate check
  (Phase 1's `UNIQUE(report_id)`, M1) makes retries idempotent rather than creating
  duplicate rows.

## Tests

- Unit: scheduler fires at startup and on the 5-minute tick (use a mocked clock/interval, consistent with how `run_refresh_scheduler` is tested today).
- Unit: scheduler started before login completes does not fail/panic, and fires immediately on login success rather than waiting for the next tick.
- Unit: one collector forced to time out, and separately one forced to panic → report still submitted with the other checks populated and the failing one `ERROR` in both cases.
- Unit: submission failure + retry reuses the same `report_id`.

## Build Check
```bash
cd client && cargo build && cargo test
```

## Implementation Checklist
- [x] **M2-B1** `daemon.rs` — startup + 5-min scheduler loop; defers cleanly if not logged in yet and fires immediately after login; per-collector-timeout-and-panic-tolerant report assembly.
- [x] **M2-B2** Submit `ReportDevicePostureRequest{access_token, device_id, report}` over the existing refreshed-access-token path; idempotent retry via stable `report_id`.
- [x] **Build gate:** `cd client && cargo build && cargo test`

## Post-Phase Fixes
_None yet._
