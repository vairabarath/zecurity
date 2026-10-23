---
title: Connector Revocation — Phase D Implementation Record (relay enforcement)
date: 2026-10-23
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§9, D6, D20)
acceptance: Connector-Revocation-PhaseD-Acceptance-2026-10-22.md
branch: feat/sprint20-m1-phase2
commits: a2af01e (implementation + tests), 56e0496 + 80773f7 (tests added for the mutation run)
status: DONE — unit + 14/14 mutants + live AD-L PASS. Committed locally, NOT pushed.
---

# Phase D — the relay refuses and drops revoked connectors

## What changed

| Area | Change |
|---|---|
| `proto/relay/v1/relay.proto` | `HeartbeatRequest.registered_connector_spiffe_ids = 8`, `HeartbeatResponse.revoked_connector_spiffe_ids = 3` (additive). Go regenerated (tracked `relay.pb.go`) |
| controller `relay/store.go` `RevokedConnectorSPIFFEIDs` | Takes the relay's registered identities. `parseConnectorSPIFFEID` keeps only well-formed `spiffe://<td>/connector/<uuid>` (malformed ignored). Joins trust domain → workspace → `connector_certificates` → `connectors` with the **Phase C retention rule** (a certificate still valid, and the row revoked or deleted). Works **across workspaces**; the identity must belong to the workspace of its trust domain. Returns canonical, sorted ids |
| controller `relay/heartbeat.go` | `revokedConnectorSubset` is computed **before** the throttled early return, and the answer goes on **both** responses. A store error is logged and answers empty (the relay set is sticky, D20) |
| relay `state.rs` | Canonical ids (lower-case uuid). **Sticky** `revoked` set (only added to). `apply_revoked` extends the set first, then closes every registered connector in it; its session task then removes the registry entry, and the bridged client streams end with the connection. `is_revoked` fails closed on a poisoned lock |
| relay `session.rs` | `Register` of a listed identity is refused after the authenticated binding. A second check after the insert closes the race with a concurrent apply. `refuse_revoked` writes `RelayAck{ok:false,"connector is revoked"}`, finishes the stream and waits at most 2 s for delivery before closing; closing at once would discard the ack, which the first test run caught |
| relay `heartbeat.rs` | The request carries `registered_connector_spiffe_ids()`; the response's list is applied, and a warning is logged when it is non-empty |
| relay `test_support.rs` | `quic_pair()`: a loopback QUIC connection pair for the session and heartbeat tests |
| relay `state.rs` (test only) | `#[cfg(test)] before_insert` hook at the start of `insert_connector`, so a test can apply a revocation inside the Register check→insert window |

**Not done (design §9 "CRL path" bullet):** the periodic CRL re-check of registered connectors' serials. Established sessions are covered by the heartbeat answer instead. New connections are still refused at accept by the existing CRL check (see AD-L).

## Tests

- Relay **110 → 116**.
- Controller full `go test ./...` green with the DB env; the DB test ran (not skipped).
- Connector 237, client 265.

| Item | Test |
|---|---|
| AD-1 | `TestRevokedConnectorSPIFFEIDsIntegration`: two workspaces with active, revoked, revoked-in-the-other-workspace, all-certs-expired, revoked-then-deleted, malformed and wrong-workspace identities |
| AD-2 | `TestHeartbeat_ReturnsRevokedConnectorsOnBothPaths` (Valkey-throttled and full), `TestHeartbeat_RevokedLookupFailureAnswersEmpty`, `TestParseConnectorSPIFFEID` |
| AD-3 | `registered_connector_is_dropped_when_revoked_and_stays_blocked`: closed and removed; neither an empty answer nor an answer listing only another connector unblocks it; it can't re-register. `only_listed_connectors_are_dropped_and_ids_are_canonical` |
| AD-4 | `revoked_connector_cannot_register` (`ok:false`, reason delivered, registry empty, **insert never attempted**). `revocation_inside_the_register_window_is_caught_by_the_recheck` (the hook revokes after the pre-check; the sweep finds nothing; the re-check refuses) |
| AD-5 | `heartbeat_reports_registered_ids_and_applies_revoked_answer` (fake transport: the request carries both registered ids; the listed connector is closed and the other kept). `heartbeat_error_keeps_the_revoked_set` (D20: a failing heartbeat does not unblock) |

