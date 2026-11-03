---
title: Sprint 20 Fix 16 — stuck resource delete (F50), connector graceful shutdown (F51), last-connector revoke and shield back-off (F46); F47 kept by design
date: 2026-10-26
type: fix (design agreed with the user, implemented, verified live)
status: FIXED and verified live (2026-10-26), local on `sprint20/fix14-shield-reconnect-sessions`, not pushed
owner: Sathiya (M1)
findings: F46, F47 (closed, kept by design), F50 (new), F51 (new) — Sprint20/path.md → Post-Sprint Fixes
---

# Fix 16 — four open findings from the certificate-audit / Fix 14 live runs

Decisions (user, 2026-10-26): F50 delete directly when the shield is revoked/gone, or when no removal ack arrives within 2 s; F51 as proposed; F46 "refuse unless alsoRevokeShields" + shield back-off; F47 **keep the design**; per-tunnel flow control left for later.

## F50 — a deleted resource stayed in 'deleting' forever

**Problem.** `deleteResource` on a protected resource tombstones it ('deleting') and waits for the shield's removal ack. With the shield revoked or gone no ack ever comes ("resource is mid-operation; wait for it to finish" forever; live: `manoj-echo`, `manoj-echo-2`, `nika-echo`).

**Timing (checked in code).** The removal ack is **not** batched: the shield sends it as soon as the rule is removed and the connector forwards each ack at once (`connector/src/control_stream.rs` `ack_rx.recv()` branch); only re-sends ride the 15 s health tick. Shield *status* (online/offline) **is** batched (15 s tick, 120 s disconnect threshold), so the controller may think a shield is online for up to ~2 min after it went away.

**Fix.**
- `deleteResource`: if the bound shield is revoked or gone, delete the row at once (`resource.DeleteIfShieldGone`), also for a tombstone already stuck — the delete twin of MarkUnprotecting's Finding 7.
- `resource.RunTombstoneSweeper` (every 1 s, `cmd/server/main.go`): removes any 'deleting' row older than **2 s** (`ReapStaleTombstones`), then re-pushes that shield's desired-state snapshot (a connected shield drops the rule now, an offline one on reconnect) and notifies the policy. Survives restarts (it is a query, not an in-memory timer).
- Commits `587359fd`, `46f9090b` (log text).

## F51 — the connector had no graceful shutdown

**Problem.** No SIGTERM/SIGINT handling in `connector/src`: `systemctl stop` and Ctrl-C ended the process at once; clients noticed at their 30 s QUIC idle timeout; the controller's `Goodbye` ("called by the connector on clean shutdown") was never called. (The goodbye the user remembered is the **shield's**, `shield/src/main.rs`.)

**Fix (`082b6805`).** On SIGTERM or SIGINT: close the QUIC device-tunnel endpoint with CONNECTION_CLOSE ("connector shutting down") and refuse new connections; close every shield Control stream as not serving; send `Goodbye` to the controller (2 s limit); wait (bounded) for the close frames; exit.

**Follow-up found live (`b3d38513`).** Under `connector 2>&1 | tee …`, Ctrl-C also kills `tee`; the first shutdown log line then hit EPIPE and tracing-subscriber 0.3.23 (`log_internal_errors` on by default) reported it with `eprintln!`, which panics on the same broken pipe — the process died before its shutdown. Connector and shield now use `log_internal_errors(false)`. (Under systemd/journald this never happened.)

## F46 — a shield stranded by its last connector's revocation

**Problem.** A shield learns connectors only from connectors. With other connectors in its remote network it already fails over (revoked ids pruned, terminal refusal rotates to the next peer). With **none** left it redialled the revoked connector every ~2.4 s forever.

**Fix.**
- Controller (`422be9a8`): `revokeConnector(id, alsoRevokeShields: Boolean = false)` (additive). If no other usable connector (active/disconnected, not revoked) remains in the remote network and enrolled (active/disconnected) shields are attached, the revoke is **refused** naming them; with `alsoRevokeShields: true` the connector and then each stranded shield are revoked (normal shield revocation: tombstone + propagation). With a peer left nothing changes. Not exposed in the admin UI (`admin/` is frozen this sprint): API only.
- Shield (`69518419`): once every known connector refused as revoked/unknown in a row, the retry interval doubles from 2 s up to 300 s, logged once ("re-enroll this shield or add a connector").

## F47 — kept by design

A suspended admin's access token keeps working until it expires (≤ 15 min). Recorded design (PENDING-04 Phase 5): revocation is enforced at refresh, no per-request DB read. **User: keep the design.** Closed.

## Tests

- Controller (real Postgres): `TestDeleteIfShieldGone`, `TestReapStaleTombstones`, `TestStrandedShields`, `TestRevokeConnector_LastConnectorWithShieldsIsRefused`, `_AlsoRevokeShieldsRevokesBoth`, `_WithAPeerTheShieldIsNotStranded`. Existing tests updated for the new argument; AF-1 (`connector_delete_shields_test`) adds a peer so its setup revoke stays allowed. 26 packages pass with every DB-backed test running.
- Connector: `quic_listener_closes_client_connections_on_shutdown` (262 pass). Shield: `a_shield_whose_connectors_are_all_revoked_backs_off`, `a_terminal_backoff_resets_after_a_successful_stream` (53 pass).
- Mutation checks: shield-status check removed (F50 delete), age check removed (sweeper), "no other connector" check removed (stranded), endpoint close removed (F51), no doubling (F46) — each makes its test fail.

## Live (2026-10-26, controller/connector/shield built from this branch)

| Check | Result |
|---|---|
| F50: 3 resources stuck in 'deleting' (shields gone) | removed by the sweeper 1 s after controller start |
| F50: delete a resource whose shield can no longer ack (expired, still "disconnected") | deleted 11:09:10.18 → removed 11:09:12.54 by the sweeper |
| F50: delete a resource on a live shield | removed in 82 ms through the shield's ack (sweeper not involved) |
| F51: SIGTERM (`kill -TERM`, as systemd) mid-stream | shutdown within 0.7 s of the signal; client session ended the same second; controller logged `connector goodbye`; shutdown complete in 83 ms |
| F51: SIGINT (`kill -INT`) | shutting down → Goodbye → complete in 21 ms |
| F51: Ctrl-C under `| tee` | before `b3d38513`: process died, no Goodbye, client stalled; after: session ended the same second, Goodbye logged |
| F46: revoke the only connector with 2 enrolled shields, no flag | refused, naming both shields; nothing revoked |
| F46: same with `alsoRevokeShields: true` | connector and both shields revoked, tombstones written |
| F46: shield after its only connector was revoked | logged once "every known connector is revoked …; backing off"; retries ~5, 8, 17, 32 s apart (2/4/8/16 s + connect) instead of every 2.4 s |
