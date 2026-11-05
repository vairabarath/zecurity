---
type: adr
status: pending
id: PENDING-16
domain: policy
priority: P1
created: 2026-08-10
updated: 2026-11-03
related:
  - PENDING-08-Device-Posture-Health
  - PENDING-09-Continuous-Authorization
  - PENDING-19-Workspace-RBAC
  - PENDING-20-Geofencing
tags: [pending, adr, policy, resource-policy, device-profile, posture]
---

# PENDING-16: Resource policies (Resource → Resource Policy → Device Profiles)

**Status:** design note, **decisions taken 2026-11-03** (O-1…O-4, §5; rewritten against `main` `07152f51`). The 2026-08-10 version predated device approval (ADR-030), geofencing (PENDING-20) and workspace roles (PENDING-19). Nothing is built.
**Base:** `main`.
**Scope:** its own epic. It needs a schema change (`[schema: reset DB]`).

---

## 1. Problem

Admins want to say **which devices** may reach a resource, by choosing device profiles for it ("Corporate Linux **or** Corporate Windows"). Today:

- profiles are bound **directly** to resources (`resource_profile_bindings`);
- a per-profile **audit/enforce** switch decides whether a binding gates anything at all;
- the console can do neither: it can't bind, and it can't change the mode.

The intended model puts a **resource policy** in between, so the policy can later carry other conditions too:

```text
Resource ──(exactly one)──▶ Resource Policy ──(zero or more, OR)──▶ Device Profile(s)
```

## 2. What exists today (facts, `main` 2026-11-03)

| Part | Today | Where |
|---|---|---|
| Profiles | `device_profiles` (name, `revision`, **`mode` audit/enforce, default `audit`**); requirements in `device_profile_requirements` (`check_id`, `allow_unsupported`, `params` JSON for the country check) | `controller/migrations/030_device_posture.sql:65-104`, `048_geofencing.sql:14` |
| Bindings | `resource_profile_bindings` (resource, profile, workspace), any number per resource, same-workspace FKs, `ON DELETE CASCADE` | `030:106-132`, `046_workspace_consistency_all.sql:61-79` |
| Gating | the compiler keeps only bindings whose profile is **enforce**. No enforce binding means the resource is **ungated**; several enforce profiles are OR'd. A device passes a profile when its evaluation is satisfied for the current revision and its report is under 10 min old (fail-closed). | `controller/internal/policy/compiler.go:140-175`, `:327-395` |
| Evaluation | **every** profile of the workspace is evaluated for every device, bound or not; transitions notify. Server-attested checks: `zecurity.device.approved`, `zecurity.network.country`. | `controller/internal/posture/evaluate.go:202-261`, `:173-200` |
| Empty profile | evaluates **unsatisfied** ("profile has no requirements"). The store refuses an empty profile in enforce mode (mode change, last-requirement removal, bind). | `evaluate.go:280-283`; `posture/store.go:559, 839, 983` |
| API | `updateDeviceProfileMode`, `bindResourceToProfile`, `unbindResourceFromProfile`, `DeviceProfile.mode/boundResources`; mutations need `POSTURE_WRITE` (Admin, Security) | `controller/graph/posture.graphqls:1-3, 41-48, 86-130`; `internal/rbac/capability.go:42-62` |
| Console | a "Policies" area with one tab, "Device Policies" (the profiles list). **No `mode` is shown or settable, and no binding UI exists** (the bind mutations are defined but unused). So every profile made in the console is `audit`, and **nothing made in the console gates access today.** | `admin/src/pages/Policies.tsx:5-8`, `DeviceProfiles.tsx`, `admin/src/graphql/queries.graphql:350-368` |
| Access | groups grant resources (`access_rules`, `assignResourceToGroup`, `ACCESS_WRITE`). There is no resource-policy or sign-in-policy object anywhere. | `012_groups_acl.sql:28-41`; `controller/graph/policy.graphqls:39-40` |
| ACL | gating is expressed only by narrowing `ACLEntry.allowed_spiffe_ids`; connectors enforce it and cut sessions that lose access | `compiler.go:222-246`, `:327-395` |

**Consequence for the migration:** only API-made setups (such as the geofencing live run) ever enforced anything. The project is pre-production with no migration framework, so a DB reset is the upgrade path.

## 3. Proposed decisions (to approve or change)