**Mutation checks: 14/14 killed** (one source edit per mutant, suite run, file restored):

| # | Mutant | Killed by |
|---|---|---|
| D1 | store: revoked/deleted predicate removed | integration test |
| D2 | store: certificate validity removed | integration test |
| D3 | store: identity not bound to its workspace | integration test |
| D4 | throttled response without the list | `…OnBothPaths` |
| D5 | full response without the list | `…OnBothPaths` |
| D6 | set replaced instead of extended | `…stays_blocked` |
| D7 | sweep closes nothing | three relay tests |
| D8 | both Register checks removed | `revoked_connector_cannot_register`, `…stays_blocked` |
| D9 | refusal closes without waiting for the ack | the same two |
| D10 | heartbeat ignores the answer | AD-5 test |
| D11 | request without registered ids | AD-5 test |
| D12 | ids not lower-cased | `…ids_are_canonical` |
| D13 | post-insert re-check removed (pre-check kept) | `revocation_inside_the_register_window…` |
| D14 | pre-check removed (re-check kept): a revoked identity briefly enters the registry | `revoked_connector_cannot_register` (insert attempted) |

The first run (12 mutants) showed that removing only one of the two Register checks survived. Two tests were added (`80773f7`) so each check is now tested on its own (D13, D14).

## Live check AD-L (2026-10-23): PASS

**Setup:**
- controller `56e0496` (HEAD build, same env as Phase C: `RELAY_CERT_TTL=1h`, `RELAY_DEV_ALLOW_PRIVATE_ADDR=true`);
- relay on manojinkyank (`omarchy`, 192.168.1.49:9093) upgraded binary-only to `51d35a2a…` by the user with sudo; backup `~/zecurity-relay.pre-phased`;
- **hostile stand-in connector** `pd-hostile` (`57612a17-23c8-4777-8cf1-590b2df5ddc0`) = the Phase A measurement build `aac7cbea…`, which ignores revocation, run as the normal user on udaya in `pc-hostile-net`. The token was sent through a hidden prompt and shredded.

That old build doesn't re-register after a relay restart (it predates Fix 07), so it was restarted once with its enrolled state and registered with the new relay at 04:37:29.70.

| Time (UTC) | Event |
|---|---|
| 04:37:36.55 | relay heartbeat: `registered_connectors=1` |
| 04:37:40.70 | `revokeConnector` (DB `revoked_at` 04:37:40.702) |
| 04:38:06.56 | relay: `revoked connector: Relay connection closed`, `controller reported revoked connectors revoked=1 closed=1`, then `Connector removed from Relay registry` (**25.9 s**, within one 30 s heartbeat) |
| 04:38:06.58 onward | 3 reconnect attempts, all rejected: `rejecting peer due to revocation check status=Revoked`. 0 registrations after the revocation |
| 04:38:36.57 | relay heartbeat: `registered_connectors=0` |
| 04:38:41 | connector process still running (pid 12124), logging `Relay session failed … entering backoff`; then stopped by us |

**Which layer refused the reconnects:** the existing accept-time CRL check (`listener.rs`). Revoking a connector also revokes its certificate, and the relay already had the new CRL. That layer shadowed the new Register refusal, so live the Register refusal is defence in depth; it is covered by AD-4 and D8. The new behaviour that only Phase D provides, and that the live run shows, is **dropping the already-established registration** of a connector that ignores its revocation.

**Note on the log line at 04:38:06.564:** the ack line at the sweep still says `registered_connectors=1`. The count is read right after the close, before the session task removes the entry; the next heartbeat shows 0.

Nothing is pushed (user rule).
