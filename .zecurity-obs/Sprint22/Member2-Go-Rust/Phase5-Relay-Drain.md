---
type: phase
member: M2
person: Barath
sprint: 22
phase: 5
execution: D
title: Relay Drain and Resume
status: planned
depends_on: [2, 4]   # A (errors, audit helpers) and X (danger-zone card, dialog)
schema_change: true   # 043_relay_drain.sql
tags:
  - go
  - react
  - provider
  - relay
  - drain
  - provider-dashboard
---

# Phase 5 (D) — Relay Drain and Resume

> **Decision Record:** **D-32** (drain in scope; last-eligible-relay guard, audit, unchanged connector migration, 120 s connector drain timeout; amends D-21), **D-31** (relay-ops may drain/resume), **D-02** (single controller), ADR-017 (connectivity changes notify the transport plane, never the ACL).

## How it works (verified 2026-10-26)

- Connectors place onto relays from the list the controller publishes (`BuildLabelledRelayList`, `relay/store.go`): active, capacity label high/medium, routable address. The list `version` is a content hash (`relayListVersion`).
- When a connector's active relay **leaves the list**, it migrates immediately: new session to another relay, the old session kept until its streams reach zero or `relay_drain_timeout_secs` (default **120**) passes (`connector/src/relay_selector.rs:336`).
- So **drain = leave the list**. No connector, relay or proto change.

## Schema — `043_relay_drain.sql` `[schema: reset DB]`

```sql
ALTER TABLE relays ADD COLUMN drain_started_at TIMESTAMPTZ;
```

## Contract

| Route | Allowed | Body | Success | Errors |
|-------|---------|------|---------|--------|
| `POST /provider/relays/{id}/drain` | super-admin, relay-ops (`relay.drain`) | `{reason?}` (optional, ≤ 500) | **204**; audit `relay.drain`; broadcast | 404 `not_found`; 409 `invalid_state` (pending), `already_revoked`, `already_deleted`, `last_eligible_relay`; already draining → 204 no-op, not audited |
| `POST /provider/relays/{id}/resume` | super-admin, relay-ops (`relay.resume`) | — | **204**; audit `relay.resume`; broadcast | 404; 409 `already_revoked` / `already_deleted`; not draining → 204 no-op, not audited |

- **List rule:** `BuildLabelledRelayList` adds `AND drain_started_at IS NULL`.
- **Guard (one transaction):** lock the currently eligible relays (`FOR UPDATE`, same predicate as the list). If the target is eligible and no other eligible relay remains → 409 `last_eligible_relay`. A target that isn't eligible (inactive, low tier, no routable address) may be drained.
- **Broadcast:** `broadcastRelayList` once, after commit. Not the ACL (ADR-017).
- **Read API:** `RelaySummary.draining_since` (nullable, additive), also on detail.

## Console

- Relays list: **Draining** badge next to the status.
- Relay detail: **Drain** (danger-zone card, `ConfirmDangerDialog` without the typed name; reason optional) and **Resume**; both roles.
- Progress line: "N connectors still attached" (from `attached_connectors`, refreshed on Refresh), then "Drained: safe to revoke or delete" at 0.
- Overview "Needs attention": draining relays with their attached count.
- Help text states the behaviour: connectors move away at once; streams on this relay keep running until they end, for up to 120 seconds.

## Tests (write first)

**Go (DB-backed):**
- Draining an eligible relay removes it from `BuildLabelledRelayList` and changes `version`; resume restores both (same version as before the drain).
- Guard: the only eligible relay → 409; two eligible relays drained **concurrently** → exactly one succeeds; a non-eligible relay → allowed.
- State rules: pending → `invalid_state`; revoked/deleted → `already_*`; repeat drain/resume → no-op, no audit.
- Authz: relay-ops allowed for drain/resume (and still 403 for revoke/delete).
- Audit rows with actor, reason, client IP; rollback on audit failure.
- Broadcast hook called exactly once per successful state change, after commit; never on no-op or error.
- Read API returns `draining_since`.

**Console:** badge, buttons per role and state, progress and "Drained" state, Overview item, API-surface pin.

**Regression:** `cd connector && cargo test relay_selector` unchanged and passing.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-D1 … AT-D7** (incl. the live drain on the lab fleet).

## Test gate

Go gate, console gate, schema gate (`043_`) and connector regression from [[Sprint22/path]].

## Post-Phase Fixes

*(None yet.)*
