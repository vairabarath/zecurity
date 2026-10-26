---
type: fix
sprint: 20
fix: 2
title: Resource mutations fire the ACL notify the wrong number of times (tests hidden from CI)
status: fixed (2026-10-26, branch sprint20/fix02-resource-acl-notify)
source_rows: [R2]
source_findings: [35]
severity: medium
component: controller (graph/resolvers) + CI
tags:
  - go
  - acl
  - resource
  - regression-gate
  - ci
---

# Fix 02 — Resource mutations fire the ACL notify the wrong number of times

> Source: live acceptance row **R2** (FAIL), Finding 35. The 4 failing tests are in
> `controller/graph/resolvers/resource_acl_coherence_test.go`.
> Status: **FIXED 2026-10-26** (`e1570c2f` code + tests, `1b2b4478` CI). See "Implementation" at the end.

## Failure (as observed)

With `RESOURCE_TEST_DATABASE_URL` + `RESOURCE_TEST_SHIELD_ID` set, 4 tests fail on `5a4f940`
(identically with and without the S20 TEMP `heartbeat.go` change):

| Test | Assertion |
|---|---|
| `TestUpdateResource_InvalidatesWhenACLFieldChanges` | `push hook fired 2 times, want 1` |
| `TestUpdateResource_NoInvalidateForIrrelevantField` | `push hook fired 1 times for ACL-irrelevant update, want 0` |
| `TestForceDeleteResource_Invalidates` | `push hook fired 2 times, want 1` |
| `TestProtectResource_DoesNotInvalidate` | `push hook fired 1 times on protect, want 0` |

## Verification (2026-09-28, this re-check)

Probe: `/tmp/s20-live/r2_probe.sh baseline` (throwaway DB `ztna_s20_probe`, all 39 migrations, seeded
active shield **with `lan_ip = 10.0.0.95`** so the Protect test reaches its real assertion; DB dropped
afterwards). Log: `/tmp/s20-live/logs/r2-probe-baseline.log`. Result: the same 4 FAIL, with
`DeleteResource_HardPath` and `UnprotectResource` passing.

The R2 run had `TestProtectResource_DoesNotInvalidate` failing only on fixture setup (no matching
`lan_ip`). With the fixture fixed it **still fails**, now on the real assertion (hook fired 1, want 0).

Root cause, from code and history:

| Where | What | Origin |
|---|---|---|
| `graph/resolvers/resource.resolvers.go:58-60` | `UpdateResource` calls `NotifyPolicyChange` **unconditionally** | `49c4732` (2026-06-20, "connector-routed unprotected resources") |
| `graph/resolvers/resource.resolvers.go:66-70` | …and again when `resource.ACLRelevantUpdate(updIn)` | `391b28b` (2026-06-23, "invalidate ACL snapshot on resource mutations that change compiled output") |
| `graph/resolvers/resource.resolvers.go:198-200` and `:202-205` | `ForceDeleteResource` notifies twice | `49c4732` + `391b28b` |
| `graph/resolvers/resource.resolvers.go:83-85` | `ProtectResource` notifies | `49c4732` |

`49c4732` and `391b28b` were written on **parallel branches** (neither is an ancestor of the other) and
combined in merge `df5a041`. Each added its own notify, so the merge kept both.

**Which side is right?** `49c4732` changed the ACL compiler so status matters:
- `internal/policy/compiler.go:395-406` `routeTypeForResource`: `pending|unprotected` → `connector`,
  `protecting|protected|failed` → `shield`.
- `internal/policy/store.go:307-317` selects `r.status` and `shield_id`.

So a **Protect/Unprotect does change compiled output** (the route type), and `391b28b`'s test comment
"protect changes only status/pending_action, which the compiler never reads" is **stale**. The tests
encode a pre-`49c4732` assumption for Protect, but they are right about the double notify on Update
and ForceDelete. `ACLRelevantUpdate` (`internal/resource/store.go:452-461`) doesn't list `status`, but
`UpdateInput` can't change status, so it stays correct.

