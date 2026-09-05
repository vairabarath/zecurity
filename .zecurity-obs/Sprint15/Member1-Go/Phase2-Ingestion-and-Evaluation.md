---
type: phase
member: M1
sprint: 15
phase: 2
title: Report Ingestion + Evaluation Engine
status: done
depends_on: [Phase1-Migration-and-Posture-Store]
tags: [go, posture, evaluation, pending-08]
---

# Phase 2 — Report Ingestion + Evaluation Engine

> Depends on Phase 1 (store) and M2-A (proto message shape — coordinate before writing the handler body).

## Goal

Validate and persist incoming `ReportDevicePosture` calls, then evaluate every bound
profile against the latest observations. This phase is still inert — nothing consults
evaluations for ACL purposes until Phase 3.

## Files

| File | Change |
|------|--------|
| `controller/internal/client/posture.go` | **new** — `(*client.Service).ReportDevicePosture` gRPC method. **This must live here, not in `internal/posture`** — the generated gRPC server interface (`clientv1.ClientServiceServer`) is only satisfiable by a method on `client.Service` (`controller/internal/client/service.go:44`, registered via `RegisterClientServiceServer` in `cmd/server/main.go:404`); a method defined in a different package cannot implement that interface. |
| `controller/internal/posture/validate.go` | **new** — validation logic, called from the `client` package handler above (pure functions/store calls, no gRPC types) |
| `controller/internal/posture/evaluate.go` | **new** — evaluation engine |
| `controller/internal/client/service.go` | add a `PostureStore`/evaluator field to `*Service`, wired in `cmd/server/main.go` |

## Validation

Concrete v1 limits (shared constants in `internal/posture`, used by handler tests):

- `reported_at`: no older than **10 minutes** and no more than **5 minutes** in the future.
- Maximum **32 checks** per report.
- `detail`: maximum **256 UTF-8 bytes** per check.
- `report_id`: canonical UUID string; one ID per collection cycle.
- `client_version`, `os_name`, `os_version`: maximum **64 bytes** each.

- **Unknown `check_id` (not in the registered check registry) → ignored for that one
  check, not a whole-report rejection.** Rejecting the entire report for one
  unrecognized check breaks rolling upgrades: a newer client adds a check the
  controller doesn't yet recognize → if the whole report were rejected, every
  *recognized* check in that same report would also be lost → those checks go stale →
  enforce-mode access gets revoked as a side effect of a version skew, not an actual
  posture regression. Instead: persist observations only for recognized check IDs from
  the report; silently drop (or store as inert, non-evaluated telemetry) any
  unrecognized ones. This is safe because `device_profile_requirements` can only ever
  reference server-registered check IDs (enforced at requirement-creation time in Phase
  3) — an unrecognized check can never accidentally become "required" by a profile.
- Oversized `detail`/value fields → reject that check (not the whole report). If no
  recognized valid checks remain after filtering, reject the report with
  `InvalidArgument`; never advance posture freshness using an empty report.
- Duplicate `report_id` for the same verified workspace/device → return the original
  successful acknowledgement as an idempotent no-op. The same ID presented for a
  different device/workspace is rejected; it must never acknowledge another device's
  report. Duplicate check IDs within one new report are `InvalidArgument`.
- Device revoked (`client_devices.revoked_at IS NOT NULL`) or `workspace_id` mismatch → reject.
- Apply the concrete 10-minute-old/5-minute-future timestamp bounds above.

## Auth — matches `GetACLSnapshot`, not a bare access token

Access tokens are **user/tenant-scoped only** — `VerifyAccessToken` carries no device
claim (confirmed against `controller/internal/client/service.go`'s existing
`GetACLSnapshot` handler). The wire message must therefore be a request wrapper, not a
bare `DevicePostureReport`:

```protobuf
message ReportDevicePostureRequest {
  string access_token = 1;
  string device_id    = 2;
  DevicePostureReport report = 3;
}
```

Handler flow (mirror `GetACLSnapshot` exactly, as `(*client.Service).ReportDevicePosture` in `controller/internal/client/posture.go`):
1. `VerifyAccessToken(access_token)` → user/tenant claims.
2. Look up `client_devices WHERE id = device_id AND user_id = <claims.user_id>`.
3. Check `workspace_id` matches the claims' workspace and `revoked_at IS NULL` — reject otherwise.
4. Only then delegate to `internal/posture`'s validation/store functions to persist the report under that verified `(device_id, workspace_id)`.

The `client.Service` receiver is required here specifically because it's the only type
registered against the generated `clientv1.ClientServiceServer` interface — the
validation/store/evaluation logic itself stays in `internal/posture` exactly as
designed below, only the RPC entry point is package-constrained.

## Evaluation Engine

- For a given profile: **every requirement is ANDed**. A requirement is satisfied only if
  its check's latest observation (from a **non-stale** report) is `PASS`, or is
  `UNSUPPORTED` **and** the requirement has `allow_unsupported = true`.
- Missing observation, stale report (> 10 minutes old), `FAIL`, `UNKNOWN`, `ERROR`, or an
  `UNSUPPORTED` check without `allow_unsupported` → requirement unsatisfied → profile unsatisfied.
