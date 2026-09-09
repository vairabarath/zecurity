---
type: phase
member: M02
sprint: 19
phase: 5
title: ACL Compiler / Resource Policy Integration
status: done
depends_on: [2, 4]
tags: [resource-policy, acl, posture, compiler, pending-16]
---

# Phase 5 — ACL Compiler / Resource Policy Integration

## Goal

Change only the Controller-side policy source so the compiler resolves:

```text
Resource
   ↓
Resource Policy
   ↓
Device Profiles
   ↓
Posture
   ↓
allowed_spiffe_ids
```

while preserving the existing ACL output contract.

## Required work

- [x] Change the profile lookup path from direct Resource → Profile binding to Resource → Resource Policy → Profile.
- [x] Preserve existing group/resource authorization behavior.
- [x] Preserve `applyPosture()` OR semantics.
- [x] Preserve Any Device semantics for zero selected Device Profiles.
- [x] Remove the production authorization dependency on `DeviceProfile.mode`.
- [x] Do not replace `mode` with a new policy-level toggle.
- [x] Keep posture visibility/evaluation independent from whether a profile is bound.
- [x] Ensure current profile revision/evaluation freshness checks remain correct.
- [x] Preserve final `allowed_spiffe_ids` generation.
- [x] Preserve ACL versioning.
- [x] Preserve route/resource/shield/connector information in the ACL snapshot.
- [x] Ensure a policy with Profile A + Profile B authorizes a device satisfying either profile.
- [x] Ensure a policy with no profiles does not accidentally produce an empty-deny ACL.

## Critical rule

The Connector must continue receiving the resolved authorization state. Do not add Resource Policy or
Device Profile concepts to the Connector authorization protocol unless a verified existing contract
requires it.

## Verification

Add tests proving:

```text
zero profiles → Any Device
one profile + pass → allow
one profile + fail → deny
two profiles + A pass/B fail → allow
two profiles + A fail/B pass → allow
two profiles + both fail → deny
```

Also prove the same behavior survives ACL snapshot generation.

---

## Implementation (completed 2026-09-09)

The cutover. `Resource → Resource Policy → Device Profile(s)` is now the real
authorization path; the ACL compiler no longer reads `resource_profile_bindings`
and no longer consults `device_profiles.mode`.

**Total change: 97 insertions, 34 deletions across two production files.** No
migration, no proto change, no `applyPosture` change, no Connector change.

### Files changed

| File | Change |
|---|---|
| `internal/posture/resource_policy_store.go` | **+78** — one new batch method |
| `internal/policy/compiler.go` | **−34/+19** — profile source swapped |
| `internal/policy/compiler_test.go` | six-row matrix test |
| `internal/policy/compiler_relay_integration_test.go` | snapshot-level gating + cutover guard |
| `internal/posture/resource_policy_store_integration_test.go` | batch-lookup subtest |

### The new store method

`ListPolicyProfilesForWorkspace(ctx, workspaceID) (map[uuid.UUID][]Profile, error)`,
keyed by resource ID — one query for the whole workspace, so compilation stays
free of per-resource lookups.

Verified beforehand that no such lookup existed: `ListProfilesForPolicy` and
`ListResourceIDsForPolicy` take a `policyID`, `GetResourcePolicyForResource` takes
a `resourceID`, and `ListResourcePolicies` returns policies with no profiles or
resource linkage. Every one would have been N+1 from the compiler.

Shape follows the existing grouped-batch convention (`EvaluationsForDevices`,
`policy.ListActiveDeviceSPIFFEsForGroups`), so **no new type was needed**; the
projection is copied verbatim from `ListProfilesForPolicy` so scan order cannot
drift.

**The INNER JOIN is load-bearing.** A resource with no policy, or a policy holding
zero profiles, is simply absent from the map. The compiler reads a nil slice,
`applyPosture` takes its existing ungated branch, and "no policy" and "empty
policy" become indistinguishable — Any Device, never deny-all. The chosen NULL
semantics therefore needed no special-casing anywhere.

Profile mode is deliberately not consulted: a profile gates a resource because the
policy references it. An audit-mode profile attached to a policy **does** gate —
the semantic inversion of this sprint, asserted explicitly in the store test.

### What the compiler lost

Deleted from `CompileACLSnapshot`: the `ListProfiles` call, the
`ListResourceBindingsForWorkspace` call, the `profileMap` build, and the entire
binding loop including `if profile.Mode != posture.ModeEnforce { continue }`.

Replaced by the batch fetch plus a four-line re-key to `map[string][]posture.Profile`
(matching `entryKey.resourceID`), so the lookup and the `applyPosture` call
downstream are structurally unchanged. The local was renamed
`enforceProfilesByResource` → `gatingProfilesByResource`, since "enforce" now names
a retired concept.

