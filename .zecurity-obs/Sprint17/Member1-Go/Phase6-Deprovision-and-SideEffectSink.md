---
type: phase
member: M1
sprint: 17
phase: 6
title: Users Deprovision + SideEffectSink → durable outbox
status: done
depends_on: [5]
tags: [go, identity, scim, deprovision, revocation, sideeffectsink, pending-05, pending-15]
---

# Phase 6 — Users: Deprovision + Reactivate + SideEffectSink → outbox

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §5, §5.1, §8 · [[PENDING-05-SCIM-Implementation-Plan]] P6.
> **Reconciled 2026-08-18:** the durable outbox already shipped (Sprint 18, `controller/internal/outbox/*`).
> There is **no interim sink** — the `SideEffectSink` is backed by the real `outbox.Enqueue` from the first commit.

## Goal
Make deprovision cut Zecurity access **now** (sessions + policy, no outbox needed) and **durably** emit
device-trust events by enqueuing them into the merged outbox inside the same identity transaction.

## Files
| File | Change |
| --- | --- |
| `controller/internal/identity/device_trust.go` | **consumed** — contract merged from `feat/identity-device-trust-contract` (NOT redefined; `SideEffectSink`, `DeviceTrustEvent`, constructors live here) |
| `controller/internal/scim/side_effect_sink_outbox.go` | **new** — `DurableOutboxSink` implements `identity.SideEffectSink` via `outbox.Enqueue` + `NewDeviceTrustRevokeEvent`/`NewDeviceTrustReEnrollmentRequired` |
| `controller/internal/scim/directory_service.go` | **edit** — `Deprovision`/`Reactivate` (tx: status + `Revoker.BumpGenerationTx` + `sink.Enqueue`); ctor takes `sink` + `revoker` |
| `controller/internal/scim/users.go` | **edit** — `DELETE /Users` (soft-delete) + `active=false→Deprovision` / `active=true→Reactivate` dispatch; drop `handleDeleteNotImplemented` |
| `controller/internal/identity/revocation.go` | **edit** — add `BumpGenerationTx` (tx-aware generation bump) |
| `controller/cmd/server/main.go` | **edit** — wire `scim.NewDurableOutboxSink(outboxStore)` + `identityRevoker` into `NewDirectoryService` |
| `controller/internal/scim/deprovision_integration_test.go` | **new** — DB tests (suspend/delete/reactivate + same-tx enqueue + abort-tx invariant) |

