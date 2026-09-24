---
type: phase
member: M1
person: Sathiya
sprint: 20
phase: 1
execution: B
title: Connector + Shield Revocation Stickiness
status: planned
depends_on: []
tags:
  - go
  - connector
  - shield
  - lifecycle
  - revocation
  - provider-dashboard
---

# Phase 1 (B) — Connector + Shield Revocation Stickiness

> Day-1 work, independent. Decision Record prerequisite #2 (Q15; discovery §15 item 1).
> **Owns** `control_stream.go` changes that M2-G later rebases onto.

## Problem (verified)

### Connector

```text
Admin revokeConnector            → status='revoked', revoked_at=NOW()    (connector.resolvers.go:113-157)
Next health report               → guarded UPDATE matches nothing → stream closed (control_stream.go:565-605) ✔
Stream-close defer               → UPDATE status='disconnected' WHERE id=$1      ✘ no guard (control_stream.go:363-381)
Connector reconnects (Control)   → check only `status == 'revoked'` → passes     ✘ (control_stream.go:~304-313)
Open UPDATE                      → status='active' WHERE id=$1                   ✘ no guard (control_stream.go:330-347)
Health reports                   → guard `status NOT IN ('revoked','deleted')` passes → stays active
RenewCert                        → checks only status == 'revoked'               ✘ (enrollment.go:~243)
Re-revoke                        → RevokeConnector requires revoked_at IS NULL → no-op
Goodbye                          → status='disconnected' WHERE id … no guard    ✘ (goodbye.go)
```

`revoked_at` survives, so the connector stays on the workspace CRL (relays reject it). But the
controller treats it as active and serves it ACL snapshots and relay lists.

### Shield

`UpdateShieldHealth` (shield/heartbeat.go:13-72) sets `status = $2` from the connector's
`ShieldStatusBatch` (always `"active"`) without checking the shield's current status. A revoked
shield returns to `active` within ~15 s. `RevokeShield` (shield.resolvers.go:72-91) only sets the
status.

## Goal

`revoked` (and, for connectors, `revoked_at IS NOT NULL`) is **terminal**. No heartbeat, stream
lifecycle event, Goodbye or renewal path can move a connector or shield out of it. Only the existing
`DeleteConnector` / `DeleteShield` remove the row.

## Files

| File | Change |
|------|--------|
| `controller/internal/connector/control_stream.go` | `Control`: revocation check + guarded activation before `Registry.add`; stream-close defer guarded; `handleConnectorHealth` guard adds `revoked_at IS NULL` |
| `controller/internal/connector/goodbye.go` | Guarded transition; notify both planes on a real transition |
| `controller/internal/connector/enrollment.go` | `RenewCert`: reject on `revoked_at`; guarded cert UPDATE |
| `controller/internal/shield/heartbeat.go` | `UpdateShieldHealth`: never update a `revoked` shield |
| `controller/internal/connector/control_stream_test.go`, `enrollment_test.go` | New DB-backed cases |
| `controller/internal/shield/heartbeat_test.go` | Revoked-shield case |

## Steps

### B1 — `Control` stream open

```sql
-- read
SELECT status, tenant_id, revoked_at FROM connectors WHERE id = $1 AND trust_domain = $2;
```

- Reject with `PermissionDenied "connector is revoked"` if `status = 'revoked'` **or** `revoked_at IS NOT NULL`.
- Move the activation UPDATE **before** `h.Registry.add(...)` and guard it:

```sql
WITH current AS (SELECT status FROM connectors WHERE id = $1),
     updated AS (
       UPDATE connectors
          SET status = 'active', last_heartbeat_at = NOW(), updated_at = NOW()
        WHERE id = $1
          AND status <> 'revoked'
          AND revoked_at IS NULL
       RETURNING id)
SELECT current.status IS DISTINCT FROM 'active' FROM current, updated;
```

- `pgx.ErrNoRows` here means revoked between the read and the write (TOCTOU with `RevokeConnector`). Return `PermissionDenied` **without** registering the client and **without** arming the disconnect defer.
- Notifications on `becameActive` are unchanged.

### B2 — stream-close defer

```sql
... UPDATE connectors SET status = 'disconnected', updated_at = NOW()
     WHERE id = $1 AND status = 'active'
    RETURNING id ...
```

Only `active → disconnected`. `revoked` / `pending` rows are never touched. Notifications stay as
they are, fired only on a real transition.

### B3 — `Goodbye`

- Same guard as B2 (`AND status = 'active'`, plus `trust_domain = $2`).
- On a real transition, call `PolicyNotifier.NotifyPolicyChange` and `TransportNotifier.NotifyTopologyChange(ws, [id])`, mirroring the defer (this needs `tenant_id` via `RETURNING tenant_id`).
- Keep the response shape. The connector binary does not call `Goodbye` today; this closes a latent path.

### B4 — `RenewCert`

