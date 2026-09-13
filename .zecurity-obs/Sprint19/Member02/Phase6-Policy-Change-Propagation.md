---
type: phase
member: M02
sprint: 19
phase: 6
title: Policy Change Propagation
status: done
depends_on: [5]
tags: [resource-policy, cache, acl-push, controller, connector, pending-16]
---

# Phase 6 — Policy Change Propagation

## Goal

Every Resource Policy or Device Profile binding change must converge through the existing:

```text
mutation
  ↓
NotifyPolicyChange
  ↓
invalidate cache
  ↓
recompile
  ↓
push
  ↓
Connector
```

path.

## Required work

- [x] Verify create/update/delete Resource Policy mutations call policy notification.
- [x] Verify Resource → Policy assignment changes call policy notification.
- [x] Verify Policy → Device Profile binding changes call policy notification.
- [x] Preserve per-workspace policy version behavior.
- [x] Preserve cache epoch protection against stale in-flight compilation.
- [x] Preserve immediate ACL push to live Connectors.
- [x] Preserve heartbeat/version reconciliation as fallback.
- [x] Verify no stale ACL can survive a successful policy mutation after convergence.
- [x] Verify a removed Device Profile causes affected access to be removed after propagation.
- [x] Verify adding a Device Profile grants access only to devices satisfying it.
- [x] Verify changing a profile requirement/revision invalidates affected authorization.

## Race tests

- [x] Policy changes while ACL compilation is in flight.
- [x] Old compilation cannot overwrite a newer cache epoch.
- [x] Connector temporarily disconnected during push.
- [x] Connector catches up through heartbeat/version reconciliation.

---

## Implementation (completed 2026-09-13)

**No production code changed.** Every box is verification. The propagation
machinery (`NotifyPolicyChange` → invalidate + version bump → push, with
heartbeat reconciliation as fallback) predates this sprint and was left untouched
— which was the expected outcome, and is itself the result: Phase 3's wiring and
Phase 5's cutover were already correct.

Three test files touched, all `_test.go`:

| File | Change |
|---|---|
| `graph/resolvers/resourcepolicy_propagation_test.go` | **new** — 4 end-to-end convergence tests |
| `internal/connector/acl_push_test.go` | +2 tests — disconnect catch-up, gate edges |
| `graph/resolvers/resourcepolicy_resolvers_test.go` | fixture exposes `notifier` + `cache` |

### Evidence for every box

Eight boxes were already satisfied before this phase began. They are cited, not
re-tested — duplicating policy-agnostic coverage would add maintenance without
adding proof.

| Box | Evidence |
|---|---|
| Create/update/delete policy notifies | **existing** — 16 notify assertions across 4 tests in `resourcepolicy_resolvers_test.go`, incl. rejection paths asserting the count does *not* move |
| Resource → Policy assignment notifies | **existing** — same |
| Policy → Device Profile binding notifies | **existing** — same |
| Preserve per-workspace policy version | **new** — `TestPropagation_NoStaleACLSurvivesMutation` asserts the version advances across a mutation; `TestPushACLSnapshot_GateEdges` covers the heartbeat gate's version comparison |
| Preserve cache epoch vs stale in-flight compile | **existing** — `cache_test.go`: `TestSetIfEpoch_RejectsOnAdvance`, `TestGetOrCompile_MidCompileInvalidationRecompiles`, `TestCache_ConcurrentGetOrCompileAndInvalidate`; `acl_push_test.go` `TestPushWorkspace_StaleInsertDefersLastChange` |
| Preserve immediate push to live Connectors | **existing** — `TestPushWorkspace_FanOut` (fan-out + tenant isolation), `TestPushWorkspace_CompileErrorNoPush` (default-deny), `TestPushWorkspace_SendQueueFull` |
| Preserve heartbeat/version reconciliation | **existing + new** — `TestPushACLSnapshot_DeliversCachedAndRespectsGate`; extended by `TestPushWorkspace_DisconnectedConnectorCatchesUpOnHeartbeat` and `TestPushACLSnapshot_GateEdges` |
| No stale ACL survives a mutation | **new** — `TestPropagation_NoStaleACLSurvivesMutation` |
| Removed Device Profile removes access | **new** — `TestPropagation_RemovingProfileRestoresAccess` |
| Added Device Profile grants access only to satisfying devices | **new** — `TestPropagation_AddingProfileRestrictsAccess` |
| Requirement/revision change invalidates authorization | **new** — `TestPropagation_RequirementChangeDeniesThenConverges` |
| Race: policy change during in-flight compile | **existing** — `TestPushWorkspace_Coalesces`: 5 changes during one compile yield exactly 2 compiles, latest version wins |
| Race: old compile cannot overwrite newer epoch | **existing** — `TestPushWorkspace_StaleInsertDefersLastChange` + the `SetIfEpoch` tests |
| Race: Connector disconnected during push | **new** — `TestPushWorkspace_DisconnectedConnectorCatchesUpOnHeartbeat` |
| Race: Connector catches up via heartbeat | **new** — same test |

