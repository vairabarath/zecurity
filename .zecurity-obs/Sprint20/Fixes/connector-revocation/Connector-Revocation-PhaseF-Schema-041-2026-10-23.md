---
title: Connector Revocation — Phase F schema: connector tombstone and no identity reuse (migration 041)
date: 2026-10-23
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§12, O14)
branch: feat/sprint20-m1-phase2
commits: e72623b (041 + tests) [schema: reset DB], 8dd191b (tests tightened after the mutation run)
status: DONE — DB tests + 8/8 mutants + live on the lab DB. Committed locally, NOT pushed.
approval: user "ok now do the update in schema" (2026-10-23); live check option 2, "apply only 041 to the lab DB by hand" (user choice)
---

# Migration 041 — `connector_revocations`

## What it adds

Follows `shield_revocations` (038).

- **`connector_revocations`** (`connector_id` PK, `tenant_id` → workspaces ON DELETE CASCADE, `revoked_at`). There is **no foreign key to `connectors`**, so the tombstone outlives a deleted row; only deleting the workspace removes it.
- **`connectors_record_revocation`** trigger: `AFTER INSERT OR UPDATE OF status, revoked_at ON connectors`. When the row is revoked (`status = 'revoked'` or `revoked_at` set, the 039 predicate) it inserts the tombstone in the same statement, `ON CONFLICT DO NOTHING`. Every revocation path is covered, including future ones.
- **`connectors_reject_revoked_identity`** trigger: `BEFORE INSERT OR UPDATE OF id ON connectors`. A tombstoned id raises `unique_violation` ("connector identity … was revoked and cannot be reused").
- **Backfill:**
  - connectors already revoked;
  - revoked-then-deleted connectors known from `connector_certificates` (revoked certificate, no row).

No Go code changed. Phases C and D keep using the certificate history, which is still the right source for "while a certificate is valid". The tombstone is the durable identity record, and the reuse guard is new.

## Tests

`controller/graph/resolvers/connector_revocations_schema_test.go`; the full controller suite is green.

| Test | Covers |
|---|---|
| `TestConnectorRevocations_TombstoneAndNoReuse` | the revoke resolver writes the tombstone; it survives the delete resolver; re-insert and update-of-id are refused (23505); active and pending-deleted connectors get none |
| `TestConnectorRevocations_RevokedAtAloneCounts` | `revoked_at` alone tombstones (checked before `status` changes); a second change adds none |
| `TestConnectorRevocations_Backfill` | 041 applied to a database seeded under 040: a revoked row and a revoked-then-deleted connector are tombstoned; an active one and a deleted never-revoked one are not |

**Mutation checks: 8/8 killed** (S1 tombstone write, S2 reuse guard, S3 trigger column `revoked_at`, S4 `revoked_at` predicate, S5/S6/S7 backfill parts, S8 update-of-id guard). The first run had S3, S4 and S7 surviving because of test gaps; the tests were tightened (`8dd191b`) and all are now killed.

## Live (lab DB, 2026-10-23)

- **Backup first:** `~/.cache/zecurity-f/lab-db-before-041-20261023T094747Z.sql` (pg_dump, 0600).
- **Applied only 041:** `psql -1 -v ON_ERROR_STOP=1`, a single transaction. This is a deliberate one-off on the local dev DB, chosen by the user, instead of the usual recreate.
- **Backfill:** exactly the 8 expected tombstones.
  - 4 rows: `pc-hostile`, `pd-hostile`, `pe-hostile`, `pm-conn`.
  - 4 deleted: `pf-c1`, `pf-c2`, `pf-c3`, `9ce7912b`.
  - Each `revoked_at` matches its original revocation.
- **Through the running controller (`6d729a7`):**
  - enrolled `pf-c4` (`0b69e532-…`, HEAD connector, user-level on this PC);
  - 09:48:18 `revokeConnector` → tombstone written at 09:48:18, and the connector got `ConnectorRevoked` as before;
  - `deleteConnector` → row gone, tombstone and revoked certificate kept;
  - re-inserting that id → `ERROR: connector identity 0b69e532-… was revoked and cannot be reused`.
- No controller errors after the apply. `pf-c4` stopped.

**Teammates:** pull and recreate the local DB (`cd controller && docker compose down -v && docker compose up -d`), per the `[schema: reset DB]` rule.

Nothing is pushed (user rule).
