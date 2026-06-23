---
type: decision
status: accepted
date: 2026-06-22
related:
  - "[[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]"
tags:
  - adr
  - controller
  - policy
  - acl
  - concurrency
  - correctness
---

# ADR-011 — SnapshotCache Epoch-CAS Protection

> Follow-up correctness item opened by the proactive-push concurrency audit
> (PR `feat/acl-proactive-propagation`, merged to `main`). Proves and fixes the
> **stale compile insertion** gap captured by the skipped test
> `controller/internal/connector/acl_push_test.go::TestPushWorkspace_StaleInsertDefersLastChange`.
> **Accepted and implemented** on branch `fix/acl-snapshot-epoch-cas`: the epoch
> CAS lands in `SnapshotCache`, all three compile paths use `GetOrCompile`, and
> the regression test above is now active and passing.

## Context

`SnapshotCache` (`controller/internal/policy/cache.go`) is a process-local,
per-workspace cache of compiled `ACLSnapshot`s. Three paths compile-on-miss and
store the result:

- **Proactive push** — `connector/acl_push.go` `ACLPusher.pushOnce`
- **Heartbeat reconciliation** — `connector/control_stream.go` `pushACLSnapshot`
- **Client pull** — `client/service.go` `GetACLSnapshot`

`NotifyPolicyChange` (`policy/notifier.go`) bumps a per-workspace monotonic
version, calls `cache.Invalidate(ws)`, then fires the proactive-push hook.

The current `Set` is version-guarded (it rejects a strictly-lower version), which
correctly closes the original *newer-overwritten-by-older* race ("Finding 1").
It does **not** close a second race.

### The proven bug (stale compile insertion)

Compiling a snapshot is not instantaneous (DB reads + a single version read at
`policy/compiler.go`). If a policy change lands while a compile is in flight, the
older compile can store its result into the slot the change just invalidated:

```
T0  change #1: Invalidate(ws); hook -> worker -> pushOnce-A -> cache MISS -> compile-A (reads version 1, slow)
T1  change #2: version -> 2; Invalidate(ws)  [cache already empty -> no-op]; sets pending
T2  compile-A returns snap{v1}; cache.Set(ws, v1)   [empty slot; version guard permits]
T3  worker delivers v1; trailing pushOnce-B: cache HIT v1 -> re-delivers v1, consumes change #2's pending
T4  worker exits.  Result: cache={v1}, connectors at v1, notifier.Version=2.  Change #2 NOT delivered.
```

Worse, the **heartbeat fallback cannot recover it**: the poisoned entry is
labelled `v1`, connectors already report `v1`, so `pushACLSnapshot`'s gate
(`connectorVersion == snap.Version`) treats them as current and never re-pushes.
Delivery of change #2 is deferred until the *next* policy change for that
workspace. For revocation / user-disable / lockout, that is a silent security
gap.

There is also a finer sub-case: even if `compile-A` reads `version 2` (after the
bump) but read the **rows** before change #2 committed, it produces
`{v2, stale-entries}` — internally consistent, version-matched, and equally
unrecoverable by heartbeat.

### Root cause

1. `CompileACLSnapshot` is not an atomic snapshot of *(DB rows, version)* — rows
   and the version are read at different points with no recheck.
2. `Invalidate` only deletes; it **cannot retract a `Set`** from a compile that
   began before the invalidation.
3. The version guard reasons about *version ordering*, not about *"did an
   invalidation occur since this compile began."* That question cannot be
   expressed with the current `Get`/`Set`/`Invalidate` API.

This bug **pre-existed** proactive push (heartbeat and client-pull have it
independently); proactive push only raised its likelihood by compiling on every
policy change.

## Affected code paths

| Path | Location | Interaction |
|------|----------|-------------|
| Proactive push | `connector/acl_push.go` `pushOnce` | Get → compile → Set |
| Heartbeat reconciliation | `connector/control_stream.go` `pushACLSnapshot` | Get → compile → Set; the version gate that gets defeated |
| Client pull | `client/service.go` `GetACLSnapshot` | Get → compile → Set |
| Cache contract | `policy/cache.go` | `Set`/`Invalidate` gain freshness awareness |
| Trigger ordering | `policy/notifier.go` `NotifyPolicyChange` | the invalidation point |

A correct fix must protect **all three** compile sites; a partial adoption leaves
a poisonable path.

