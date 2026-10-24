---
title: Connector Revocation — Phase E Implementation Record (shield pruning)
date: 2026-10-23
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§10, D6, D21)
acceptance: Connector-Revocation-PhaseE-Acceptance-2026-10-23.md
branch: feat/sprint20-m1-phase2
commits: c9d9cda (implementation + tests)
status: DONE — unit + 11/11 mutants + live AE-L PASS. Pushed 2026-10-23 on `feat/sprint20-m1-phase2`.
---

# Phase E — the shield prunes revoked connectors from its peer list

## What changed

| Area | Change |
|---|---|
| `proto/shield/v1/shield.proto` | `PeerConnectorList.revoked_connector_ids = 2` (additive). The Go stub is regenerated but untracked, as before |
| connector `policy/mod.rs` | `PolicyCache::revoked_connector_ids()`: the ACL's `revoked_connectors` ids, sorted and de-duplicated |
| connector `agent_server.rs` `build_peer_connector_list` | Fills `revoked_connector_ids`, filters revoked ids out of `peers`, and returns `None` only when there are neither peers nor revoked ids. A list with no peers is therefore still sent, so the shield can prune |
| shield `types.rs` | `ShieldState.revoked_connectors: HashSet<String>`, `#[serde(skip)]`: **memory only** |
| shield `control_stream.rs` | `prune_revoked_connectors` runs first on every `PeerConnectorList`: it extends the set, prunes the current list, and saves unless the list became empty. Incoming `peers` are then filtered by the set, so a revoked connector is **never re-added** while the process runs. That is stricter than D21's "while listed", and equivalent to it because revocation is terminal. `handle_reenroll` carries the set over a certificate renewal, because `renew_cert` builds a fresh state |
| shield `renewal.rs`, `enrollment.rs` | A new state starts with an empty set |

**All peers revoked:** the in-memory list becomes empty and `build_client` fails with "Shield has no peer connectors to dial", so the shield dials nothing. `state.json` keeps its last non-empty list, because `ShieldState::load` refuses an empty one and the shield would not start. After a restart the set is empty (memory only, D21) and that list would be tried again. An honest revoked connector refuses the shield (§7); a hostile one is the accepted D21 limit.

## Tests

- Shield **35 → 40**, connector **237 → 239**.
- Controller (`internal/shield`, `internal/policy`, full build), client 265 and relay 116: all green.

| Item | Test |
|---|---|
| AE-1 | `revoked_id_prunes_immediately_even_without_peers` (list and `state.json`) |
| AE-2 | `revoked_connector_is_never_re_added` |
| AE-3 | `revoked_set_is_memory_only` (not in `state.json`; a reload starts empty) |
| AE-4 | `all_peers_revoked_dials_none_and_keeps_the_file_loadable` |
| AE-5 | `renewal_keeps_the_revoked_set` |
| AE-6 | `peer_list_carries_revoked_ids_and_drops_revoked_peers`, `peer_list_is_sent_for_revoked_ids_alone` |

**Mutation checks: 11/11 killed.**

| # | Mutant | Killed by |
|---|---|---|
| E1 | prune keeps revoked entries | AE-1, AE-4 |
| E2 | pruned list not saved | AE-1 |
| E3 | later peers not filtered | AE-2 |
| E4 | set never extended | AE-1, AE-2, AE-4 |
| E5 | `#[serde(skip)]` removed | AE-3 and four more |
| E6 | renewal drops the set | AE-5 |
| E7 | empty list saved | AE-4 |
| E8 | connector sends no revoked ids | AE-6 |
| E9 | connector offers revoked peers | AE-6 |
| E10 | not sent for revoked ids alone | AE-6 |
| E11 | policy cache returns nothing | AE-6 |

## Live check AE-L (2026-10-23): PASS

