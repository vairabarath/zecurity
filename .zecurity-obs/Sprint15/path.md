---
type: planning
status: active
sprint: 15
tags:
  - sprint15
  - dependencies
  - execution-path
  - posture
  - device
  - continuous-authz
  - pending-08
  - pending-09
---

# Sprint 15 — Device Posture & Health (PENDING-08) + Bounded Continuous Authz (PENDING-09 Option B)

> **Read this before writing a single line of code.**
> Source of truth: `.zecurity-obs/pending/PENDING-08-Device-Posture-Health.md` and
> `.zecurity-obs/pending/PENDING-09-Continuous-Authorization.md`.
> Scope: Linux-only posture v1, audit-mode-first rollout, **plus** a connector-side
> ACL-diff teardown mechanism that gives PENDING-09 Option B behavior without a new
> revocation RPC — this piece was scoped in review discussion, not in the original
> PENDING-08 doc, and is captured here as Phase set M3.

## Sprint Goal

Today access is gated purely by identity (SPIFFE cert + group → ACL); there is no device
posture signal, and nothing re-checks an already-open tunnel after it's established. This
sprint closes both gaps together, because they share one mechanism:

1. **Collect posture** (Linux: OS/version, LUKS, firewall, Secure Boot) via the client
   daemon, store raw observations, evaluate against reusable Device Profiles
   (AND within a profile, OR across profiles bound to a resource).
2. **Gate the ACL** on profile satisfaction in enforcement mode (audit mode records only).
3. **Make the ACL gate actually bite live sessions.** Connector ACL propagation is
   **proactive push** (`NotifyPolicyChange`, controller side) with a **15-second**
   connector-side health-reconciliation fallback (`control_stream.rs`,
   `HEALTH_INTERVAL_SECS`) — this was previously mis-described in this doc as "~60s";
   the 60s constants (`ACL_REFRESH_TTL_SECS`/`NO_SESSION_POLL_SECS`) belong to the
   **client daemon's** ACL polling, not connector propagation. Instead of building a
   brand-new push-revocation channel, diff the newly-applied snapshot's
   `(spiffe_id, resource_id)` allow-set against the previous one, and abort any live
   tunnel whose pair dropped out. This gives bounded (push-latency, worst case ~15s on
   the reconciliation fallback) mid-session revocation for **any** cause — posture loss,
   group revoke, or device revoke — not just posture. See Key Design Decisions for the
   corrected mechanics (CancellationToken-based registration, QUIC coverage, relay
   child-task cancellation, and the authorization/registration race fix — all confirmed
   as real gaps in a design review pass and fixed below).

