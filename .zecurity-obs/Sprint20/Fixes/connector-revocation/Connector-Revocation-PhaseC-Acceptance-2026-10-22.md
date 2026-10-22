---
title: Connector Revocation — Phase C acceptance list (client enforcement)
date: 2026-10-22
type: acceptance list (design §8, §17)
status: DONE 2026-10-22 — unit + 13/13 mutants + live AC-L PASS (record: Connector-Revocation-PhaseC-Implementation-2026-10-22.md)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (D6, D19)
---

# Phase C — the client refuses and cuts revoked connectors

**Goal:** the client enforces connector revocation itself, so a connector that ignores its revocation (hostile or old build) still can't carry client traffic.

## Scope

1. **Proto (additive).**
   - `ACLSnapshot.revoked_connectors = 11` → `RevokedConnector{connector_id = 1, connector_spiffe = 2}`.
   - Regenerate Go (tracked `client.pb.go`); the client and the connector compile it from `client.proto`.
2. **Controller compile (D19 realisation).**
   - A connector is listed while **any of its certificates is still valid** (`connector_certificates.not_after > now()`) **and** its connector row is revoked (`status='revoked'` or `revoked_at` set) **or no longer exists** (revoked-then-deleted).
   - Trust domain from `connectors`, falling back to `workspaces`.
   - This realises "until its last certificate can no longer be valid" exactly, instead of approximating it with `revoked_at + TTL`.
   - `RevokeConnector` already calls `NotifyPolicyChange`, so the ACL push reaches clients and connectors.
3. **Client: blocked-connector set.**
   - The set is the ACL's `revoked_connectors`, or **every connector** when the ACL is stale (no successful sync for **1 h**, monotonic clock, D19).
   - The staleness check runs on every 60 s tick, including failed syncs; the next successful sync lifts it.
4. **Client: no new flows.** Coordinate resolution drops blocked connectors from both the transport and ACL lists. The relay task re-reads the shared blocked set before trying each candidate, and again right after its tunnel is accepted. A flow whose connector is blocked is reset.
5. **Client: cut existing flows.** New `ResourceCmd::CloseConnectorFlows`:
   - flows record their selected connector (shared cell on `ActiveRelay`, set when the tunnel is accepted);
   - flows on a blocked connector, and flows still selecting a connector, are aborted (RST);
   - the command is idempotent;
   - it is sent whenever the blocked set changes and after every apply (`up`, transport hot-apply, resource hot-apply).

   The existing map-swap behaviour (`established_flow_survives_map_swap_that_removes_its_connector`) is unchanged.
6. **Client: exact identity on the direct path.** Each connector's `TunnelPool` uses `ExactSpiffeVerifier` with that connector's SPIFFE id instead of the trust-domain prefix verifier. An empty id fails closed (all-or-nothing build, as today).

## Acceptance

| # | Check | Level |
|---|---|---|
| AC-1 | Controller lists a revoked connector, a revoked-then-deleted one while its cert is valid, and not an expired one or an active one | Go DB test |
| AC-2 | Coordinates exclude blocked connectors from both planes; a resource with only blocked connectors fails closed | unit |
| AC-3 | `CloseConnectorFlows` resets exactly the flows on the listed connectors (plus still-selecting flows), keeps others and listeners, and acks | net_stack Lab test |
| AC-4 | A flow that picks a connector which becomes blocked during selection is reset (race re-check) | unit |
| AC-5 | Staleness: < 1 h nothing blocked; ≥ 1 h every connector blocked; a successful sync lifts it | unit, injected clock |
| AC-6 | Exact verifier: the right SPIFFE id connects, a different connector id is rejected, a renewed cert (same id) still connects | tunnel_pool tests |
| AC-7 | All existing client, controller and connector tests pass (incl. test :1456), plus the cross-language test | suites |
| AC-8 | Mutation checks: close trigger, race re-check, retention query, staleness gate, exact verifier | mutants |
| AC-L | **Live:** a connector that ignores revocation (Phase A measurement build, user-level on a lab host) carries a held client flow; revoking it makes the **client** cut the flow and refuse new ones, while the connector itself keeps serving | live |

Nothing is pushed (user rule).