- Read `revoked_at` along with `status`. Reject (`PermissionDenied "connector is revoked"`) if `status = 'revoked'` or `revoked_at IS NOT NULL`.
- Guard the cert UPDATE and check rows affected:

```sql
UPDATE connectors
   SET cert_serial = $1, cert_not_after = $2, updated_at = NOW()
 WHERE id = $3 AND tenant_id = $4
   AND status <> 'revoked' AND revoked_at IS NULL;
```

- 0 rows ⇒ revoked during renewal: return `PermissionDenied` and **do not return the signed cert**. This mirrors the client pattern in `internal/client/store.go` `updateClientDeviceCertOnRenewal`.

### B5 — `handleConnectorHealth`

Add `AND revoked_at IS NULL` to the existing guard (`status NOT IN ('revoked','deleted')`) as
defense in depth. Behaviour on no match is unchanged (close the stream).

### B6 — `UpdateShieldHealth`

In the `updated` CTE add `AND sh.status <> 'revoked'`. When the shield is revoked:
- no row is updated (`connector_id`, `lan_ip`, `last_heartbeat_at`, `status` unchanged);
- the `synced` resource re-point does not run (it is already gated on `updated`);
- the function returns `(false, false, nil)`, not an error. Check how the final `SELECT … FROM updated` behaves with zero rows (`pgx.ErrNoRows`) and map it to "no change";
- log once at debug/info that a status report for a revoked shield was ignored.

### Data repair (operational, not a migration)

Existing databases may already contain resurrected rows. The guards use `revoked_at`, so those rows
are rejected from now on regardless. Optionally repair the status column manually:

```sql
UPDATE connectors SET status = 'revoked', updated_at = NOW()
 WHERE revoked_at IS NOT NULL AND status <> 'revoked';
```

Shields have no `revoked_at`, so a shield already resurrected cannot be detected. Re-revoke it from
the admin UI after this phase ships.

## Invariants

- **I1** No UPDATE in `connector` / `shield` packages sets `status` to anything other than `revoked` on a row whose `status = 'revoked'`, or (connectors) whose `revoked_at IS NOT NULL`.
- **I2** `revoked_at IS NOT NULL` ⇒ connector is treated as revoked by `Control`, `handleConnectorHealth`, `RenewCert`, `Goodbye`.
- **I3** A renewal that loses a race with revocation returns no certificate.
- **I4** `RevokeConnector`, `RevokeShield`, `DeleteConnector`, `DeleteShield` semantics are unchanged.
- **I5** Do **not** change `workspaces.status='active'` predicates (SPIFFE validator, disconnect watchers). They belong to the suspension sprint (D-07/D-08).
- **I6** Do **not** add shield CRL entries or `revoked_at` for shields. That is undecided (Out of Scope in `path.md`).

## Tests (DB-backed; `ENROLLMENT_TEST_DATABASE_URL`, `SHIELD_TEST_DATABASE_URL`)

Connector:
- Revoke an active connector with an open stream → the next health report closes it → the defer runs → row is `status='revoked'`, `revoked_at` unchanged.
- `Control` for `status='revoked'` → `PermissionDenied`; client not registered.
- `Control` for a legacy row `status='disconnected'`, `revoked_at` set → `PermissionDenied`.
- Revoke racing stream open (revoke between read and activation) → `PermissionDenied`, status stays `revoked`.
- `Goodbye` on `revoked` → status stays `revoked`, no notifications. `Goodbye` on `active` → `disconnected`, both planes notified.
- `RenewCert` on `revoked` / `revoked_at` set → `PermissionDenied`.
- `RenewCert` racing revoke (row revoked after the status read, before the UPDATE) → `PermissionDenied`, `cert_serial` unchanged.

Shield:
- Shield `revoked`; connector sends `ShieldStatusBatch` with `status="active"` and a new `lan_ip` → shield stays `revoked`, `lan_ip` and resource hosts unchanged, function returns no error.
- Non-revoked shield → existing behaviour unchanged (regression).

## Acceptance Criteria

- [ ] End-to-end: revoke a running connector in the admin UI → within 15 s its stream closes; after ≥ 3 reconnect attempts the row is still `revoked`, and the UI shows REVOKED.
- [ ] Revoke a running shield → UI still shows REVOKED after 2 minutes of connector heartbeats.
- [ ] All tests above pass (not skipped).

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/connector/... ./internal/shield/...
```

## Implementation Checklist

- [ ] **M1-B1** `Control` open: revocation check + guarded activation before registry add
- [ ] **M1-B2** stream-close defer: `active → disconnected` only
- [ ] **M1-B3** `Goodbye`: guarded + notify both planes
- [ ] **M1-B4** `RenewCert`: `revoked_at` check + guarded UPDATE
- [ ] **M1-B5** `handleConnectorHealth`: `revoked_at IS NULL`
- [ ] **M1-B6** `UpdateShieldHealth`: skip revoked shields
- [ ] **M1-B7** Tests
- [ ] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
