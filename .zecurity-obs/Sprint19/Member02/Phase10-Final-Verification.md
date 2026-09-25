---
type: phase
member: M02
sprint: 19
phase: 10
title: Final Verification / Cleanup
status: complete
depends_on: [9]
tags: [verification, documentation, cleanup, pending-16]
---

# Phase 10 — Final Verification / Cleanup

## Goal

Prove that Sprint 19 implements the complete PENDING-16 architecture and leaves no partial path.

## Checklist

- [x] Resource Policy is a real persistent entity.
- [x] Resource has exactly one Resource Policy.
- [x] Resource Policy supports zero or more Device Profiles.
- [x] Multiple Device Profiles use OR.
- [x] Empty Device Profile list means Any Device.
- [x] Device Profiles do not carry an enforcement mode in the target path.
- [x] Device Profile posture visibility works without Resource Policy binding.
- [x] Linux Device Profile works end-to-end.
- [x] Existing posture evaluation remains correct.
- [x] ACL compilation remains correct.
- [x] ACL snapshot contract remains compatible.
- [x] Connector remains the identity enforcement point.
- [x] Shield remains network/firewall enforcement.
- [x] Policy changes invalidate stale ACL state.
- [x] ACLs are recompiled and pushed.
- [x] Heartbeat/version fallback still converges.
- [x] Existing legacy bindings are safely migrated/coexist as planned.
- [~] **No customer access silently changes during migration.** — access for one shape *did* change, but not silently: see §Box 18.
- [x] No unrelated pending/ADR work was implemented.
- [x] Only Member02 changes are included in the Sprint 19 work.
- [x] Source code, migration, API, UI, and tests are all documented.
- [x] PENDING-16 status/checklist is updated only after all acceptance criteria pass. *(flipped last, after everything below)*

## Final architecture evidence

Produce a short implementation note showing:

```text
Admin
  ↓
Resource Policy
  ↓
Device Profile
  ↓
Linux Posture
  ↓
Controller evaluation
  ↓
ACL Snapshot
  ↓
Connector
  ↓
Resource
```

and the corresponding actual code paths/functions.

## Final build gate

All backend, frontend, integration, and Linux end-to-end tests must pass.


---

# Final verification — 2026-09-25

Same evidence standard as Phase 9: **I** implemented / **T** test exists / **X**
executed and passing in this phase / **M** manual on real hardware. Only **X** or
**M** earns a tick.

## Architecture evidence — the required chain, mapped to real code

```
Admin          graph/resolvers/resourcepolicy.resolvers.go
               createResourcePolicy · addProfileToResourcePolicy · assignResourcePolicy
               all @hasRole(roles:[ADMIN]); 7 NotifyPolicyChange call sites (:44…:316)
  ↓
Resource       device_resource_policies  (migrations/037_device_resource_policies.sql)
Policy         internal/posture/resource_policy_store.go   — 12 operations
  ↓            resources.device_resource_policy_id, composite FK (id, workspace_id)
Device         resource_policy_profile_bindings → device_profiles
Profile
  ↓
Linux          client/src/posture.rs  collect() → collect_with(root, runner, timeout)
Posture        4 checks: os.version · disk_encryption.luks · firewall.active · secure_boot
               daemon.rs:2010 run_posture_scheduler, 300s + posture_resync
  ↓            gRPC ReportDevicePosture
Controller     internal/client/posture.go  ReportDevicePosture
evaluation       → posture.ValidateReport → InsertReport
                 → posture.Evaluator.EvaluateDevice (internal/posture/evaluate.go:39)
                 → EvaluateProfile (:231)  → device_profile_evaluations
  ↓
ACL            internal/policy/compiler.go  CompileACLSnapshot (:26)
Snapshot         → postureStore.ListPolicyProfilesForWorkspace (:147)
                 → applyPosture (:295)  — OR across profiles, posture-bounded ValidUntil
                 → ACLEntry.allowed_spiffe_ids
  ↓
Connector      policyNotifier.RegisterPushHook(aclPusher.PushWorkspace)  cmd/server/main.go:502
                 → control stream → connector/src/control_stream.rs:371 CBody::AclSnapshot
                 → connector/src/policy/mod.rs  PolicyCache::is_allowed (:59)
               heartbeat fallback: connector sends acl_version (control_stream.rs:186);
               controller compares and re-pushes (internal/connector/control_stream.go:645)
  ↓
Resource       connector/src/device_tunnel.rs:186  — allow/deny, then proxy
```