## Steps (all complete)
- [x] Consume the merged `identity.SideEffectSink` + `DeviceTrustEvent` contract (single source of truth; the spec's `identity/side_effect_sink.go` plan is superseded by the merged `device_trust.go` — `Type` folded into `outbox.EventType`).
- [x] `DurableOutboxSink` (the only impl): maps a `DeviceTrustEvent` → `outbox.Enqueue(ctx, tx, identity.NewDeviceTrustRevokeEvent(...))` / `...ReEnrollmentRequired(...)`. Enqueue runs in the **caller's tx** → commits atomically with the identity mutation.
- [x] Deprovision (one tx): `active=false`→`suspended` / `DELETE`→`deleted` (soft-delete tombstone) + `identity_generation` bump (`Revoker.BumpGenerationTx`) + audit + `policy.Notifier` + `SideEffectSink.Enqueue(device.trust.revoke.requested)`. Unscoped/non-scim-owned → 409.
- [x] Reactivate: `active=true` → `status='active'` + enqueue `device.trust.re_enrollment_required`; **no generation bump** (devices were already revoked on suspend; re-enroll via login per ADR-028).
- [x] Event `Type` strings match ADR-025 §5.1 exactly (`device.trust.revoke.requested`, `device.trust.re_enrollment_required`). SCIM only enqueues; never revokes a device synchronously.

## Rules honored
- Identity effects (suspend/delete + generation + session kill + ACL invalidation) are fully correct and tested **without** relying on the outbox handler existing (verified with a fake sink).
- The enqueue is transactional but delivery/execution is asynchronous (outbox → PENDING-13). A forced enqueue error **rolls back the whole tx** (user stays active, generation unbumped) — this is the integration-test invariant, the opposite of "device failure rolls back identity."
- Downstream (device) failure must never roll back the committed identity mutation — exactly why the outbox exists.

## Build gate
`go build ./...` + tests: deprovision identity effects (suspend/delete + generation + session kill) with a **fake sink**; integration asserting an `outbox_events` row is written **in the same transaction**; forced-enqueue-error aborts the whole tx. All green against live Postgres (`PKI_TEST_DATABASE_URL`).

## Notes / deferrals (honest)
- We ENQUEUE only; the PENDING-13 consumer (Track 1) is a separate branch. Not built here (per sprint boundary).
- Phase 8 `scim_identity_conflicts` row on collision: still only 409 (unchanged from Phase 5).
- The `name`/profile column gap (Phase 5) is unaffected.

---

## Post-Phase Fixes

### Fix: Okta's deactivation PATCH shape was silently ignored (2026-09-03)

**Issue:** unassigning a user from the SCIM app in Okta (Assignments → the row's
✕) did NOT deprovision them in Zecurity. The user stayed `active`.

**Root cause:** `internal/scim/users.go`, `applyPatchValue()`. Okta deactivates
with the RFC 7644 §3.5.2 whole-resource shape — **no `path`**, value is an
object:

```json
{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
 "Operations":[{"op":"replace","value":{"active":false}}]}
```

The empty-path branch only handled a bare **string**:

```go
case "", "emails":
    // value may be a string (email) or map
    if s, ok := value.(string); ok && s != "" { ... }   // ← map falls through
```

so the object was dropped. `p.Active` stayed nil → `dispatchActive()` returned
early → the request degraded into a plain attribute update. The failure mode was
maximally quiet: **2xx returned to Okta, `users.updated_at` bumped, `status`
unchanged.** Okta showed a clean provisioning task; nothing was wrong on its side.

The inline comment ("or empty with a value object") shows the object case was
intended — only the string half was implemented.

**Fix applied:** dispatch each key of a no-path object as its own attribute path.
```go
if lower == "" {
    if obj, ok := value.(map[string]any); ok {
        for k, v := range obj {
            if strings.TrimSpace(k) == "" { continue }
            applyPatchValue(p, k, v)
        }
        return
    }
}
```
Keys are attribute names and never empty, so this recurses exactly one level;
the empty-key guard makes that structural rather than incidental. A bare string
with no path keeps its previous meaning (an email change).

**How it was found:** `users.updated_at` was bumped at the moment of the
unassign while `status` stayed `active` — proof that the request ARRIVED and was
accepted rather than never being sent. The Okta side was verified healthy first:
`userMgmtSettings.pushDeactivation` is checked and editable ("Deactivate Users"
enabled), and the tunnel was reachable.

**Tests** (`internal/scim/user_patch_shape_test.go`):
- `TestUserPatch_NoPathObjectDeactivates` / `…Reactivates` — both directions
  through Okta's shape.
- `TestUserPatch_ExplicitActivePathStillWorks` — the `path:"active"` form.
- `TestUserPatch_NoPathObjectAppliesEveryAttribute` — multi-attribute object.
- `TestUserPatch_NoPathBareStringStillEmail` — the string case is unchanged.

Three of these were verified FAILING on the unfixed tree (`Active` was `<nil>`).

**Live verification (controller restarted 17:39:43 with the fix):**

| Step | Okta | Zecurity |
|---|---|---|
| Unassign `shin chan`, **PRE-fix** | removed from Assignments | `active` — **bug reproduced**, `updated_at` bumped only |
| Re-assign, POST-fix | assigned | `active`, audit `device.re_enroll_required` |
| Unassign, **POST-fix** | removed | **`suspended`**, audit `session.generation.bump` |

`Deprovision(hard=false)` yields `status='suspended'` (soft deactivate), which
`identity.CheckLifecycle` rejects at login, and the `session.generation.bump`
audit event invalidates already-issued JWTs. A DELETE (`?hard=true`) is the
tombstone path — distinct and unaffected.

**Pattern worth noting:** this is the THIRD bug this session caused by an Okta
request shape the parser did not accept — alongside the group member-filter case
folding and the metadata-only group replace. All three failed silently. Any new
SCIM request-shape handling should be checked against Okta's actual payloads,
not only against the RFC's canonical examples.
