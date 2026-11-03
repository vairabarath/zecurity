---
type: phase
member: M1
sprint: 15
phase: 1
title: Migration + Posture Store
status: done
depends_on: []
tags: [go, posture, data-model, pending-08]
---

# Phase 1 — Migration + Posture Store

> Depends on nothing — Day 1. Inert (nothing reads it yet) → zero risk to live auth.

## Goal

Persist raw posture observations (never a collapsed bool) so re-evaluation is possible
when profiles, requirements, or bindings change without a new client report.

## Files

| File | Change |
|------|--------|
| `controller/migrations/030_device_posture.sql` | **new** — reports, observations, profiles, requirements, resource bindings, latest evaluations |
| `controller/internal/posture/store.go` | **new** — insert/read methods |

## controller/migrations/030_device_posture.sql

The planned number was **029**, but `029_connector_revocation.sql` landed first. The
posture migration therefore uses the next free number, **030**. Suggested schema:

**Device records live in `client_devices` (`controller/migrations/011_client.sql`), not
a `devices` table — no such table exists in this codebase.** Every table below carries
a **real foreign key** `workspace_id UUID NOT NULL REFERENCES workspaces(id)` — a
`workspaces` table exists (`001_schema.sql`), and every existing tenant-scoped table
(`client_devices`, `groups_acl`, `workspace_members`, `connector_logs`) already declares
this exact FK, typically with `ON DELETE CASCADE`. A bare `workspace_id UUID NOT NULL`
with no FK would be a deviation from established convention, not a stylistic choice.

**`client_devices` is never hard-deleted — a plain FK without `ON DELETE` blocks the
delete, it does not "delete while preserving history."** Postgres's default FK action
is `RESTRICT`/`NO ACTION`: if `device_posture_reports.device_id` references
`client_devices(id)` with no `ON DELETE` clause, attempting to delete a device with
report history simply **errors**. Since `client_devices` already has a `revoked_at`
soft-delete column (consistent with how relays are handled — never hard-deleted, see
Sprint 14), device removal in this system means setting `revoked_at`, not a `DELETE`.
Posture tables therefore need no special `ON DELETE` handling for this to work; they
just must not assume a device row can vanish while reports referencing it remain.

```
device_posture_reports(
  id             UUID PK default gen_random_uuid(),
  report_id      TEXT NOT NULL,                     -- client-generated idempotency key
  device_id      UUID NOT NULL REFERENCES client_devices(id),
  workspace_id   UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  client_version TEXT NOT NULL,
  os_info        JSONB NOT NULL,
  reported_at    TIMESTAMPTZ NOT NULL,               -- client clock
  received_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
-- UNIQUE(report_id); INDEX(device_id, received_at DESC); INDEX(workspace_id)

device_posture_observations(
  id         UUID PK default gen_random_uuid(),
  report_id  UUID NOT NULL REFERENCES device_posture_reports(id) ON DELETE CASCADE,
  check_id   TEXT NOT NULL,                       -- registered check ID, not free-form
  status     TEXT NOT NULL CHECK (status IN ('PASS','FAIL','UNSUPPORTED','UNKNOWN','ERROR')),
  detail     TEXT,                                -- short human string, never raw command output
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
-- UNIQUE(report_id, check_id) -- one report cannot carry conflicting duplicate observations
-- INDEX(report_id)

device_profiles(
  id           UUID PK default gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  mode         TEXT NOT NULL DEFAULT 'audit' CHECK (mode IN ('audit','enforce')),
  revision     BIGINT NOT NULL DEFAULT 1,             -- incremented whenever requirements change
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
)
-- UNIQUE(workspace_id, name)

device_profile_requirements(
  id               UUID PK default gen_random_uuid(),
  profile_id       UUID NOT NULL REFERENCES device_profiles(id) ON DELETE CASCADE,
  check_id         TEXT NOT NULL,
  allow_unsupported BOOLEAN NOT NULL DEFAULT FALSE  -- explicit: an UNSUPPORTED result for this
                                                      -- check is tolerated (replaces the ambiguous
                                                      -- "required" flag — see Post-Phase Fixes)
)
-- UNIQUE(profile_id, check_id)

resource_profile_bindings(
  id           UUID PK default gen_random_uuid(),
  resource_id  UUID NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
  profile_id   UUID NOT NULL REFERENCES device_profiles(id) ON DELETE CASCADE,
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE  -- denormalized for
                                                       -- tenant-scoped queries; application code
                                                       -- must still verify resource_id and
                                                       -- profile_id both belong to this same
                                                       -- workspace_id at write time (the FK alone
                                                       -- doesn't cross-check that agreement)
)
-- UNIQUE(resource_id, profile_id)

device_profile_evaluations(
  device_id    UUID NOT NULL REFERENCES client_devices(id),
  profile_id   UUID NOT NULL REFERENCES device_profiles(id) ON DELETE CASCADE,
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  satisfied    BOOLEAN NOT NULL,
  profile_revision BIGINT NOT NULL,                   -- revision used to compute this result
  reason       TEXT,                                 -- e.g. "check disk_encryption: FAIL"
  evaluated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  report_id    UUID REFERENCES device_posture_reports(id) ON DELETE SET NULL,  -- nullable:
                                                       -- a plain FK with no ON DELETE would
                                                       -- RESTRICT the retention job from ever
                                                       -- deleting a report still cited as the
                                                       -- "latest evaluation's source" — SET NULL
                                                       -- lets retention proceed; treat a NULL
                                                       -- report_id as stale/unsatisfied at read
                                                       -- time (no source left to check freshness)
  PRIMARY KEY (device_id, profile_id)
)
```