- **P-1: Replace, don't layer (Option A).** One new migration:
  - adds `resource_policies` and `resource_policy_profiles`;
  - gives every resource a `policy_id`;
  - **drops** `resource_profile_bindings` and `device_profiles.mode`.

  No compatibility layer, so there is one representation, not two (rejects Options B and C).
- **P-2: Exactly one policy per resource**, as a `NOT NULL` column `resources.policy_id`, FK to the workspace's own policy.
- **P-3: A persisted Default policy per workspace.** `is_default` with a partial unique index (one per workspace). It is created with the workspace by a DB trigger, so every creation path and every test gets one. It **can't be deleted or renamed**, but **its profiles can be edited**. New resources get the Default policy unless one is given.
- **P-4: Profiles in a policy are OR'd; no profiles means "Any Device"** (ungated, exactly as an unbound resource is today).
- **P-5: No audit/enforce mode anywhere.** A policy's profiles always gate. The "what would happen" visibility audit mode was meant to give is a **preview** instead (§4.5): before saving, the console shows how many devices currently allowed would pass.
- **P-6: Deleting a policy moves its resources to Default** in the same transaction, then notifies.
- **Deleting a profile that policies use needs explicit confirmation (D-4).** `deleteDeviceProfile(id, confirmInUse: true)`; without it the server refuses and lists the policies. With it, the profile is removed from those policies in the same transaction, and any policy left with no profiles becomes "any device". The console warns about that by name before confirming. The audit entry lists every affected policy and which became "any device".
- **A profile's last requirement can't be removed while policies use it.** An empty profile is unsatisfied, so it would block every device of those policies.
- **P-7: Roles.** Creating, editing and deleting policies, and choosing their profiles, needs `POSTURE_WRITE` (Admin, Security), the same as profiles today. **Assigning a resource to a policy** also needs `POSTURE_WRITE`, because it changes which devices may connect. DevOps (`NETWORK_WRITE`) still creates resources; they land in Default.
- **P-8: No proto, connector, client or shield change.** Gating stays "narrow `allowed_spiffe_ids`". The compiler reads `resource → policy → profiles` instead of the enforce bindings. The fail-closed evaluation rules (current revision, report under 10 min) are unchanged.
- **P-9: Extensibility.** Future conditions (e.g. a location rule at policy level, or sign-in strength) become new child tables or columns of `resource_policies`. The resource → policy link doesn't change.
- **P-10: Out of scope.** "Sign In Policy", per-group policies, time-of-day rules, and moving geofence or approval rules from profiles to policies (they stay profile requirements).

## 4. Design

### 4.1 Schema (one new file, next free number at merge: `049_resource_policies.sql`; `[schema: reset DB]`)

```sql
CREATE TABLE resource_policies (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  name         TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
  description  TEXT,
  is_default   BOOLEAN NOT NULL DEFAULT FALSE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (workspace_id, name),
  UNIQUE (id, workspace_id)
);
CREATE UNIQUE INDEX resource_policies_one_default ON resource_policies (workspace_id) WHERE is_default;

CREATE TABLE resource_policy_profiles (
  policy_id    UUID NOT NULL,
  profile_id   UUID NOT NULL,
  workspace_id UUID NOT NULL,
  PRIMARY KEY (policy_id, profile_id),
  FOREIGN KEY (policy_id, workspace_id)  REFERENCES resource_policies(id, workspace_id) ON DELETE CASCADE,
  FOREIGN KEY (profile_id, workspace_id) REFERENCES device_profiles(id, workspace_id)   ON DELETE CASCADE
);

-- Default policy for every workspace (trigger) + resources.policy_id NOT NULL,
-- FK (policy_id, tenant_id) -> resource_policies(id, workspace_id) ON DELETE NO ACTION
-- (not RESTRICT: a workspace delete cascades to resources and policies together).
DROP TABLE resource_profile_bindings;
ALTER TABLE device_profiles DROP COLUMN mode;
```

- `ON DELETE CASCADE` on the profile side: the server only deletes an in-use profile after the explicit confirmation (D-4), and records what changed.
- **Triggers:** `AFTER INSERT ON workspaces` creates the Default policy. `BEFORE INSERT ON resources` sets `policy_id` to the workspace's Default policy when it's NULL. So every insert path (resolvers, discovery promote, SQL in tests) gets a policy without changes, and `policy_id NOT NULL` still holds.
- Policy deletion is application code: move the resources to Default, then delete, in one transaction under a row lock.

### 4.2 Controller

