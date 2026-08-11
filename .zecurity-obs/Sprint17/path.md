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
> Two members: **M1 (identity/SCIM engine)** and **M2 (durable outbox + delivery)**.
> ADR-025 is the authoritative contract — **implement it, do not redesign it.**

## Sprint Goal

Make the workspace roster and group membership **directory-driven**, and make **offboarding
automatic**: an IdP (Okta/Entra/…) pushes Users and Groups over SCIM 2.0; deprovision **cuts Zecurity
access immediately** (sessions + policy) and **durably triggers device-trust revocation** through the
outbox.

```text
BEFORE                                      AFTER
------                                      -----
users appear only at first login (JIT);     directory provisions/updates/deprovisions users + groups;
groups hand-managed; offboarding is manual   deprovision kills sessions + policy now, and enqueues
  → stale access after someone leaves         durable device-trust revocation (outbox → PENDING-13)
```

## The one hard boundary (memorize it)

```text
PENDING-05 (M1)  → DECIDE + ENQUEUE   (identity lifecycle; SideEffectSink.Enqueue in-tx)
PENDING-15 (M2)  → DURABLE DELIVERY   (outbox: persist, claim, retry, recover)
PENDING-13       → DEVICE EXECUTION    (consume device.trust.* → cert/device revocation)
```

**Only durable device-trust delivery depends on PENDING-15.** Everything else (provision, update, groups,
conflict, connection lifecycle, and **access-cutting deprovision via `identity.Revoker`**) is independent
and ships without it.

## Key Design Decisions (locked)

| Decision | Detail |
| --- | --- |
| SideEffectSink boundary | M1 defines `identity.SideEffectSink.Enqueue(ctx, tx, DeviceTrustEvent)`; M2 provides the durable impl. **No fake/best-effort device revocation** — the interim sink only audit-records "REQUESTED, not delivered" and signals it loudly. Guarantee = PENDING-15 only. |
| Break-glass = real permission | `identity.mapping.break_glass` is a **dedicated fine-grained permission**, NOT ADMIN. Smallest permission primitive; +mandatory reason +audit +MFA-when-PENDING-06. |
| Canonical Identity Key | Per-connection `subject_claim` (OIDC) + `scim_identifier` (SCIM) resolve to `external_identities.subject`. Never hardcode `sub==externalId`. Never merge by email. |
| Ownership | Directory owns directory attrs (rejected at the **mutation layer**, read-only in UI); Zecurity owns role/policies/manual-groups. `provisioned_by` (immutable) + `provisioning_owner` (mutable). |
| Conflict, not merge | SCIM hitting a JIT/manual identity → `409 identity_conflict` + pending record; admin Accept-Link/Reject/Reopen. No auto-takeover, never by email. |
| One engine | Provider **profiles** (Okta/Entra/JumpCloud/Keycloak/Generic) + per-connection overrides. **No per-provider handler types.** |
| Atomic tx | identity mutation + audit + `policy.Notifier` + `Revoker` + `SideEffectSink.Enqueue` commit in ONE tx; downstream failure never rolls back identity. |
| Migration number | **Not reserved.** M2 outbox = `033_outbox_events.sql` (PENDING-15). M1 SCIM migration number assigned at integration, after `030` (FQDN) + `033` land. |
| Full stack | Backend + GraphQL + React. Frontend is a coordinated hand-off (see §Frontend), not part of the 2-member backend core. |

## Team Assignments

| Member | Role | Area |
| --- | --- | --- |
| **M1** — you | Go (Controller) | SCIM identity engine: schema, SCIM token auth, break-glass permission primitive, provider profiles + mapping validation, Users provision/update/deprovision, Groups, identity-conflict workflow, connection lifecycle + health + sync instances, and the `SideEffectSink` **interface** + interim sink. |
| **M2** — Sathiya | Go (Controller) | Durable Outbox (PENDING-15): `outbox_events` + Enqueue + claim/retry/recovery + background processor; the `DurableOutboxSink` implementation; P10 wiring + device-event contract handoff to PENDING-13; gap reconciliation. |