## Gate results

```
go build ./...                                   OK
go vet ./...                                     OK
go test ./...   (PKI_TEST_DATABASE_URL set)      7 failures, all out-of-scope (below)
cargo test  (client)                             83 passed, 0 failed
cargo test  (shield)                             15 passed, 0 failed, 2 ignored*
npm test                                         17 files, 90 tests passed
npx tsc -b                                       clean
npm run lint                                     18 problems, none in PENDING-16 files
```

\* The two ignored shield tests are `live_nft_rebuild_no_gap` and
`live_nft_failed_apply_preserves_rules` — `#[ignore]` because they need a live
kernel netns (`unshare -rn`) and root. The two that *do* run,
`flush_precedes_rule_adds` and `rebuild_is_single_transaction`, are the ones that
assert the atomic flush-then-rebuild property (the F21 fail-open fix).

## The enforcement split — boxes 12 and 13

These were the weakest boxes going in. Phase 8 deliberately used an `unprotected`
resource, which routes via the connector and needs no Shield, so **Shield was
never exercised in Sprint 19**.

**First: nothing changed.** Verified by diff, not by assertion:

```
git diff origin/fixed-pendings...pending-16 -- proto/       → empty
git diff origin/fixed-pendings...pending-16 -- shield/      → empty
git diff origin/fixed-pendings...pending-16 -- connector/   → empty
```

All three byte-identical. PENDING-16 changes *which SPIFFE IDs land in
`allowed_spiffe_ids`* and nothing else about routing, the wire format, or either
enforcement point. `routeTypeForResource` (compiler.go:380) and the
`shieldIDs[key] = rule.ShieldID` assignment (compiler.go:64) are not in the diff.

**Second: the shield route is now actually exercised.** It never was — no test in
the repo, on any branch, had ever asserted `route_type == "shield"`. Every
compiler integration fixture inserts `status='unprotected'`. Seeding an active
shield and a `protected` resource produced the first shield-routed compiled entry:

```
phase10-shield-routed  route_type=shield     shield_id=590c9283-993c-…
phase8-http            route_type=connector  shield_id=(none)
phase8-tunnel-only     route_type=connector  shield_id=(none)
```

**Third — and this is the combination nothing had ever proven** — PENDING-16
posture gating applies to a shield-routed entry exactly as it does to a
connector-routed one:

```
shield-routed, no policy (Any Device):       route_type=shield  allowed_spiffe_ids=1
shield-routed, gating policy, stale posture: route_type=shield  allowed_spiffe_ids=0
```

Gating is orthogonal to routing. That is the architectural claim, now executed
rather than asserted.

The mechanical basis for the split is in the code: `shield/src/tunnel.rs:47`
`handle_tunnel_open_tcp` performs **no SPIFFE check, no ACL lookup, no identity
check of any kind** — it dials `destination:port` and pipes bytes, trusting its
connector completely. Identity is decided upstream at
`connector/src/device_tunnel.rs:186`. Phase 8 captured both outcomes there:

```
access allowed  dest=172.17.0.4:80 proto=tcp route="connector"
access denied   dest=172.17.0.4:80 reason="no_acl_match"
```

## Unlocking the shield tests — and what they revealed

`resource_acl_coherence_test.go` is gated on **two** variables,
`RESOURCE_TEST_DATABASE_URL` *and* `RESOURCE_TEST_SHIELD_ID`, and had never run.
Unlike the Phase 9 harnesses it does **not** create its own database — it connects
to an existing one and requires a real `shields` row. With one seeded:

