---
type: planning
status: active
sprint: 17
tags:
  - sprint17
  - dependencies
  - execution-path
  - identity
  - scim
  - pending-05
  - pending-15
---

# Sprint 17 — Enterprise Directory Sync (SCIM) — PENDING-05

> **Read this before writing a single line of code.**
> Source of truth: [[ADR-025-SCIM-Directory-Synchronization]] (ACCEPTED) + the locked plan
> [[PENDING-05-SCIM-Implementation-Plan]].
> Branch: `feat/pending-05-scim` (copy of `fixed-pendings`).
> ADR-025 is the authoritative contract — **implement it, do not redesign it.**

## ⚠️ Reconciled 2026-08-18 — the durable outbox already shipped

**PENDING-15 is DONE and merged into `fixed-pendings` as Sprint 18** (33/33 phases, PRs #75/#76):
`controller/internal/outbox/*` + `migrations/033_outbox_events.sql`. The `outbox.Enqueue(ctx, tx, evt)`
API commits inside the caller's transaction — exactly the contract our deprovision path needs.

Consequences for this sprint:
- **Sprint 17 is now a solo M1 (SCIM) sprint.** M2's outbox work landed as Sprint 18 — there is no M2 lane here anymore.
- **No `UnwiredSink` interim, no "not delivered" audit-record, no gap reconciliation.** The durable path
  exists on day one; P6 wires `SideEffectSink` straight to the real `outbox.Enqueue`.
- **SCIM migration number = `034`** (next free slot after `030`/`031`/`032`/`033` on the merged tree).

## Sprint Goal

Make the workspace roster and group membership **directory-driven**, and make **offboarding
automatic**: an IdP (Okta/Entra/…) pushes Users and Groups over SCIM 2.0; deprovision **cuts Zecurity
access immediately** (sessions + policy) and **durably triggers device-trust revocation** through the
already-merged outbox.

```text
BEFORE                                      AFTER
------                                      -----
users appear only at first login (JIT);     directory provisions/updates/deprovisions users + groups;
groups hand-managed; offboarding is manual   deprovision kills sessions + policy now, and enqueues
  → stale access after someone leaves         durable device-trust revocation (outbox → PENDING-13)
```

## The one hard boundary (memorize it)

```text
PENDING-05 (this sprint) → DECIDE + ENQUEUE   (identity lifecycle; SideEffectSink.Enqueue in-tx)
PENDING-15 (Sprint 18, DONE) → DURABLE DELIVERY   (outbox: persist, claim, retry, recover)
PENDING-13 → DEVICE EXECUTION    (consume device.trust.* → cert/device revocation)
```

SCIM's only job at the boundary is to **enqueue a device-trust event inside the identity transaction**.
Persistence, retry, lease recovery, and the background processor are all provided by the merged outbox.
Everything else (provision, update, groups, conflict, connection lifecycle, and **access-cutting
deprovision via `identity.Revoker`**) is pure identity work with no outbox dependency at all.

## Key Design Decisions (locked)

| Decision | Detail |
| --- | --- |
| SideEffectSink boundary | M1 defines `identity.SideEffectSink.Enqueue(ctx, tx, DeviceTrustEvent)` and backs it with `DurableOutboxSink` → `outbox.Enqueue` (the merged Sprint 18 infra). The interface stays so `scim`/`identity` never import `outbox` directly and unit tests can inject a fake. **No interim sink, no best-effort/fake revocation** — the durable path is real from the first commit. |
| Break-glass = real permission | `identity.mapping.break_glass` is a **dedicated fine-grained permission**, NOT ADMIN. Smallest permission primitive; +mandatory reason +audit +MFA-when-PENDING-06. |
| Canonical Identity Key | Per-connection `subject_claim` (OIDC) + `scim_identifier` (SCIM) resolve to `external_identities.subject`. Never hardcode `sub==externalId`. Never merge by email. |
| Ownership | Directory owns directory attrs (rejected at the **mutation layer**, read-only in UI); Zecurity owns role/policies/manual-groups. `provisioned_by` (immutable) + `provisioning_owner` (mutable). |
| Conflict, not merge | SCIM hitting a JIT/manual identity → `409 identity_conflict` + pending record; admin Accept-Link/Reject/Reopen. No auto-takeover, never by email. |
| One engine | Provider **profiles** (Okta/Entra/JumpCloud/Keycloak/Generic) + per-connection overrides. **No per-provider handler types.** |
| Atomic tx | identity mutation + audit + `policy.Notifier` + `Revoker` + `SideEffectSink.Enqueue` commit in ONE tx; downstream failure never rolls back identity. |
| Migration number | **`034`** — next free slot after the merged `030` (device posture) / `031` (identity-federation + device-profile-manual-trust both exist) / `032` (platform-login-toggle) / `033` (outbox). Confirm no collision at branch-cut. |
| Full stack | Backend + GraphQL + React. Frontend is a coordinated hand-off (see §Frontend). |

## Ownership

| Member | Role | Area |
| --- | --- | --- |
| **M1** — you | Go (Controller) | The whole SCIM identity engine: schema, SCIM token auth, break-glass permission primitive, provider profiles + mapping validation, Users provision/update/deprovision, Groups, identity-conflict workflow, connection lifecycle + health + sync instances, the `SideEffectSink` interface, and the thin `DurableOutboxSink` adapter over the merged outbox. |
| **Outbox** | Sathiya (Sprint 18) | **Already delivered & merged** — `controller/internal/outbox/*`. Not part of this sprint. If Sathiya wants to own the `DurableOutboxSink` adapter (≈15 lines) that's a trivial coordination; otherwise M1 writes it. |

## Critical Rule: Conflict Zones

The outbox is merged, so there is no live cross-member editing this sprint. Boundaries to respect:

| File / area | Rule |
| --- | --- |
| `controller/internal/scim/**` | the SCIM engine, handlers, provider profiles, DirectoryService |
| `controller/internal/idp/store.go` | SCIM-related connection columns + queries |
| `controller/internal/identity/**` | `SideEffectSink` interface, resolver/linker reuse, permission checks |
| `controller/internal/permission/**` | break-glass permission primitive |
| `controller/graph/idp.graphqls` + `graph/resolvers/idp*.go` | SCIM admin API, token mint, conflict queue |
| `controller/migrations/034_scim.sql` | SCIM schema (number confirmed at branch-cut) |
| `controller/internal/outbox/**` | **Do not modify** — merged Sprint 18 infra; SCIM only *calls* `outbox.Enqueue`. |
| `controller/cmd/server/main.go` | wire the SCIM engine + `DurableOutboxSink` (constructed from the existing `outbox.Outbox`). |

## Dependency Graph

```text
M1-1 Schema (Day 1, independent)          [outbox already merged — no parallel infra to build]
   ↓
   ├── M1-2 SCIM token auth
   ├── M1-3 Break-glass permission primitive
   ↓
   M1-4 Provider profiles + mapping validation      (needs M1-2, M1-3)
   ↓
   M1-5 Users provision + update                    (needs M1-4)
   ↓
   ├── M1-6 Users deprovision + SideEffectSink→outbox (needs M1-5)
   ├── M1-7 Groups                                   (needs M1-5)
   ├── M1-8 Identity conflict workflow               (needs M1-5)
   └── M1-9 Connection lifecycle + health + sync     (needs M1-5)
```

> Day-1 start: **M1-1** (schema). The `SideEffectSink` shape is M1's to define; it wraps the existing
> `outbox.Event` / `outbox.Enqueue`, so there is no cross-member contract to negotiate first.

## Execution Path

### M1 — Identity / SCIM engine

#### Phase 1 — Schema  `[I]`
> See [[Sprint17/Member1-Go/Phase1-Schema]]. Depends on nothing — Day 1.
- [x] **M1-1a** migration `034_scim_directory_sync.sql`: `identity_connections` (+`subject_claim`, `scim_identifier`, `scim_enabled`, `last_sync_at`, `status`+`'deleted'`); `users` (+`provisioned_by`, `provisioning_owner`, `sync_instance_id`); `groups` (+`origin`, `external_id`); `external_identities` (+`sync_instance_id`); new `scim_tokens`, `scim_identity_conflicts` (+ unique pending index), `scim_sync_instances`.
- [x] **Build gate:** `cd controller && go build ./...`; migration applies on a fresh DB after `033`; PENDING-04 tests still green.

#### Phase 2 — SCIM token authentication  `[I]`
> See [[Sprint17/Member1-Go/Phase2-SCIM-Token-Auth]]. Depends on Phase 1.
- [x] **M1-2a** `scim_tokens` store: HMAC-SHA256 keyed by `SCIM_TOKEN_HASH_KEY`; dual-token rotation (≤2 active, 24h grace, `SCIM_TOKEN_ROTATION_GRACE_HOURS`); lookup-by-hash → bind `(workspace_id, connection_id)`; revoke + auto-expire event.
- [x] **M1-2b** Bearer middleware for `/scim/v2/*`; GraphQL mint/rotate/revoke/list (plaintext shown once).
- [x] **Build gate:** `go build ./...` + token unit tests (rotation/grace/scope).

#### Phase 3 — Break-glass permission primitive  `[I]`
> See [[Sprint17/Member1-Go/Phase3-BreakGlass-Permission]]. Depends on nothing (parallel with 1/2).
- [x] **M1-3a** minimal permission store (`workspace_permissions`) + `HasPermission`; grant/revoke (audited).
- [x] **M1-3b** register `identity.mapping.break_glass`; ADMIN does **not** implicitly hold it; MFA hook (enforced when PENDING-06 lands).
- [x] **Build gate:** `go build ./...` + tests (explicit possession; ADMIN-without-grant denied).

#### Phase 4 — Provider profiles + mapping validation  `[I]`
> See [[Sprint17/Member1-Go/Phase4-Provider-Profiles-and-Mapping]]. Depends on Phase 2, 3.
- [x] **M1-4a** built-in provider profiles + per-connection overrides (one engine, no per-provider handlers).
- [x] **M1-4b** (Phase 4 boundary) `testIdpConnection` runs the achievable active checks — OIDC discovery probe + mapping-config validation + fail-closed `MappingGate` — and the `identity.mapping.break_glass` override (reason + `scim.mapping.break_glass_override` audit). The literal probe-user round-trip `POST→GET→verify→DELETE` is **deferred to Phase 5** (the `/Users` endpoint lands there); until it runs, the gate stays `unproven` and SCIM disabled unless overridden. Phase 5 plugs into the same gate via `MappingGateResult.WithRoundTrip` without changing this contract.
- [x] **Build gate:** `go build ./...` + mapping-validation tests.

#### Phase 5 — SCIM Users: provision + update  `[I]` `[done]`
> See [[Sprint17/Member1-Go/Phase5-Users-Provision-Update]]. Depends on Phase 4.
- [x] **M1-5a** `internal/scim` engine + `identity.DirectoryService` binding `(workspace,connection)` into every query (§10).
- [x] **M1-5b** provision via `Resolver`/`Linker` (`provisioned_by=scim`, `provisioning_owner=scim`, `sync_instance_id`); update = directory-owned attrs only, Zecurity-owned rejected at mutation layer; RFC 7644 envelopes, `meta.version`, `eq` filter, tombstones hidden. `409 identity_conflict` on JIT/manual collision (conflict-row write deferred to Phase 8).
- [x] **Build gate:** `go build ./...` + provision/update DB-integration + scope-isolation tests (8/8 subtests pass on live Postgres).
- [!] **Known gap:** `users` has no `name`/`displayName`/`title`/`department` columns, so directory-owned attribute writes are scoped to existing columns (`email`, `status`, `sync_instance_id`); unsupported-attr patches return `400`. Schema extension tracked separately (ADR-025 §5).

#### Phase 6 — Users: deprovision + reactivate + SideEffectSink→outbox  `[I]` `[done]`
> See [[Sprint17/Member1-Go/Phase6-Deprovision-and-SideEffectSink]]. Depends on Phase 5.
- [x] **M1-6a** consume merged `identity.SideEffectSink { Enqueue(ctx, tx, DeviceTrustEvent) }` (from `feat/identity-device-trust-contract`, NOT redefined) + `DurableOutboxSink` that calls `outbox.Enqueue(ctx, tx, identity.NewDeviceTrustRevokeEvent(...))` / `...ReEnrollmentRequired(...)` **inside the identity tx**. No interim sink.
- [x] **M1-6b** deprovision (one tx): `active=false`→suspend / `DELETE`→soft-delete + `identity_generation` bump (`Revoker.BumpGenerationTx`) + `Revoker` session kill + audit + `policy.Notifier` + `SideEffectSink.Enqueue(device.trust.revoke.requested)`; reactivate enqueues `device.trust.re_enrollment_required` (no gen bump). Repeated DELETE idempotent; unscoped/non-scim → 409.
- [x] **Build gate:** `go build ./...` + deprovision identity-effect tests (fake sink) **and** integration test asserting the outbox row is written in the same tx, plus a forced-enqueue-error-aborts-the-whole-tx invariant (4/4 subtests pass on live Postgres).
- [!] The plan's `identity/side_effect_sink.go` file is superseded: the contract was merged as `internal/identity/device_trust.go` with `Type` folded into `outbox.EventType`. SCIM consumes it; only `side_effect_sink_outbox.go` imports `outbox`.

#### Phase 7 — SCIM Groups  `[I]`
> See [[Sprint17/Member1-Go/Phase7-Groups]]. Depends on Phase 5.
- [x] **M1-7a** `scim`-origin groups keyed on connection-scoped `external_id`; sync `group_members`; `NotifyPolicyChange`; out-of-order member → `404` (no dangling); origin-aware identifiers.
- [x] **Build gate:** `go build ./...` + group-sync tests.

#### Phase 8 — Identity conflict workflow  `[I]`
> See [[Sprint17/Member1-Go/Phase8-Identity-Conflict-Workflow]]. Depends on Phase 5.
- [x] **M1-8a** `scim_identity_conflicts`: `409 identity_conflict` on JIT/manual collision (Provision/Update/Deprovision/Reactivate); one pending per `(workspace,connection,canonical_identity_key)` (reused on retry, never duplicated — guard skips insert when any row exists so a REJECTED conflict stays rejected); consistent across all verbs; no auto-takeover. Best-effort persistence: write failure is audited (`scim.user.conflict_persist_failed`), never swallowed, and never blocks the 409.
- [x] **M1-8b** Accept-Link (atomic tx: verify pending → verify `identity.mapping.break_glass` → confirm/insert `external_identities` link → `provisioning_owner→scim` (immutable `provisioned_by` untouched, so roles/policies/devices preserved) → audit `scim.user.conflict_approved`); Reject / Reopen(→pending, audit `scim.user.conflict_reopened`) — all audited, fail-closed on invalid transitions. GraphQL admin API: `scimConflicts(connectionId)` query + `acceptScimConflict`/`rejectScimConflict`/`reopenScimConflict` mutations (boundary `@hasRole(roles:[ADMIN])`; accept enforces `identity.mapping.break_glass` server-side, ADMIN alone → 403).
- [x] **Build gate:** `go build ./...` + `go vet ./...` + `TestConflict_Integration` (12 subtests, all pass on live Postgres); full `go test ./...` green except the pre-existing/environmental `internal/permission` `ca_root` failure (unrelated to Phase 8).
- [!] **Known gap:** `scim_identity_conflicts` (migration 034) has no `resolution_reason` column, so the accept/reject/reopen `reason` is captured only in the audit `Details`, not persisted on the conflict row. Persisting reason on the row needs a follow-up migration (deferred — out of Phase 8 scope per "no migration unless schema insufficient"; flagged here).
- [!] **Schema note:** conflict terminal state is `approved` (migration 034 CHECK = pending/approved/rejected/expired), not `linked`; Phase 8 uses `approved`.

#### Phase 9 — Connection lifecycle + health + sync instances  `[I]`
> See [[Sprint17/Member1-Go/Phase9-Connection-Lifecycle-Health-Sync]]. Depends on Phase 5.
- [x] **M1-9a** connection `active→disabled→deleted`: DISABLE (reversible) suspends SCIM-owned users + revokes sessions via `Revoker.BumpGeneration` + flips `provisioning_owner scim→unmanaged` (immutable `provisioned_by` preserved); SCIM writes refused while disabled (resolveScope 403); DELETE guarded (refuse unless `force` when linked users > 0) → soft-delete `status='deleted'` + ownership flip, preserving users/external_identities; hard-delete only when 0 linked users.
- [x] **M1-9b** `last_sync_at` → **Identity Health**: Healthy (≤24h) / Delayed (≤72h) / Disconnected (>72h or null) / Disabled (status≠active); derived in `DirectoryService.IdentityHealth`; surfaced on `WorkspaceIdpConnection.identityHealth` + `lastSyncAt` in GraphQL.
- [x] **M1-9c** `scim_sync_instances`: `EnsureSyncInstance` opens one per connection (reused until reconnect); provisioned users/external_identities/groups stamp `sync_instance_id`; `ReconcileStaleUsers`/`ReconcileStaleGroups` identify prior-instance objects on reconnect. Added migration `035_groups_sync_instance.sql` (the only schema gap — `groups.sync_instance_id` was omitted from 034).
- [x] **Build gate:** `go build ./...` + `go vet ./...` + `lifecycle_integration_test.go` (10 subtests) + full `go test ./internal/scim/... ./internal/idp/... ./graph/...` green on live Postgres.
- [!] **Deferred (out of scope per ADR §12 re-enable flow):** the explicit authorized admin action that re-enrolls `unmanaged` users back to `scim` ownership after a re-enable. Phase 9 guarantees ownership is NOT auto-restored on re-enable; the re-enroll verb is a separate future action. The backend Identity Health surface (`identityHealth` + `lastSyncAt`) delivered here is consumed by **FE Phase 2** (the health badge).

#### Phase 13 — Consume the proven mapping in `UpdateScimConfig` (close residual Finding C)  `[I]` `[done]`
> See [[Sprint17/Member1-Go/Phase13-UpdateScimConfig-Proven-Mapping]]. Depends on Phase 10, 11, 12.
> Closes path.md acceptance criterion 5 / Finding C.
- [x] **M1-13a** `UpdateScimConfig(scimEnabled:true)` runs its OWN fresh `ProbeMapping` round-trip (`graph/resolvers/idp.resolvers.go` enable branch, Rule 2) and flips `identity_connections.scim_enabled=true` only when `WithRoundTrip` reports `Verified` — no persisted proof flag, no migration (Option B). Unproven fails closed with `extensions.code="SCIM_MAPPING_UNPROVEN"` pointing at `enableScimBreakGlass`. **`TestIdpConnection` (C1) still never persists `scim_enabled=true`; break-glass semantics unchanged.**
- [x] **Build gate:** `go build ./...` + `go vet ./...` + live-Postgres resolver integration (`TestUpdateScimConfig_EnableAfterProvenMapping`, `_UnprovenMappingRefused`, `_NonDefaultSubjectClaimEnables`, `_NoCrossConnectionProofReuse`, `_DisableUnchanged`, `_BreakGlassFallbackUnchanged`; `_CanonicalKeyMismatchFailsClosed` SKIP→engine-level coverage) + `TestIdpConnection_DoesNotPersistScimEnabled` regression — all PASS on live Postgres. Full `go test ./internal/scim/... ./internal/auth/... ./internal/identity/... ./graph/...` green.
- [!] **Scope discipline:** the ONLY prod file changed is `controller/graph/resolvers/idp.resolvers.go` (enable branch) + a `scimEnableRefusedError` helper in `idp_helpers.go`. `ProbeMapping`/`mapping_probe.go`, `TestIdpConnection`, break-glass, migrations, frontend, outbox/PENDING-13, `directory_service.go`, Phase 11 OIDC wiring: all untouched.

### Outbox — already merged (Sprint 18)

The durable outbox is **not implemented in this sprint** — it is `controller/internal/outbox/*` on
`fixed-pendings`. SCIM consumes it via `outbox.Enqueue`. See [[PENDING-15-Durable-Outbox-Infrastructure]]
(status: IMPLEMENTED) and `.zecurity-obs/Sprint18/path.md`. The device-event contract
(`device.trust.revoke.requested` / `device.trust.re_enrollment_required`) is handed to **PENDING-13**,
which registers the handler that executes device/cert revocation.

## Frontend (hand-off to M1-Frontend / React — coordinated, not in the backend core)

> Phase files live in `Member1-Frontend/Phase{1..5}-*.md`. **Three of the five items are blocked on
> backend GraphQL gaps** (no read/write of `subjectClaim`/`scimIdentifier`/`scimEnabled`, no
> `origin` on `Group`, no provisioning-source on `User`) — documented in each phase file's
> "Backend prerequisite / gap" section.
>
> **There is no IdP UI in the admin app today.** `admin/src/pages/` has no `IdpConnections.tsx` /
> `IdpConnectionDetail.tsx`, and `admin/src/graphql/queries.graphql` has zero `idpConnection*`
> operations. **FE-1 owns creating both host pages and the connection queries**, so its *unblocked
> half* (page shells + connection queries + SCIM base-URL box + token mint/rotate/revoke panel) must
> land before FE-2 can mount anything. Only **FE-4** is genuinely standalone-buildable today, via its
> own `ScimConflicts.tsx` page.
>
> **Suggested order:** FE-1 (unblocked half) → FE-2 + FE-4 → [backend gap closure] → FE-1 (mapping/
> enable half), FE-3, FE-5.

- [ ] **FE-0** Enterprise IdP Connection Onboarding (Create OIDC connection dialog, `CreateIdpConnection` mutation, empty state replacement) — [[Sprint17/Member1-Frontend/Phase0-Enterprise-Idp-Onboarding]]
- [ ] **FE-1** SCIM config on a connection (provider preset, mapping fields, enable-SCIM, mint token shown-once, SCIM base URL) — [[Sprint17/Member1-Frontend/Phase1-SCIM-Connection-Config]] · *split: **base-URL box + token panel + host pages/queries buildable now**; mapping fields + enable/disable toggle blocked on backend config-surface gap*. **Ships the `IdpConnections.tsx` / `IdpConnectionDetail.tsx` shells that FE-2 needs.**
- [ ] **FE-2** Identity Health indicator (Healthy / Delayed / Disconnected) — [[Sprint17/Member1-Frontend/Phase2-Identity-Health-Indicator]] · *backend surface complete (`identityHealth` + `lastSyncAt`), but **blocked on FE-1** — both files it edits are created by FE-1 and do not exist yet. The badge component alone can be built and unit-tested in isolation.*
- [ ] **FE-3** Directory-owned fields read-only ("Managed by Google Workspace / Microsoft Entra") — [[Sprint17/Member1-Frontend/Phase3-Directory-Owned-Readonly]] · *blocked: backend `User` provisioning-source gap*.
- [ ] **FE-4** Provisioning-Conflicts queue (Accept-Link / Reject / Reopen) — [[Sprint17/Member1-Frontend/Phase4-Provisioning-Conflicts-Queue]] · *buildable now (standalone page)*. **Three known data gaps**, see the phase file: (a) `scim_username_snapshot`/`scim_email_snapshot` exist in migration 034 but are **not exposed on GraphQL `ScimConflict`**, so rows render raw UUIDs; (b) `resolutionReason` is exposed but has no column → always null (M1-8 known gap); (c) `ErrorPresenter` exists (`controller/graph/resolvers/presenter.go`, wired at `controller/cmd/server/main.go:317`) and is fail-closed — only `*apperr.UserError`/`*gqlerror.Error` reach the client verbatim — but no SCIM resolver returns `apperr` (`grep -c apperr` on `idp.resolvers.go`/`scim_helpers.go` is 0), so `AcceptScimConflict`'s `fmt.Errorf("acceptScimConflict: %w", serr)` break-glass `403` is masked to `message: "an unexpected error occurred"` + `extensions.code="INTERNAL"` and is indistinguishable from any other error; every SCIM mutation error (conflict not found, invalid transition, missing reason, connection not found) is masked the same way, so this also blocks FE-1's token/enable flows, not just FE-4.
- [ ] **FE-5** Origin-labelled groups (`Engineering · SCIM` / `· Local` / `· System`) — never display-name alone — [[Sprint17/Member1-Frontend/Phase5-Origin-Labelled-Groups]] · *blocked: backend `Group.origin` gap*.
- [ ] **FE-7** SCIM config UI — missing fields / manual-verification fixes (enable toggle not rendering in `ScimConfigCard`; `ScimBaseUrlBox` shows the SPA origin `localhost:5173` instead of the controller origin) — [[Sprint17/Member1-Frontend/Phase7-SCIM-Config-Missing-Fields]] · *found 2026-08-28 while running the Okta→Zecurity SCIM manual gate that Phase 1 deferred; backend (`enableScimBreakGlass`/token mint) confirmed working over GraphQL. Closes Phase 1's `implemented-unverified` manual gate.*

### Backend GraphQL follow-up required to unblock the frontend

> **All 9 landed 2026-08-26.** The GraphQL exposure work is done — schema, resolvers, stores and
> `make gqlgen` + `npm run codegen` regenerated; `go build`/`go vet` and the full live-DB suite green.
> Migration `034` now persists the conflict resolution reason, and `ErrorPresenter` now surfaces
> client-actionable `scim.SCIMError`s with `extensions.code`/`status`/`scimType` while still masking
> every 5xx, zero-status and 401. **FE-1/3/4/5 are unblocked.**
>
> **`updateScimConfig` is deliberately NOT a free `scim_enabled` setter** — see §3.2. Disable is
> always allowed; enable runs the fail-closed `MappingGate` and today always refuses, pointing the
> admin at `enableScimBreakGlass`. Editing `subjectClaim`/`scimIdentifier` force-disables SCIM in the
> same write, because `resolveScope` re-reads both on every push and a changed mapping would
> otherwise apply to the next directory write.
>
> **Note:** `go generate ./graph/...` (per CLAUDE.md) is a **no-op** — there are no `go:generate`
> directives. The real command is `make gqlgen` from the repo root, which uses an isolated
> GOMODCACHE.


None of these are backend *logic* gaps — every value already exists in the database and the Go
layer. They are **exposure** gaps: the data is not on the GraphQL surface, so the UI cannot read it.
This is the critical path for FE-1/3/5 and for FE-4's usability.

- [x] `WorkspaceIdpConnection`: expose `subjectClaim`, `scimIdentifier`, `scimEnabled` (columns exist per migration 034; `conn.SubjectClaim` / `conn.ScimIdentifier` are already read in `graph/resolvers/idp.resolvers.go`). → **FE-1**
- [x] Add an `updateScimConfig` mutation covering `subjectClaim` / `scimIdentifier` / `scimEnabled`. Today `scim_enabled` can only be set via `enableScimBreakGlass` (unproven-mapping override) or `testIdpConnection` — there is **no clean enable path for a proven mapping and no disable path at all**. → **FE-1**
- [x] Expose the provider preset / per-connection override (profiles live in `internal/scim/profiles.go`, not on the GraphQL surface). → **FE-1**
- [x] `User`: expose `provisionedBy` / `provisioningOwner` (or a derived `directoryManaged: Boolean!`). Columns exist per migration 034; the type currently exposes only `id/email/role/provider/createdAt`. → **FE-3**
- [x] `Group`: expose `origin` (and ideally `externalId` / `connectionId`). Columns exist per migrations 034/035; the type exposes none of them. → **FE-5**
- [x] `ScimConflict`: expose `scimUsernameSnapshot` / `scimEmailSnapshot` (columns exist in migration 034 and are required conflict-record fields per ADR-025 §4.1); ideally resolve `userId` → `User`. Without these the conflicts queue shows raw UUIDs. → **FE-4**
- [x] Add `scim_identity_conflicts.resolution_reason` (folded into migration `034`, not a new file), so the exposed `ScimConflict.resolutionReason` stops being permanently null (M1-8 known gap). → **FE-4**
- [x] Have SCIM resolvers return `apperr.UserError` for user-actionable failures and extend the existing `ErrorPresenter` (`controller/graph/resolvers/presenter.go`, wired at `controller/cmd/server/main.go:317`) to surface `scim.SCIMError.Status`/`ScimType` in `extensions` — today `AcceptScimConflict` wraps via `fmt.Errorf("acceptScimConflict: %w", serr)` and with no `apperr` in SCIM resolvers the fail-closed presenter masks it to `message:"an unexpected error occurred"` + `code:"INTERNAL"`. → **FE-4** (also unblocks FE-1 — all SCIM mutation errors are masked the same way: token mint/enable, conflict not found, invalid transition, missing reason, connection not found).
- [x] Fix the stale comment on `controller/graph/idp.graphqls` (~line 101): `# pending | linked | rejected` should be `# pending | approved | rejected | expired`, matching `internal/scim/conflict.go:44` and migration 034's CHECK constraint.

## Final Build Gates
```bash
cd controller && go build ./... && go vet ./... && go test ./internal/... ./graph/...
cd controller && go run github.com/99designs/gqlgen generate --config graph/gqlgen.yml   # after schema changes
cd admin && npm run codegen && npm run build                                             # frontend
```

## Acceptance Criteria

> **Verification pass 2026-08-26** — all ten criteria checked against code (graph + source reads).
> Result at audit time: **5 met, 1 partially met, 3 not met, 1 not run.** Criterion 9 has since
> been **fixed and is now met** (Finding B, below); criterion 2 remains open, **criterion 5 closed 2026-08-29 by Phase 13** (Finding C below).
> Details inline below; the three defects found are written up in full under "Verification findings" after the list.

- [~] SCIM provisions/updates users; directory-owned attrs read-only (mutation-layer enforced); Zecurity-owned editable. — **PARTIAL.** Backend half met: `DirectoryService.Update` rejects non-directory attrs via `supportedDirectoryAttr` → `400 invalidValue`, and a non-`scim` `provisioning_owner` → `409 identity_conflict`. The **admin-UI read-only half is FE-3**, which is unbuilt and blocked on `User` provisioning-source exposure.
- [ ] Deprovision (`active=false`/`DELETE`) **suspends/deletes + bumps generation + kills sessions + invalidates ACL**, atomically. — **NOT MET — see Finding A.** Status change + generation bump + outbox enqueue are correctly atomic, but **sessions are never proactively killed and no audit event is emitted** on the SCIM path. Also, `DELETE` does **not** remove `group_members`, which ADR-025 §5 explicitly requires.
- [x] Deprovision enqueues `device.trust.revoke.requested` via `SideEffectSink` → the **durable outbox**, committed in the same tx as the identity mutation; a downstream (device) failure never rolls back identity. — **MET.** `sink.Enqueue(ctx, tx, evt)` runs inside the deprovision tx (`directory_service.go` ~L385); downstream consumption is decoupled through the outbox (PENDING-13), so a device failure cannot roll identity back.
- [x] `identity.mapping.break_glass` is a dedicated permission (ADMIN alone is denied); overrides require reason + audit. — **MET.** `DirectoryService.AcceptLink` performs an explicit `perm.HasPermission(..., permission.BreakGlassMapping)` and returns `403` when absent; possession is row-based in `workspace_permissions` with no role implication; `enableScimBreakGlass` rejects an empty reason.
- [x] Mapping validation is an **active probe-user round-trip**; unproven mapping keeps SCIM disabled. — **MET (2026-08-29, Phase 13).** `MappingGateResult.WithRoundTrip` now has a production caller on the normal enable path: `UpdateScimConfig(scimEnabled:true)` runs its own fresh `ProbeMapping` round-trip (`graph/resolvers/idp.resolvers.go` enable branch, Rule 2) and flips `scim_enabled=true` only when `WithRoundTrip` reports `Verified`. Unproven mapping fails closed with `extensions.code="SCIM_MAPPING_UNPROVEN"` pointing at `enableScimBreakGlass`. `TestIdpConnection` (C1) still never persists `scim_enabled=true`; break-glass remains the exception when the probe cannot run.
  - **STATUS NOTE on the 2026-08-26 audit:** the "VERIFIED NOT MET" claim was accurate *at that time* — `WithRoundTrip` had no production caller and the normal enable path could only refuse. Phase 13 (2026-08-29) closed exactly this gap with **Option B** (run the probe at enable time, no persisted proof flag, no migration) and is **verified against live Postgres** (6 resolver subtests pass; `TestIdpConnection_DoesNotPersistScimEnabled` regression green). The original finding text (no caller / sole break-glass path) is preserved above as the historical record; it is no longer true post-Phase-13.
  - **STATUS NOTE (autoAdd=true probe boundary, 2026-08-29):** `UpdateScimConfig` *does* invoke the real `ProbeMapping` and a `Verified` result enables SCIM without break-glass; unproven *configuration* still fails closed. However the production probe runs with `autoAdd=true`, which fabricates the configured non-default `subjectClaim` onto the synthetic probe person's claims. The probe therefore genuinely exercises the SCIM `Provision→Get` round-trip and the canonical-key equivalence machinery, but it does **not** independently validate a real customer's independently-supplied OIDC claim against SCIM data — a live customer claim mismatch is not detectable by this probe. `TestUpdateScimConfig_UnprovenMappingRefused` reaches the unproven gate via mapping *configuration* rejection (e.g. `subjectClaim==scimIdentifier`), not via `ProbeMapping` returning `Verified=false` from an actual live claim mismatch. This caveat does **not** invalidate Phase 13's implementation acceptance; it scopes the remaining verification boundary (live-claim equivalence remains the Phase 12 configuration-proof, engine-level `autoAdd=false` T2/T3).
- [x] Groups sync with `origin`/`external_id`; out-of-order member → `404`; policy snapshot invalidated. — **MET.** All group reads/writes filter `origin = 'scim'` and are connection-scoped; `userIDsByExternalOrUUID` resolves members only within the token's connection and excludes tombstones; unknown members are collected and returned as `404` **before any mutation** (`groups.go:212`, `:323`); `NotifyPolicyChange` fires on membership change and delete.
- [x] JIT/manual collision → `409 identity_conflict` + pending record; Accept-Link is admin-authorized, atomic, audited, never email-based. — **MET** on the backend. Keyed on the canonical identity key, never email; `AcceptLink` is a single tx (verify pending → permission → link → ownership flip → audit). *Caveat:* the `reason` is audit-only (no `resolution_reason` column), and the admin queue that consumes this is **FE-4, unbuilt**.
- [x] Connection DISABLE suspends users reversibly; DELETE guarded; Identity Health surfaces sync staleness. — **MET** (fully verified). `DeleteIdpConnection` refuses when `linked > 0 && !force`; with `force` it soft-deletes, preserves users, flips ownership to `unmanaged`, audits, and bumps sessions. `resolveScope` refuses all SCIM writes with `403` when `conn.Status != "active"` or `!conn.ScimEnabled`; the resolver-side lifecycle path (`bumpUsers` / `revokeConnectionSessions` / `DeleteIdpConnection`) reaches `Revoker.afterBump`, so **this path does really kill sessions** — unlike the SCIM deprovision path (Finding A). `IdentityHealth` is derived server-side.
- [x] Every SCIM path is scoped to `(workspace_id, connection_id)` from the token — never from the payload. — **FIXED 2026-08-26 (Finding B closed).** Scope *derivation* is correct and payload is never trusted, but the deprovision path never validates the **target user** against that scope, and the generation bump has no tenant predicate at all — a valid token for workspace A can bump a user in workspace B.
- [ ] SCIM conformance suite green; **live Okta/Entra interop = PENDING tenant access** (do not mark done without tenants). — **NOT RUN.** No conformance suite, runner, or fixture exists anywhere in the repo (`find -iname '*conformance*'` returns nothing outside planning docs). Interop remains gated on tenant access.

### Verification findings (2026-08-26 code audit)

Three defects found while checking the acceptance criteria. All are **backend**, all are in code
already marked `[x]` with green build gates — they are behavioral, so compiling tests did not catch
them.

#### Finding A — SCIM deprovision never kills sessions and emits no audit event  *(criterion 2)*

`Revoker.BumpGenerationTx` (`internal/identity/revocation.go:80`) is a bare
`return r.bump(ctx, tx, ...)`. It **never calls `afterBump`** — despite its own doc comment
asserting *"The best-effort session invalidation + audit still happen via afterBump on the
Revoker's pool (post-commit, never failing the tx)."* That comment is false, and it is almost
certainly why this passed review.

Graph confirms it: `afterBump` is reachable only through `BumpGeneration` (the non-Tx variant).
`DirectoryService.Deprovision` uses `BumpGenerationTx`, so on the SCIM path:

- `InvalidateUserSessions` never runs → **live sessions are not proactively revoked**
- the `ActionGenerationBump` audit event is never published → **deprovision is unaudited**

Verified package-wide, not just in one file: the only `Publish` / `audit.RecordTx` call sites in
`internal/scim` are in `conflict.go` (the conflict workflow) and `provisioner.go:93` (the *provision*
path). Neither `Deprovision` nor the `users.go` handler layer emits anything. Suspending or deleting
a user via SCIM therefore leaves **no audit record at all**.

The generation bump itself *is* persisted, so access dies at the next token refresh rather than
immediately. ADR-025's opening paragraph calls precisely this class of failure "a **security
incident** … not a bug". The resolver-side connection-lifecycle path is unaffected (it calls
`BumpGeneration`), which is why criterion 8 passes and criterion 2 does not.

