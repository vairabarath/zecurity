---
type: phase
member: M02
sprint: 19
phase: 9
title: Full PENDING-16 Testing
status: complete
depends_on: [1, 2, 3, 4, 5, 6, 7, 8]
tags: [tests, postgres, graphql, acl, posture, frontend, pending-16]
---

# Phase 9 — Full PENDING-16 Testing

## Goal

Convert every PENDING-16 requirement and Sprint 19 invariant into tests.

## Database integration tests

- [x] One Resource cannot have two Resource Policies.
- [x] A Resource Policy can have zero profiles.
- [x] A Resource Policy can have multiple profiles.
- [x] Duplicate Policy/Profile binding is rejected.
- [x] Cross-workspace relationships are rejected.
- [~] **Existing legacy bindings survive the migration.** — **N/A, no migration exists.** Superseded by *non-interference*, which is proven: see §Migration boxes.
- [~] **Migration preserves effective access relationships.** — **N/A, property undefined.** Phase 5 deliberately changed it; the one accepted change is asserted: see §Migration boxes.

## Controller/store tests

- [x] Create/read/update/delete policy.
- [x] Assign/unassign policy.
- [x] Add/remove profile.
- [x] Workspace isolation.
- [x] Duplicate/invalid operations.
- [x] Policy change triggers notification.

## GraphQL tests

- [x] Queries return correct workspace-scoped data.
- [x] Mutations enforce admin authorization.
- [x] Invalid second policy assignment fails.
- [x] Empty profile list is accepted.
- [x] Profile binding changes propagate.

## ACL compiler tests

- [x] Zero profiles = Any Device.
- [x] One passing profile = allow.
- [x] One failing profile = deny.
- [x] Multiple profiles = OR.
- [x] All selected profiles fail = deny.
- [x] Profile revision mismatch/stale posture follows existing semantics.
- [x] Final ACL still contains the expected allowed SPIFFE IDs.
- [x] Device Profile IDs do not need to be added to the Connector ACL protocol.

## Propagation tests

- [x] Cache invalidation occurs after policy mutation.
- [x] Epoch blocks stale in-flight compilation.
- [x] New ACL is pushed to live Connectors.
- [x] Connector catches up after missed push.
- [x] Revoked/removed authorization is reflected after convergence.

## Linux end-to-end tests

- [x] Real Linux posture report.
- [x] Real Linux posture evaluation.
- [x] Passing Linux device authorized.
- [x] Failing Linux device denied.
- [x] Resource Policy binding changes alter live authorization.

## Frontend tests

- [x] Resource Policy CRUD.
- [x] Resource assignment constraint.
- [x] Profile selection.
- [x] Empty selection = Any Device.
- [x] Multiple selection = OR.
- [x] No Audit/Enforce control.
- [x] Linux Device Profile creation/editing.

## Required gates

```bash
cd controller && go build ./...
cd controller && go vet ./...
cd controller && go test ./...
```

Run the repository's frontend test/build commands as defined by its existing package scripts.

All mandatory tests must pass before Sprint 19 is marked complete.


---

# Implementation — evidence run, 2026-09-24

## Evidence standard

No box here is ticked because an implementation or a test file exists. Each is
recorded at one of four levels, and only **X** or **M** earns a tick:

| | Level | Meaning |
|---|---|---|
| **I** | implemented | production code exists |
| **T** | test exists | a test asserts the property |
| **X** | executed | that test was **run and passed in this phase**, output captured |
| **M** | manual | proven against real hardware, observation captured |

That distinction is the reason this phase exists. Going in, **41 of 43 boxes were
already at T** — and almost none had ever reached X.

## What was actually wrong

Not missing tests. **Missing execution.**

Every database-backed test in this feature is gated on `PKI_TEST_DATABASE_URL`
and `t.Skip`s when it is unset. `cd controller && go test ./...` — the gate this
phase specifies — reported `ok` while skipping the entire Resource Policy store
suite, the 713-line resolver suite, all four propagation tests and every
integration-level ACL gating assertion. Verified directly:

```
=== RUN   TestResourcePolicyStoreIntegration
    resource_policy_store_integration_test.go:23: PKI_TEST_DATABASE_URL not set
--- SKIP: TestResourcePolicyStoreIntegration (0.00s)
ok  	github.com/yourorg/ztna/controller/internal/posture	0.003s
```