## Constraint: preserve external behavior

External observable behavior must not change:

- `Get` returns the freshest valid snapshot or a miss.
- `Invalidate` forces a recompile on next access.
- Connectors/clients receive *the latest* snapshot, default-deny on compile error.
- No proto, gRPC, DB-schema, or wire change. The fix is internal to the cache +
  the three call sites' compile-and-store flow.

## Considered options

### 1. Epoch model (per-workspace monotonic invalidation counter)

Add a per-workspace `epoch` to the cache. `Invalidate` bumps it. A compile
captures the epoch **before** reading any state; the store succeeds **only if the
epoch is unchanged** at store time.

- **Completeness:** any policy change calls `Invalidate`, which bumps the epoch,
  so *any* in-flight compile (whether its stale-ness is in the version or the
  rows) is rejected. Closes the proven bug **and** the rows/version sub-case.
- **Subsumes Finding 1:** an older compile captured an older epoch; the newer
  change bumped it; the older `Set` is rejected. The version guard becomes
  redundant-but-harmless defense-in-depth.
- **Coupling:** epoch lives *inside* the cache and is bumped by the existing
  `Invalidate` call — **no new cache→notifier dependency** (avoids the import
  cycle risk).
- **Cost:** one `map[string]uint64` + two methods; callers capture-then-store.

### 2. CAS model (compare-and-swap on the slot value/pointer)

No separate counter; store `(snapshot, token)` and swap only if the observed
prior token still matches (optimistic concurrency on the slot itself).

- **Weakness — ABA:** the danger sequence empties the slot (Invalidate) and may
  refill it. A CAS keyed on the *slot value/pointer* can observe "empty → empty"
  and conclude nothing changed, even though an invalidation happened in between.
  A monotonic counter (Option 1) cannot ABA. Pure value-CAS therefore needs an
  ever-incrementing token to be safe — at which point it *is* the epoch model.
- **Verdict:** correct only when backed by a monotonic token; degenerates into
  Option 1. Not distinct as a standalone safe design.

### 3. Version + generation model (notifier version as freshness token, plus a generation)

Reject a `Set` whose `snapshot.Version` is below `notifier.Version(ws)` at store
time; add a generation counter to disambiguate equal versions.

- **Incomplete on version alone:** catches stale *version* but not the
  rows-read-early / version-read-late sub-case (`{v2, stale-rows}` passes).
- **Coupling:** the cache must consult the notifier at store time → constructor
  change + a cache→notifier reference (the notifier already holds the cache;
  this risks a cycle) or threading the expected version through every caller
  (re-introducing the same TOCTOU the fix is meant to remove).
- **The "generation" half is just the epoch** — once you add a monotonic
  generation bumped on invalidation, you have Option 1 with extra version
  plumbing. Strictly heavier, no added safety.

### 4. Other alternatives

- **4a. Serialize compiles under a per-workspace lock held across the whole
  compile.** Simple and correct, but holds a lock across DB I/O, serializing
  concurrent readers and pushing latency/contention onto the request and
  heartbeat paths. Rejected (liveness regression).
- **4b. `singleflight` per workspace.** Collapses concurrent compiles but has the
  wrong sharing semantics: a newer caller that joins an in-flight compile gets
  the *older* in-flight result. Does not fix the empty-slot insert and can
  actively deliver stale. Rejected.
- **4c. Compile in a single DB transaction / snapshot isolation, reading the
  version transactionally.** Fixes the *(rows, version)* atomicity at the source
  and is independently worthwhile, but does **not** fix the cache-insert
  ordering (an in-flight transactionally-consistent v1 still lands in a slot
  invalidated by v2). Necessary-but-insufficient; complementary, not a
  substitute.
- **4d. Tombstone with an "invalidated-at" timestamp compared to compile-start.**
  Equivalent to the epoch with wall-clock instead of a counter; introduces clock
  and resolution hazards for no benefit. Rejected.
- **4e. Make the notifier the single source of truth (merge cache into the
  notifier, store snapshot+version+epoch together).** Cleanest long-term shape
  but a larger refactor touching all callers' construction. Out of scope for a
  targeted correctness fix; revisit if the cache grows further responsibilities.

## Decision

**Adopt the Epoch model (Option 1), implemented as a compare-and-swap on a
monotonic per-workspace invalidation epoch, encapsulated in a single
`GetOrCompile` helper that all three call sites use.**