**Fix:** call `afterBump` from the caller after `tx.Commit`, or give `BumpGenerationTx` a returned
closure the caller invokes post-commit. Do **not** call it inside the tx (it must not fail the tx).

**Also in scope of criterion 2:** ADR-025 §5 specifies `DELETE` → "soft-delete **+ remove
`group_members`**". `Deprovision` never touches `group_members`; the only deletes are in
`groups.go` (member-replace/patch). A deleted user therefore remains in their groups and continues
to appear in ACL snapshot membership.

#### Finding B — cross-tenant generation bump via the deprovision path  *(criterion 9)*

The target user is never validated against the token's scope before mutation:

1. `handleDelete` / `handlePatch` pass the raw path `id` straight to `Deprovision` — **no scoped
   `Get` first** (`DirectoryService.Get` *is* correctly scoped, but is not on this path).
2. `Deprovision`'s only gate is `provisioningOwner(ctx, userID)`, whose query is
   `SELECT provisioning_owner FROM users WHERE id = $1` — **no `tenant_id`, no `connection_id`**.
3. The status `UPDATE` does bind `tenant_id`, so it silently affects **0 rows** — pgx returns no
   error for that, so execution continues.
4. `Revoker.bump` then runs
   `UPDATE users SET identity_generation = identity_generation + 1 WHERE id = $1` — **no tenant
   predicate at all** (`tenantID` is a parameter used only for the audit event).