A green gate proved close to nothing. **38 test files** repo-wide are gated this
way; **7 of them carry Phase 9 boxes**. The other 31 are SCIM, IdP, PKI, relay,
outbox, identity, auth, resource, shield, transport, connector and permission —
outside PENDING-16 and out of scope here.

Setting `PKI_TEST_DATABASE_URL` alone unlocks all seven. These were, as far as the
evidence shows, **executed for the first time in this phase**.

## Results — every Phase 9 test executed

```bash
export PKI_TEST_DATABASE_URL='postgres://ztna:…@localhost:5440/postgres?sslmode=disable'
cd controller && go test ./internal/posture/ ./internal/policy/ \
                         ./graph/resolvers/ ./internal/client/ ./internal/connector/
```

**Store + database (boxes 1–5, 6–10)** — `internal/posture`, all 7 subtests:

```
--- PASS: TestResourcePolicyStoreIntegration/CRUD (0.01s)
--- PASS: TestResourcePolicyStoreIntegration/ProfileAttachments (0.02s)
--- PASS: TestResourcePolicyStoreIntegration/ResourceAssignment (0.04s)
--- PASS: TestResourcePolicyStoreIntegration/ConcurrentAssignment (0.03s)
--- PASS: TestResourcePolicyStoreIntegration/DatabaseRejectsCrossWorkspaceRows (0.04s)
--- PASS: TestResourcePolicyStoreIntegration/ListPolicyProfilesForWorkspace (0.07s)
--- PASS: TestResourcePolicyStoreIntegration/LegacyBindingsPreserved (0.02s)
```

`DatabaseRejectsCrossWorkspaceRows` is the one worth naming: it bypasses the store
entirely and asserts the composite foreign keys refuse a direct `UPDATE
resources` / `INSERT resource_policy_profile_bindings`, so tenant safety is proven
in the schema and not only in application code.

**ACL compiler (boxes 17–21, 23)** — `internal/policy`:

```
--- PASS: TestCompileACLSnapshot_ResourcePolicyGating/policy_with_zero_profiles_is_Any_Device
--- PASS: TestCompileACLSnapshot_ResourcePolicyGating/one_profile_satisfied_allows_the_device
--- PASS: TestCompileACLSnapshot_ResourcePolicyGating/one_profile_unsatisfied_denies_the_device
--- PASS: TestCompileACLSnapshot_ResourcePolicyGating/two_profiles,_second_satisfied_allows_(OR)
--- PASS: TestCompileACLSnapshot_ResourcePolicyGating/two_profiles,_both_unsatisfied_denies
--- PASS: TestCompileACLSnapshot_LegacyBindingNoLongerGates (0.89s)
```

Box 22 (revision mismatch / stale posture) is covered at unit level by
`TestApplyPosture_FailsClosedForInvalidEvaluations` — 4 subtests: unsatisfied,
revision mismatch, missing report timestamp, expired report — and end to end by
`TestPropagation_RequirementChangeDeniesThenConverges`.

**GraphQL + propagation (boxes 12–16, 25, 29)** — `graph/resolvers`:

```
--- PASS: TestResourcePolicyOperationsRequireAdmin (0.00s)
--- PASS: TestResourcePolicyQueriesAndCRUDResolvers (0.83s)
--- PASS: TestResourcePolicyAssignmentResolvers (0.86s)
--- PASS: TestResourcePolicyProfileResolvers (0.89s)
--- PASS: TestResourcePolicyResolversRejectCrossWorkspace (0.84s)
--- PASS: TestLegacyProfileBindingStillWorksAlongsideResourcePolicy (0.84s)
--- PASS: TestPropagation_RemovingProfileRestoresAccess (0.87s)
--- PASS: TestPropagation_AddingProfileRestrictsAccess (0.83s)
--- PASS: TestPropagation_RequirementChangeDeniesThenConverges (0.92s)
--- PASS: TestPropagation_NoStaleACLSurvivesMutation (0.87s)
```

