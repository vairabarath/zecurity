---
type: phase
member: M1
sprint: 14
phase: 1
title: Relay Cert-History + Revocation Data Model
status: done
depends_on: []
tags: [go, relay, pki, revocation, data-model, pending-02]
---

# Phase 1 — Relay Cert-History + Revocation Data Model

> Depends on nothing — Day 1. Fully inert (no verifier consults it yet) → zero risk to live auth.

## Goal

Express "which relay serials are revoked" in a **renewal-safe** way. Today `relays.cert_serial` holds
only the latest serial and `RecordHeartbeat`/`MarkProvisioned` overwrite it — so a single column
cannot cover a relay that has held more than one unexpired cert. Add a history table and the store
methods the later phases build on.

## Files

| File | Change |
|------|--------|
| `controller/migrations/027_relay_certificates.sql` | **new** — `relay_certificates` history table |
| `controller/internal/relay/store.go` | add `RecordIssuedCert`, `RevokeAllForRelay`, `ListRevokedRelaySerials`; extend `MarkProvisioned`; exclude revoked in `BuildLabelledRelayList` |

## controller/migrations/027_relay_certificates.sql

Next free number is **027** (latest on disk: `026_provider_audit_logs.sql`). `relays.id` is `UUID`
(`019_relays.sql`). Suggested schema:

```
relay_certificates(
  id                UUID PK default gen_random_uuid(),
  relay_id          UUID NOT NULL REFERENCES relays(id),   -- do NOT cascade-delete; history outlives the relay
  serial            TEXT NOT NULL,                          -- hex, matches relays.cert_serial convention
  issued_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  not_after         TIMESTAMPTZ NOT NULL,
  revoked_at        TIMESTAMPTZ,
  revocation_reason TEXT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
-- UNIQUE(serial); INDEX(relay_id); partial INDEX(serial) WHERE revoked_at IS NOT NULL
```

- **No `ON DELETE CASCADE`** — the CRL must keep a revoked serial until `not_after`, even if the relay
  row is later removed. (Ties to the "never hard-delete" rule in Phase 2.)

## controller/internal/relay/store.go

New methods on `*Store` (mirror the existing `Exec`→`RowsAffected`→`ErrRelayNotFound` idiom):

- `RecordIssuedCert(ctx, relayID, serial string, notAfter time.Time) error` — INSERT one row.
- `RevokeAllForRelay(ctx, relayID, reason string) (int, error)` — `UPDATE … SET revoked_at=NOW(),
  revocation_reason=$2 WHERE relay_id=$1 AND revoked_at IS NULL`; return rows affected.
- `ListRevokedRelaySerials(ctx) ([]RevokedSerial, error)` — `SELECT serial, revoked_at FROM
  relay_certificates WHERE revoked_at IS NOT NULL AND not_after > NOW()` (unexpired only — the CRL
  need not carry expired serials).

Modify:
- `MarkProvisioned` (`store.go:103`) — wrap the existing `relays` UPDATE **and** a
  `RecordIssuedCert` insert in **one transaction** (`s.pool.Begin`/`tx.Exec`/`Commit`) so the current
  serial and the history row are always consistent.
- `BuildLabelledRelayList` (`store.go:269`) — add a filter so revoked relays are not advertised
  (Phase 2 also flips relay status, but exclude on revocation state as defense-in-depth).

## Tests

- `RecordIssuedCert` inserts; duplicate serial rejected by the unique index.
- `RevokeAllForRelay` marks all unrevoked rows for a relay, idempotent (second call → 0 rows).
- `ListRevokedRelaySerials` returns only revoked **and** unexpired serials.

## Build Check
```bash
cd controller && go build ./...
```

## Implementation Checklist
- [x] **M1-A1** migration `027_relay_certificates.sql` (no cascade-delete).
- [x] **M1-A2** `RecordIssuedCert` / `RevokeAllForRelay` / `ListRevokedRelaySerials`.
- [x] **M1-A3** `MarkProvisioned` records the issued cert in the same transaction; `BuildLabelledRelayList` excludes revoked.
- [x] **Build gate:** `cd controller && go build ./...`

## Pre-Implementation Corrections (validated review — codex)
- **Serial normalization (must-fix).** Existing issuance/heartbeat storage uses
  `leaf.SerialNumber.Text(16)` (`relay/heartbeat.go:84,108`). The new store API takes an arbitrary
  `string`, so case / leading-zeros / invalid hex could cause a CRL↔checker mismatch (silent
  fail-open). Store a **single canonical validated form** at every write, normalize the checker/CRL
  lookup from `leaf.SerialNumber` with the **same helper**, and **reject** invalid serials (check the
  `ok` return of `big.Int.SetString`; never ignore it).

## Post-Phase Fixes
_None yet._