```text
BEFORE                                          AFTER
------                                          -----
ACL = identity only (SPIFFE + group).           ACL = identity + (in enforce mode, ignoring
Tunnels checked at connect time only;             audit-mode profiles entirely) profile
nothing re-checks a live tunnel.                  satisfaction. Connector diffs each newly
                                                   applied ACL snapshot per (spiffe,resource)
                                                   and cancels tunnels whose entry dropped
                                                   out — live sessions now die within one
                                                   push/reconciliation cycle, on TCP, QUIC,
                                                   and relay-routed paths alike.
```

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| Linux-only v1 | Windows/macOS use the same collector interface later (out of scope). |
| Check states | `PASS`, `FAIL`, `UNSUPPORTED`, `UNKNOWN`, `ERROR`; registered check IDs, not free-form names. |
| Profile logic | Requirements **within** a profile are ANDed. A resource bound to multiple profiles is satisfied if **any one** profile fully passes (OR across profiles). |
| Raw retention | Store raw per-check observations, never a single collapsed "healthy" bool — needed for re-evaluation when profiles/bindings change without new reports. |
| Staleness | A report is stale after **10 minutes**. Missing, stale, `FAIL`, `UNKNOWN`, `ERROR`, and required-but-`UNSUPPORTED` all count as unsatisfied. |
| Report auth | `ReportDevicePostureRequest` wraps `{access_token, device_id, report}` — **the same shape as `GetACLSnapshotRequest`** (`proto/client/v1/client.proto`), not a bare access token. `VerifyAccessToken` returns only user/tenant claims (tokens are **not** device-scoped); the handler separately looks up `client_devices WHERE id=$device_id AND user_id=$user_id`, and checks `workspace_id` match + `revoked_at IS NULL` — mirroring the existing `GetACLSnapshot` handler exactly (`controller/internal/client/service.go`). |
| Rejection and replay rules | Reject oversized values, duplicate check IDs within one report, revoked or cross-workspace devices, cross-device/workspace reuse of a report ID, and reports too old/too far in the future. Replaying the same collection-cycle report ID for the same verified device/workspace is idempotent. **Unknown `check_id`s are ignored per-check, not treated as a whole-report rejection**; reject only when no recognized valid check remains. |
| Rollout | All profiles default to **audit mode**: evaluation runs and is recorded, but audit-mode profiles are **entirely ignored for authorization** — they never grant *or* deny access. |
| ACL gate (corrected) | `CompileACLSnapshot` (`controller/internal/policy/compiler.go`) includes a device SPIFFE ID only if group access succeeds **and**: if the resource has **no enforce-mode bound profile**, identity-only (unchanged, audit-mode profiles present or not don't matter); if it has **one or more enforce-mode bound profiles**, the device must satisfy **at least one of them** (OR across enforce profiles only). The original draft's "audit mode never removes access, OR satisfies an enforce profile" wording was a bug — a failing audit profile alone would grant access even when every enforce profile also failed. Audit profiles must never appear in the authorization OR-set at all, only in recorded evaluations. |
| Staleness must actively expire the **cached ACL**, not just the compile function | A compile-time freshness check inside `CompileACLSnapshot` is **dead code once a snapshot is cached** — `GetOrCompile` (`controller/internal/policy/cache.go:96-98`) returns a cache hit with zero freshness check and never re-enters `CompileACLSnapshot` until an explicit `Invalidate()`. Nothing invalidates on the clock alone today. Fix: bake `snapshot_valid_until` (see the corrected OR-aware formula below) into the **cached** snapshot at compile time; `GetOrCompile` checks `now() > cached.valid_until` on every read and recompiles if so. The connector's existing 15s health heartbeat (`control_stream.rs` → `handleConnectorHealth`, `controller/internal/connector/control_stream.go:645`) unconditionally calls `pushACLSnapshot` → `GetOrCompile` on every tick for any workspace with a connected connector, so this read-path check gets exercised roughly every 15s "for free" — **but recompiling alone is not sufficient, see the version-bump fix immediately below.** |
| **Expiry-triggered recompiles are silently dropped without a version bump (release blocker, confirmed)** | `pushACLSnapshot` (`controller/internal/connector/control_stream.go:718`) compares the connector's last-acknowledged version against `Notifier.Version(workspace_id)` (`controller/internal/policy/notifier.go:74-81`) and **skips sending if they're equal**. `Notifier.Version()` only changes inside `NotifyPolicyChange` — a cache-expiry-triggered recompile inside `GetOrCompile` produces a snapshot with **different `Entries`** (a device's SPIFFE now excluded) but the **same `Version` number**, so `pushACLSnapshot` would silently never deliver the corrected snapshot to the connector. This is not solved by recompiling correctly; the version number is the actual signal `pushACLSnapshot` acts on. Fix: when `GetOrCompile` detects `now() > cached.valid_until`, it must call `NotifyPolicyChange(workspace_id)` **as part of handling the expiry** (bumping the version through the exact same mechanism every other policy mutation uses — not a special-cased "force delivery" flag in `pushACLSnapshot`, which would weaken the version contract instead of using it correctly) before recomputing and re-caching. `NotifyPolicyChange` invalidates the cache entry as a side effect; `GetOrCompile` then treats this as an ordinary cache miss, recompiles, and stores the new snapshot under the now-bumped version — so `pushACLSnapshot`'s next comparison correctly sees a difference and delivers it. No separate background scheduler is needed: this all happens synchronously inside the same `GetOrCompile` call the 15s heartbeat already triggers. |
| Cache-expiry formula must respect OR semantics, not a blanket minimum | `snapshot_valid_until = min(received_at)` across **every** evaluation touching a workspace is too conservative — it would count evaluations that don't currently justify any allowed `(device,resource)` pair (e.g. a failing enforce profile whose OR-partner is what's actually granting access), causing unnecessary early recompiles. Correct formula: for each currently-**allowed** `(device, resource)` pair, `pair_valid_until = max(received_at + 10min)` across only the **enforce-mode profiles that are actually satisfying** that pair (the OR-winners); then `snapshot_valid_until = min(pair_valid_until)` across all allowed pairs in the workspace. This is the tightest correct bound — using a flat minimum over all evaluations (including non-satisfying ones) would produce an already-expired cache entry on write in some cases. |
| Batch evaluation query (no N+1) | `CompileACLSnapshot` already follows a bulk-fetch-then-loop pattern (`ListEnabledRulesWithResources` once, then loops in-memory). `EvaluationsForDevice` must **not** be called per-device/per-resource inside that loop — add a batch method (e.g. `EvaluationsForDevices(ctx, deviceIDs) map[deviceID][]Evaluation`) fetched once per compile pass. |
| Policy-change triggers | `NotifyPolicyChange` must fire on **every** posture-relevant mutation — profile create, mode change (audit↔enforce), requirement add/remove, resource binding/unbinding, profile deletion — **not only** on evaluation-satisfied transitions. A profile flipped to enforce with no evaluation change yet must still push a new snapshot. |
| GraphQL admin authz | Device Profile mutations require **ADMIN** role and must enforce workspace ownership on every create/update/delete/bind/unbind — same pattern as existing resource/policy admin mutations. |
| **PENDING-09 Option B, bounded variant** | No new revocation RPC. Connector diffs `(spiffe_id, resource_id)` between the previous and newly-applied ACL snapshot in the existing `control_stream.rs` `AclSnapshot` handler, and cancels tunnels for pairs that dropped out. |
| **Teardown scope correction** | Kill scope is **`(spiffe_id, resource_id)`, never "all connections for a SPIFFE."** ACL entries are keyed per-resource (`allowed_spiffe_ids` per `AclEntry`) — a device can be authorized for resource A and lose resource B in the same snapshot; a device-wide kill would wrongly drop the still-valid session. |
| **Registration mechanism (corrected)** | The original "thread the `JoinHandle` into `handle_stream`" design is **structurally circular** — `tokio::spawn`'s `JoinHandle` is only available in the *outer* scope, but `handle_stream` runs *inside* the spawned future itself; a future cannot be handed a handle to itself. Fix: create a `CancellationToken` **before** `tokio::spawn`, clone it into the future, and race it inside a `tokio::select!` around the actual I/O (`copy_bidirectional`/`relay_udp`/relay `select!`). Register `(spiffe_id, resource_id) → token.clone()` in the registry **after** ACL resolution succeeds inside `handle_stream`, and use a drop guard to unregister on every exit path (normal close, error, or cancellation) so the registry never leaks stale entries. Requires adding `dashmap` and `tokio-util` to `connector/Cargo.toml` — **neither is currently a dependency**. |
| **Registry key collision (corrected)** | `DashMap<(spiffe,resource), Vec<CancellationToken>>` is unsafe on its own: if two or more tunnels share the same `(spiffe,resource)` pair (a device can legitimately open more than one tunnel to the same resource) and one closes, a drop guard that removes the whole outer key by `(spiffe,resource)` would delete every other still-live tunnel's token for that pair too — they'd become uncancellable, silently. Fix: nest a per-session map — `DashMap<(spiffe,resource), HashMap<SessionId, CancellationToken>>`, where `SessionId` is a fresh UUID generated at registration. The drop guard removes only its own `SessionId` entry from the inner map, and removes the outer key only when the inner map becomes empty. |
| **Authorization/registration race (corrected)** | A tunnel can authorize against ACL version N, and register in the registry *after* a diff-and-cancel pass for version N+1 has already finished scanning — such a tunnel would never appear in that scan and could survive indefinitely on stale authorization. **The fix must not gate re-verification on "did the version change"** — policy versions are currently process-local (a controller restart resets them without necessarily changing observable content), so a version-equality check can miss a real content change. Instead: after registering, **unconditionally** re-run the same `is_allowed(resource, spiffe)` check against whatever the *current* policy cache holds (no version comparison in the control-flow at all — version numbers may still be logged for diagnostics, but must never decide whether the re-check runs). Cancel immediately if that re-check fails. |
| **Relay child-task cancellation (confirmed bug, not just unverified)** | `RelaySession::relay_stream()` (`connector/src/agent_tunnel.rs`) spawns an independent `d2s` child task and only calls `d2s.abort()` as the **last line**, after its own event loop exits *normally*. If the outer tunnel task is cancelled externally (exactly what the registry's cancellation does), that line never runs and `d2s` is orphaned — a real structured-concurrency leak, confirmed by reading the function, not a hypothetical. Fix: share one `CancellationToken` between the outer task and `d2s` (or run both directions in the same `select!` scope) so cancelling the outer task deterministically tears down `d2s` too. |
| **QUIC path coverage** | `connector/src/quic_listener.rs` independently spawns its own tasks that also call `handle_stream` (a second registration site, separate from `device_tunnel.rs`'s `listen()`). Both the TCP and QUIC accept loops must use the identical `CancellationToken`-based registration/teardown path — this was missing from the original design entirely. |
| Session query hardening | Connector active-session registry (keyed by `(spiffe_id, resource_id)` → `HashMap<SessionId, CancellationToken>`, nested to avoid the key-collision bug above) also directly satisfies PENDING-08's own §3 ask ("Connector active-session registry... cancel streams no longer authorized after every ACL replacement") — one build serves both pendings, across TCP, QUIC, and relay-routed tunnels. |
| No kill-switch scope creep | This sprint does **not** build a separate push/event revocation channel beyond what already exists (`NotifyPolicyChange` + 15s reconciliation fallback) — only the ACL-diff-and-cancel logic on top of it. A sub-second dedicated kill channel is future work if this bound proves insufficient. |
| Metrics framework gap | The connector has **no existing Rust metrics framework** today. M3's observability phase must either explicitly add one (e.g. `metrics`/`prometheus` crate) as a scoped sub-task, or be limited to structured `tracing` logs only for this sprint — do not assume a metrics crate is already wired in. |
| Workspace referential integrity | Directly workspace-queried posture tables (reports, profiles, bindings, evaluations) declare `workspace_id UUID NOT NULL REFERENCES workspaces(id)`. Observations and requirements inherit tenant scope through their parent FK and intentionally do not duplicate the column. |
| Device-delete vs. history preservation (corrected) | A plain FK from `device_posture_reports.device_id` to `client_devices(id)` with no `ON DELETE` clause defaults to Postgres `RESTRICT` — it **blocks** deleting a device with report history, it does not "delete while preserving history" as an earlier draft claimed. If devices are meant to be hard-deletable while keeping posture history, the FK needs `ON DELETE SET NULL` (nullable `device_id`) or `client_devices` needs to stay soft-delete-only (consistent with `revoked_at`, which is already the existing pattern for devices — prefer this: don't hard-delete `client_devices` rows at all, matching how relays are handled). |
| Rolling-upgrade compatibility for unknown checks | Rejecting an **entire report** for one unrecognized `check_id` breaks rolling upgrades: a newer client adds a check the controller doesn't know yet → the whole report (including all its still-recognized checks) is rejected → those known checks go stale → enforce-mode access gets revoked as a side effect of an upgrade, not an actual posture regression. Fix: ingestion **ignores unrecognized check IDs** for evaluation purposes (they're simply not persisted as observations, or persisted as inert telemetry) while still accepting and evaluating every check ID it *does* recognize from the same report. Profile requirements may only ever reference server-registered check IDs (enforced at profile-requirement-creation time in M1-E), so an unrecognized check can never silently become "required" — it's purely a forward-compatibility no-op until the controller is upgraded to know it. |
| Empty enforce profile is a vacuous-truth trap | A profile with **zero requirements**, evaluated with ordinary "all requirements pass" (AND) semantics, is vacuously satisfied by every device — binding an empty profile in enforce mode to a resource would silently grant every device with group access, defeating the entire posture gate. Fix: reject (at the GraphQL mutation layer) any attempt to switch a profile to `enforce` mode, or to bind an enforce-mode profile to a resource, while it has zero requirements. |
| Raw report retention | One report every 5 minutes is 288/device/day with no bound today — unindexed, unbounded growth. Define retention explicitly: raw `device_posture_reports`/`device_posture_observations` retained **30 days**, deleted via a scheduled batched job keyed off an index on `received_at` (small batches, not one giant `DELETE`); `device_profile_evaluations` (the latest-evaluation cache) is retained as long as the device/profile rows exist — it's derived state, not raw history, so it isn't subject to the same retention clock. |
| OS version is visibility-only in v1, not enforceable | `device_profile_requirements` (v1 schema) stores only `check_id` + `allow_unsupported` — there is no operator/expected-value pair, so `linux.os.version`'s reported value can be **displayed** in posture visibility but **cannot gate access** on a minimum version in this sprint. Do not describe OS-version posture as "enforced" anywhere in rollout communication; it's PASS/FAIL only in the trivial sense of "did the collector run," not "is the version acceptable." A minimum-version requirement is future scope (needs an `operator`/`expected_value` column pair on `device_profile_requirements`). |
| Retention deletion is blocked without a nullable FK (confirmed) | `device_profile_evaluations.report_id UUID REFERENCES device_posture_reports(id)` has no `ON DELETE` clause — the 30-day retention job cannot delete a `device_posture_reports` row still referenced as the "latest evaluation's source report," `RESTRICT` blocks it. Fix: `report_id UUID REFERENCES device_posture_reports(id) ON DELETE SET NULL`. A `NULL` `report_id` on an evaluation must be treated as **stale/unsatisfied** at read time (there's no source report left to check freshness against, so it can't be trusted as current) — this is a small addition to the freshness check already being built for the cache-expiry fix, not a new code path. |
| Removing the last requirement can reintroduce the empty-profile bug | The empty-profile guard only covers `updateDeviceProfileMode(ENFORCE)` and `bindResourceToProfile` — it does **not** stop `removeProfileRequirement` from taking an already-enforced profile down to zero requirements (enforce profile with 1 requirement → remove it → vacuously satisfied by everyone, silently, no mutation-time signal that anything dangerous just happened). Fix: `RemoveRequirement` must check transactionally whether the profile is currently `enforce` mode and this is its last requirement, and **reject** the removal in that case (the operator must switch the profile to `audit` first, an explicit and visible step, rather than the store silently auto-downgrading behind their back). |
| `ReportDevicePosture` package placement (confirmed structural bug) | The generated gRPC server interface requires this RPC to be a method on `*client.Service` (`controller/internal/client/service.go:44`, which embeds `clientv1.UnimplementedClientServiceServer` and is registered via `RegisterClientServiceServer` in `cmd/server/main.go:404`) — a method defined in a different package (`internal/posture`) cannot satisfy that interface. Fix: the RPC method itself lives in `controller/internal/client/posture.go` as `(*Service).ReportDevicePosture(...)`, calling into `internal/posture`'s validation/store/evaluator functions (which stay exactly where designed — only the RPC entry point moves). `client.Service` needs a new field for the injected posture store/evaluator, wired in `cmd/server/main.go` alongside its other dependencies. |
| GraphQL resolver wiring is a real dependency, not automatic | Adding `posture.graphqls` requires more than the schema file: `resolvers.Resolver` (`controller/graph/resolvers/resolver.go:17-30`) is a flat struct of injected dependencies constructed once in `cmd/server/main.go:190-204` — the generated posture resolvers need a `PostureStore`/evaluator field added there and wired at construction time, exactly like every other resolver dependency. `go generate ./graph/...` produces the resolver *stubs*; it does not wire their dependencies. |
| Cache entry needs a concrete wrapper type | `SnapshotCache.entries` is currently `map[string]*clientv1.ACLSnapshot` (`cache.go:21`) — a bare snapshot, no expiry metadata. The fix needs a real `cacheEntry{snapshot *clientv1.ACLSnapshot, validUntil time.Time}` wrapper (`validUntil` zero-value = no posture-driven expiry, e.g. a workspace with no enforce profiles), with `map[string]*cacheEntry` replacing the bare-snapshot map. **Note there is already a separate `epoch` counter** in this cache used as a CAS race-guard, unrelated to `Notifier.versions` (the value `pushACLSnapshot` compares) — keep these two counters conceptually distinct; the expiry fix does not touch `epoch`. Also specify: a compile failure discovered while handling an expired entry must not fall back to returning the (now known-stale) expired snapshot — propagate the error instead. Use an injectable clock (not bare `time.Now()`) so expiry logic is deterministically testable. |
| Retention job must be decision-complete, not just "a scheduled job" | Define concretely: a controller background worker started from `cmd/server/main.go` (not ad hoc), daily cadence, retention window configurable with a 30-day default, a fixed batch size per delete iteration with an explicit loop-termination condition (not an unbounded loop), and `context.Context` cancellation wired to server shutdown so it doesn't leak past process lifetime. Add a test proving a report still referenced by a `device_profile_evaluations` row (via the now-nullable `report_id`) does not block its own or other rows' cleanup. |
| Workspace-scoping claim corrected | Not every posture table carries its own `workspace_id` FK — `device_posture_observations` and `device_profile_requirements` intentionally do **not** (they inherit tenant scoping transitively through `report_id`→`device_posture_reports.workspace_id` and `profile_id`→`device_profiles.workspace_id` respectively). This is fine as an intentional design (every *directly queried* table — reports, profiles, bindings, evaluations — does carry the FK), but earlier phrasing overstated it as "every table" without that caveat; don't repeat that overstatement in implementation or docs. |
| Frontend check-ID registry should be server-driven, not a hardcoded UI list | The Admin UI's requirements editor needs a list of valid check IDs to offer as options. A fixed list hardcoded in the frontend would drift from the server-side registry the moment a new check is added server-side, requiring a synchronized frontend release just to expose it. No existing GraphQL query in this codebase already provides a "supported/available X" capability list (confirmed — this is a new pattern, not a gap in following one), but it's the right shape here: add `supportedPostureChecks: [PostureCheckDescriptor!]!` (`id`, `label`, `platform`, whether `allowUnsupported` is meaningful for it) to `posture.graphqls`, and have the requirements editor populate its options from this query instead of a hardcoded list. |
| Compiler/cache expiry contract | `CompileACLSnapshot` returns an internal `CompiledACL{Snapshot, ValidUntil}` because only the compiler has the profile/evaluation data needed for the OR-aware expiry calculation. The compile closures in ClientService, connector control/heartbeat, and ACLPusher return that internal result to `GetOrCompile`; the cache stores `cacheEntry{snapshot, validUntil}` and still returns only `*clientv1.ACLSnapshot`, so downstream caller logic is unchanged. Direct compiler and cache tests/mocks adopt the internal type. |
| Cache/notifier wiring | Avoid a cache↔notifier construction cycle: `SnapshotCache.RegisterExpiryNotifier(func(workspaceID string) error)` is called once after both objects are constructed. Expiry handling releases all cache locks before invoking it because `NotifyPolicyChange` calls `cache.Invalidate()`. Missing registration fails closed. The per-workspace singleflight closure rechecks/claims expiry and performs exactly one notification/version bump plus one compile. |
| Evaluation revision safety | `device_profiles.revision` increments transactionally whenever requirements change; `device_profile_evaluations.profile_revision` records the definition used. The compiler accepts an evaluation only when revisions match. Requirement mutation commits the new revision and notifies immediately, so old evaluations fail closed while re-evaluation catches up. |
| Concrete ingestion/retention defaults | Reports allow at most 32 checks; detail is at most 256 UTF-8 bytes; client/OS strings are at most 64 bytes; `reported_at` may be at most 10 minutes old or 5 minutes in the future; report IDs are canonical UUIDs generated once per collection cycle. Retention uses `POSTURE_RETENTION_DAYS=30` and `POSTURE_RETENTION_BATCH_SIZE=2000`. |

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Go (Controller) | Migration `030_device_posture.sql`, report ingestion + validation, profile evaluation engine, GraphQL admin (Device Profile CRUD, bindings, audit/enforce), ACL compiler hook, policy-version bump + cache invalidation on evaluation change |
| **M2** | Rust (Client) | Linux posture collectors, `DevicePostureReport` proto + `ReportDevicePosture` RPC, daemon collection scheduler (startup + 5-min interval, per-collector timeouts, partial-failure tolerant) |
| **M3** | Rust (Connector) | Active-session registry keyed by `(spiffe_id, resource_id)`, ACL-snapshot diff-and-cancel teardown (TCP + QUIC + relay child task), posture-aware tunnel auth path, structured logging (+ metrics only if a framework is explicitly added) for report accept/reject and session termination |
| **M4** | Frontend (Admin UI) | Device Profile administration screens (create/edit profile, requirements editor, resource-binding picker, audit/enforce toggle, per-device posture visibility) — GraphQL alone is not usable admin surface |

## Critical Rule: Conflict Zones

| File | Who | Rule |
|------|-----|------|
| `controller/migrations/030_device_posture.sql` | **M1 only** | 029 was taken by connector revocation before implementation |
| `controller/internal/posture/*` (new package) | **M1 only** | validation, store, evaluation engine — **no gRPC method lives here** |
| `controller/internal/client/service.go` / new `controller/internal/client/posture.go` | **M1 only** | `(*Service).ReportDevicePosture` must live here — `client.Service` is the only type that satisfies the generated gRPC interface; a method in `internal/posture` cannot |
| `controller/internal/policy/compiler.go` | **M1 only** | posture filter + `CompiledACL{Snapshot, ValidUntil}` return contract |
| `controller/internal/policy/cache.go` | **M1 only** | cache entry metadata, expiry-notifier callback, injectable clock, singleflight expiry claim |
| `controller/graph/posture.graphqls` (new) | **M1 only** | Device Profile CRUD schema, mirrors `resource.graphqls`/`policy.graphqls`; add `supportedPostureChecks` query |
| `controller/graph/resolvers/resolver.go` | **M1 only** | add `PostureStore`/evaluator field — required for generated resolvers to have anything to call |
| `cmd/server/main.go` | **M1 only, shared file — be surgical** | wire posture store/evaluator into both `client.Service` and `resolvers.Resolver`; add the retention worker's startup + shutdown wiring |
| `proto/client/v1/client.proto` | **M2 drafts, M1 reviews** | `ReportDevicePostureRequest{access_token, device_id, report}` + `ReportDevicePostureResponse` + `DevicePostureReport`/`PostureCheck` messages. **`buf generate` only regenerates the Go stubs** (`buf.gen.yaml` has no Rust plugin) — Rust stubs come from `client/build.rs` (`tonic_build`), regenerated automatically on `cargo build`. Run both after merging; land this first and rebase M1/M3 on it. |
| `client/src/*` (new `posture.rs` + `daemon.rs` scheduler hook) | **M2 only** | collectors + scheduling |
| `connector/src/device_tunnel.rs` | **M3 only** | `CancellationToken` created pre-spawn, registry insert after ACL resolution in `handle_stream`, drop-guard unregister |
| `connector/src/quic_listener.rs` | **M3 only** | same `CancellationToken`-based registration path as `device_tunnel.rs` — both accept loops must be identical |
| `connector/src/agent_tunnel.rs` | **M3 only** | `RelaySession::relay_stream()` — share the cancellation token with the `d2s` child task (or unify into one `select!`) |
| `connector/src/control_stream.rs` | **M3 only** | diff-and-cancel in the `AclSnapshot` match arm; authorization/registration race fix |
| `connector/src/policy/mod.rs` | **M3 only** | snapshot-diff helper (flatten `(spiffe,resource)` allow-set before overwrite) |
| `admin/src/**` (Device Profile screens) | **M4 only** | new admin pages/components, `npm run codegen` after M1's schema lands |

No Go/Rust file overlaps beyond the shared proto file, which is sequenced (M2 lands it first). M4 depends only on M1's GraphQL schema (Phase E), not on M2/M3.

## Dependency Graph

```text
M2-A Proto: ReportDevicePostureRequest/Response  (Day 1, independent — land first;
     + DevicePostureReport/PostureCheck messages   buf generate = Go stubs, cargo build = Rust stubs)
   ↓                                              ↓
M2-B Linux collectors + normalization        M1-A Migration 030 + posture store (Day 1, independent)
   ↓                                              ↓
M2-C Daemon scheduler (startup+5min,         M1-B Report ingestion (access_token+device_id auth,
     handles not-logged-in, per-collector         batch-safe) + validation (needs M2-A message shape)
     isolation for panics)                        ↓
                                              M1-C Profile evaluation engine (AND-within, OR-across
                                                   enforce profiles only; freshness computed from
                                                   received_at, not a cached bool)
                                                   ↓
                                              M1-D GraphQL admin (ADMIN-only, workspace-scoped CRUD;
                                                   audit/enforce toggle fires NotifyPolicyChange too)
                                                   ↓
                                              M1-E ACL compiler hook (batch evaluation query, no N+1;
                                                   posture-gated SPIFFE inclusion, enforce-only OR;
                                                   cache-expiry + version-bump fix)
                                                   ↓
                                              M1-F0 Retention worker (needs Phase C's ON DELETE SET NULL)

M3-A Active-session registry, CancellationToken-based,   (Day 1, independent)
     covers TCP (device_tunnel.rs) + QUIC (quic_listener.rs)
   ↓
M3-B ACL-snapshot diff-and-cancel teardown +             (needs M3-A; independent of M1/M2 — works for
     authorization/registration race fix +                ANY ACL change, not just posture)
     relay d2s child-task shared-cancellation fix
   ↓
M3-C Posture-aware tunnel auth + structured logs/metrics  (needs M1-E for the ACL to actually carry posture)

M4-A Admin UI: Device Profile screens                     (needs M1-D's GraphQL schema; independent of M2/M3)
```

> Day-1 parallel starts: **M2-A** (proto), **M1-A** (migration), and **M3-A** (registry) have no dependencies.
> **M3-B lands value independently of M1/M2** — it hardens revocation for existing group/device-revoke
> cases even before posture exists, so it should not be blocked waiting on the Go/client tracks.
> **M4-A only needs M1-D**, not M2/M3 — can start as soon as the GraphQL schema is stable.

## Execution Path

### Phase A — M2: Proto + Collectors
> See [[Sprint15/Member2-Rust-Client/Phase1-Proto-and-Linux-Collectors]]. Depends on nothing — Day 1.
- [ ] **M2-A1** `proto/client/v1/client.proto` — `ReportDevicePostureRequest{access_token, device_id, report}`, `ReportDevicePostureResponse`, `DevicePostureReport`/`PostureCheck` messages (unix-epoch `int64` timestamps, no `google.protobuf.Timestamp`/`prost-types` dependency).
- [ ] **M2-A2** `buf generate` from repo root regenerates the **Go** stubs only; `cargo build` in `client/` regenerates the Rust stubs via `client/build.rs`. Commit both.
- [ ] **M2-A3** `client/src/posture.rs` (new) — Linux collectors: OS/version, LUKS, firewall, Secure Boot (does **not** require a TPM); normalize to `PASS/FAIL/UNSUPPORTED/UNKNOWN/ERROR` against registered check IDs; per-collector timeout **and** panic isolation (run each collector in its own task, handle `JoinError` as `ERROR` — a bare timeout does not catch a panic).
- [ ] **Build gate:** `cd client && cargo build`

### Phase B — M2: Daemon Collection Scheduler
> See [[Sprint15/Member2-Rust-Client/Phase2-Daemon-Collection-Scheduler]]. Depends on Phase A.
- [ ] **M2-B1** `client/src/daemon.rs` — run collection at startup and every 5 minutes; if not logged in yet at startup, defer and trigger immediately after successful login instead of silently skipping; one failed collector must not block submission of the rest.
- [ ] **M2-B2** Submit `ReportDevicePostureRequest{access_token, device_id, report}` over the existing refreshed access token path (reuse `fetch_acl_snapshot_with_refresh`'s auth-attach pattern).
- [ ] **Build gate:** `cd client && cargo build && cargo test`

### Phase C — M1: Migration + Posture Store
> See [[Sprint15/Member1-Go/Phase1-Migration-and-Posture-Store]]. Depends on nothing — Day 1.
- [x] **M1-C1** `controller/migrations/030_device_posture.sql` — correct device/workspace FKs, report/observation uniqueness, retention index, nullable evaluation report FK, and profile/evaluation revision columns. Phase C owns schema only; Phase F0 exclusively owns cleanup execution.
- [x] **M1-C2** `controller/internal/posture/store.go` — insert report/observations, workspace-scoped profile CRUD, atomic requirement+revision changes, last-requirement guard, revision-bearing evaluation upsert/read, and batch `EvaluationsForDevices`.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase D — M1: Report Ingestion + Evaluation Engine
> See [[Sprint15/Member1-Go/Phase2-Ingestion-and-Evaluation]]. Depends on Phase C + M2-A (message shape).
- [x] **M1-D1** `(*client.Service).ReportDevicePosture` in `controller/internal/client/posture.go` — device ownership auth; canonical UUID; ≤32 checks; ≤256-byte detail; ≤64-byte client/OS fields; timestamp between now−10m and now+5m; unknown/oversized checks filtered individually; zero valid recognized checks rejected; same-device duplicate report acknowledged idempotently and cross-device duplicate rejected.
- [x] **M1-D2** Evaluation engine — AND within profiles, enforce-only OR across profiles, fail-closed states/freshness, and revision-bearing results. Requirement changes commit a new revision and notify before re-evaluation so preceding-revision results cannot authorize.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase E — M1: GraphQL Admin + ACL Compiler Hook
> See [[Sprint15/Member1-Go/Phase3-GraphQL-Admin-and-ACL-Hook]]. Depends on Phase D.
- [ ] **M1-E1** `controller/graph/posture.graphqls` (new, mirrors `resource.graphqls`) — Device Profile CRUD, requirements, resource bindings, audit/enforce toggle, device posture visibility (failure reason, observation time, report age, collector error — never raw command output), `supportedPostureChecks` query (server-driven check-ID list, not a hardcoded frontend list). All mutations require **ADMIN** role + workspace ownership check. **Reject switching a profile to `enforce` mode, binding an enforce-mode profile to a resource, or removing a requirement that would leave an already-enforced profile with zero requirements** — the empty-profile guard must cover all three paths, not just mode-switch and bind.
- [x] **M1-E1b** `controller/graph/resolvers/resolver.go` + `cmd/server/main.go` — add `PostureStore`/evaluator field to `resolvers.Resolver` and wire construction; the generated posture resolvers have nothing to call without this.
- [x] **M1-E2** `cd controller && go generate ./graph/...`
- [ ] **M1-E3** `CompileACLSnapshot` returns `CompiledACL{Snapshot, ValidUntil}`, filters on matching profile revisions with a batch query, and computes OR-aware expiry from posture-gated pairs only. Update the compile closures in ClientService, connector control/heartbeat, and ACLPusher plus direct compiler/cache tests; `GetOrCompile` unwraps the result and keeps returning `*clientv1.ACLSnapshot` downstream.
- [ ] **M1-E3b** `SnapshotCache` stores `cacheEntry`, uses an injectable clock, and exposes one-time `RegisterExpiryNotifier` wiring. A per-workspace singleflight closure rechecks/claims expiry, releases locks, notifies/version-bumps exactly once, compiles exactly once, and fails closed on missing notifier or compile error.
- [x] **M1-E4** Every posture-relevant mutation (create/mode-change/requirement/binding/delete) **and** every evaluation transition bumps workspace policy version, invalidates ACL cache, pushes new snapshot (reuse existing `NotifyPolicyChange(workspace_id)` path).

### Phase F0 — M1: Retention Worker
> See [[Sprint15/Member1-Go/Phase4-Retention-Worker]]. Depends on Phase C's `ON DELETE SET NULL` fix.
- [ ] **M1-F0** Controller worker with `POSTURE_RETENTION_DAYS=30`, `POSTURE_RETENTION_BATCH_SIZE=2000`, daily cadence, and bounded batches. `main.go` adds `signal.NotifyContext`, waits for worker exit, and gracefully stops HTTP/gRPC with a 10-second bound.
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/...`

### Phase E2 — M4: Admin UI
> See [[Sprint15/Member4-Frontend/Phase1-Device-Profile-Admin-UI]]. Depends on Phase E's GraphQL schema.
- [ ] **M4-A1** Device Profile list/create/edit screens, requirements editor, resource-binding picker, audit/enforce toggle, per-device posture visibility.
- [ ] **M4-A2** `App.tsx` routes + sidebar/nav entry + permission-based visibility — pages are **unreachable without this**; confirmed every existing page (`Resources.tsx` etc.) requires an explicit `<Route>` (`App.tsx:85-87`).
- [ ] **M4-A3** Component tests for the new screens.
- [ ] **Build gate:** `cd admin && npm run codegen && npm run build`

### Phase F — M3: Active-Session Registry
> See [[Sprint15/Member3-Rust-Connector/Phase1-Active-Session-Registry]]. Depends on nothing — Day 1.
- [x] **M3-F0** `connector/Cargo.toml` — add `dashmap` and `tokio-util` (neither is a dependency today).
- [ ] **M3-F1** `connector/src/device_tunnel.rs` — shared `DashMap<(SpiffeId, ResourceId), HashMap<SessionId, CancellationToken>>` (nested per-session map, **not** `Vec` — a bare `Vec`/single-level map keyed only by `(spiffe,resource)` would let one tunnel's cleanup remove a sibling tunnel's still-live token when two sessions share the same pair). Create the token **before** `tokio::spawn`, clone it into the future, register `(spiffe,resource) → {session_id: token}` **after** ACL resolution succeeds inside `handle_stream`, drop-guard removes only its own `session_id`, removing the outer key only when the inner map is empty.
- [x] **M3-F2** `connector/src/quic_listener.rs` — identical registration path for QUIC-accepted streams (also calls `handle_stream`).
- [x] **Build gate:** `cd connector && cargo build`

### Phase G — M3: ACL-Diff Teardown
> See [[Sprint15/Member3-Rust-Connector/Phase2-ACL-Diff-Teardown]]. Depends on Phase F.
- [ ] **M3-G1** `connector/src/policy/mod.rs` — before `policy_cache.update(snap)` overwrites it, flatten the *previous* snapshot into a `HashSet<(spiffe_id, resource_id)>`.
- [ ] **M3-G2** `connector/src/control_stream.rs` (`AclSnapshot` match arm) — diff old vs. new; for every `(spiffe,resource)` pair present in old but missing in new, `.cancel()` every token in that pair's inner session map. Scope is **per-pair, never device-wide**.
- [ ] **M3-G3** Fix the authorization/registration race: after registering, **unconditionally** re-run `is_allowed(resource, spiffe)` against the current policy cache — no version-equality gate (versions are process-local and can miss real content changes across a controller restart) — and cancel on the spot if it fails.
- [ ] **M3-G4** `connector/src/agent_tunnel.rs` — `RelaySession::relay_stream()`'s `d2s` child task must share the same `CancellationToken` as the outer task (or be unified into one `select!`), so external cancellation doesn't orphan it — this was a confirmed leak, not a hypothetical.
- [ ] **Build gate:** `cd connector && cargo build`

### Phase H — M3: Posture-Aware Auth Path + Observability
> See [[Sprint15/Member3-Rust-Connector/Phase3-Posture-Aware-Auth-and-Metrics]]. Depends on Phase G + M1-E (ACL carries posture).
- [ ] **M3-H1** Confirm tunnel auth at connect time naturally inherits posture gating for free (ACL already excludes non-compliant SPIFFE in enforce mode) — no separate posture check needed connector-side.
- [ ] **M3-H2** Structured logs (report accept/reject visibility is mostly M1's concern; connector logs the consequences it sees): registry size, cancellations fired (labeled direct-TCP/QUIC/relay-routed), stale devices, ACL invalidations. Add a metrics crate only if explicitly scoped — none exists in the connector today.
- [ ] **Build gate:** `cd connector && cargo build && cargo test`

## Final Build Gates
```bash
cd controller && go build ./... && go test ./internal/...
cd client && cargo build && cargo test
cd connector && cargo build && cargo test
cd admin && npm run codegen && npm run build
buf generate   # from repo root, after M2-A lands — regenerates Go stubs only; Rust stubs regenerate via cargo build
```

## Acceptance Criteria
- [ ] Linux collectors return normalized `PASS/FAIL/UNSUPPORTED/UNKNOWN/ERROR` against registered check IDs; one failed or **panicking** collector doesn't block report submission (isolated task + `JoinError` handling, not just a timeout).
- [ ] `030_device_posture.sql` references `client_devices(id)` and `workspaces(id)`, enforces tenant-scoped and `(report_id, check_id)` uniqueness, indexes `received_at`, preserves nullable evaluation provenance with `ON DELETE SET NULL`, and includes `device_profiles.revision` plus `device_profile_evaluations.profile_revision`; Phase F0 alone owns retention execution.
- [ ] `(*client.Service).ReportDevicePosture` lives in `controller/internal/client/posture.go` — **not** `internal/posture`, since the generated gRPC interface can only be satisfied by a method on `client.Service`. Authenticates via `{access_token, device_id}` exactly like `GetACLSnapshot`.
- [ ] Ingestion enforces the documented concrete age, count, string, and detail limits; validates device ownership; treats same-device replay of one collection-cycle `report_id` idempotently; rejects cross-device/workspace reuse and duplicate check IDs; ignores unknown checks individually; and rejects a report when no recognized valid checks remain.
- [ ] Evaluation: AND within a profile, OR across **enforce-mode** profiles bound to a resource (audit-mode profiles never affect authorization, pass or fail); a profile cannot be switched to enforce, bound to a resource while enforced, **or have its last requirement removed while enforced** — all three paths guarded, preventing the vacuous-truth trap of an empty profile silently granting everyone; re-evaluates on report/profile/requirement/binding change.
- [ ] Requirement changes increment `device_profiles.revision`; only evaluations with a matching `profile_revision` may authorize, so re-evaluation lag is fail-closed.
- [ ] GraphQL resolvers for `posture.graphqls` have their store/evaluator dependency wired via `resolvers.Resolver` + `cmd/server/main.go` — not left unconnected after `go generate`.
- [ ] **The cached ACL snapshot itself expires**, using a `cacheEntry{snapshot, validUntil}` wrapper (not a bare `*ACLSnapshot`), with `validUntil` computed via the OR-aware formula (max over each allowed pair's satisfying enforce profiles, then min across pairs — not a flat minimum over every evaluation, which would over-expire). `GetOrCompile` recompiles once wall-clock passes it, verified sufficient via the connector's existing 15s heartbeat unconditionally re-entering `GetOrCompile`.
- [ ] **Expiry-triggered recompiles are actually delivered to the connector** — `GetOrCompile` calls `NotifyPolicyChange(workspace_id)` as part of handling an expired entry (bumping `Notifier.Version()`) before recomputing, so `pushACLSnapshot`'s version comparison (`control_stream.go:718`) sees a real difference instead of silently dropping the corrected snapshot. This was a confirmed release blocker in review — a recompile with no version bump is invisible to the push path.
- [ ] Concurrent expiry performs exactly one notification/version bump and one compile; expiry notification is invoked outside cache locks through registered callback wiring.
- [ ] All profiles default to audit mode; enforce mode is explicit per profile; profile mutations require ADMIN + workspace ownership.
- [ ] `CompileACLSnapshot` gates enforce-mode profiles only, requires evaluation/profile revision equality, uses a batch query, and returns `CompiledACL{Snapshot, ValidUntil}` with OR-aware expiry derived only from observations required by posture-gated allowed pairs; identity-only pairs are excluded.
- [ ] Every posture-relevant mutation (not only evaluation transitions) bumps policy version, invalidates ACL cache, pushes a new snapshot.
- [ ] `connector/Cargo.toml` declares `dashmap` and `tokio-util` explicitly (neither existed before this sprint).
- [ ] `supportedPostureChecks` GraphQL query exists and the Admin UI's requirements editor sources its check-ID options from it, not a hardcoded frontend list.
- [ ] Connector maintains a `(spiffe_id, resource_id) → {session_id: token}` active-session registry (nested per-session map, not a flat `Vec`) covering **TCP and QUIC** accept paths identically; a drop guard removes only its own session, never a sibling session sharing the same `(spiffe,resource)` pair; never a device-wide kill.
- [ ] Registration uses a pre-spawn `CancellationToken`, not a post-spawn `JoinHandle` (which is structurally circular).
- [ ] The authorization/registration race is closed via an **unconditional** re-check of `is_allowed` immediately after registering — not gated on ACL-version equality, since versions are process-local and can miss real content changes.
- [ ] `RelaySession::relay_stream()`'s `d2s` child task is cancelled deterministically alongside the outer task (shared token or unified `select!`) — confirmed as a real leak otherwise, not just an unverified risk.
- [ ] GraphQL admin exposes failure reason, observation time, report age, collector error — never raw command output.
- [ ] Structured logs cover collector failures, stale devices, evaluation transitions, ACL invalidations, terminated sessions (labeled by transport); a metrics crate is only added if explicitly scoped.
- [ ] Admin UI provides usable Device Profile administration **and is actually reachable** — routes wired into `App.tsx`, sidebar/nav entry present, permission-based visibility applied, component tests written. A page file with no route is not a completed phase.
- [ ] `linux.os.version` is documented as visibility-only in v1 — not described as enforced anywhere, since the requirement schema has no operator/expected-value comparison this sprint.
- [ ] `cargo test` passes for both `client` and `connector`, not just `cargo build`.

## Post-Sprint Fixes

### Fix: Posture migration renumbered to 030

**Issue:** Sprint 15 originally reserved migration 029 for posture, but
`029_connector_revocation.sql` landed before M1 implementation began.

**Fix:** The posture schema and all Sprint 15 references now use
`controller/migrations/030_device_posture.sql`. See the M1 Phase 1 Post-Phase Fixes
section for details.

### Fix: Report replay preserves evaluation convergence

**Issue:** An inserted report whose first evaluation attempt failed could be acknowledged
on retry without another evaluation attempt; ownership-query infrastructure errors could
also be masked as permission failures.

**Fix:** The report handler now re-evaluates valid same-device replays and handles
not-found, database-error, workspace-mismatch, and revoked-device outcomes separately.
See the M1 Phase 2 Post-Phase Fixes section for details.

### Fix: Empty enforce transition is transactionally rejected

**Issue:** A zero-requirement audit profile could be switched to enforce mode and become
vacuously satisfied.

**Fix:** `UpdateProfileMode` now locks the workspace-scoped profile and rejects the
transition unless at least one requirement exists. See the M1 Phase 1 Post-Phase Fixes.

### Fix: Posture GraphQL mutations fail safely and re-evaluate

**Issue:** Expected mutation errors were hidden by the production error presenter,
binding reads were N+1, and requirement changes did not immediately refresh evaluations.

**Fix:** Posture resolvers now expose only explicit safe user errors, batch-load bound
resources, and run a workspace re-evaluation after requirement changes. Schema-level
ADMIN, notification, tenant-isolation, and binding regression tests were added. See the
M1 Phase 3 Post-Phase Fixes section.

## Deferred (out of scope this sprint)
- Windows/macOS collectors (same interface, later).
- MDM/EDR or hardware-attested posture sources (PENDING-08 Options B/C).
- A true sub-second dedicated push-revocation channel — this sprint only ships ACL-diff-and-cancel on top of the existing `NotifyPolicyChange` push + 15s reconciliation fallback.
- Risk-scored step-up (PENDING-09 Option C / PENDING-06 integration).
- Requirement value operators beyond presence/absence (e.g. "OS version ≥ X") — v1 requirements are `check_id` + `allow_unsupported`; minimum-version comparison is a future requirement-schema extension.
- Promoting PENDING-08 / PENDING-09 → `ADR-0NN` (revisit after the sprint lands and the revocation latency bound is validated in practice).

## Notes for AI Agents
1. Read this `path.md`, then your first unchecked phase whose `depends_on` are satisfied.
2. **M2-A (proto) should land first** — both M1 and M3 depend on generated stubs existing, and M1/M3 file sets don't overlap with M2's, so M2 can start immediately regardless. Remember: `buf generate` only touches Go; Rust stubs come from `cargo build`.
3. **M3-F/M3-G do not depend on posture existing at all** — they hunt down mid-session teardown for any ACL change (group revoke, device revoke, posture). Do not block M3 on M1/M2 finishing; only M3-H needs M1-E.
4. Teardown scope is **always `(spiffe_id, resource_id)`** — never key or cancel by SPIFFE alone.
5. Every posture-driven access decision must **fail closed**: missing/stale/rejected reports are treated as unsatisfied, never as pass.
6. Registration is **`CancellationToken`-based, created before spawn** — do not attempt to obtain or thread a `JoinHandle` into the same task it belongs to; that construction is impossible.
7. Any tunnel accept path (TCP in `device_tunnel.rs`, QUIC in `quic_listener.rs`) must register/unregister identically — a posture or ACL fix that only covers one path is incomplete.