- `device_posture_observations` is never collapsed into a single "healthy" flag on the
  report row — `device_profile_evaluations` is a derived, re-computable cache, not the
  source of truth. Its `satisfied` bool also does **not** account for staleness by
  itself — see Phase 2's freshness-at-query-time design; do not treat this column as
  authoritative without checking `evaluated_at`/the source report's `received_at`.
- `device_posture_reports.device_id` has no `ON DELETE` clause against `client_devices`
  (defaults to `RESTRICT`) — but this is moot in practice because `client_devices` rows
  are never hard-deleted (soft-delete via `revoked_at`, same convention as relays), so
  report history is preserved by simply never running the `DELETE` that `RESTRICT`
  would block, not by a cascade or nullable FK.
- `allow_unsupported` replaces an earlier `required: bool` design that was ambiguous
  about what `false` meant (optional check entirely, or "tolerate UNSUPPORTED but still
  require PASS/FAIL to resolve"). `allow_unsupported=true` means only an `UNSUPPORTED`
  status is tolerated as satisfying; `FAIL`/`UNKNOWN`/`ERROR` still fail the requirement
  either way.
- **Known v1 gap (tracked, not built this sprint):** `device_profile_requirements` has
  no way to express a comparison (e.g. "OS version ≥ X") — `linux.os.version`'s observed
  value can only be reported for visibility, not compared against an expected minimum.
  A future migration would add an `operator`/`expected_value` column pair if this is needed.
  Do not describe OS-version posture as "enforced" in any rollout communication this sprint.
- `device_profiles.revision` protects authorization from stale derived evaluations.
  `AddRequirement` and `RemoveRequirement` increment it in the same transaction as the
  requirement mutation. `device_profile_evaluations.profile_revision` records the
  revision used to compute that row. The ACL compiler treats a missing or mismatched
  revision as unsatisfied (fail closed) until re-evaluation catches up.

## Retention (schema only — the worker itself is Phase 4, not this phase)

One report every 5 minutes is 288/device/day — unbounded growth with no cleanup today.
**This phase's only job is to make the schema retention-friendly**; the actual worker
(cadence, batch size, lifecycle, tests) is fully specified in
[[Sprint15/Member1-Go/Phase4-Retention-Worker]] — do not re-design or re-implement it
here, and do not add a checklist item for it in this phase.

- `INDEX(received_at)` on `device_posture_reports` (already in the schema above) is what
  makes Phase 4's age-based batch deletion cheap.
- **The schema-level enabler for retention is `device_profile_evaluations.report_id
  ON DELETE SET NULL`** (already in the schema above) — without it, a report still
  cited as the latest evaluation's source would `RESTRICT` Phase 4's delete
  indefinitely. This phase's job is just to have that nullable FK in place; Phase 4
  owns proving the delete actually works against it.
- `device_profile_evaluations` itself (the latest-evaluation cache) is never deleted by
  retention — it's derived, single-row-per-`(device,profile)` state kept for the life of
  the device/profile, overwritten on every re-evaluation regardless of report age.

## Workspace-scoping clarification

Not every table above carries its own `workspace_id` FK. `device_posture_reports`,
`device_profiles`, `resource_profile_bindings`, and `device_profile_evaluations` do —
these are the tables queried/filtered directly by workspace. `device_posture_observations`
and `device_profile_requirements` intentionally do **not** — they inherit tenant scoping
transitively through `report_id → device_posture_reports.workspace_id` and
`profile_id → device_profiles.workspace_id` respectively. This is a deliberate design,
not an inconsistency — don't add redundant `workspace_id` columns to those two tables,
and don't describe "every table" as carrying the FK in documentation going forward.

## controller/internal/posture/store.go

New package, mirrors the existing `Exec`→`RowsAffected`→`ErrNotFound` idiom used in
`internal/relay/store.go`:

- `InsertReport(ctx, workspaceID, deviceID, report) error` — report + observations in **one transaction**; `UNIQUE(report_id)` on reports and `UNIQUE(report_id, check_id)` on observations give idempotent duplicate rejection at the DB layer.
- `LatestReport(ctx, workspaceID, deviceID) (*Report, error)`.
- `ListObservations(ctx, reportID) ([]Observation, error)`.
- Profile CRUD (all workspace-scoped): `CreateProfile`, `UpdateProfileMode`, `DeleteProfile`, `AddRequirement`, `RemoveRequirement`, `BindResource`, `UnbindResource`. Every method takes `workspaceID` and filters/validates against it — `BindResource`/`UnbindResource` must reject a cross-workspace resource/profile pair. `AddRequirement`/`RemoveRequirement` lock the profile row and increment `device_profiles.revision` in the same transaction. **`RemoveRequirement` must check transactionally whether the target profile is currently `enforce` mode and this is its last requirement, and reject the removal in that case** (error, not a silent auto-downgrade to audit).
- `UpsertEvaluation(ctx, workspaceID, deviceID, profileID, profileRevision, satisfied, reason, reportID) error`.
- **`EvaluationsForDevices(ctx, workspaceID, deviceIDs []uuid.UUID) (map[uuid.UUID][]Evaluation, error)`** — batch method, one query for N devices. Used by the ACL compiler hook (Phase 3), which loops over resources/devices in-memory after one bulk fetch — a per-device `EvaluationsForDevice` call inside that loop would be an N+1 query bug; this batch form is the only one the compiler should call.

## Tests

- `InsertReport` with a duplicate `report_id` returns a distinguishable "duplicate" error (unique-violation mapped, not a generic 500).
- Profile CRUD round-trips; `BindResource` rejects a cross-workspace resource/profile pair.
- `UpsertEvaluation` is idempotent per `(device_id, profile_id)` (upsert, not insert-only).
- `RemoveRequirement` on an enforce-mode profile's last requirement is rejected; on a non-last requirement, or on an audit-mode profile, it succeeds.
- Requirement mutation increments the profile revision atomically; an evaluation written
  for the preceding revision is rejected by the compiler until recomputed.
- (The retention-deletability test — deleting a report referenced by a `device_profile_evaluations` row succeeds and nulls that reference — belongs to Phase 4, not this phase; this phase only needs the schema to support it.)

## Build Check
```bash
cd controller && go build ./...
```

## Implementation Checklist
- [x] **M1-C1** migration `030_device_posture.sql` — real workspace FKs, report/observation uniqueness, retention index, nullable evaluation report FK, plus `device_profiles.revision` and `device_profile_evaluations.profile_revision`.
- [x] **M1-C2** `internal/posture/store.go` — report/observation insert, workspace-scoped profile CRUD, atomic requirement+revision mutations (including the last-requirement guard), revision-bearing evaluation upsert/read, batch `EvaluationsForDevices`.
- [x] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

### Fix: Posture migration number collision

**Issue:** The original phase plan assigned `029_device_posture.sql`, but
`029_connector_revocation.sql` reached the branch before posture implementation.

**Root Cause:** The phase plan was written while migration 028 was the latest file.

**Fix Applied:** The posture schema is implemented as
`controller/migrations/030_device_posture.sql`, preserving deterministic migration order
without renaming the already-landed connector revocation migration.

### Fix: Empty profile could be switched to enforce mode

**Issue:** `UpdateProfileMode` allowed an audit profile with zero requirements to be
switched to `enforce`, making its empty AND-set vacuously satisfied.

**Root Cause:** The last-requirement removal path was transactionally guarded, but the
mode-change path updated the profile directly without locking and counting requirements.

**Fix Applied:** `controller/internal/posture/store.go` now locks the workspace-scoped
profile row, counts its requirements, rejects an empty enforce transition with
`ErrEmptyEnforceProfile`, and commits the mode update in the same transaction.

### Fix: Binding guard and profile-name normalization

**Issue:** A manually corrupted empty enforce profile could be bound to a resource, and
blank/whitespace-only profile names were accepted.

**Fix Applied:** `CreateResourceBinding` now locks the profile and transactionally
rejects an empty enforce profile before inserting a binding. `CreateProfile` trims the
name and returns `ErrInvalidProfileName` before database access when it is blank.