Net effect: a valid SCIM token for workspace A, given the UUID of a SCIM-owned user in workspace B,
returns `204` while bumping **B's** identity generation (invalidating that user's sessions on
refresh) and enqueueing a `device.trust.revoke.requested` event carrying `WorkspaceID = A` with
`UserID` from B — asking PENDING-13 to revoke a foreign user's device trust.

**The outbox does not incidentally block this.** `outbox_events` (migration `033`) constrains
`workspace_id` and `user_id` with two *independent* FKs and no composite tenant check — workspace A
and user-in-B each exist, so both FKs are satisfied, `Enqueue` succeeds, and the tx commits. There is
no accidental rollback saving this path.

**Mitigating:** requires knowing a target UUID, and no scoped SCIM read exposes out-of-scope IDs, so
this is not enumerable through the API. It is still a tenant-isolation break.

**FIXED — 2026-08-26.** Landed across two overlapping efforts; final state:

- `Revoker.bump` carries `AND tenant_id = $2`.
- All four SCIM mutation paths (Provision / Update / Deprovision / Reactivate) resolve ownership
  through `scopedProvisioningOwner`, which binds `tenant_id` **and** reports connection linkage.
- A 0-row status `UPDATE` in Deprovision/Reactivate is now `404` rather than silent fall-through.
- The dead unscoped `provisioningOwner` helper was **deleted**. It had survived with a doc comment
  claiming it was scoped while its body was not — precisely the trap that produced this bug.
