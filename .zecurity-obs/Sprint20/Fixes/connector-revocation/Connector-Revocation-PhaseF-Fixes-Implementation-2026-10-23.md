---
title: Connector Revocation — Phase F fixes Implementation Record (delete guard; shield failover from a revoked connector)
date: 2026-10-23
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§10, §12)
findings: Connector-Revocation-PhaseF-Shield-Cascade-Check-2026-10-23.md (A, B)
acceptance: Connector-Revocation-PhaseF-Fixes-Acceptance-2026-10-23.md
branch: feat/sprint20-m1-phase2
commits: f1518d6 (implementation + tests), 4f4f00a (both sides pin the terminal text)
status: DONE — unit + 12/12 mutants + live AF-L PASS. Committed locally, NOT pushed.
---

# Phase F fixes

## What changed

| Area | Change |
|---|---|
| controller `graph/resolvers/connector.resolvers.go` `DeleteConnector` | **Finding A, option a.** The delete adds `AND NOT EXISTS (SELECT 1 FROM shields s WHERE s.connector_id = connectors.id AND s.status IN ('active','disconnected'))`: one statement, so there is no race. A revoked connector gains no shields, because heartbeat re-pointing and shield tokens need an `active` connector. On refusal, a count query produces `delete connector: N shield(s) still attached; let them fail over to another connector or revoke them first`. Pending shields (no certificate yet) and revoked shields (tombstoned) don't block. No schema change; `admin/` is untouched |
| connector `agent_server.rs` | **Finding B.** `ShieldRegistry::not_serving_status()`: in a terminal state (`Revoked`, `UnknownIdentity`) the refusal is `Unavailable("connector is revoked or unknown to the controller")` (`CONNECTOR_TERMINAL_MESSAGE`); otherwise it is unchanged. It is used both when a stream is refused at open and when the terminal transition closes open streams (the gate is published before `close_all_streams`) |
| connector `authorization.rs` (test only) | `AuthzGate::controllable_for_tests()` + `TestGateControl::set(state)` |
| shield `control_stream.rs` | **Finding B.** `connector_terminal(err)` matches that exact message. `reconnect_loop` then moves the refusing head to the tail **in memory** and retries after `NOT_SERVING_RETRY_SECS` (backoff reset), so the next `build_client` walks to the next peer; the failover that follows persists the new head. "Not serving yet" still waits for the same connector (B2 behaviour) |
| both | Each side pins the literal text in a test, because the message is matched by string across crates |

## Tests

- Controller full suite green with the DB, including the new `TestDeleteConnector_RefusedWhileShieldsAttached` (AF-1) and `TestDeleteConnector_AllowedOnceShieldsRevokedOrMoved` (AF-2: revoked plus tombstone, moved, pending-only, pending connector). `TestDeleteRevokedConnector_SerialsStayOnCRL` still passes.
- Connector **239 → 242**: `terminal_connector_refuses_shields_with_the_terminal_message` (AF-3: Revoked, UnknownIdentity, Starting, Disabled), `open_stream_closed_by_a_terminal_transition_carries_the_terminal_message` (AF-4: Revoked and Disabled), `terminal_message_matches_the_shield`.
- Shield **40 → 42**: `terminal_refusal_fails_over_but_not_serving_waits` (AF-5: rotate, short retry, single-connector retry, not-serving keeps the head), `terminal_message_matches_the_connector`.
- Client 265, relay 116: green.

**Mutation checks: 12/12 killed.**

| # | Mutant | Killed by |
|---|---|---|
| F1 | delete guard disabled | AF-1, AF-2 |
| F2 | revoked shields block | AF-2 |
| F3 | pending shields block | AF-2 |
| F4 | refusal doesn't name the count | AF-1 |
| F5 | terminal state not detected | AF-3, AF-4 |
| F6 | open refusal uses the old text | AF-3 |
| F7 | close path uses the old text | AF-4 |
| F8 | connector text changed | connector pin test |
| F9 | shield doesn't rotate | AF-5 |
| F10 | shield doesn't recognise the text | AF-5 |
| F11 | shield text changed | shield pin test |
| F12 | terminal uses the long backoff | AF-5 |

## Live check AF-L (2026-10-23): PASS

**Setup:**
- controller `4f4f00a` (`k2-controller` restarted from `~/.cache/zecurity-f/controller`, same env);
- network `pf-net`;
- `pf-c3` (`edf18dff-…`): HEAD connector `e174ee78`, user-level on this PC;
- `pf-c2` (`1130a22b-…`): the same HEAD build, user-level on udaya;
- `pf-shield2` (`75cfb3c4-…`): HEAD shield `82442a7e`, user-level on this PC, attached to `pf-c3`; list = [`pf-c3`, `pf-c2`].

**Part 1 — shield attached and running; the revoked connector's process keeps running:**

| Time (UTC) | Event |
|---|---|
| 07:33:14.453 | `revokeConnector(pf-c3)` |
| 07:33:14.459 | shield: `connector is revoked or unknown; trying the next peer connector` (6 ms) |
| 07:33:14.460 | `deleteConnector(pf-c3)` → **refused**: `1 shield(s) still attached; …` |
| 07:33:16.812 | shield: Control stream established on **`pf-c2`** (2.36 s after the revoke); Phase E pruned `pf-c3` |
| by 07:33:31 | shield row re-pointed to `pf-c2` (next heartbeat) |
| 07:33:38 | `deleteConnector(pf-c3)` → **true**; the shield row survives on `pf-c2`; `pf-c3`'s certificate stays revoked in `connector_certificates` (CRL) |

Before the fix, the same situation (finding B) left the shield retrying the revoked connector every ~2.4 s for as long as its process ran.

**Part 2 — shield offline:**

1. The shield was stopped.
2. 07:33:48: `revokeConnector(pf-c2)`, then `deleteConnector(pf-c2)` → **refused** (`1 shield(s) still attached`). Both rows stay.
3. `revokeShield(pf-shield2)`, then `deleteConnector(pf-c2)` → **true**. The shield row cascades away, but its tombstone (`shield_revocations`) remains, so no unrevoked orphan is left. Finding A is closed.

All test processes were stopped afterwards.

## Notes

- An **old** shield (before this fix) treats the new message as an ordinary error: backoff, same head, as before. It is still stuck, but no worse than before.
- An old connector still sends the old text, so a new shield waits as before. The fix needs both sides.
- The delete guard means an admin with a permanently offline shield on a revoked connector must revoke that shield before deleting the connector. That is the intended behaviour of option (a).

Nothing is pushed (user rule).