Why CI is green anyway: `.github/workflows/ci.yml:71-80` sets `RESOURCE_TEST_DATABASE_URL` but **not
`RESOURCE_TEST_SHIELD_ID`** (0 occurrences), so every `resource_acl_coherence_test.go` test (and
`snapshot_integration_test.go`, `recordack_reap_test.go`, the posture resolver tests) `t.Skip`s in CI.

**Conclusion:** confirmed. 3 real code bugs (double notify on Update ×2, ForceDelete) plus 1 stale test
(Protect), plus a CI env gap that hides all of it.

## Goal

Every resource mutation fires `NotifyPolicyChange` exactly as many times as the compiled ACL output
changes (0 or 1). The coherence suite runs in CI and is green.

## Design (proposal; confirm before implementing)

1. `UpdateResource`: delete the unconditional notify at `resource.resolvers.go:58-60` and keep the
   `ACLRelevantUpdate`-gated one.
2. `ForceDeleteResource`: keep a single notify (drop `:198-200`, keep the commented one at `:202-205`).
3. `ProtectResource` / `UnprotectResource`: **keep** the notify (the route type changes). Update
   `TestProtectResource_DoesNotInvalidate` → `TestProtectResource_Invalidates` (want 1) with a comment
   citing `compiler.go:395`. Then check `Unprotect`:
   - the revoked-shield branch notifies (`:107-109`);
   - the `protecting` branch pushes only the shield snapshot, but the route type still changes
     shield → connector once it ends `unprotected`.
   Confirm where that later transition notifies (RecordAck path). If nowhere, add it.