Sketch (no implementation in this ADR):

- `SnapshotCache` gains `epoch map[string]uint64`; `Invalidate` bumps
  `epoch[ws]`. Add `Epoch(ws) uint64` and
  `SetIfEpoch(ws, snap, observedEpoch) bool` (stores only if
  `epoch[ws] == observedEpoch`; retains the version guard as defense-in-depth).
- `GetOrCompile(ws, compileFn)`: return a cache hit; else capture `Epoch`,
  compile, `SetIfEpoch`; on CAS failure return a fresher cached entry if one
  appeared, else recompile at the new epoch.
- `pushOnce`, `pushACLSnapshot`, and `GetACLSnapshot` call `GetOrCompile` instead
  of hand-rolling Get→compile→Set.

### Why this and not the others

- It is the **only complete** option: it keys correctness on *the act of
  invalidation*, which every policy change performs, so it catches both the
  version-stale and rows-stale sub-cases — unlike Option 3 (version-only gaps on
  rows) and Option 2 (ABA).
- It has the **least coupling**: the epoch is internal to the cache and rides the
  existing `Invalidate` call — no cache→notifier cycle, no version threaded
  through callers.
- It **preserves external behavior**: `Get`/`Invalidate` semantics are unchanged
  to callers; only the internal store path becomes epoch-aware, centralized in
  `GetOrCompile`.
- It **subsumes the existing version guard** (Finding 1), so the two protections
  do not need separate reasoning.
- Option 4c (transactional compile) is recommended as a **complementary**
  follow-up for `(rows, version)` atomicity, but is not required for this fix and
  does not replace it.

## Consequences

- **Positive:** the proven bug closes; heartbeat fallback regains its guarantee;
  the skipped regression test flips to active as the acceptance gate; Finding 1
  protection is unified under one mechanism.
- **Negative / risks:**
  - *`GetOrCompile` retry under churn* — a workspace under continuous mutation
    could recompile repeatedly. Bounded by the real mutation rate; decide during
    review whether to cap retries (falling back to best-effort latest) or accept
    unbounded-but-converging.
  - *Client-pull tail latency* — `GetACLSnapshot` may recompile on CAS contention
    under heavy mutation. Low impact.
  - *Blast radius* — three call sites + cache contract must land together; a
    partial conversion leaves a poisonable path.
- **Reversibility:** purely internal; revertible by restoring bare `Set`.

## Migration plan

1. Add `epoch`, `Epoch`, `SetIfEpoch`, `GetOrCompile`; keep `Set`/`Get`/
   `Invalidate` during transition.
2. Convert the three call sites to `GetOrCompile`, each independently testable.
3. Once all three are converted, make `Set` internal to `GetOrCompile` (or
   remove it).
4. Flip `TestPushWorkspace_StaleInsertDefersLastChange` from skipped to active;
   add per-site analogues.
5. No DB migration, no proto change.

## Test strategy

- **Unit (cache):** `Invalidate` bumps `Epoch`; `SetIfEpoch` rejects on epoch
  advance, accepts on match; version-guard defense-in-depth still holds;
  `GetOrCompile` returns latest under a mid-compile invalidation; bounded
  behavior under churn.
- **Per-site regression:** unskipped `TestPushWorkspace_StaleInsertDefersLastChange`
  for push; analogues asserting heartbeat re-pushes after a poisoned-slot
  scenario and client-pull never serves stale after a mid-compile change.
- **`-race`:** concurrent `Invalidate` + `GetOrCompile` across workspaces;
  concurrent heartbeat-compile + proactive-compile + client-pull-compile for one
  workspace, asserting convergence to the latest version and never serving a
  version below `notifier.Version`.
- **Integration:** mutation during a slow compile → connector ends at the latest
  version; heartbeat self-heals a previously poisoned cache.

## References

- Skipped proof: `controller/internal/connector/acl_push_test.go::TestPushWorkspace_StaleInsertDefersLastChange`
- Cache: `controller/internal/policy/cache.go`
- Compiler version read: `controller/internal/policy/compiler.go`
- Compile sites: `connector/acl_push.go`, `connector/control_stream.go` (`pushACLSnapshot`), `client/service.go` (`GetACLSnapshot`)
- Trigger: `controller/internal/policy/notifier.go` (`NotifyPolicyChange`)
- [[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]