- **`internal/policy` store:** CRUD for policies, the profile set, `assignResourcePolicy`, and delete-and-reassign.
  - Each change that affects access bumps `revision` and calls `NotifyPolicyChange(workspace)`: a policy's profile set, a resource's policy, a policy delete, and a profile's requirements (unchanged).
  - Writes are race-safe: the row is locked before the delete-and-reassign, and the profile is locked before "remove last requirement" / "delete profile".
- **Compiler:** per entry, `profiles = policy(resource).profiles`. None means ungated; otherwise OR with today's fail-closed evaluation rules. It replaces the enforce-binding load (`compiler.go:140-175`); `applyPosture` keeps its logic.
- **Posture:** all `mode` reads, constants and the empty-enforce guards go. They're replaced by:
  - **"profile in use"** refusals (delete, remove last requirement);
  - **"empty profile can't be added to a policy"**, because an empty profile is unsatisfied and would block everyone. To mean any device, a policy has no profiles.

  Evaluation is unchanged; every profile is still evaluated, which also feeds the preview.
- **GraphQL:**
  - New `ResourcePolicy { id name description isDefault profiles resources resourceCount }`.
  - New queries `resourcePolicies` and `resourcePolicy(id)`.
  - New mutations `createResourcePolicy`, `updateResourcePolicy(name, description)`, `setResourcePolicyProfiles(id, profileIds)`, `deleteResourcePolicy(id)`, `setResourcePolicy(resourceId, policyId)`, and `previewResourcePolicy(id, profileIds)`.
  - New field `Resource.policy`.
  - `createResource` gets an optional `policyId`.
  - **Removed:** `DeviceProfileMode`, `DeviceProfile.mode`, `updateDeviceProfileMode`, `bindResourceToProfile`, `unbindResourceFromProfile`, `DeviceProfile.boundResources`. `DeviceProfile.policies` replaces the last one.
  - Every refusal is a user-safe error.
- **Audit:** `resource_policy.create`, `.update`, `.profiles`, `.delete`, and `resource.policy` (old → new). Each has the actor; a delete also lists the moved resources.

### 4.3 Console (`admin/`)

- **Policies area tabs:** **Resource Policies** (new) | **Device Profiles** (the existing list, renamed from "Device Policies").
- **Resource Policies:**
  - The list shows name, profiles (as chips; "Any device" when empty), resource count, and a Default badge.
  - Create and edit cover name, description, and a profile multi-select (empty profiles disabled, with the reason).
  - Delete confirms "N resources move to Default".
- **Resource page:** a **Policy** section with the current policy, a selector, and the profiles it requires.
- **Device Profiles:** the list shows "used by N policies" in place of a bound-resource count. Delete and remove-last-requirement are disabled while the profile is in use, with the reason.
- **Preview (P-5):** before saving a profile set or a resource move, the console shows "*N of M devices that can reach these resources today would pass*", from current evaluations. An ACCESS-style confirm appears if N < M.

### 4.4 What doesn't change

- The proto, ACL snapshot shape, connectors, shields, relays and the client.
- Groups and access rules: who may reach a resource is still group-based. The policy decides which of their devices.
- Device posture collection and evaluation, device approval, and geofencing. Those are profile requirements and stay so.

### 4.5 Known limits

- **Narrowing takes effect at once.** There's no "audit first" mode; the preview stands in for it.
- **The preview is a snapshot.** It reflects the latest evaluations; devices that haven't reported recently count as not passing, as the fail-closed rule says.

## 4.6 Behaviour change vs today, stated plainly

- **Profiles in `audit` mode bound by API stop existing.** After the reset there are no bindings; nothing to migrate.
- **Every resource starts in the Default policy, "Any device"**, i.e. ungated. That's today's state for every console-made setup.

## 5. Decisions taken (2026-11-03, Sathiya)

- **D-1 (was O-1):** the Default policy starts as **any device**.
- **D-2 (was O-2):** assigning a resource to a policy needs **`POSTURE_WRITE`** (Admin, Security). DevOps-created resources land in Default.
- **D-3 (was O-3):** **no report-only mode.** The preview before saving replaces audit mode.
- **D-4 (was O-4):** deleting a profile that policies use is **allowed after an explicit, server-checked confirmation** (P-6). Without it, the delete is refused with the list of policies.

## 6. Tests (build gate)

