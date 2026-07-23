---
type: phase
member: M1
sprint: 15
phase: 3
title: GraphQL Admin + ACL Compiler Hook
status: planned
depends_on: [Phase2-Ingestion-and-Evaluation]
tags: [go, posture, graphql, acl, pending-08]
---

# Phase 3 — GraphQL Admin + ACL Compiler Hook

> Depends on Phase 2. This phase is where posture starts affecting live authorization —
> everything upstream of it is inert.

## Goal

Give operators Device Profile CRUD + visibility, and make the ACL compiler actually gate
on profile satisfaction in enforce mode.

## Files

| File | Change |
|------|--------|
| `controller/graph/posture.graphqls` | **new**, mirrors `resource.graphqls`; includes `supportedPostureChecks` query |
| `controller/graph/resolvers/resolver.go` | add `PostureStore`/evaluator field to `resolvers.Resolver` — the generated posture resolvers have no store to call without this |
| `cmd/server/main.go` | wire the posture store/evaluator into `resolvers.Resolver`'s construction (alongside the `client.Service` wiring from Phase 2) |
| `controller/internal/policy/compiler.go` | posture filter plus the internal `CompiledACL{Snapshot, ValidUntil}` result contract |
| `controller/internal/policy/cache.go` | cache metadata, injectable clock, expiry callback registration, and per-workspace singleflight |
| `controller/internal/policy/*` (notify path) | register an expiry callback after construction and reuse `NotifyPolicyChange(workspace_id)` |
| `controller/internal/client/service.go`, `controller/internal/connector/control_stream.go`, `controller/internal/connector/acl_push.go` | change only their `GetOrCompile` compile closures to return `*CompiledACL`; `GetOrCompile` unwraps and still returns `*clientv1.ACLSnapshot`, so downstream snapshot handling is unchanged |
| `controller/go.mod`, `controller/go.sum` | add `golang.org/x/sync/singleflight` if it is not already a direct dependency |

## GraphQL Admin

Mirror the existing CRUD shape used in `resource.graphqls`/`policy.graphqls`:

- `DeviceProfile` type: `id`, `name`, `mode` (`audit`/`enforce`), `requirements`, `boundResources`.
- Mutations: `createDeviceProfile`, `updateDeviceProfileMode`, `deleteDeviceProfile`, `addProfileRequirement`, `removeProfileRequirement`, `bindResourceToProfile`, `unbindResourceFromProfile`.
- Queries: device posture visibility — **failure reason, observation time, report age,
  collector error** per device/profile. **Never expose raw collector command output** —
  only the normalized `detail` string from `device_posture_observations`. Also add
  **`supportedPostureChecks: [PostureCheckDescriptor!]!`** (`id`, `label`, `platform`,
  whether `allowUnsupported` is meaningful for it) — a server-driven list the Admin UI's
  requirements editor sources its options from, instead of a hardcoded frontend list
  that would drift the moment a new check is added server-side.
- **All mutations require the `ADMIN` role and enforce workspace ownership** — same
  pattern as existing resource/policy admin mutations. A profile/requirement/binding
  operation for a workspace the caller doesn't own must be rejected before it reaches
  the store layer, not merely filtered out of results afterward.
