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
- [ ] **M1-7a** `scim`-origin groups keyed on connection-scoped `external_id`; sync `group_members`; `NotifyPolicyChange`; out-of-order member → `404` (no dangling); origin-aware identifiers.
- [ ] **Build gate:** `go build ./...` + group-sync tests.

#### Phase 8 — Identity conflict workflow  `[I]`
> See [[Sprint17/Member1-Go/Phase8-Identity-Conflict-Workflow]]. Depends on Phase 5.
- [ ] **M1-8a** `scim_identity_conflicts`: `409 identity_conflict` on JIT/manual collision; one pending per key; consistent across verbs; no auto-takeover.
- [ ] **M1-8b** Accept-Link (atomic: link + `provisioning_owner→scim`, preserve local) / Reject / Reopen — all audited; GraphQL admin queries.
- [ ] **Build gate:** `go build ./...` + conflict-FSM tests.

#### Phase 9 — Connection lifecycle + health + sync instances  `[I]`
> See [[Sprint17/Member1-Go/Phase9-Connection-Lifecycle-Health-Sync]]. Depends on Phase 5.
- [ ] **M1-9a** connection `active→disabled→deleted`: DISABLE suspends users (reversible) + kills sessions + `provisioning_owner scim→unmanaged`; DELETE guarded (0 users or destructive confirmation).
- [ ] **M1-9b** `last_sync_at` → Identity Health (Healthy/Delayed/Disconnected); `scim_sync_instances` reconcile on reconnect.
- [ ] **Build gate:** `go build ./...` + lifecycle/health tests.

### Outbox — already merged (Sprint 18)

The durable outbox is **not implemented in this sprint** — it is `controller/internal/outbox/*` on
`fixed-pendings`. SCIM consumes it via `outbox.Enqueue`. See [[PENDING-15-Durable-Outbox-Infrastructure]]
(status: IMPLEMENTED) and `.zecurity-obs/Sprint18/path.md`. The device-event contract
(`device.trust.revoke.requested` / `device.trust.re_enrollment_required`) is handed to **PENDING-13**,
which registers the handler that executes device/cert revocation.

## Frontend (hand-off to M1-Frontend / React — coordinated, not in the backend core)
- [ ] SCIM config on a connection (provider preset, mapping fields, enable-SCIM, mint token shown-once, SCIM base URL).
- [ ] Identity Health indicator (Healthy / Delayed / Disconnected).
- [ ] Directory-owned fields read-only ("Managed by Google Workspace / Microsoft Entra").
- [ ] Provisioning-Conflicts queue (Accept-Link / Reject / Reopen).
- [ ] Origin-labelled groups (`Engineering · SCIM` / `· Local` / `· System`) — never display-name alone.

## Final Build Gates
```bash
cd controller && go build ./... && go vet ./... && go test ./internal/... ./graph/...
cd controller && go run github.com/99designs/gqlgen generate --config graph/gqlgen.yml   # after schema changes
cd admin && npm run codegen && npm run build                                             # frontend
```

## Acceptance Criteria
- [ ] SCIM provisions/updates users; directory-owned attrs read-only (mutation-layer enforced); Zecurity-owned editable.
- [ ] Deprovision (`active=false`/`DELETE`) **suspends/deletes + bumps generation + kills sessions + invalidates ACL**, atomically.
- [ ] Deprovision enqueues `device.trust.revoke.requested` via `SideEffectSink` → the **durable outbox**, committed in the same tx as the identity mutation; a downstream (device) failure never rolls back identity.
- [ ] `identity.mapping.break_glass` is a dedicated permission (ADMIN alone is denied); overrides require reason + audit.
- [ ] Mapping validation is an **active probe-user round-trip**; unproven mapping keeps SCIM disabled.
- [ ] Groups sync with `origin`/`external_id`; out-of-order member → `404`; policy snapshot invalidated.
- [ ] JIT/manual collision → `409 identity_conflict` + pending record; Accept-Link is admin-authorized, atomic, audited, never email-based.
- [ ] Connection DISABLE suspends users reversibly; DELETE guarded; Identity Health surfaces sync staleness.
- [ ] Every SCIM path is scoped to `(workspace_id, connection_id)` from the token — never from the payload.
- [ ] SCIM conformance suite green; **live Okta/Entra interop = PENDING tenant access** (do not mark done without tenants).

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