## Critical Rule: Conflict Zones

| File / area | Who | Rule |
| --- | --- | --- |
| `controller/internal/scim/**` | **M1 only** | the SCIM engine, handlers, provider profiles, DirectoryService |
| `controller/internal/idp/store.go` | **M1 only** | SCIM-related connection columns + queries |
| `controller/internal/identity/**` | **M1 only** | `SideEffectSink` interface, resolver/linker reuse, permission checks |
| `controller/internal/permission/**` | **M1 only** | break-glass permission primitive |
| `controller/graph/idp.graphqls` + `graph/resolvers/idp*.go` | **M1 only** | SCIM admin API, token mint, conflict queue |
| `controller/migrations/<scim>.sql` | **M1 only** | number assigned at integration (after 030+033) |
| `controller/internal/outbox/**` | **M2 only** | outbox infra (PENDING-15) |
| `controller/migrations/033_outbox_events.sql` | **M2 only** | outbox schema |
| `DurableOutboxSink` impl | **M2 only** | implements M1's `SideEffectSink` over the outbox |
| `controller/cmd/server/main.go` | **coordinate** | M1 wires the SCIM engine + interim sink; M2 wires the outbox + swaps `DurableOutboxSink`. Different regions — announce edits. |

**The contract between M1 and M2 is one interface:** `identity.SideEffectSink`. M1 defines it and ships
the interim sink; M2 implements `DurableOutboxSink`. Agree its shape on Day 1, then work independently.

## Dependency Graph

```text
M1-1 Schema (Day 1, independent)
   ↓
   ├── M1-2 SCIM token auth
   ├── M1-3 Break-glass permission primitive
   ↓
   M1-4 Provider profiles + mapping validation      (needs M1-2, M1-3)
   ↓
   M1-5 Users provision + update                    (needs M1-4)
   ↓
   ├── M1-6 Users deprovision + SideEffectSink       (needs M1-5; sink IFACE only, interim impl)
   ├── M1-7 Groups                                   (needs M1-5)
   ├── M1-8 Identity conflict workflow               (needs M1-5)
   └── M1-9 Connection lifecycle + health + sync     (needs M1-5)

M2-1 Durable outbox infrastructure (Day 1, independent)
   ↓
M2-2 DurableOutboxSink + wiring + reconciliation     (needs M1-6 sink iface + M2-1 outbox)
```

> Day-1 parallel starts: **M1-1** (schema) and **M2-1** (outbox). Agree the `SideEffectSink` shape Day 1.

## Execution Path

### M1 — Identity / SCIM engine

#### Phase 1 — Schema  `[I]`
> See [[Sprint17/Member1-Go/Phase1-Schema]]. Depends on nothing — Day 1.
- [ ] **M1-1a** migration (number TBD at integration): `identity_connections` (+`subject_claim`, `scim_identifier`, `scim_enabled`, `last_sync_at`, `status`+`'deleted'`); `users` (+`provisioned_by`, `provisioning_owner`, `sync_instance_id`); `groups` (+`origin`, `external_id`); `external_identities` (+`sync_instance_id`); new `scim_tokens`, `scim_identity_conflicts` (+ unique pending index), `scim_sync_instances`.
- [ ] **Build gate:** `cd controller && go build ./...`; migration applies on a fresh DB; PENDING-04 tests still green.

#### Phase 2 — SCIM token authentication  `[I]`
> See [[Sprint17/Member1-Go/Phase2-SCIM-Token-Auth]]. Depends on Phase 1.
- [ ] **M1-2a** `scim_tokens` store: HMAC-SHA256 keyed by `SCIM_TOKEN_HASH_KEY`; dual-token rotation (≤2 active, 24h grace, `SCIM_TOKEN_ROTATION_GRACE_HOURS`); lookup-by-hash → bind `(workspace_id, connection_id)`; revoke + auto-expire event.
- [ ] **M1-2b** Bearer middleware for `/scim/v2/*`; GraphQL mint/rotate/revoke/list (plaintext shown once).
- [ ] **Build gate:** `go build ./...` + token unit tests (rotation/grace/scope).

