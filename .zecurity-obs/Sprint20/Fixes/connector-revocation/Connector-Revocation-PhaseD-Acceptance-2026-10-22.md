---
title: Connector Revocation — Phase D acceptance list (relay enforcement)
date: 2026-10-22
type: acceptance list (design §9, §17)
status: DONE 2026-10-23 — unit + 12/12 mutants + live AD-L PASS (record: Connector-Revocation-PhaseD-Implementation-2026-10-23.md)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (D6, D20)
---

# Phase D — the relay refuses and drops revoked connectors

## Scope

1. **Proto (additive, `relay.proto`).** `HeartbeatRequest.registered_connector_spiffe_ids = 8`; `HeartbeatResponse.revoked_connector_spiffe_ids = 3` (only the subset of what the relay sent).
2. **Controller.**
   - The heartbeat answers which of the relay's registered connector identities are revoked, using the Phase C rule (certificate history; revoked, or revoked then deleted, while any certificate is valid), **across workspaces** (trust domain → workspace).
   - The answer goes on **both** responses (the throttled early return and the full path).
   - A DB error logs and returns an empty list; the relay's set is sticky, so nothing is unblocked (D20).
3. **Relay.**
   - **Sticky set:** a revoked set (canonical `spiffe://<td>/connector/<uuid>`) that is only ever added to for the life of the process. A stale or failing heartbeat keeps the last list and keeps serving (D20).
   - **Applying a heartbeat answer:** add the ids to the set first, then close the connection of every registered connector in it (which ends its bridged client streams).
   - **Register:** refused for a listed identity, with `RegisterAck{ok:false}` and close, after the authenticated binding. The check is repeated after insertion (race with a concurrent apply).
   - **Heartbeat request:** carries the canonical identities of registered connectors.

## Acceptance

| # | Check | Level |
|---|---|---|
| AD-1 | Store: of a mixed list of identities, only revoked / revoked-then-deleted-with-valid-cert ones are returned, across two workspaces; malformed ids ignored | Go DB test |
| AD-2 | Heartbeat returns the revoked subset on the throttled and the full path; a store error returns an empty list, not an RPC error | Go unit |
| AD-3 | Relay state: the set is sticky (an empty later answer doesn't unblock); applying closes exactly the listed registered connectors | Rust unit |
| AD-4 | Register of a revoked identity is refused (`ok:false`), including the post-insert re-check | Rust unit |
| AD-5 | Heartbeat loop sends the registered ids and applies the answer; on error the set is kept | Rust unit (fake transport) |
| AD-6 | Existing relay, controller, connector and client suites green | suites |
| AD-7 | Mutation checks: store predicate, both response paths, sticky set, sweep, register refusal, post-insert re-check, request ids | mutants |
| AD-L | **Live:** a revocation-ignoring connector registered with the manojinkyank relay is dropped by the relay after revocation and can't re-register, while it keeps running | live |

Nothing is pushed (user rule).
