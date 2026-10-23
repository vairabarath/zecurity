---
title: Connector Revocation — Phase E acceptance list (shield pruning)
date: 2026-10-23
type: acceptance list (design §10, §17)
status: DONE 2026-10-23 — unit + 11/11 mutants + live AE-L PASS (record: Connector-Revocation-PhaseE-Implementation-2026-10-23.md)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (D6, D21)
---

# Phase E — the shield prunes revoked connectors from its peer list

**Goal:** a shield never fails over to (or restarts onto) a connector that any of its connectors reports revoked.

With an honest connector, enforcement needs no shield change: the revoked connector closes its own shield streams (§7). Phase E covers the **stale peer list**. The shield keeps its connector list in `state.json` and only replaces it on a non-empty `PeerConnectorList`. A revoked connector is dropped from new lists (the controller lists only active connectors), but it stays in the shield's saved list until a list arrives, and it would be dialled on failover or restart.

## Scope

1. **Proto (additive, `shield.proto`).** `PeerConnectorList.revoked_connector_ids = 2`.
2. **Connector.**
   - Fills `revoked_connector_ids` from its ACL `revoked_connectors` and never offers a revoked id as a peer.
   - Sends the list when there are peers **or** revoked ids, so a connector in no remote network still lets the shield prune.
3. **Shield (D21).**
   - **Immediate prune:** revoked ids from **any** connector's list are added to an in-memory set and pruned from the current list at once. The pruned list is saved.
   - **Never re-added while the process runs:** later lists are filtered by the set.
   - **Memory only:** the set is never written to `state.json`. It survives a certificate renewal (the renewal rebuilds the state).
   - **All peers revoked:** the shield dials none. `state.json` keeps its last non-empty list, because the shield refuses to start from an empty one.
4. **Accepted limit (D21).** A **hostile** revoked connector never sends its own id. A shield attached only to it is not protected by Phase E. That is bounded by the connector's certificate lifetime and by the client (Phase C) and relay (Phase D) refusing it.

## Acceptance

| # | Check | Level |
|---|---|---|
| AE-1 | A revoked id prunes the current list at once and is saved, even with no peers in the list | shield unit |
| AE-2 | Never re-added: a later list offering it as a peer (and not listing it revoked) leaves it out | shield unit |
| AE-3 | The revoked set is memory only (absent from `state.json`; a reload starts empty) | shield unit |
| AE-4 | All peers revoked: nothing is dialled; `state.json` stays loadable | shield unit |
| AE-5 | A certificate renewal keeps the in-memory set | shield unit |
| AE-6 | Connector: the list carries the ACL's revoked ids, drops revoked peers, is sent for revoked ids alone, and isn't sent with neither | connector unit |
| AE-7 | Existing shield, connector, controller, client and relay suites green | suites |
| AE-8 | Mutation checks: prune, filter, memory-only, renewal carry-over, connector fill, peer filter, send condition | mutants |
| AE-L | **Live:** a shield whose saved list holds an honest connector and a revocation-ignoring one; revoking the hostile one makes the **honest** connector's next list prune it from the shield's list (and `state.json`) | live |

Nothing is pushed (user rule).
