---
type: phase
member: M02
sprint: 19
phase: 7
title: Frontend Policies / Device Profiles
status: done
depends_on: [3, 5]
tags: [frontend, resource-policy, device-profile, linux, pending-16]
---

# Phase 7 — Frontend Policies / Device Profiles

## Goal

Deliver the complete administrative workflow for the new policy model.

The UI must represent:

```text
Policies
├── Resource Policies
├── Sign In Policy
└── Device Profiles
```

## Resource Policies

- [x] Resource Policy list page.
- [x] Create Resource Policy.
- [x] Edit Resource Policy.
- [x] Delete Resource Policy safely.
- [x] Show which Resource is assigned to each policy.
- [x] Enforce the one-policy-per-Resource constraint in the UI while preserving backend enforcement.
- [x] Select zero or more Device Profiles.
- [x] Add/remove Device Profiles.
- [x] Clearly show that multiple profiles are OR.
- [x] Clearly show that no selected profiles means Any Device.
- [x] Do not show an Audit/Enforce toggle.

## Device Profiles

- [x] Keep Device Profile management available independently of Resource Policies.
- [x] Device Profile requirements remain editable.
- [x] Device Profile posture satisfaction remains visible independently of Resource Policy binding.
- [x] Linux is fully supported.
- [x] Do not present unsupported platform checks as working merely because the UI can render them.
- [x] Remove the old Audit/Enforce control from the Device Profile experience.
- [x] Ensure profile creation/editing works with the current supported Linux posture checks.

## UX requirements

The UI must not imply:

```text
Device Profile → directly grants/denies Resource access
```

It must communicate:

```text
Device Profile = trust definition
Resource Policy = access requirement
```

## Verification

- [x] Admin can create a Linux Device Profile.
- [x] Admin can configure its supported Linux posture requirements.
- [x] Admin can see posture satisfaction/visibility without binding the profile.
- [x] Admin can create a Resource Policy.
- [x] Admin can attach the Linux profile.
- [x] Admin can remove the profile.
- [x] Admin can leave the profile list empty.
- [x] Admin cannot attach a second Resource Policy to the same Resource.

---

## Implementation (completed 2026-09-22)

The admin surface for the model Phases 1–6 made real. Before this phase the
Resource Policy API from Phase 3 had **no UI at all** — an admin could not create
a policy, attach a profile, or assign a resource.

**No backend change.** `controller/`, `connector/`, `shield/`, `client/`,
`relay/` and `proto/` are untouched; this is `admin/` only.

### Files

| File | |
|---|---|
| `admin/codegen.yml` | **+1 line** — the blocker (see below) |
| `admin/src/pages/ResourcePolicies.tsx` | **new** — list page, delete + resource-assignment dialogs |
| `admin/src/components/CreateResourcePolicyModal.tsx` | **new** |
| `admin/src/components/EditResourcePolicyModal.tsx` | **new** — rename + profile picker |
| `admin/src/components/DeviceProfilePostureModal.tsx` | **new** — posture visibility |
| `admin/src/pages/Policies.tsx` | two tabs, Resource Policies default |
| `admin/src/pages/DeviceProfiles.tsx` | `boundResources` column replaced; Posture action added |
| `admin/src/graphql/{queries,mutations}.graphql` | 2 queries + 7 mutations |
| `admin/src/generated/*` | regenerated (committed, as this repo does) |
| 4 test files | 1 new page test, 2 new modal tests, 2 updated |

### The blocker that had to clear first

`codegen.yml` lists the controller's `.graphqls` files **explicitly**, and
`resourcepolicy.graphqls` was not among them — so codegen failed on every unknown
field and nothing could compile. One line fixed it; `npm run codegen` then
generated 42 `ResourcePolicy` references.

`node_modules` was also absent; `npm ci` installed exactly what the lockfile pins.

### The Audit/Enforce box was already satisfied — by absence

The phase asks to *"remove the old Audit/Enforce control"*. **There was none.**
`updateDeviceProfileMode` is not defined in the frontend's GraphQL documents and
`GetDeviceProfiles` never selected `mode`. Verified, not assumed; a test asserts
the create panel offers no audit/enforce control.

### The real fix: the misleading column

`GetDeviceProfiles` selected `boundResources` and the page rendered it as a
per-profile count. That is the **legacy** `resource_profile_bindings`
relationship, which Phase 5 removed from authorization entirely — it has no
effect on access, yet the UI displayed it as though a profile gated resources.
That is precisely what this phase's UX requirement forbids:

> The UI must not imply: Device Profile → grants/denies Resource access

`boundResources` is now **not selected at all**. The column is replaced by
**"Required By"**, listing the Resource Policies that reference the profile —
the same admin question ("where is this used?") answered through the model that
actually governs access. `DeviceProfile` exposes no reverse field, so it is
derived client-side by inverting `resourcePolicies { deviceProfiles { id } }`.

### Making the two dangerous semantics legible

Both are stated in words, never left as a count:

- **Zero profiles = "Any Device"**, shown as such in the policy row, in the edit
  panel, and in the create panel ("Starts as Any Device"). Never rendered as
  none/empty, which would read as deny-all.
- **Several profiles = OR**, rendered as `Profile A or Profile B` with
  "any one of N profiles" beneath, and as live wording in the picker that tracks
  the selection.

### One policy per resource

Expressed in the UI by filtering the assignment picker to resources that carry no
policy at all — the pattern `GroupDetail.tsx` already uses for group membership.
The server enforces it independently (`ErrResourceAlreadyAssigned`), and its
refusal is surfaced rather than assumed impossible.

### Sign In Policy — deliberately not built

The Goal section's tree lists a Sign In Policy tab. **No backend for it exists
anywhere in the controller** — no schema, no resolver, no store — so a tab would
be a control that cannot do anything. Omitted, with the reason recorded in
`Policies.tsx` and asserted by a test.

## Verification

```bash
cd admin
npm run codegen   # succeeds — fails first if codegen.yml is not updated
npx tsc -b        # clean (the type gate; there is no typecheck script)
npm test          # 86 passed (17 files)
npm run lint      # 18 problems, ALL in pre-existing files, none in new code
npm run build     # succeeds
```

### What is and is not verified

**Automated (17 test files, 86 tests):** policy list renders; empty state; zero
profiles shown as Any Device; multiple profiles shown as OR; delete confirmation;
edit modal opens; the assignment picker offers only unassigned resources; both
tabs present and switchable; no Sign In Policy tab; create requires a name; no
audit/enforce control; edit preselects attached profiles and its wording tracks
the selection.

**Not yet exercised against a running stack.** The eight "Admin can…" boxes are
checked on the strength of the components and their tests, not a manual pass
through a live app with a real Linux device reporting posture. That end-to-end
proof is **Phase 8**'s job, and these boxes should be re-confirmed there rather
than treated as already demonstrated.

## Carried forward

1. **No sidebar entry was added.** `Sidebar.tsx` `policyItems` would need two
   entries pointing at the same `/policies` path, and both would highlight,
   because the tab is local `useState` rather than URL-backed. Deep-linking would
   mean introducing a `?tab=` convention — deliberately not invented here.
2. **`sonner`'s `<Toaster />` is never mounted anywhere in the app**, so every
   `toast.*()` call — existing ones included — is a visual no-op. The new panels
   follow the convention but keep the inline error banner, which is the channel
   that actually works. Worth fixing app-wide, outside this phase.
3. The legacy `BindResourceToProfile` / `UnbindResourceFromProfile` operations
   remain defined and unused in `mutations.graphql`. Removing them is a later
   cleanup tied to retiring the legacy model.