```
--- PASS: TestDeleteResource_HardPath_Invalidates
--- PASS: TestUnprotectResource_DoesNotInvalidate
--- FAIL: TestUpdateResource_InvalidatesWhenACLFieldChanges     (:112 fired 2, want 1)
--- FAIL: TestUpdateResource_NoInvalidateForIrrelevantField     (:132 fired 1, want 0)
--- FAIL: TestForceDeleteResource_Invalidates                   (:171 fired 2, want 1)
--- FAIL: TestProtectResource_DoesNotInvalidate                 (:185 fired 1, want 0)
```

**These are pre-existing, and that was proven by execution rather than inferred.**
A detached worktree at `origin/fixed-pendings` (db843e2), run against the same
database, produces the *same four failures at the same line numbers with the same
messages*. The test file and `internal/resource/` are byte-identical across the
branches; the only PENDING-16 delta anywhere near this path is a 5-line additive
resolver registration in `resource.resolvers.go`.

All four reduce to **one** issue: the push hook fires once more than the tests
expect. Root cause for the protect case is worth recording, because the test is
wrong rather than the code — `ProtectResource` calls `NotifyPolicyChange`
(`resource.resolvers.go:83`), and it is right to, since `routeTypeForResource`
maps `protecting` → `"shield"` where `unprotected` → `"connector"`. That **is**
compiler-visible output. The test's comment ("which the compiler never reads — it
must NOT invalidate") encodes a premise the routing logic contradicts.

Direction matters here: these are *over*-notifications. Over-invalidation costs a
redundant recompile; it cannot cause stale authorization. Boxes 14 and 15 are
about not missing an invalidation, and nothing observed misses one.

One environmental note for anyone re-running: `TestProtectResource_` seeds
`host = 10.0.0.95`, and `MarkProtecting` (`internal/resource/store.go:476`)
requires `shields.lan_ip = resources.host`. The fixture shield's `lan_ip` must
match or the test fails before reaching its assertion.

## Box 6 — enforcement mode is out of the target path

`compiler.go` contains **zero** mode references beyond two explanatory comments,
one of which states outright that "Profile mode is deliberately not consulted".
The last surviving gate, `ResourceSatisfied` (`internal/posture/evaluate.go:281`,
`if result.Mode != ModeEnforce`), has **no production caller** — grep finds only
its own test. Mode survives in the GraphQL schema, in write-time validation
(`store.go` rejects an enforce profile with zero requirements), and in that dead
function. It reaches no authorization decision.

## Box 7 — posture visibility without binding

`ListDevicePostureVisibility` (`internal/posture/store.go:1302`) joins
`device_profile_evaluations` → `client_devices` only. No join to policies or
bindings; `EvaluateDevice` evaluates **every** profile in the workspace regardless
of what references it. Phase 8 produced the live proof incidentally: `Any Linux OS`
evaluated `satisfied=true` while attached to no policy at all.

## Box 18 — access did change, but not silently

Recorded as **N/A-with-qualification** rather than ticked, because a plain tick
would be false.

There is no migration (Phase 4 withdrawn), so "during migration" has no referent.
But the Phase 5 cutover **did** change effective access for exactly one shape: a
resource carrying a legacy enforce-mode binding and **no** Resource Policy was
gated before and is ungated (Any Device) now.

That change is: **deliberate** (recorded in Phase 4 and Phase 5 and in
`path.md`), **asserted by a test** that passes —
`TestCompileACLSnapshot_LegacyBindingNoLongerGates`
(`compiler_relay_integration_test.go:875`), whose own comment calls it "the one
accepted behaviour change of the PENDING-16 Phase 5 cutover" — and **inert**,
because no database anywhere holds such a row. It is the opposite of silent.

No other shape changed: a resource with no bindings and no policy was Any Device
before and is Any Device now.

## Boxes 19–21 — scope, authorship, documentation