- **Migration and schema:** a fresh DB gives every workspace exactly one Default policy (including workspaces created after the migration); `resources.policy_id` is never null; cross-workspace policy and profile links are refused by FKs.
- **Store:** create, rename and delete (Default refused); delete moves resources to Default atomically, with a race test (delete vs assign); deleting an in-use profile is refused without confirmation and, with it, removes the profile from its policies (a policy left empty becomes any device, audited); last-requirement removal is refused while in use; an empty profile can't be added to a policy; revision bumps; the notifier is called on every access-changing write.
- **Compiler:** no profiles means ungated; one or more profiles OR'd; the fail-closed cases from `TestApplyPosture_FailsClosedForInvalidEvaluations` re-pinned on policies; moving a resource between policies changes `allowed_spiffe_ids`.
- **Resolvers:** capabilities per role (Admin and Security write; DevOps, Viewer and Helpdesk refused, under O-2); user-safe errors; audit rows; preview counts.
- **Console:** the policies list, the editor (empty profiles disabled), the delete confirm, the resource policy section, the preview text; capability-gated controls.
- **Live run:** a device allowed by group; move its resource to a policy whose profile it fails, and access is cut and the session ended; add an OR profile it passes, and access returns; delete the policy, and the resource moves to Default (any device); deleting a profile in use is refused without confirmation, and with it the policy becomes any device and access widens accordingly.

## 7. Phases

| Phase | What | Depends on |
|---|---|---|
| **R1** | Migration `049_`, store, compiler, GraphQL, audit, tests | approval |
| **R2** | Console: Resource Policies tab, resource page section, profile "in use", preview | R1 |
| **R3** | Live run (§6), then one PR `[schema: reset DB]` | R1, R2 |

## 8. Build record

**R1a: migration `049_resource_policies.sql` (2026-11-03).** Written and checked on throwaway databases; the Go code isn't updated yet (R1b).
- **All 52 migration files apply in order.** On a fresh database:
  - each new workspace gets exactly one Default;
  - a resource inserted without a policy gets the Default;
  - a second Default is refused;
  - cross-workspace policy and profile links are refused;
  - a policy still used by a resource can't be deleted;
  - deleting a profile removes it from its policies;
  - `resource_profile_bindings` and `device_profiles.mode` are gone;
  - `policy_id` can't be NULL;
  - deleting a workspace with resources, policies and profiles cascades cleanly.
- **Backfill:** applying 049 over existing data (a workspace, a resource and an enforce-mode binding created under 048) gives the workspace its Default and the resource that policy.
- **Deviations from the §4.1 sketch:**
  - no `revision` column (nothing would read it);
  - `NO ACTION` instead of `RESTRICT` on `resources.policy_id`, so a workspace delete still cascades.
- R1b (below) brings the Go code in line.

