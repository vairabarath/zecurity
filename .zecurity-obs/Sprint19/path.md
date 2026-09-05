---
type: planning
status: active
sprint: 19
tags:
  - sprint19
  - pending-13
  - pending-16
  - client-device-lifecycle
  - device-trust
  - outbox-consumer
  - resource-policy
  - device-posture
  - execution-path
  - ztna
---

# Sprint 19 — two independent tracks

> Two separate pieces of work were both opened as "Sprint 19" on different
> branches, and this file is the merge of both plans. They share only the sprint
> number; neither depends on the other.
>
> - **Track A — PENDING-16, Resource Policy → Device Profile Binding.**
>   Owner Member02, phase specs in `Member02/`. Plan below.
> - **Track B — PENDING-13, Client Device Lifecycle.**
>   Owner Member2-Go, phase specs in `Member2-Go/`. Plan further below.
>
> Neither plan has been edited; both are reproduced verbatim.

---

# Track A — Resource Policy → Device Profile Binding (PENDING-16)


# Sprint 19 — Resource Policy → Device Profile Binding (PENDING-16)

> **Read this before writing a single line of code.**
> Source of truth: [[PENDING-16-Resource-Policy-Device-Profile-Binding]] plus the team decisions
> recorded during the PENDING-16 discussion.
>
> **This sprint is owned entirely by Member02. No other member is assigned to this task.**
>
> The sprint must deliver the **complete PENDING-16 path**, not only the database or frontend pieces.
> A Resource Policy must become a real working authorization path through the existing posture,
> ACL compilation, propagation, and Connector enforcement pipeline.
>
> **Linux is the minimum end-to-end supported Device Profile platform for this sprint.**
> The implementation is not considered complete if the UI can create a Linux profile but a real
> Linux device cannot report posture, satisfy the profile, become authorized through a Resource
> Policy, and successfully reach a protected Resource.

## Sprint Goal

Replace the direct Resource → Device Profile enforcement model with the policy-layer model:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
    ↓
Device Posture Evaluation
    ↓
ACL Compilation
    ↓
ACL Snapshot
    ↓
Connector Enforcement
    ↓
Shield / Resource
```

The Resource Policy is the access-policy layer. Device Profiles define trusted-device requirements
and remain observable even when no Resource Policy references them. Enforcement occurs because a
Resource Policy references the Device Profile; there is no new Audit/Enforce switch on the profile
or Resource Policy.

## Normative decisions

| Decision | Detail |
|---|---|
| Policy hierarchy | `Policies` is the common parent containing `Resource Policies`, `Sign In Policy`, and `Device Profiles`. |
| Resource relationship | A Resource can be assigned to **exactly one** Resource Policy. |
| Profile relationship | A Resource Policy can reference zero or more Device Profiles. |
| Multiple profiles | Device Profiles in one Resource Policy use **OR** semantics. A device satisfying any selected profile can satisfy the policy. |
| Empty profile set | A Resource Policy with no selected Device Profiles is valid and means **Any Device**; no posture gate is applied by that policy. |
| Enforcement location | Controller computes the authorization result; Connector is the identity-based enforcement point; Shield remains the network/firewall enforcement layer. |
| Profile mode | The new architecture has **no Audit/Enforce mode** on Device Profiles or Resource Policies. Do not invent a replacement toggle. Existing `device_profiles.mode` dependencies must be migrated/retired safely as part of this sprint. |
| Device Profile meaning | A Device Profile defines device trust requirements and provides posture visibility. It does not itself grant or deny resource access. |
| Visibility | Device posture/profile satisfaction can be observed even when a Device Profile is not bound to any Resource Policy. |
| Existing bindings | Keep `resource_profile_bindings` temporarily and migrate existing data safely; do not perform a destructive cutover that can silently change customer access. |
| ACL format | Preserve the existing ACL snapshot contract unless a code-level blocker is proven. The Connector should continue receiving the resolved authorization state, not Resource Policy internals. |
| Posture engine | Reuse the existing posture evaluation and `applyPosture()` behavior wherever possible. |
| Propagation | Preserve the existing policy-change path: invalidate → recompile → push; heartbeat/version reconciliation remains the fallback. |
| Platform support | Linux is the minimum required end-to-end supported Device Profile platform for Sprint 19. |
| Ownership | **Member02 only.** No work is assigned to other members. |
| Backend first | Backend/data/authorization behavior must be complete and tested before frontend completion is declared. |

## Before / after

### Current

```text
Resource
   ↓
resource_profile_bindings
   ↓
Device Profile(s)
   ↓
Mode == ENFORCE
   ↓
Posture evaluation
   ↓
ACL Snapshot
   ↓
Connector
```

### Target

```text
Resource
   ↓
Resource Policy
   ├── Device Profile A
   ├── Device Profile B
   └── Device Profile C
          ↓
     OR evaluation
          ↓
Posture evaluation
          ↓
ACL Snapshot
          ↓
Connector
```

## Non-goals / boundaries

- Do not redesign the Connector authorization protocol.
- Do not make Connector aware of Resource Policy IDs or Device Profile definitions.
- Do not redesign Shield.
- Do not create a second policy engine in the Connector.
- Do not introduce a new Audit/Enforce toggle.
- Do not silently delete or ignore existing `resource_profile_bindings`.
- Do not remove posture evaluation merely because ACL compilation already resolves posture.
- Do not claim Linux Device Profiles work based only on UI or unit tests; prove the complete path.
- Do not implement unrelated PENDING/ADR work.

## Dependency graph

```text
P1  Database model + migration                          [x] done
 ├── P2 Store/domain model                              [x] done
 │    └── P3 GraphQL API                                 [x] done
 ├── P4 Safe legacy-binding migration/compatibility      [ ] NEXT
 └── P5 ACL compiler integration                         [ ]
       └── P6 Policy-change propagation                  [ ]
             └── P8 End-to-end authorization             [ ]