On box 11 — the **store does not notify**. `NotifyPolicyChange` fires in the
resolvers (7 call sites in `resourcepolicy.resolvers.go`) and is asserted there
via a `fires` counter, including that cross-workspace rejections fire **zero**
notifications. Box 11's evidence is therefore resolver-level, not store-level.

**Push/catch-up (boxes 26–28)** — `internal/connector`, 11 tests pass; these need
no database and already ran. Note `TestPusher_ConcurrentPushAndDisconnect` is
**not** counted toward box 28: it is a `-race` smoke test that asserts nothing
about delivery, as its own file comment says. Box 28 rests on
`TestPushWorkspace_DisconnectedConnectorCatchesUpOnHeartbeat` and
`TestPushACLSnapshot_GateEdges`.

**Linux end-to-end (5 boxes)** — recorded at **M**, carried from Phase 8's live
two-machine run on 2026-09-23, not re-derived. The load-bearing evidence is the
Connector's own decisions, since the resource had to be made unreachable except
through the tunnel before any access claim was falsifiable:

```
access allowed  dest=172.17.0.4:80 proto=tcp route="connector"
access denied   dest=172.17.0.4:80 reason="no_acl_match"
```

**Frontend (7 boxes)** — `npm test`: **17 files, 90 tests pass**; `npx tsc -b`
clean.

## New tests written

Only two things were genuinely uncovered. Nothing was added for being merely
useful — in particular `DeviceProfilePostureModal` still has no test, because no
Phase 9 box names it.

### Box 24 — the ACL wire contract had no guard

"Device Profile IDs do not need to be added to the Connector ACL protocol" held
only by nobody having edited the proto. `ACLEntry` has 10 fields, none
profile-related, and `grep -i profile connector/src/` returns zero hits across the
crate — but nothing failed if someone added `repeated string profile_ids`.

New: `controller/internal/policy/acl_wire_contract_test.go`. It walks the
descriptors of `ACLSnapshot`, `ACLEntry`, `ACLConnector` and `ACLRemoteNetwork`
and fails on any field name containing `profile`, plus a positive half asserting
`allowed_spiffe_ids` remains a repeated string — the flattened answer that
*replaces* profile identity on the wire. Checked by name rather than against a
frozen field list, so unrelated additions do not trip it.

Confirmed non-vacuous: the descriptor walk sees all 10 `ACLEntry` fields.

### Frontend boxes 1 and 3 — no test executed any mutation

30 tests existed across 7 files and **not one mocked a mutation**. Only
form-gating and wording were asserted; `createResourcePolicy`,
`updateResourcePolicy`, `deleteResourcePolicy`, `addProfileToResourcePolicy` and
`removeProfileFromResourcePolicy` were never sent. A test that never performs a
create cannot prove "CRUD", so this was required by the boxes, not extra.