**Scope.** 14 commits ahead of `origin/fixed-pendings`; cumulative diff 53 files,
+9411/−277. Every file classifies as PENDING-16 work (33) or documentation (20).
**Nothing falls outside.** No SCIM, IdP, relay, transport, outbox, identity or
shield source file appears. Two near-misses checked and cleared: the `relay`
integration test diff is gofmt plus Resource-Policy fixtures, and the `Idp`/`Scim`
lines in `graph/generated.go` are column re-alignment (identifier count identical
at 272 on both branches) plus one real change — adding `resourcepolicy.graphqls`
to the `//go:embed` list.

One caveat worth stating: commit `e0bad61` (a legacy external-identities backfill
migration) *is* off-scope and *is* in branch history — but it was reverted two
commits later by `c900d04` and leaves no trace in the cumulative diff.

**Authorship.** Two identities, one person on the Member02 / Track A lane:
`developer-bairava <dev1@meekaan.com>` (4 commits) and `suriyapriya` (10). No third
author. **No Track B / PENDING-13 code**: `ADR-028` appears only inside the merge
commit (i.e. it arrived from the base branch), and the only PENDING-13 text is
prose naming it as Member2-Go's separate track.

**Documentation.** Migration, store, GraphQL schema, the compiler change-site and
all five Go test files carry substantial file-level "why" headers. Five files are
bare and are recorded as such rather than glossed: `resourcepolicy.resolvers.go`
(gqlgen boilerplate only — mitigated by `resourcepolicy_helpers.go`, which
explains why the helpers were split out), `compiler_test.go`, and three
`admin/src/**/*.test.tsx` files.

## Cleanup candidates — recorded, not actioned

Phase 10 is titled "Final Verification / Cleanup"; by decision this run verifies
only and changes no code. The dead and legacy surface found:

| Candidate | Why | Risk of removing |
|---|---|---|
| `posture.ResourceSatisfied` + its test | zero production callers; the last `device_profiles.mode` gate | none — pure dead code |
| `bindResourceToProfile` / `unbindResourceFromProfile` | defined and codegen'd; **no** component imports them | breaking schema change |
| `DeviceProfile.boundResources` | legacy relationship, authorization-inert since Phase 5 | breaking schema change |
| `updateDeviceProfileMode` | mode reaches no authorization decision | breaking schema change |
| `controller/cmd/phase8/` | Phase 8 harness; mints admin JWTs and enrols without OAuth | keep or gate behind a build tag — see below |

The three GraphQL items are deliberately grouped: removing them is an API-breaking
change that would also require deleting the coexistence tests Phase 9 just proved,
so it deserves its own decision rather than being folded into a verification phase.

## Out-of-scope failures, unchanged from Phase 9

- **7 × `TestGroupOrigin_*`** — `workspaces_status_check` violation; the fixture
  inserts `status='ACTIVE'` where migration 001 allows lowercase only.
  Byte-identical on `fixed-pendings`, fixed upstream by `8cf2cea`.
- **`createGroup` broken** — nine scan destinations against a six-column
  `RETURNING`. Cannot affect any PENDING-16 test (they insert groups with raw
  SQL). Fixed upstream by `c5eb347`.
- **4 × `resource_acl_coherence_test.go`** — newly unlocked this phase, proven
  identical on the baseline worktree. Over-notification, not missed notification.

None is a PENDING-16 result. **Zero PENDING-16 tests fail.**

## Carried forward

- `pending-16` still has no CI workflow; the one on `fixed-pendings` provides
  Postgres but does **not** set `RESOURCE_TEST_SHIELD_ID`, so the shield-route
  tests would still skip there even after a merge.
- A resource with no Resource Policy is ungated, and `internal/resource` never
  writes `device_resource_policy_id`, so every newly created resource starts open.
  Consistent with the Phase 4 decision, but "unassigned" and "deliberately open"
  remain indistinguishable in the data model.
- `TestProtectResource_DoesNotInvalidate` asserts a premise the routing logic
  contradicts. Worth fixing in the branch that owns that test.