**Setup** (a separate topology, so other sessions' lab state isn't touched):
- controller `1005c12`;
- new network `pe-net` (`c57b54d2-…`) with resource `pe-res` (192.168.1.164:18099/tcp, nothing listening) in a new **empty** group `pe-group` (`989a8b40-…`), so no user's client captures it; the resource only puts `pe-net` into the ACL;
- **honest connector** `pe-honest` (`5efef87a-…`): HEAD build `d3a26e8f…`, run as the normal user on this PC (`~/.cache/zecurity-e`);
- **shield** `pe-shield` (`233e7886-…`): HEAD build `5ba9d7d4…`, run as the normal user on this PC, enrolled while `pe-honest` was the only connector, so it attached to `pe-honest`;
- **hostile connector** `pe-hostile` (`2126d75f-…`): the Phase A build `aac7cbea…`, which ignores revocation, run as the normal user on udaya.

All tokens went through 0600 files (or the hidden prompt for udaya) and were shredded.

**Before the resource existed:** `pe-net` wasn't in the ACL, so the shield got lists with revoked ids and no peers and logged `ignored empty PeerConnectorList`. That is the "sent for revoked ids alone" path working end to end.

| Time (UTC) | Event |
|---|---|
| 04:52:53.36 | shield `peer connector list refreshed peers=2`; `state.json` = [`pe-honest`@.164, `pe-hostile`@.41] |
| 04:52:59.466 | `revokeConnector(pe-hostile)` |
| 04:52:59.474 | `pe-honest`: ACL snapshot v9 stored (8 ms) |
| 04:53:08.364 | shield: `revoked connectors pruned from the peer list pruned=1 remaining=1`, then `PeerConnectorList unchanged, no update peers=1` (**8.9 s**, at the next 15 s health exchange) |
| after | `state.json` = [`pe-honest`]; no revoked data in the file; `pe-hostile` still running on udaya (stopped by us afterwards) |

**Stated honestly:**
- In this topology the existing full-list replacement would also have removed `pe-hostile` (the honest connector's new list holds only active peers). The log shows the Phase E prune running first.
- The cases that only Phase E covers are unit-tested and mutation-checked:
  - pruning from a list with no peers (also seen live before the resource existed);
  - never re-adding from a stale list sent by another connector;
  - keeping the set across a renewal.
- The D21 limit stands: a shield attached only to a hostile revoked connector gets no list from it. That is bounded by the connector's certificate lifetime and by the client (Phase C) and relay (Phase D) refusing that connector.

**Lab state left behind:**
- `pe-net`, `pe-res`, `pe-group`, `pe-honest` (active row, process stopped), `pe-shield` (process stopped), and `pe-hostile` (revoked, process stopped) remain in the DB.
- Local files are in `~/.cache/zecurity-e`.

## Post-Phase Fixes

### Fix: the shutdown goodbye used the startup peer list (F39, Sprint 20 Fix 13, `0289eba`)
**Issue:** after a prune or failover, the SIGTERM goodbye dialled the startup head (possibly the revoked connector).

**Root Cause:** `main.rs` called `goodbye(&state, …)` with the state loaded at startup; the control stream works on its own copy.

**Fix Applied (`shield/src/control_stream.rs`, `shield/src/main.rs`):**
```rust
// BEFORE:
control_stream::goodbye(&state, &cfg).await;

// AFTER:
control_stream::goodbye(&control_stream::with_live_peers(&state, &live_peers), &cfg).await;
```
`PeerPublisher` publishes the list after a terminal rotation, a connect-time failover and each `PeerConnectorList`; an empty list sends no goodbye.

### Fix: `ignored empty PeerConnectorList` warning noise (F41, Sprint 20 Fix 13, `f1412b8`)
**Issue:** a list holding only revoked ids (sent every heartbeat on a network without resources) warned every 15 s.

**Fix Applied (`apply_peer_connector_list`):** note `carried_revoked` before the prune; empty peers → `debug` if it carried revoked ids, `warn` otherwise.

Details: [[Sprint20/Fixes/Fix13-Connector-Revocation-Low-Findings]].

Nothing is pushed (user rule).