- The `hard`-delete membership purge is scoped to the workspace
  (`group_id IN (SELECT id FROM groups WHERE workspace_id = $2)`).

> **Subtlety worth keeping.** The first cut of the scoped guard used an INNER JOIN on
> `external_identities`, which regressed ADR-025 §4.1: a JIT/manual user has no link row, so the
> collision that must return `409 identity_conflict` (and write a pending conflict record) instead
> returned `404` and recorded nothing. Existence and linkage must stay separate signals —
> `tenant_id` gates the `404`; linkage feeds the conflict decision. `scopedProvisioningOwner` now
> returns `(owner, linked, err)` and every caller treats `!linked || owner != "scim"` as a conflict,
> which also keeps a SCIM user owned by a *different* connection out of the current token's reach.

**Regression test:** `TestDeprovision_Integration/cross-tenant_deprovision_is_refused_and_mutates_nothing`
(`internal/scim/deprovision_integration_test.go`). It lives in that file deliberately — it is the only
SCIM test that wires a real `Revoker` and sink; under `users_integration_test.go`'s `nil, nil` the
generation and outbox assertions pass vacuously. Verified by reintroducing the full original defect:
the cross-tenant call then returns **success (`<nil>`, HTTP 204)** and the test fails. It also carries
a positive control asserting the in-scope path still bumps, tombstones, and enqueues.