#### Phase 3 — Break-glass permission primitive  `[I]`
> See [[Sprint17/Member1-Go/Phase3-BreakGlass-Permission]]. Depends on nothing (parallel with 1/2).
- [ ] **M1-3a** minimal permission store (`workspace_permissions`) + `HasPermission`; grant/revoke (audited).
- [ ] **M1-3b** register `identity.mapping.break_glass`; ADMIN does **not** implicitly hold it; MFA hook (enforced when PENDING-06 lands).
- [ ] **Build gate:** `go build ./...` + tests (explicit possession; ADMIN-without-grant denied).

#### Phase 4 — Provider profiles + mapping validation  `[I]`
> See [[Sprint17/Member1-Go/Phase4-Provider-Profiles-and-Mapping]]. Depends on Phase 2, 3.
- [ ] **M1-4a** built-in provider profiles + per-connection overrides (one engine, no per-provider handlers).
- [ ] **M1-4b** `testIdpConnection` active probe-user round-trip (`POST→GET→verify→DELETE`), read-only fallback; fail-closed unless `identity.mapping.break_glass` override (reason + audit).
- [ ] **Build gate:** `go build ./...` + mapping-validation tests.

#### Phase 5 — SCIM Users: provision + update  `[I]`
> See [[Sprint17/Member1-Go/Phase5-Users-Provision-Update]]. Depends on Phase 4.
- [ ] **M1-5a** `internal/scim` engine + `identity.DirectoryService` binding `(workspace,connection)` into every query (§10).
- [ ] **M1-5b** provision via `Resolver`/`Linker` (`provisioned_by=scim`, `provisioning_owner=scim`, `sync_instance_id`); update = directory-owned attrs only, Zecurity-owned rejected at mutation layer; RFC 7644 envelopes, `meta.version`, `eq` filter, tombstones hidden.
- [ ] **Build gate:** `go build ./...` + provision/update DB-integration + scope-isolation tests.

#### Phase 6 — Users: deprovision + reactivate + SideEffectSink  (identity `[I]`, sink iface `[I]`)
> See [[Sprint17/Member1-Go/Phase6-Deprovision-and-SideEffectSink]]. Depends on Phase 5.
- [ ] **M1-6a** define `identity.SideEffectSink` interface + interim `UnwiredSink` (audit-records "not delivered", never revokes) + a visible "device-trust delivery: not configured" signal.
- [ ] **M1-6b** deprovision (active=false→suspend, DELETE→soft-delete) + generation bump + `Revoker` session kill + audit + `policy.Notifier` + `SideEffectSink.Enqueue`, **atomic in one tx**; reactivate enqueues `re_enrollment_required`. Repeated DELETE idempotent.
- [ ] **Build gate:** `go build ./...` + deprovision identity-effect tests (independent of PENDING-15).

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
- [ ] **M1-9b** `last_sync_at` → Identity Health (Healthy/Delayed/Disconnected + "device-trust delivery: not configured" while interim); `scim_sync_instances` reconcile on reconnect.
- [ ] **Build gate:** `go build ./...` + lifecycle/health tests.

### M2 — Durable outbox + delivery

#### Phase 1 — Durable outbox infrastructure (PENDING-15)  `[I]`
> See [[Sprint17/Member2-Go/Phase1-Durable-Outbox]]. Depends on nothing — Day 1. Source: [[PENDING-15-Durable-Outbox-Infrastructure]].
- [ ] **M2-1a** `migrations/033_outbox_events.sql` — `outbox_events` (status/retry/lease/claimed_at/next_attempt_at/correlation_id).
- [ ] **M2-1b** `internal/outbox`: `Enqueue(ctx, tx, evt)` (in-caller-tx), `ClaimEvents` (`FOR UPDATE SKIP LOCKED`), processing lifecycle, crash/lease recovery, handler registry, background processor, idempotency, observability.
- [ ] **Build gate:** `go build ./...` + outbox concurrency/recovery tests.