- **Empty-profile guard covers all three paths that can produce a zero-requirement
  enforce profile, not just the two obvious ones:**
  - `updateDeviceProfileMode(id, mode: ENFORCE)` — reject if zero requirements.
  - `bindResourceToProfile` — reject if the target profile is enforce-mode with zero requirements.
  - **`removeProfileRequirement`** — reject if removing this requirement would leave an
    already-enforced profile with zero requirements (this is the store-layer check
    added in Phase 1's `RemoveRequirement`; the resolver here just surfaces that
    rejection as a clear GraphQL error, it doesn't duplicate the transactional check).
  An enforce-mode profile with no requirements is vacuously satisfied by every device
  under ordinary AND-of-requirements semantics — missing any one of these three paths
  lets an operator silently grant everyone access to a gated resource.

## Resolver Wiring

Adding `posture.graphqls` is not self-sufficient from `go generate` alone:
`resolvers.Resolver` (`controller/graph/resolvers/resolver.go`) is a flat struct of
injected dependencies constructed once in `cmd/server/main.go`. The generated posture
resolver stubs need a `PostureStore` (and/or evaluator) field added to `Resolver`, and
that field wired at construction time in `main.go` — exactly like every other
resolver's store dependency. Without this, the generated resolver methods have nothing
to call and won't compile against real logic.

Run `cd controller && go generate ./graph/...` after schema changes.

## ACL Compiler Hook

In `CompileACLSnapshot` (`controller/internal/policy/compiler.go`), where SPIFFE IDs are
currently aggregated per-resource purely by group membership
(`ListActiveDeviceSPIFFEsForGroups`):

- A device's SPIFFE ID is included for a resource only if:
  1. Group access already succeeds (unchanged), **and**
  2. The resource has **zero enforce-mode bound profiles** (identity-only — this
     includes resources with only audit-mode profiles, or no profiles at all), **or**
  3. The resource has **one or more enforce-mode bound profiles** and the device
     satisfies **at least one of them** (OR across enforce-mode profiles **only**).
- **Audit-mode profiles never participate in step 2/3's OR at all** — not as a
  fallback, not as an "audit never blocks" exception. They are evaluated and their
  results are recorded/visible (Phase 2), but they must have zero effect on ACL
  membership in either direction. (The original version of this rule read "at least one
  bound profile is in audit mode OR satisfies enforce" — that is wrong: it would let a
  device pass a resource gated by one failing enforce profile and one failing audit
  profile, because the audit branch alone was enough. Do not implement that version.)
- Use the **batch** `EvaluationsForDevices` (Phase 1) — fetch all evaluations for the
  device set once per compile pass, then look up in-memory per resource. Do not call a
  per-device evaluation query inside the per-resource/per-device loop; `CompileACLSnapshot`
  already follows a bulk-fetch-then-loop pattern for `ListEnabledRulesWithResources` and
  this must match it, not introduce an N+1.
- An evaluation authorizes only when `evaluation.profile_revision == profile.revision`.
  Missing or mismatched revisions are unsatisfied. This makes requirement changes
  immediately fail closed even if bulk re-evaluation finishes after the mutation.
- At compile time, apply the freshness check from Phase 2: an evaluation whose source
  report's `received_at` is older than 10 minutes is treated as unsatisfied regardless
  of its stored `satisfied` value.
- **Every posture-relevant mutation** — profile create, mode change (audit↔enforce),
  requirement add/remove, resource bind/unbind, profile delete — must call
  `NotifyPolicyChange(workspace_id)`, not only evaluation-satisfied transitions. A
  profile flipped from audit to enforce with no new report since then still changes
  what the ACL should look like and must push a new snapshot.

## Cache-Expiry Fix (`controller/internal/policy/cache.go`) — the actual staleness fix

**A freshness check inside `CompileACLSnapshot` alone does not solve passive posture
expiration**, because `GetOrCompile` (`controller/internal/policy/cache.go:96-98`)
returns a cache hit and never calls `CompileACLSnapshot` again once a snapshot is
cached — the only trigger for recompilation today is an explicit `Invalidate()` call
from a mutation path. A report quietly crossing the 10-minute staleness line produces
no mutation, so nothing invalidates the cache and a connector could keep receiving an
ACL compiled before the report went stale.

### Step 1 — compiler result and cache-entry types

`validUntil` is computed by the policy compiler, which has the profile/evaluation data;
the cache cannot derive it from the protobuf snapshot. Change the internal compiler
contract explicitly:

```go
type CompiledACL struct {
    Snapshot   *clientv1.ACLSnapshot
    ValidUntil time.Time
}

func CompileACLSnapshot(...) (*CompiledACL, error)
```

`CompiledACL` is internal compiler-to-cache metadata, not a replacement for the
public/cache-facing snapshot result. Change the three production `GetOrCompile`
closures (`client.Service`, connector control/heartbeat, and `ACLPusher`) to return
`*CompiledACL`. `GetOrCompile` stores `ValidUntil`, then returns only
`compiled.Snapshot` as `*clientv1.ACLSnapshot`; code after each call site does not
change. Direct compiler tests must unwrap/assert `result.Snapshot` and
`result.ValidUntil`; cache tests update their compile mocks to return `*CompiledACL`.

Change `SnapshotCache.GetOrCompile`'s compile callback to return `*CompiledACL`. It may
continue returning `*clientv1.ACLSnapshot` to consumers after storing the metadata.
Update all callers (`client.Service`, Connector heartbeat reconciliation, `ACLPusher`)
and compiler/cache tests. Direct compiler tests assert through `result.Snapshot`.

`SnapshotCache.entries` is currently `map[string]*clientv1.ACLSnapshot` — no room for
expiry metadata. Introduce:

```go
type cacheEntry struct {
    snapshot   *clientv1.ACLSnapshot
    validUntil time.Time // zero value = no posture-driven expiry (no enforce profiles bound)
}
```

and change `entries` to `map[string]*cacheEntry`. This is a genuinely new type, not a
relabeling of something that already exists — keep it distinct from the cache's
existing `epoch` counter (a separate CAS race-guard, unrelated to this expiry check and
unrelated to `Notifier.versions`, see step 3). Use an **injectable clock** (not a bare
`time.Now()` call) so expiry behavior is deterministically testable. If `CompileACLSnapshot`
fails while handling an expired entry, propagate the error — never fall back to
returning the entry we already know is stale.

### Step 2 — compute `validUntil` with OR-aware semantics, not a flat minimum

Taking `min(received_at)` across *every* evaluation in the workspace is too
conservative: it counts evaluations that don't currently justify any allowed access
(e.g. a failing enforce profile whose OR-partner is what's actually granting a device
its access), causing needless early recompiles — or, worse, could produce an
already-expired `validUntil` at write time if some irrelevant failing evaluation is
old. The correct formula only considers evaluations that are actually load-bearing:

```
for each currently-allowed (device, resource) pair in the compiled snapshot:
    if resource has zero enforce-mode bound profiles:
        skip this pair entirely — it's identity-only, not posture-gated, and has
        no defined pair_valid_until (do not treat it as "unbounded" and do not let
        it enter the min below as if it were)
    else:
        pair_valid_until = max(received_at + 10min) across only the enforce-mode
                           profiles that are actually SATISFYING that pair (the OR-winners)

snapshot_valid_until = min(pair_valid_until) across only the pairs that had one computed
                       (i.e. only posture-gated pairs) — if there are none (no enforce
                       profile bound anywhere in the workspace, or every allowed pair is
                       identity-only), the result is unbounded: set validUntil to the
                       zero value.
```

**Be explicit about this exclusion when implementing** — identity-only pairs must never
contribute to the `min`, whether by being skipped outright or by being treated as
"infinitely valid" and thus naturally dropping out of a min comparison. Either
implementation is fine as long as the *test* explicitly covers a workspace with a mix of
identity-only and posture-gated pairs and confirms only the posture-gated ones bound
`snapshot_valid_until`.

### Step 3 — expiry notifier wiring and version bump

`SnapshotCache` must not directly own a `*Notifier`: `Notifier` already owns the cache,
so that would create cyclic construction. Add a post-construction callback:

```go
type ExpiryNotifier func(workspaceID string) error

func (c *SnapshotCache) RegisterExpiryNotifier(fn ExpiryNotifier)
```

During startup, after both objects exist, register a closure that calls
`policyNotifier.NotifyPolicyChange(context.Background(), workspaceID)`. Registration
happens once before serving. If an entry expires without a registered notifier, return
an error and fail closed. Copy/remove the expired entry and release every cache mutex
before invoking the callback: notification calls `cache.Invalidate()` and would
deadlock if invoked under the cache lock.

`GetOrCompile` checks `now() > cached.validUntil` on **every read**, in addition to the
existing `Invalidate()`-driven miss path. **Critically, recomputing on expiry is not
enough by itself** — `pushACLSnapshot` (`controller/internal/connector/control_stream.go:718`)
compares the connector's last-acknowledged version against `Notifier.Version(workspace_id)`
(`controller/internal/policy/notifier.go:74-81`) and **skips sending if they're equal**.
`Notifier.Version()` only changes inside `NotifyPolicyChange` — a bare recompile inside
`GetOrCompile` produces a snapshot with different `Entries` but the **same `Version`**,
so the corrected snapshot would be silently dropped by the push path, defeating the
entire fix. Therefore, on detecting expiry, `GetOrCompile` must:

1. Call the registered expiry notifier — this invokes `NotifyPolicyChange`, bumps the version **and** invalidates
   the cache entry as a side effect (do not special-case a "force delivery" flag in
   `pushACLSnapshot` instead; that would weaken the version contract rather than use it
   correctly — the version bump *is* the correct signal).
2. Proceed as an ordinary cache miss: recompile via `CompileACLSnapshot`, compute the
   fresh `validUntil` (step 2), and store the new `cacheEntry` under the now-bumped version.
3. Return the fresh snapshot to the caller.

This does not risk infinite recursion — `NotifyPolicyChange` only bumps the version and
clears the cache entry, it does not itself call back into `GetOrCompile` or recompile
anything.

**Concurrent expiry must be claimed once.** If two goroutines
call `GetOrCompile` for the same expired workspace at roughly the same time (plausible:
multiple connectors in one workspace, or a heartbeat racing an unrelated GraphQL read),
both must not independently notify and compile. Put the **expiry re-check, exactly one
notification, and compilation** inside a per-workspace `singleflight.Group` closure.
The winner re-reads the entry inside the closure: if another caller already refreshed
it, return that fresh result; otherwise atomically claim/remove the expired entry,
release locks, notify once, and compile. Waiting callers share the result. Test both
compile count and notifier/version-bump count — each must be exactly one.

### Step 4 — verified sufficient without a separate sweeper

The connector's existing 15-second health heartbeat (`connector/src/control_stream.rs`)
drives `handleConnectorHealth` (`controller/internal/connector/control_stream.go:645`),
which **unconditionally** calls `pushACLSnapshot` → `GetOrCompile` on every tick,
regardless of whether anything was explicitly invalidated. So for any workspace with at
least one connected connector, the expiry-and-version-bump sequence above gets
exercised roughly every 15 seconds "for free" — no new background job needed for
*ACL* staleness specifically (the separate retention worker,
[[Sprint15/Member1-Go/Phase4-Retention-Worker]], handles raw-data cleanup — an
unrelated concern; don't conflate the two). A workspace with zero connected connectors has
no live tunnel to revoke anyway, so isn't a gap.

## Tests

- Unit: unprofiled resource — ACL membership unaffected by posture.
- Unit: resource bound only to an audit-mode profile, device failing it — SPIFFE still included, failure recorded, **no exclusion**.
- Unit: resource bound to one enforce-mode profile (failing) **and** one audit-mode profile (failing) — SPIFFE **excluded** (this is the audit-bypass regression test — must fail before the fix and pass after).
- Unit: profile in enforce mode, device failing it, no audit profiles present — SPIFFE excluded.
- Unit: enforce-mode resource bound to 2 enforce profiles, device satisfies exactly one — SPIFFE included (OR, enforce-only).
- Unit: evaluation `satisfied=true` but source report `received_at` > 10 minutes old — treated as unsatisfied at compile time.
- Unit: `CompileACLSnapshot` over N devices issues one batch evaluation query, not N.
- Unit: evaluation revision mismatch fails closed; matching revision uses the stored result.
- Integration: evaluation transition (pass→fail under enforce) triggers `NotifyPolicyChange` and a new snapshot with the SPIFFE removed.
- Integration: profile mode flipped audit→enforce with no new evaluation change — still triggers `NotifyPolicyChange`.
- Integration: non-ADMIN caller attempting a profile mutation is rejected; ADMIN caller from a different workspace attempting to bind another workspace's resource is rejected.
- Unit: attempting to switch a zero-requirement profile to `enforce`, to bind an enforce-mode zero-requirement profile to a resource, **or to remove the last requirement of an already-enforced profile** — all three rejected.
- Unit: `pair_valid_until`/`snapshot_valid_until` computed correctly with the OR-aware formula — a resource with a failing enforce profile (old `received_at`) and a passing enforce profile (fresh `received_at`) uses the **passing** profile's expiry, not the failing one's; confirm this does not equal the naive flat-minimum result.
- Unit: a workspace with a mix of identity-only allowed pairs (no bound enforce profile) and posture-gated allowed pairs — `snapshot_valid_until` is bound only by the posture-gated pairs; an identity-only pair's absence of a `pair_valid_until` must not be misread as "unbounded, ignore the whole min."
- Unit/integration: two concurrent `GetOrCompile` calls for the same expired workspace result in exactly one expiry notification/version bump and one `CompileACLSnapshot` execution.
- Unit: expiry notifier is invoked after cache locks are released (no deadlock when it calls `Invalidate`); missing notifier returns an error and never returns the expired snapshot.
- Unit: `CompileACLSnapshot` returns `CompiledACL{Snapshot, ValidUntil}` and all three production callers preserve the metadata through `GetOrCompile`.
- **Integration (critical regression test): populate the cache via `GetOrCompile`, advance time past `validUntil` with no explicit `Invalidate()` call — confirm the next `GetOrCompile` call recompiles rather than returning the stale cached snapshot.** Must fail against a plain `CompileACLSnapshot`-only freshness check with no cache-level expiry, pass after.
- **Integration (the second, distinct release-blocker regression test — do not conflate with the one above): after the expiry-triggered recompile, confirm `pushACLSnapshot` actually delivers the new snapshot to a connector that already holds the old version** — i.e. assert `Notifier.Version(workspace_id)` changed as a result of the expiry, not just that `GetOrCompile`'s return value changed. A fix that only recomputes without calling `NotifyPolicyChange` passes the first test and silently fails this one.
- Integration: simulate the connector's 15s heartbeat path (`handleConnectorHealth` → `pushACLSnapshot`) hitting an expired cache entry — confirm it recompiles, bumps the version, and the pushed snapshot reflects the now-stale device's exclusion.

## Build Check
```bash
cd controller && go build ./... && go test ./internal/...
```

## Implementation Checklist
- [ ] **M1-E1** `controller/graph/posture.graphqls` — Device Profile CRUD + bindings + audit/enforce toggle + posture visibility + `supportedPostureChecks` query; all mutations ADMIN + workspace-scoped; enforce-mode requires ≥1 requirement across mode-switch, bind, **and** requirement-removal.
- [ ] **M1-E1b** `controller/graph/resolvers/resolver.go` + `cmd/server/main.go` — `PostureStore`/evaluator field added and wired; the generated resolvers have nothing to call otherwise.
- [ ] **M1-E2** `go generate ./graph/...`.
- [ ] **M1-E3** `CompileACLSnapshot` returns `*CompiledACL{Snapshot, ValidUntil}`; applies enforce-only OR, freshness, profile-revision matching, and batch evaluation queries. Update only the compile closures in ClientService, connector control/heartbeat, and ACLPusher; `GetOrCompile` continues returning a bare protobuf snapshot to their downstream logic. Update direct compiler and cache mocks/tests for the internal result type.
- [ ] **M1-E3b** `controller/internal/policy/cache.go` — `cacheEntry{snapshot, validUntil}`, injectable clock, and one-time `RegisterExpiryNotifier` callback wired after cache/notifier construction. On expiry, a per-workspace singleflight closure rechecks and claims expiry, releases locks, calls `NotifyPolicyChange` exactly once, recompiles exactly once, and never returns a known-stale snapshot.
- [ ] **M1-E4** Every posture-relevant mutation (not only evaluation transitions) → policy version bump → ACL cache invalidation → snapshot push, via existing `NotifyPolicyChange`.
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/...`

## Post-Phase Fixes
_None yet._