#### Finding C — the mapping probe was never wired  *(criterion 5)*  — RESOLVED 2026-08-29 (Phase 13, Option B)

Originally: `MappingGateResult.WithRoundTrip` had no production caller, so `scimEnabledAllowed` was
permanently `false` and break-glass was the only route to enabling SCIM. That was accurate at the
2026-08-26 audit. **Closed by Phase 13:** `UpdateScimConfig(scimEnabled:true)` now runs its own fresh
`ProbeMapping` round-trip and flips `scim_enabled=true` only when it reports `Verified` — no persisted
proof flag, no migration (Option B). Verified against live Postgres (6 resolver subtests pass;
`TestIdpConnection_DoesNotPersistScimEnabled` regression green). Criterion 5 is now MET. The
Phase 13 phase file carries the full recipe + evidence:
`Sprint17/Member1-Go/Phase13-UpdateScimConfig-Proven-Mapping.md`.

> **Method note:** findings A and B were reached via the code graph (`trace_path` on `afterBump`
> confirming a single reachable caller) and confirmed by reading source. Criteria 3, 4, 6, 7, 8 were
> verified positively against source. Not independently re-run: the phase build gates themselves —
> these findings are behavioral, and the existing tests pass.

#### Finding D — `createIdpConnection` never contacted the IdP; the setup UI implied it had  *(FE-0 / FE-6)*  — RESOLVED 2026-08-31