#### Phase 2 — DurableOutboxSink + wiring + reconciliation  `[15]`
> See [[Sprint17/Member2-Go/Phase2-DurableOutboxSink-and-Wiring]]. Depends on M1-6a (sink iface) + M2-1.
- [ ] **M2-2a** implement `DurableOutboxSink` over `outbox.Enqueue`; swap it in at `main.go` (replaces interim sink).
- [ ] **M2-2b** verify enqueue commits inside the deprovision tx; downstream failure never rolls back identity.
- [ ] **M2-2c** gap reconciliation: replay device-trust intents recorded in `audit_logs` during the interim window.
- [ ] **Build gate:** `go build ./...` + end-to-end deprovision→outbox test (with a stub PENDING-13 consumer).

## Frontend (hand-off to M1-Frontend / React — coordinated, not in the 2-member backend core)
- [ ] SCIM config on a connection (provider preset, mapping fields, enable-SCIM, mint token shown-once, SCIM base URL).
- [ ] Identity Health indicator (+ "device-trust delivery: not configured" while interim).
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
- [ ] Deprovision (`active=false`/`DELETE`) **suspends/deletes + bumps generation + kills sessions + invalidates ACL**, atomically — independent of PENDING-15.
- [ ] Deprovision enqueues `device.trust.revoke.requested` via `SideEffectSink`; interim sink **audit-records "not delivered"** and never fakes revocation.
- [ ] `identity.mapping.break_glass` is a dedicated permission (ADMIN alone is denied); overrides require reason + audit.
- [ ] Mapping validation is an **active probe-user round-trip**; unproven mapping keeps SCIM disabled.
- [ ] Groups sync with `origin`/`external_id`; out-of-order member → `404`; policy snapshot invalidated.
- [ ] JIT/manual collision → `409 identity_conflict` + pending record; Accept-Link is admin-authorized, atomic, audited, never email-based.
- [ ] Connection DISABLE suspends users reversibly; DELETE guarded; Identity Health surfaces sync staleness.
- [ ] Every SCIM path is scoped to `(workspace_id, connection_id)` from the token — never from the payload.
- [ ] Durable outbox: transactional enqueue, concurrent claim (`SKIP LOCKED`), retry+backoff, crash/lease recovery.
- [ ] `DurableOutboxSink` swap makes deprovision device-trust durable; gap reconciliation replays interim intents.
- [ ] SCIM conformance suite green; **live Okta/Entra interop = PENDING tenant access** (do not mark done without tenants).

## Deferred (out of scope this sprint)
- Contractor→employee explicit **identity linking / conversion** → Stage 4 Governance, reserved [[ADR-026-Identity-Governance-and-Identity-Linking]].
- MFA on break-glass (activates with PENDING-06 step-up).
- Pull-based directory sync (Google Directory / MS Graph); broker-owned SCIM.
- Optimistic concurrency (`If-Match`) beyond emitting `meta.version`.

## Notes for AI Agents
1. Read this `path.md`, then [[PENDING-05-SCIM-Implementation-Plan]] and [[ADR-025-SCIM-Directory-Synchronization]], then your first unchecked phase whose deps are satisfied.
2. **ADR-025 is authoritative — implement, don't redesign.**
3. The M1↔M2 contract is exactly `identity.SideEffectSink`; agree its shape Day 1, then work independently.
4. **No fake device revocation.** Never create a path that could read as guaranteed enforcement before PENDING-15.
5. Never key identity on email. Never let ADMIN alone satisfy `identity.mapping.break_glass`.
6. Migration number is assigned at integration (after `030` + `033`), not reserved now.
