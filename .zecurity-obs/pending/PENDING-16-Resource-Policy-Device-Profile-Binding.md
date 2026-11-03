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

**Status:** design note, **awaiting approval** (rewritten 2026-11-03 against `main` `07152f51`). The 2026-08-10 version predated device approval (ADR-030), geofencing (PENDING-20) and workspace roles (PENDING-19). Nothing is built.
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
- **P-6: Deleting a policy moves its resources to Default** in the same transaction, then notifies. A **profile used by any policy can't be deleted** (refused, listing the policies), and **its last requirement can't be removed**. Either change would silently turn "these devices" into "any device", a fail-open (today's cascade delete has that hole).
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
  revision     BIGINT  NOT NULL DEFAULT 1 CHECK (revision > 0),
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
  FOREIGN KEY (profile_id, workspace_id) REFERENCES device_profiles(id, workspace_id)   ON DELETE RESTRICT
);

-- Default policy for every workspace (trigger) + resources.policy_id NOT NULL,
-- FK (policy_id, tenant_id) -> resource_policies(id, workspace_id) ON DELETE RESTRICT.
DROP TABLE resource_profile_bindings;
ALTER TABLE device_profiles DROP COLUMN mode;
```

- `ON DELETE RESTRICT` on the profile side backs P-6 in the database.
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

## 5. Decisions still open (for approval)

- **O-1:** P-3 — should the **Default policy start as "Any device"** (proposed), or should admins be required to pick profiles for Default at setup?
- **O-2:** P-7 — should assigning a resource to a policy need `POSTURE_WRITE` (proposed), or `NETWORK_WRITE`, so DevOps can choose a policy when creating resources?
- **O-3:** P-5 — is a **preview** an acceptable replacement for audit mode, or should a per-policy "report only" switch be kept?
- **O-4:** P-6 — should deleting a profile that's in use be **refused** (proposed), or should it be allowed with the profile removed from those policies after an explicit confirm?

## 6. Tests (build gate)

- **Migration and schema:** a fresh DB gives every workspace exactly one Default policy (including workspaces created after the migration); `resources.policy_id` is never null; cross-workspace policy and profile links are refused by FKs.
- **Store:** create, rename and delete (Default refused); delete moves resources to Default atomically, with a race test (delete vs assign); profile delete and last-requirement removal are refused while in use; an empty profile can't be added to a policy; revision bumps; the notifier is called on every access-changing write.
- **Compiler:** no profiles means ungated; one or more profiles OR'd; the fail-closed cases from `TestApplyPosture_FailsClosedForInvalidEvaluations` re-pinned on policies; moving a resource between policies changes `allowed_spiffe_ids`.
- **Resolvers:** capabilities per role (Admin and Security write; DevOps, Viewer and Helpdesk refused, under O-2); user-safe errors; audit rows; preview counts.
- **Console:** the policies list, the editor (empty profiles disabled), the delete confirm, the resource policy section, the preview text; capability-gated controls.
- **Live run:** a device allowed by group; move its resource to a policy whose profile it fails, and access is cut and the session ended; add an OR profile it passes, and access returns; delete the policy, and the resource moves to Default (any device); deleting a profile in use is refused.

## 7. Phases

| Phase | What | Depends on |
|---|---|---|
| **R1** | Migration `049_`, store, compiler, GraphQL, audit, tests | approval |
| **R2** | Console: Resource Policies tab, resource page section, profile "in use", preview | R1 |
| **R3** | Live run (§6), then one PR `[schema: reset DB]` | R1, R2 |