Four tests added, following the house idiom (`MockedProvider`, per-file
`renderWithMocks()`, codegen'd `*Document` constants, explicit `__typename`) and
adding `variables` to the mocks, which no existing test did — so a mutation sent
with the wrong arguments fails to match and the test fails:

- `CreateResourcePolicyModal` — sends `createResourcePolicy` with the **trimmed**
  name (input padded deliberately; the mock would not match otherwise)
- `EditResourcePolicyModal` — renames only when the name changed, with no profile
  mocks offered so a stray add/remove fails rather than passing unnoticed
- `EditResourcePolicyModal` — diffs the selection into one add and one remove with
  the right ids
- `ResourcePolicies` — the delete **confirm** branch (only Cancel was covered)

**A false positive caught while writing these.** The delete test first asserted
the dialog closing — but the page closes it from `onError` as well as
`onCompleted`, so a mutation that failed to match looked identical to success. It
now asserts the list turning over to its empty state, which only the success path
reaches via `refetch()`. Verified by deliberately breaking the mock's id and
confirming the test fails.

## Migration boxes

Both are recorded **N/A with a reason**, not passed and not failed.

Phase 4's backfill was implemented as `resource_policy_migration.go` with three
operations (`InventoryLegacyBindings`, `MigrateLegacyBindings`,
`VerifyMigrationEquivalence`), verified 2026-09-05 against nine synthetic legacy
shapes — 7 tests, `LegacySet == NewSet` for every fixture, idempotent second run —
and then **deliberately withdrawn**. The project is pre-production; a backfill
would move zero rows, so the lasting deliverable was judged to be the decision
record, not code needing re-verification when real data first appears.

The code is **not recoverable**: `git log --all -S"MigrateLegacyBindings"` returns
only `ae0c66f`, the documentation commit. It was never committed on any branch.

**"Existing legacy bindings survive the migration"** — there is no migration to
survive. The property that *is* true and now proven is **non-interference**:
legacy `resource_profile_bindings` rows are untouched by the Resource Policy
model. At **X** via `TestResourcePolicyStoreIntegration/LegacyBindingsPreserved`
and `TestLegacyProfileBindingStillWorksAlongsideResourcePolicy`.

**"Migration preserves effective access relationships"** — undefined without
migration code, and Phase 5 **deliberately inverted it**: a resource with a legacy
enforce binding and no Resource Policy is now ungated. Writing this test would
assert a property the codebase intentionally does not hold, about code that does
not exist. The single accepted change is instead asserted at **X** by
`TestCompileACLSnapshot_LegacyBindingNoLongerGates`, so the widening is proven to
be the *only* change rather than assumed.

Net: two unprovable boxes became three executed assertions.

## Failures — kept strictly separate

Running the full controller suite **with** the database produces exactly **7
failures**, and none is a Phase 9 test.

**(a) 7 × `TestGroupOrigin_*` — pre-existing, out of scope.** In
`graph/resolvers/policy_group_origin_test.go`, a SCIM group-provenance test that
carries no Phase 9 box. Root cause captured:

```
policy_group_origin_test.go:327: insert workspace: ERROR: new row for relation
"workspaces" violates check constraint "workspaces_status_check" (SQLSTATE 23514)
```

The fixture inserts `status='ACTIVE'` while `migrations/001_schema.sql:32`
constrains it to `('provisioning','active','suspended','deleted')`; same for
`remote_networks` and `shields`. Byte-identical on `fixed-pendings` and fixed
upstream by `8cf2cea`. **Not a Phase 9 result in either direction.**

**(b) `createGroup` is broken on this branch — real, but cannot fail Phase 9.**
`CreateGroup` passes nine scan destinations against a six-column `RETURNING`
(`internal/policy/store.go`), so it fails outright; `UpdateGroup` has the silent
form, always returning an empty `origin`. Found live in Phase 8 against the real
GraphQL API. **Every Phase 9 integration test creates groups with raw SQL**
(`mustInsertGroup`, `compiler_relay_integration_test.go:541`) and no Phase 9 test
calls `CreateGroup` — verified by grep — so this bug cannot produce a Phase 9
failure. Fixed upstream by `c5eb347`. Recorded as a finding.

**(c) Genuine Phase 9 failures: none.** All seven Phase 9 files passed on their
first real execution.

## Gate

```
go build ./...                                    OK
go vet ./...                                      OK
go test ./...            (no env)                 green, but skips 38 files
go test ./...            (PKI_TEST_DATABASE_URL)  7 failures, all out-of-scope (a)
npm test                                          17 files, 90 tests pass
npx tsc -b                                        clean
```

The two gate lines are reported separately on purpose. **The first is the gate as
specified and it is misleading** — it cannot fail for any PENDING-16 reason,
because nothing PENDING-16 runs under it.

## Carried forward

- **`pending-16` has no CI workflow.** The one on `fixed-pendings` provides
  Postgres and sets the five `*_TEST_DATABASE_URL` vars, which would make the gate
  real in CI rather than only locally. Not merged; the merge has been declined,
  and everything above was done locally without it.
- **That CI is itself incomplete**: it does not set `RESOURCE_TEST_SHIELD_ID`, so
  `resource_acl_coherence_test.go` and `graph/resolvers/posture_resolvers_test.go`
  still skip there, and its "verify integration tests ran" guard probes only one
  test in `internal/client` and would not notice.
- **A resource with no Resource Policy is ungated** — `applyPosture` takes its Any
  Device branch, and `internal/resource` never writes `device_resource_policy_id`,
  so every newly created resource starts open. Consistent with the Phase 4
  decision, but "unassigned" and "deliberately open" are indistinguishable. Worth
  an explicit decision in Phase 10.