### Why the convergence tests have teeth

They compile through `SnapshotCache.GetOrCompile`, not by calling
`CompileACLSnapshot` directly. A test that compiled directly would pass even if
cache invalidation were broken; going through the cache means each assertion also
proves the mutation's invalidation actually landed.
`TestPropagation_NoStaleACLSurvivesMutation` makes that explicit: two compiles
with no mutation between them must return the *same pointer* (a cache hit), and a
mutation between them must produce a different snapshot with a higher version.

### The requirement-change deny window

`AddRequirement` bumps `device_profiles.revision` in its transaction, and
`applyPosture` (`compiler.go:330`) rejects any evaluation whose `ProfileRevision`
differs from the profile's current revision. So the instant a requirement lands,
**every** stored evaluation is stale and every device is denied — the system fails
closed. Convergence comes from `ReevaluateWorkspace`, which the
`addProfileRequirement` resolver calls immediately afterwards
(`posture.resolvers.go:236`).

The test drives the two steps separately — adding the requirement through the
**store**, which does not re-evaluate — so it can assert *both* halves: denied at
the bump, allowed again after re-evaluation. The window is real but brief in
production, since the resolver does both back to back. It is asserted here so it
is never mistaken for a bug.

### Two facts worth not re-deriving

- **The pusher has no version gate, deliberately** (`acl_push.go:108-115`): it
  sends unconditionally to every live Connector, and never retries. Per-workspace
  version behaviour lives in the notifier's counter, the heartbeat gate, and the
  Client's `up_to_date` check — never in the pusher.
- **The heartbeat gate skips only on exact equality** with a non-zero reported
  version (`control_stream.go:719`). A Connector reporting a version *ahead* of
  the Controller is therefore treated as behind and resynced — the safe
  direction. Both edges are now covered by `TestPushACLSnapshot_GateEdges`.

## Known gaps, recorded rather than fixed

1. **The heartbeat wiring itself is untested.** Nothing constructs a
   `ConnectorHealthReport`, so `control_stream.go:645` — where the reported
   `AclVersion` feeds the gate — is uncovered. The blocker is structural:
   `handleConnectorHealth` opens with `h.Pool.QueryRow` on a concrete
   `*pgxpool.Pool`, not an interface, so a heartbeat-level test needs a database
   (this package has none) or a refactor. Testing at the `pushACLSnapshot` seam is
   the established pattern and is what these tests do.
2. **`GetACLSnapshot` has zero test coverage anywhere.** The Client convergence
   path (`internal/client/service.go:740`) and its `known_version` / `up_to_date`
   gate are entirely untested. A real hole, but the phase's boxes are
   Connector-centric — recorded as a follow-up.
3. **A stale cache entry matching the Connector's reported version blocks
   heartbeat recovery**, because the gate compares against the cache rather than
   the notifier. Documented at `acl_push_test.go:261-278`; not fixed here.

## Verification

```bash
cd controller
go build ./...                                                    # 0, production code untouched

PKI_TEST_DATABASE_URL=<dsn> go test ./graph/resolvers/ -race \
    -run 'ResourcePolicy|Propagation|LegacyProfileBinding'         # ok
go test ./internal/connector/ -race                                # ok
go test ./internal/policy/... -race                                # ok (cited coverage)
PKI_TEST_DATABASE_URL=<dsn> go test ./internal/posture/... -race    # ok
```

Whole controller suite: **23 packages ok**, zero Phase 6 failures.

**Pre-existing, unrelated:** the 7 `TestGroupOrigin_*` tests in `graph/resolvers`
fail on `fixed-pendings` too (fixture inserts `status='ACTIVE'` against a
lowercase-only check constraint).

**Flaky under load, not a regression:** `TestProcessorRunProcessesEventIntegration`
(`internal/outbox`, Sprint 18 code untouched by this phase) failed once during a
full parallel run and passed both in isolation and on a repeat full run — DB
contention against a single test Postgres, not a behavioural change.

## Architecture boundary held

No production file changed. `migrations/`, `internal/policy/`, `internal/posture/`,
`internal/connector/` (non-test), `graph/` (non-test), `connector/`, `shield/`,
`client/`, `relay/`, `proto/`, `go.mod`, `go.sum` are all untouched. Propagation
still runs through the pre-existing path; no second notification mechanism was
introduced.