4. Test fixture: make `newACLCoherenceFixture` self-provision a workspace/network/connector/**active
   shield with `lan_ip` = the seeded host** (like `internal/resource/unprotect_dead_shield_test.go`), so
   it no longer needs `RESOURCE_TEST_SHIELD_ID`. Otherwise add that env to CI.
5. CI: extend the "Verify integration tests actually ran" step (`ci.yml:119-134`) with
   `./graph/resolvers -run TestUpdateResource_InvalidatesWhenACLFieldChanges`.

Open questions (recommended default in **bold**):
- Q1: Protect: is invalidation correct? → **yes, keep the notify and fix the test** (compiler reads status).
- Q2: Self-provisioning fixture or CI env var? → **self-provisioning** (no hand-crafted shield needed).

## Files (expected)

| File | Change |
|---|---|
| `controller/graph/resolvers/resource.resolvers.go` | remove 2 duplicate notifies |
| `controller/graph/resolvers/resource_acl_coherence_test.go` | fixture self-provisions; Protect expectation → 1 |
| `.github/workflows/ci.yml` | ran-check covers resolvers (and `RESOURCE_TEST_SHIELD_ID` if Q2 = env) |

## Build / test gate

```bash
cd controller && go build ./... && go vet ./...
/tmp/s20-live/r2_probe.sh after     # or the CI env on a throwaway DB
go test -count=1 -v ./graph/resolvers/ -run 'Test(UpdateResource_|ForceDeleteResource_|DeleteResource_HardPath|ProtectResource_|UnprotectResource_)'
```

## Acceptance Criteria

- [ ] Red first: the 4 tests fail on current code (already shown in `r2-probe-baseline.log`).
- [ ] Green: all 6 coherence tests pass, with no skips, and without `RESOURCE_TEST_SHIELD_ID`.
- [ ] Full R2 gate on a throwaway DB: `go test ./...` exit 0 (together with Fix 03 and Fix 04).
- [ ] CI ran-check fails if the coherence tests skip.


## Implementation (2026-10-26)

**Decisions (user, 2026-10-26):** Q1 — keep Protect's notify, fix the test; Q2 — self-provisioning fixture.

| Commit | What |
|---|---|
| `e1570c2f` | code + tests (below) |
| `1b2b4478` | CI "Verify integration tests actually ran" requires 4 of these tests to run and pass |
| `d702e42f` | `ACLRelevantUpdate` includes `remote_network_id` (security review of `e1570c2f`) |

**Code (`controller/`).**
- `graph/resolvers/resource.resolvers.go`: `UpdateResource` — unconditional notify removed (the `ACLRelevantUpdate`-gated one stays); `ForceDeleteResource` — duplicate notify removed.
- **Design step 3 confirmed and fixed:** the transition that finishes an unprotect (`protecting` → `unprotected`, by the shield's ack) re-routes the resource shield → connector, but nothing notified. `resource.RecordAck` now returns `aclChanged` (tombstone reaped **or** route class changed: `pending`/`unprotected` ↔ the rest, mirroring `policy.routeTypeForResource`), reading the previous status with `UPDATE … FROM (SELECT … FOR UPDATE) prev … RETURNING prev.status`; `internal/connector/control_stream.go` `handleResourceAcks` notifies on it.

**Tests.**
- `TestProtectResource_DoesNotInvalidate` → `TestProtectResource_Invalidates` (want 1).
- `newACLCoherenceFixture` creates its own migrated throwaway database (`newMigratedTestPool`, `RESOURCE_TEST_DATABASE_URL` = admin DSN) with a workspace, network, connector and active shield (`lan_ip = aclFixtureShieldLAN` = 10.0.0.95). CI's Postgres starts empty, so writing into `ztna_platform` would not work there.
- `internal/resource`: `seedShield` helper; `TestRecordAck_ReapReturnsReaped` and `TestBuildShieldSnapshotGeneration` use it; new `TestRecordAckReportsACLChanges` (6 cases).
- `RESOURCE_TEST_SHIELD_ID` is no longer read anywhere.

**Why 9 failed in the live run, not 4.** With `RESOURCE_TEST_SHIELD_ID` pointing at a lab shield, every fixture user ran inside the lab workspace: real devices changed posture decisions (an extra notify in `AddProfileRequirement`/`RemoveProfileRequirement`, 4 visibility rows instead of 1), and the shield was revoked (unprotect took the revoked-shield branch, which notifies). With a self-provisioned fixture those 5 posture tests and the unprotect test pass on unchanged code.

**Results.**
- Red (unchanged code, self-provisioned fixture): exactly 4 FAIL — Update ×2, ForceDelete, Protect (on the real assertion); 31 PASS, 0 SKIP.
- Green: 35 PASS, 0 FAIL, 0 SKIP, also against an **empty** database (as in CI); throwaway databases dropped.
- Mutation: unconditional Update notify restored → both Update tests fail; route-change report disabled → `TestRecordAckReportsACLChanges` fails.
- Full controller suite (all DB env vars, no `RESOURCE_TEST_SHIELD_ID`): **1387 PASS, 0 FAIL, 2 SKIP** (both intentional `t.Skip` in code). Before: 1334 PASS, 32 SKIP.

## Acceptance Criteria (result)

- [x] Red first: the 4 tests fail on current code.
- [x] Green: all coherence tests pass, with no skips, and without `RESOURCE_TEST_SHIELD_ID`.
- [x] Full gate: `go test ./...` exit 0 (1387 pass).
- [x] CI ran-check fails if the coherence tests skip.

**Follow-up (security review of `e1570c2f`).** With the unconditional notify gone, an update changing only `remote_network_id` notified nothing — but the compiler routes each resource through its remote network's connectors (`ListEnabledRulesWithResources` selects `r.remote_network_id`). `ACLRelevantUpdate`'s doc listed it as compiler-invisible (written before that column joined the query); the duplicate notify had hidden the gap. Fixed: `ACLRelevantUpdate` includes `RemoteNetworkID`; unit case corrected; new `TestUpdateResource_InvalidatesWhenRemoteNetworkChanges` (want 1, mutation-checked). Full controller: **1388 PASS, 0 FAIL, 2 SKIP**.