**R1b: backend (2026-11-04).**
- **`internal/posture/resource_policy.go`**:
  - Types `ResourcePolicy`, `PolicyRef`, `ProfileDeleteResult`, plus user-safe errors.
  - `List`/`Get`/`Create`/`UpdateResourcePolicy`, `SetResourcePolicyProfiles`, `DeleteResourcePolicy`, `SetResourcePolicy`, `PolicyOfResource`, `ResourcesInPolicy`, `PoliciesUsingProfile`, and `ResourceProfilesForWorkspace` (the compiler's batch query).
  - **Lock order:** profile rows (sorted), then the policy.
  - **Delete-and-reassign:** under the policy row lock; a racing assignment waits on that lock, then fails as "not found".
  - **`DeleteProfile(…, confirmInUse)`** refuses without confirmation with a `ProfileInUseError`. That error unwraps to a user-safe message naming the policies. With confirmation the result reports `RemovedFrom` and `NowAnyDevice`.
- **`internal/posture` store:**
  - `mode`, `UpdateProfileMode`, the binding functions, `ResourceSatisfied` and `ProfileResult.Mode` are removed.
  - `RemoveRequirement` refuses a profile's last requirement only while a policy uses it (`ErrLastRequirement`, now user-safe).
  - Evaluation itself is unchanged.
- **Compiler:** `requiredProfiles(...)` from `ResourceProfilesForWorkspace` replaces the enforce-binding filter. `applyPosture` is unchanged except for parameter naming.
- **`internal/policy/preview.go`:** `PreviewResourcePolicy` counts the devices the resources' groups allow, and those passing the proposed profiles, by calling `applyPosture` itself.
- **GraphQL:**
  - New `resource_policy.graphqls`, registered in `gqlgen.yml` and the admin `codegen.yml`.
  - Posture schema: mode and bind/unbind removed; `DeviceProfile.policies`; `deleteDeviceProfile(confirmInUse)`.
  - Resolvers:
    - audit `resource_policy.create`, `.update` and `.profiles` (from/to), `resource.policy` (from/to), `resource_policy.delete` (moved resources), and `device_profile.delete` (removed from, now any device);
    - `NotifyPolicyChange` on every access-changing write.
  - gqlgen's import reorder was reverted; its `Resource()` declaration kept in `resource.resolvers.go`.
- **Console (just enough for R1 to build):**
  - the profiles query reads `policies`;
  - the bind/unbind operations are removed;
  - the capability map has the new mutations;
  - the profile list shows "Used by policies".
- **Tests:**
  - **Ported** (same intent, on policies):
    - change-hint approval;
    - store integration (empty profile, last requirement, cross-workspace);
    - workspace-consistency FKs;
    - resolver notify/load, empty-profile and cross-workspace refusals;
    - invalid-UUID refusals;
    - the RBAC "forbidden" probe;
    - `TestApplyPosture_PolicyWithNoProfilesAllowsAnyDevice`.
  - **Schema authz table:** 80 gated operations (−3, +8).
  - **New:**
    - `TestResourcePolicyStore` (7 subtests);
    - `TestDeleteResourcePolicyRacesAssignment` (seam `afterPolicyDeleteLock`);
    - `TestResourcePolicy_CompileAndPreview`: Default allows; an approval-only policy denies; OR with a passing profile allows; delete sends the resource back to Default; preview counts;
    - `TestResourcePolicyIsolation`: foreign, missing and malformed policy or resource give the same answer, change nothing and notify nothing;
    - `TestResourcePolicyAudit`.
  - **Mutants killed:** compiler ignores policy profiles; policy delete doesn't move resources; D-4 confirmation ignored; empty profile allowed in a policy.
- **Gates:**
  - `go build`, `go vet`, and `go test -count=1 ./...` with every DB and Valkey variable: 1677 pass / 0 fail / 2 expected skips. `scripts/resource-test-fixture.sh` ok.
  - Admin: codegen, `tsc`, 268 tests, build ok; lint unchanged (17, pre-existing).

**R2: Console (2026-11-04).**
- **Policies page (`admin/src/pages/Policies.tsx`):**
  - Added tabs: "Resource Policies" (default) and "Device Profiles".
- **Resource Policies view (`admin/src/pages/ResourcePolicies.tsx`):**
  - Displays all resource policies with Default badge, profiles chip list, and associated resource count.
  - Creation, editing, and deletion modals.
  - Deletion modal warns that all associated resources will be moved to the Default policy.
- **Resource Policy Modal (`admin/src/components/ResourcePolicyModal.tsx`):**
  - Allows selecting profiles for a policy. Empty profiles (with zero requirements) are disabled with a tooltip explaining that empty profiles cannot be added.
  - Live preview queries `PreviewResourcePolicy` showing how many devices will pass the selected profiles.
  - Displays access reduction warning with an explicit confirmation checkbox if passing device count drops.
- **Device Profiles safeguards (`admin/src/pages/DeviceProfiles.tsx`, `EditDeviceProfileModal.tsx`):**
  - In-use profile deletion modal warns user of affected policies and whether any policy will become "Any device" (ungated). Requires confirmation per D-4.
  - Last active requirement in an in-use profile cannot be toggled off.
- **Resource Detail policy card (`admin/src/pages/ResourceDetail.tsx`):**
  - Displays current policy, Default badge, and attached profiles.
  - Policy switcher allows reassigning resource to another policy, featuring real-time preview and reduction confirmation.
- **Frontend test suite:**
  - Added `ResourcePolicies.test.tsx`, `ResourceDetail.policy.test.tsx`.
  - Updated `DeviceProfiles.test.tsx`, `EditDeviceProfileModal.test.tsx`, `Policies.test.tsx`, `ResourceDetail.protect.test.tsx`.
  - All 32 test files, 278 unit tests pass cleanly. `tsc -b && vite build` succeeds.

**R3: Live verification (2026-11-04).**
- **Clean database reset:**
  - Ran `cd controller && docker compose down -v && docker compose up -d`.
  - Confirmed `049_resource_policies.sql` applied cleanly along with all 52 migrations.
  - Verified `scripts/resource-test-fixture.sh` succeeded.
- **End-to-end verification (all 10 steps passed 100%):**
  1. Default policy auto-created by database trigger on workspace creation (`is_default = true`).
  2. Resource inserted without `policy_id` is auto-assigned to Default policy by `BEFORE INSERT` database trigger.
  3. Setting up group access, member, and client device.
  4. Device submits posture report (firewall pass, not approved); posture evaluated and cached.
  5. Default policy with zero profiles allows any device (verified via ACL compiler snapshot).
  6. Strict policy created with "Approved Only" profile; preview shows 0 of 1 devices passing; resource reassigned; access cut in ACL snapshot (`allowed_spiffe_ids` excluded).
  7. Added OR profile "Firewall On"; preview shows 1 of 1 devices passing; access returned in ACL snapshot under OR rule.
  8. In-use profile deletion guard (D-4): deleting profile in use without confirmation refused with descriptive error naming the policy; confirmed delete succeeded, reporting `RemovedFrom` and `NowAnyDevice`, widening access to Any Device.
  9. Policy deletion moves resources back to Default policy atomically; verified in database.
  10. Attempting to delete Default policy is refused by server (`posture.ErrDefaultPolicyFixed`).
- **All R1, R2, and R3 phases complete. Ready for PR `[schema: reset DB]`.**


**Post-R3 fixes and live run on the real system (2026-11-05).**
- **Live run (real system):** PR head `dc072d6c` controller and console; the system `zecurity-client.service`
  running the branch build; a connector installed on a lab host with the normal install script; a resource on that
  host that serves only connections coming through the connector. Moving the resource to a policy the device fails
  ended the open session in 30–52 ms (connector: `ACL diff: revoked sessions torn down`); adding an OR profile it
  passes, a confirmed in-use profile delete, policy delete → Default and the console's policy edit (preview 0 of 1,
  Save gated by the confirmation box) all behaved as specified. D-2 checked live: a DEVOPS member (NETWORK_WRITE, no
  POSTURE_WRITE) is refused every policy write.