**Gap.** The single entry point for IdP setup (`AddIdentityProviderMenu` → `GuidedIdpSetupWizard` →
`CreateIdpConnectionDialog`, FE Phase 6) presented itself as a *connection* flow, but
`createIdpConnection` (`graph/resolvers/idp.resolvers.go`) went straight to
`IdpStore.CreateWorkspaceConnection` — encrypt secret, `INSERT`, audit. **Zero network calls.** A
mistyped Okta domain, a wrong Client ID or a revoked secret all saved successfully and the dialog
reported "Identity provider connection created.", after which the wizard advanced to SCIM. Operators
had no way to distinguish *configuration saved* from *IdP reachable*.

A correct discovery client already existed and was **unreachable from the UI**:
`OIDCProvider.Probe` (`internal/auth/providers/oidc.go`), whose only caller was the
`testIdpConnection` resolver — which has no GraphQL document in `admin/src/graphql/` and no UI
affordance anywhere.

**Two traps found while fixing it (both matter to anyone touching this next):**

1. **`testIdpConnection` must NOT be wired to a "Test Connection" button as-is.** It does more than
   probe: it also runs `ProbeMapping` and then unconditionally calls
   `SetSCIMEnabled(ctx, tenantID, id, false)` (the deliberate C1 invariant, see Phase 13). Exposing
   it as a post-create test would mean an operator clicking "Test" on a healthy, SCIM-enabled
   connection **silently force-disables SCIM**. A real Test Connection button needs a new
   discovery-only resolver.