- Only **enforce-mode** profiles participate in authorization at all. For a resource with
  multiple bound **enforce-mode** profiles: satisfied if **any one** is fully satisfied
  (OR across enforce profiles only). Audit-mode profiles are still evaluated and stored
  (for visibility/reporting) but are excluded from this OR entirely — they must never be
  able to grant or block access. (An earlier draft of this design let a failing
  audit-mode profile's "audit never blocks" treatment leak into the authorization OR,
  which would have let it mask a failing enforce-mode profile — do not reintroduce that.)
- **Staleness is not caught by re-evaluation alone, and a compile-time-only check is
  not enough either.** `UpsertEvaluation` writes a `satisfied` bool at evaluation time,
  but nothing re-runs evaluation purely because time has passed. Worse: even a
  freshness check inside the ACL *compile* function doesn't help once a compiled
  snapshot is cached — `GetOrCompile` (`controller/internal/policy/cache.go`) returns a
  cache hit without ever re-entering the compile function until an explicit
  `Invalidate()`. **The real fix lives at the cache layer, not here** — Phase 3 bakes a
  `snapshot_valid_until` into the cached snapshot itself (computed per allowed
  `(device,resource)` pair from its *satisfying* enforce profiles' expiry, taking the
  min across pairs — not a flat minimum over every evaluation, see Phase 3), and
  `GetOrCompile` checks wall-clock against it on every read, **also bumping the policy
  version via `NotifyPolicyChange` when it recompiles on expiry** (otherwise the
  corrected snapshot is computed but never delivered — see Phase 3). This phase's job is
  simply to make sure each stored evaluation carries enough information (the source
  report's `received_at`) for Phase 3 to compute the expiry bound — do not assume a
  per-request freshness check here is sufficient on its own.
- Re-evaluate (recompute and re-`UpsertEvaluation`) a device's profiles when: a new
  report for that device lands, a profile's requirements change, or a resource binding
  changes for a profile the device is subject to — this keeps the stored `reason`/
  `satisfied` fresh for the common case; the **cache-level `snapshot_valid_until`
  check (Phase 3)** is what actually protects against the passive-staleness gap between
  re-evaluations.
- Write results via `UpsertEvaluation` (Phase 1).
- Requirement changes are fail-closed even when re-evaluating a large workspace takes
  time: the requirement mutation first commits a new `device_profiles.revision` and
  immediately calls `NotifyPolicyChange`. Existing evaluation rows now have a revision
  mismatch and cannot authorize. Re-evaluation then writes rows for the new revision;
  transitions notify again so newly compliant devices are restored. Never notify while
  still exposing an old evaluation as current for the new requirement set.

## Tests

- Unit: every check status (`PASS/FAIL/UNSUPPORTED/UNKNOWN/ERROR`) × `allow_unsupported` true/false → correct satisfied/unsatisfied.
- Unit: stale report (> 10 min) → unsatisfied even if the last observation was `PASS`.
- Unit: profile AND — one failing requirement fails the whole profile.
- Unit: resource OR — resource bound to enforce profile A (fail) and enforce profile B (pass) → resource-level satisfied.
- Unit: a report containing one unrecognized `check_id` alongside several recognized ones → recognized checks are still persisted and evaluated normally; only the unrecognized one is dropped (regression test for the rolling-upgrade fix — the whole report must not be rejected).
- Integration: duplicate `report_id` → no new row, no error surfaced.
- Integration: report for a revoked/cross-workspace device → rejected.
- Integration: adding a requirement to an enforced profile immediately makes all
  preceding-revision evaluations unusable; access stays denied until new-revision
  evaluations are written.

## Build Check
```bash
cd controller && go build ./... && go test ./internal/posture/...
```

## Implementation Checklist
- [x] **M1-D1** `(*client.Service).ReportDevicePosture` in `controller/internal/client/posture.go` — ownership auth, concrete size/time/count validation, per-check forward compatibility, and device-scoped idempotent duplicate handling.
- [x] **M1-D2** Evaluation engine in `internal/posture/evaluate.go` — AND-within, enforce-only OR, revision-bearing results, fail-closed revision mismatch, and `received_at` inputs for cache expiry.
- [x] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

### Fix: Idempotent retry re-runs evaluation

**Issue:** A report could be committed successfully and then return an evaluation error.
The original replay path acknowledged the existing report without retrying evaluation,
leaving it persisted but unevaluated.

**Root Cause:** The idempotent fast path returned before `EvaluateDevice`.

**Fix Applied (`controller/internal/client/posture.go`):** Same-device replays and
insert-race duplicates now pass through `evaluateAndAcceptPosture`; cross-device reuse
still fails closed.

### Fix: Preserve internal database errors

**Issue:** A database failure during device ownership lookup could be misreported as
`PermissionDenied` because the zero-value workspace ID was compared before checking the
query error.

**Fix Applied (`controller/internal/client/posture.go`):** The handler now handles
`pgx.ErrNoRows`, other database errors, workspace mismatch, and revocation in that order.