P7  Frontend Resource Policies + Device Profile usability [ ]
 └── P8 End-to-end verification                          [ ]

P9  Testing                                              [ ]
 └── P10 Final verification / documentation / build gates [ ]
```

## Progress — Phases 1–3 complete (2026-09-05)

The new model exists end to end as an admin API and **coexists** with the legacy
one. Nothing has been migrated and no authorization behaviour has changed yet:
`compiler.go` still reads `resource_profile_bindings` exclusively, and
`applyPosture()` is untouched.

| Phase | State | Deliverable |
|---|---|---|
| P1 | done | `controller/migrations/037_device_resource_policies.sql` |
| P2 | done | `controller/internal/posture/resource_policy_store.go` (12 operations) |
| P3 | done | `controller/graph/resourcepolicy.graphqls` + resolvers (2 queries, 7 mutations, 3 relationship fields) |

### Post-Sprint Fixes (P1–P3)

1. **Workspace safety moved into the schema.** The original P1 foreign keys
   referenced `id` alone, so only application code stopped a cross-workspace
   policy assignment. Replaced with tenant-paired composite FKs
   `(policy, tenant) -> (policy, workspace)`. Detail in `Member02/Phase1-*`.
2. **Migration renumbered `034` -> `037`.** It was authored as `034` before
   `034_device_status`, `034_scim_directory_sync`, `035` and `036` existed.
   Confirmed first that no persistent database had ever received the `034`
   version, so a rename was safe rather than needing a delta migration.
3. **Migration made idempotent** (`IF NOT EXISTS` + guarded `ADD CONSTRAINT`),
   because staging/prod is hand-applied with psql and there is no tracking table.
   Same reasoning as `018`.

### Known pre-existing issues inherited from `fixed-pendings`

These were merged in, are **not** caused by P1–P3, and are deliberately left alone:

- **7 failing `TestGroupOrigin_*` tests** in
  `graph/resolvers/policy_group_origin_test.go`: the fixture inserts
  `status = 'ACTIVE'` but migration 001 permits only lowercase
  `('provisioning','active','suspended','deleted')`. Both that test file and
  `001_schema.sql` are byte-identical to `fixed-pendings`, so they fail there too.
- **gqlgen v0.17.90 cannot run under Go 1.27** (`x/tools` v0.42.0 export-data
  mismatch). Use `GOTOOLCHAIN=go1.25.0`, the version `go.mod` declares.
- **`controller/gen` protobuf stubs are gitignored** and must be generated
  (`make generate-proto`) before `go build ./...` or gqlgen will fail.

### Not started

P4 onwards. In particular **no legacy binding has been migrated**, and the open
Phase 4 questions (audit-only bindings, migrated-policy naming under
`UNIQUE (workspace_id, name)`, `deleting`-status resources) remain undecided.

## Team assignment

| Member | Role | Area |
|---|---|---|
| **Member02** | Full-stack / ZTNA | Entire PENDING-16 implementation: database, Controller/store, GraphQL, compiler integration, propagation, frontend, Linux Device Profile end-to-end validation, tests, migration, and final verification. |

## Critical rule

**There is only one owner for Sprint 19: Member02.**

Do not create parallel work assignments for Member01, Member03, etc. If a shared file is touched,
Member02 owns the change and must keep it surgical and compatible with existing work.

---

# Track B — Client Device Lifecycle (PENDING-13)


# Sprint 19 — Client Device Lifecycle (PENDING-13)

> **Read this before writing a single line of code.**
> Source of truth: [[PENDING-13-Client-Device-Lifecycle]] and its promotion
> [[ADR-028-Client-Device-Lifecycle-and-Cert-Renewal]]. This sprint closes the
> SCIM→device outbox loop and the server→client trust signal for client devices.

## Scope
- Track 1 (DONE, merged contract + handler branch `feat/pending-13-device-revoke-handler`):
  first outbox consumers revoke a user's devices on `device.trust.revoke.requested`,
  and record `device.trust.re_enrollment_required`.
- Track 2 (DONE): server→client **device directive** on the 60s ACL poll —
  `REVOKED` / `RE_ENROLL_REQUIRED` / `RENEW_SOON` / `NONE`, with the client daemon
  reacting (wipe key, stop tunnels, surface the right message). See
  `Member2-Go/Track2-Device-Trust-Directive.md` (all acceptance criteria checked).
- Track 3 (next): `RenewCert` RPC + daemon renewal scheduler, riding the
  `RENEW_SOON` channel defined in Track 2. See
  `Member2-Go/Track3-Renew-Reenroll.md`.

## Dependency shape
- Track 1 ships independently; its branch is stacked on the merged
  `feat/identity-device-trust-contract` (now in `fixed-pendings`).
- Track 2 stacks on the Track 1 branch and upgrades `ReEnrollHandler` to set
  `client_devices.status`.
- The SCIM producer (Sathiya, Sprint 17 Phase 6) enqueues the events via the shared
  `identity` contract; until it lands, the loop can't fire end-to-end but the
  consumer/handler is correct and tested.

## Roles
- Member2-Go: controller + client (Rust) implementation of PENDING-13.
- Member1-Go (Sathiya): SCIM producer (PENDING-05 / Sprint 17 Phase 6) — owns the
  enqueue side, not the consumer.