2. **`Probe` is the wrong primitive for validating operator input.** `discoveryCache` is keyed on the
   **issuer alone**, process-global, 1h TTL — while `discoveryEndpoint()` *prefers* an explicit
   `discoveryUrl` override. So a cache-consulting check can pass with **no request at all** on a warm
   issuer (warmed by a login, an earlier probe, or *another workspace's* connection to the same Okta
   org), and never fetches a bogus override. That is a stale-success hole, not a validation.

**Fix applied.** Option A — validate before persisting, reusing the existing discovery client rather
than adding a second one:

- `internal/auth/providers/oidc.go` — split `discover` into a cache-consulting wrapper +
  `fetchDiscovery(ctx, cacheResult bool)` holding the existing validation verbatim; added
  `ProbeFresh`, which is cache-**neutral** (neither reads nor writes `discoveryCache`, so login
  caching is unchanged and an admin action can never seed the entry the login path reads).
- `graph/resolvers/idp_helpers.go` — `validateOIDCDiscovery`, constructed with **empty client ID and
  secret** (discovery is unauthenticated, so no credential can be sent, logged, or embedded in an
  error — structural, not a logging convention). 5s timeout so a dead host cannot hold the dialog for
  the HTTP client's full 10s. Returns `apperr.UserErrorf` because `ErrorPresenter` is fail-closed: a
  bare `fmt.Errorf` would reach the operator as "an unexpected error occurred".