- **Fix: `deleteDeviceProfile` didn't report what it changed.** The store computed the policies the profile was
  removed from and those left with no profiles (`ProfileDeleteResult.RemovedFrom` / `NowAnyDevice`), but the
  mutation returned `Boolean!` and only the audit row carried the lists, contrary to this record and the R1
  summary. Now it returns `DeviceProfileDeletion { removedFrom, nowAnyDevice: [ResourcePolicyRef!]! }`
  (`posture.graphqls`, `posture.resolvers.go`, `toResourcePolicyRefs`). Test
  `TestDeleteDeviceProfileResultNamesPolicies` (a profile in two policies, one keeping another profile; the mutant
  returning `RemovedFrom` as `nowAnyDevice` fails it).
- **Fix: the console's delete dialog didn't say which policy becomes Any Device.** It said generically "any policy
  left with no other profiles will become Any Device". `GetDeviceProfiles` now loads each policy's profile ids, and
  the dialog names the policies left with no profiles, or says none will be. After the delete a toast reports the
  server's result ("Removed from …. Now allowing any device: …"). Three new tests in `DeviceProfiles.test.tsx`
  (the mutant naming every policy fails two).
- **Not changed:** the in-use refusal has no `extensions.code`; no workspace GraphQL error carries one (`apperr`
  has no codes), so it matches the rest of the API.
- **Gates:** controller `go test -count=1 ./...` with every `*_TEST_DATABASE_URL` and the Valkey URL set: 1678 pass,
  0 fail, the 2 usual stub skips; `scripts/resource-test-fixture.sh` with all variables: 170 pass, 1 stub skip. Console
  281 tests pass, build passes, lint unchanged (17, as before). Live: the API returned
  `removedFrom: [Gone or other, Only gone], nowAnyDevice: [Only gone]`; the console dialog named "Only gone" as becoming
  Any Device and the toast showed the result.
- **Tooling note:** under Go 1.27 the documented `go tool gqlgen generate --config graph/gqlgen.yml` fails with
  `internal error: package "time" without types was imported …` (x/tools v0.44.0) and deletes
  `graph/generated.go` and `graph/models_gen.go`. Running it as `GOTOOLCHAIN=go1.25.0 go tool gqlgen generate …` (the
  module's `go` version) works; revert the import reorder it makes in `graph/resolvers/resource.resolvers.go`.