`CompileACLSnapshot`'s **signature is unchanged**, which is why the three
production call sites (`client/service.go:740`, `connector/control_stream.go:713`,
`connector/acl_push.go:61`) and the five injected-compiler closures in
`connector/acl_push_test.go` all needed no edit. `internal/connector` tests pass
untouched.

### `device_profiles.mode` after this phase

`compiler.go:160` was the **only** production authorization read. Confirmed by
inspection: `posture.ResourceSatisfied` (`evaluate.go:281`) is the only other
mode-based authorization helper and is called *exclusively from its own test* —
dead production code, left in place as unrelated cleanup.

Deliberately retained: the column, the GraphQL field, `updateDeviceProfileMode`,
and all four write-path guards (`UpdateProfileMode` validation and empty-enforce,
`RemoveRequirement` last-requirement, `CreateResourceBinding` empty-enforce). They
protect a real invariant on the legacy write path. Retiring `mode` entirely is not
this phase's job.

Posture **evaluation and visibility** needed no change and were verified
independent already: `evaluate.go:141` evaluates against every profile in the
workspace, and `ListDevicePostureVisibility` joins neither the binding table nor
`mode`.

## Verification

### The matrix, proven twice

| profiles on policy | posture | expected | `applyPosture` | full snapshot |
|---|---|---|---|---|
| zero | — | allow (Any Device) | ✅ | ✅ |
| one | pass | allow | ✅ | ✅ |
| one | fail | deny | ✅ | ✅ |
| two | A pass, B fail | allow (OR) | ✅ | ✅ |
| two | A fail, B pass | allow (OR) | ✅ | ✅ |
| two | both fail | deny | ✅ | ✅ |

`TestApplyPosture_PolicyProfileMatrix` covers the helper directly.
`TestCompileACLSnapshot_ResourcePolicyGating` proves the same behaviour survives
full `CompileACLSnapshot` output, asserting on `AllowedSpiffeIds` and confirming
routing (`route_type`, `remote_network_id`) is unaffected even when a device is
denied.

**These tests have teeth.** Their fixtures create *no* legacy bindings at all, so
the "denies" rows could only pass if the compiler genuinely resolves through the
policy — under the old code every case would have been ungated.

Closing a real gap: before this phase `compiler_relay_integration_test.go` had **no
posture seeding whatsoever**, so every existing subtest compiled with zero profiles
and the gated path had never been exercised end to end.

### Cutover guard

`TestCompileACLSnapshot_LegacyBindingNoLongerGates` asserts the one accepted
behaviour change: a resource with a legacy **enforce** binding and **no** Resource
Policy is now **ungated**. That is a deliberate widening, safe only because the
project is pre-production and no such row exists in any real database. The test
exists so the consequence is asserted rather than discovered.

### Commands

```bash
cd controller
go build ./...                                             # 0
PKI_TEST_DATABASE_URL=<dsn> go test ./internal/policy/...    -race   # ok
PKI_TEST_DATABASE_URL=<dsn> go test ./internal/posture/...   -race   # ok
go test ./internal/connector/ -race                                  # ok (untouched)
PKI_TEST_DATABASE_URL=<dsn> go test ./graph/resolvers/ -race \
    -run 'ResourcePolicy|LegacyProfileBinding'                       # ok
```

Whole controller suite: **23 packages ok**, zero Phase 5 failures.

**Pre-existing, unrelated:** the 7 `TestGroupOrigin_*` tests in `graph/resolvers`
still fail — the fixture inserts `status='ACTIVE'` against a lowercase-only check
constraint. They fail on `fixed-pendings` too and were left alone.

## Architecture boundary held

`migrations/`, `graph/`, `internal/resource/`, `connector/`, `shield/`, `client/`,
`relay/`, `admin/`, `proto/`, `go.mod`, `go.sum` — all unmodified. `ACLEntry`
still carries only `allowed_spiffe_ids` plus routing, so the Connector never
learns that Resource Policies exist. No Audit/Enforce toggle was added at the
policy level. `resource_profile_bindings` was neither dropped nor written to; the
`boundResources` GraphQL field still reads it for visibility.

## Carried forward

1. **New resources still start with a `NULL` policy** — `internal/resource` never
   writes `device_resource_policy_id`. Under "NULL = Any Device" that matches how
   an unbound resource behaves today, so nothing is broken, but "every resource has
   exactly one policy" is not self-maintaining. Deliberately out of scope here.
2. **Phase 6** owns propagation verification. Phase 3 already wired
   `NotifyPolicyChange` into all seven Resource Policy mutations, so Phase 5 added
   none.