- `graph/resolvers/idp.resolvers.go` — `CreateIdpConnection` validates before the store call and
  writes nothing on failure. `UpdateIdpConnection` validates **only when `discoveryUrl` is supplied**
  (issuer is immutable, so that override is the only field that changes what gets fetched) — closing
  the same hole via the sibling mutation, without making routine edits depend on the IdP being up.
- `admin/src/components/idp/CreateIdpConnectionDialog.tsx` — honest wording only.

**No schema change and no migration** — nothing in the existing schema was insufficient. No GraphQL
schema change, so no codegen run.

**What is now verified vs. what is NOT** — keep this distinction in any copy written about this flow:

| # | Concern | Verified at create? |
|---|---------|---------------------|
| 1 | Issuer/domain reachability | **Yes** |
| 2 | OIDC discovery validity (200, `issuer` match, required endpoints) | **Yes** |
| 3 | OAuth client ID validity | No |
| 4 | OAuth client secret validity | No |
| 5 | Redirect URI correctness | No |
| 6 | Actual user authentication | No |
| 7 | SCIM connectivity / provisioning | No (separate; the wizard's Step 2 probe is an **internal** Zecurity mapping-consistency check and never contacts the IdP) |

Report it as **"OIDC discovery successful"**, never "Okta credentials verified". There is no safe
pre-login credential check here: the flow is authorization-code + PKCE with no client_credentials
grant, so one was deliberately not invented.

**Tests (25 added).** `internal/auth/providers/oidc_probe_fresh_test.go` (9, DB-free) — the
reachability/validity matrix plus the **warm-cache bypass** proof (prime cache, break the issuer,
assert `ProbeFresh` still fails while `Probe` still returns the stale success) and a
no-credential-material-on-the-wire assertion. `graph/resolvers/idp_create_discovery_validation_test.go`
(11; 6 DB-free + 5 live-Postgres) — refusal + **row absent**, success + row present, update-override
refused + not persisted, displayName-only update **skips** the probe (anti-over-validation),
cross-workspace rejected as not-found. `CreateIdpConnectionDialog.verification.test.tsx` (5) — copy
honesty, refusal surfacing, plus a meta-test proving the overclaim guard actually fires.
*Caveat:* `TestCreateIdpConnection_PersistsWhenDiscoverySucceeds` passes an **empty** client secret
(the shared harness builds its `idp.Store` with a nil encryptor), so it does not cover secret
encryption.

**Verified:** `go build ./...`, `go vet ./...`, `go test ./...` clean; 11 resolver tests green against
live Postgres; admin `pnpm test` 69 passed, `pnpm build` clean.

#### Finding E — `policy_group_origin_test.go` fixture violates the workspace status constraint  *(OPEN, not mine to fix)*

Seven `TestGroupOrigin_*` tests fail whenever `PKI_TEST_DATABASE_URL` is set. Cause is in the test
fixture, not the code under test: it inserts `INSERT INTO workspaces (... status ...) VALUES (...,
'ACTIVE', ...)` (uppercase, lines ~79 and ~407) while `migrations/001_schema.sql` constrains
`status IN ('active','suspended','deleted')`. Every one of the seven fails in its own workspace
`INSERT` with SQLSTATE 23503/23514 **before any resolver runs**, so the group-origin behaviour they
are meant to prove is currently **unverified**. One-character fix (`'ACTIVE'` → `'active'`), left to
the owner of that in-flight file. Flagged because the failures are easy to misread as a regression
from unrelated work.

## Deferred (out of scope this sprint)
- Contractor→employee explicit **identity linking / conversion** → Stage 4 Governance, reserved [[ADR-026-Identity-Governance-and-Identity-Linking]].
- MFA on break-glass (activates with PENDING-06 step-up).
- Pull-based directory sync (Google Directory / MS Graph); broker-owned SCIM.
- Optimistic concurrency (`If-Match`) beyond emitting `meta.version`.
- The **PENDING-13** device-trust handler that consumes the outbox events (separate track).

## Notes for AI Agents
1. Read this `path.md`, then [[PENDING-05-SCIM-Implementation-Plan]] and [[ADR-025-SCIM-Directory-Synchronization]], then your first unchecked phase whose deps are satisfied.
2. **ADR-025 is authoritative — implement, don't redesign.**
3. The durable outbox is **already merged** (`internal/outbox/*`). Do not rebuild it; call `outbox.Enqueue`. Keep the `identity.SideEffectSink` interface as the seam so `scim`/`identity` never import `outbox` directly.
4. **No fake device revocation.** The durable path is real now — enqueue inside the identity tx; never invent a synchronous "device revoked" path in SCIM (execution is PENDING-13's job).
5. Never key identity on email. Never let ADMIN alone satisfy `identity.mapping.break_glass`.
6. SCIM migration is `034` — confirm no collision against the merged tree at branch-cut.
