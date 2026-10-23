---
title: Connector Revocation — Phase F fixes acceptance list (delete guard, shield failover from a revoked connector)
date: 2026-10-23
type: acceptance list
status: AUTHORIZED 2026-10-23 — user chose "A-a and B" for the findings in Connector-Revocation-PhaseF-Shield-Cascade-Check-2026-10-23.md
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§10, §12)
---

# Phase F fixes

## Fix A (option a) — `deleteConnector` refuses while shields are attached

- The delete runs only if no **enrolled, non-revoked** shield row (`active` or `disconnected`) points to the connector:
  - `… AND NOT EXISTS (SELECT 1 FROM shields s WHERE s.connector_id = connectors.id AND s.status IN ('active','disconnected'))`.
  - `pending` shields have no certificate (enrollment sets `active` and the certificate together), so they don't block.
  - It is one statement, so there is no check-then-delete race. A revoked connector can't gain shields: heartbeat re-pointing and new shield tokens require an `active` connector.
- Revoked shields don't block the delete either: their tombstone in `shield_revocations` survives the row.
- When the delete is refused because of shields, the error says how many are attached and what to do: let them fail over to another connector (Fix B), or revoke them. The existing "not found or must be revoked" error is unchanged.
- No schema change. `admin/` is not modified; it shows the GraphQL error as for any failed delete.

## Fix B — a shield fails over away from a revoked connector

- **Connector:** in a terminal state (`Revoked`, `UnknownIdentity`) the shield Control stream is refused, and an open stream is closed, with `Unavailable("connector is revoked or unknown to the controller")`. Non-terminal states keep `"connector is not authorized to serve"` (the B2 wait-and-retry behaviour).
- **Shield:** on that message the shield moves the refusing connector (the current head) to the tail of its in-memory list and retries after the short not-serving interval, so the next attempt walks to the next peer. A single-connector list just retries. The existing failover then persists the new head.
- No proto change (a status message string, like the existing pair). An old shield treats the new message as an ordinary error (backoff, same head), as before the fix.

## Acceptance

| # | Check | Level |
|---|---|---|
| AF-1 | Delete is refused while an active/disconnected shield is attached; the error names the count; the connector and shield rows remain | Go DB test |
| AF-2 | Delete succeeds once that shield is revoked (its tombstone remains) or re-pointed to another connector; pending shields don't block; a pending connector still deletes | Go DB test |
| AF-3 | Connector: terminal state refuses a new shield stream with the terminal message; Disabled/Starting keep the not-serving message | connector unit |
| AF-4 | Connector: an open shield stream closed by the terminal transition ends with the terminal message | connector unit |
| AF-5 | Shield: the terminal message rotates the head to the tail and uses the short retry; the not-serving message keeps the head | shield unit |
| AF-6 | Existing controller, connector, shield, client and relay suites green | suites |
| AF-7 | Mutation checks: delete guard, revoked-shield exemption, terminal message (open and close), shield rotation, not-serving unchanged | mutants |
| AF-L | **Live:** a shield attached to `c1` with peer `c2`; revoking `c1` makes the shield fail over to `c2` while `c1` keeps running; then deleting `c1` succeeds (the shield moved). Before that, while a shield is still attached, delete is refused | live |

Nothing is pushed (user rule).
