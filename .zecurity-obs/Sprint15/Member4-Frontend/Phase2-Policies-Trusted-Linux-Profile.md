---
type: phase
member: M4
sprint: 15
phase: 1
title: Policies Page + Create Trusted Linux Profile Panel
status: in-progress
depends_on:
  - Member4-Frontend/Phase1-Device-Profile-Admin-UI
tags:
  - frontend
  - admin-ui
  - posture
  - policies
  - pending-08
---

# Phase 2 — Policies Page + Create Trusted Linux Profile Panel

> Follow-up to Phase 1, driven directly by the user rather than derived from
> PENDING-08. Restructures where Device Profiles lives in the nav (under a new
> "Policies" page instead of its own top-level item) and redesigns the create flow
> into an OS-first, mockup-matched side panel, scoped to Linux only for now.

## Why this phase exists

Phase 1 shipped Device Profiles as its own standalone `/device-profiles` nav item.
This phase:
1. Introduces a **new "Policies" page** (didn't exist anywhere in this codebase
   before this phase — no route, no nav item, no prior concept) with Device Profiles
   as its first tab, using the pill-tab pattern already established in
   `admin/src/pages/GroupDetail.tsx`.
2. Adds an **OS category step** to profile creation — Linux only enabled for now
   (Windows/macOS shown disabled, "Soon" badge, matching the pattern already used in
   `admin/src/components/InstallCommandModal.tsx` for the Docker option).
3. Rebuilds the create panel to match a provided mockup ("Create Trusted Linux
   Profile"): Profile Name, Verification Requirements (Manual Trust + a stubbed
   Connect Trust Methods), Device Posture Checks.

**Backend boundary crossed, explicitly user-directed:** the mockup's "Verification
Requirements" section had zero backing schema anywhere — confirmed by reading
`posture.graphqls` end-to-end and grepping the whole repo for "trust"/"verification".
The user chose to add a real persisted field (`manualTrust`) rather than fake it in
the UI only, which meant touching `controller/graph/posture.graphqls`,
`controller/internal/posture/store.go`, and `controller/graph/resolvers/*` — files
this sprint's `path.md` conflict-zone table reserves for **M1**. Done here with the
user's explicit go-ahead since they're driving cross-member work directly in this
session; flagging it here so it's visible to whoever picks up M1's remaining phases.

## Backend changes (M1-owned files, user-approved exception)

| File                                                                                                                  | Change                                                                                                                                                                                                                                                                             |
| --------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `controller/migrations/031_device_profile_manual_trust.sql`                                                           | **new** — `device_profiles.manual_trust_enabled BOOLEAN NOT NULL DEFAULT true`                                                                                                                                                                                                     |
| `controller/internal/posture/store.go`                                                                                | `Profile.ManualTrustEnabled`; `CreateProfile` takes/persists it; new `UpdateProfileManualTrust` + `ErrNoVerificationMethod` guard (Manual Trust is the only verification method that exists today, so disabling it is rejected — same shape as the empty-enforce-profile guard)    |
| `controller/graph/posture.graphqls`                                                                                   | `DeviceProfile.manualTrust: Boolean!`; `createDeviceProfile(..., manualTrust: Boolean = true)`; new `updateDeviceProfileManualTrust(id, enabled): DeviceProfile!`                                                                                                                  |
| `controller/graph/resolvers/posture.resolvers.go`, `posture_helpers.go`                                               | Resolver bodies for the above, regenerated via `go run github.com/99designs/gqlgen@v0.17.90 generate --config graph/gqlgen.yml` (**not** `go generate ./graph/...` — there's no `//go:generate` directive anywhere in this repo; the real command is the `gqlgen` Makefile target) |
| `controller/internal/posture/store_test.go`, `store_integration_test.go`, `graph/resolvers/posture_resolvers_test.go` | Tests for the guard (unit, nil-pool), the real DB round-trip (integration), and the resolver's safe user-facing error                                                                                                                                                              |

## Frontend changes

| File | Change |
|------|--------|
| `admin/src/graphql/mutations.graphql`, `queries.graphql` | `CreateDeviceProfile` gains `$manualTrust`; new `UpdateDeviceProfileManualTrust`; `GetDeviceProfiles` selects `manualTrust` |
| `admin/src/components/ui/switch.tsx` | **new** — small local toggle (no Radix dep added; none existed before) |
| `admin/src/components/CreateDeviceProfileModal.tsx` | Rebuilt: OS step (Linux enabled; Windows/macOS disabled+"Soon") → "Create Trusted Linux Profile" panel matching the mockup. Manual Trust renders always-on/locked (no interaction — it's the only method that exists). "Connect Trust Methods" and the "Manage verification methods"/"Learn More" links all fire a `toast(...)` ("coming soon") rather than linking anywhere real. Device Posture Checks are sourced from `supportedPostureChecks` filtered to `platform === 'linux'` — all four registered checks are offered, not just the two shown in the mockup screenshot, per Phase 1's existing rule against hardcoding a check subset. Submit flow: `createDeviceProfile` → `addProfileRequirement` per toggled check. |
| `admin/src/pages/Policies.tsx` | **new** — pill-tab page (pattern borrowed from `GroupDetail.tsx`), one tab today: Device Profiles, rendering the existing `DeviceProfiles` page component as-is |
| `admin/src/App.tsx` | `/device-profiles` route replaced with `/policies` → `Policies` |
| `admin/src/components/layout/Sidebar.tsx` | `Device Profiles` nav item replaced with `Policies` (same position, same `ShieldCheck` icon, same admin-only gating) |
| `admin/src/pages/DeviceProfiles.tsx` | "Manage →" now navigates to `/policies/device-profiles/${id}` (forward-looking — `DeviceProfileDetail.tsx` itself is still Phase 1's pending M4-A3, not built in this phase) |

## Scope notes

- **Linux only.** OS dropdown only enables Linux; Windows/macOS are visibly present but
  disabled, matching PENDING-08's own Linux-v1 scope.
- **"Connect Trust Methods" is a pure stub.** No trust-method provider exists anywhere
  in this codebase; it's a `toast()`, not a route or feature.
- **Manual Trust cannot be disabled via this panel or the mutation** — `manualTrust:
  false` is rejected server-side (`ErrNoVerificationMethod`) because it's currently the
  only verification method that exists; the button is also non-interactive in the UI to
  avoid implying otherwise.
- **DeviceProfileDetail.tsx is still not built** — Phase 1's M4-A3 remains open. This
  phase only changes where the list lives and how creation starts.

## Build Check
```bash
cd controller && go build ./... && go test ./internal/posture/... ./graph/...
cd admin && npm run codegen && npm run build
```

## Implementation Checklist
- [x] **M4-B1** Migration + store changes for `manual_trust_enabled` (backend, M1-boundary exception).
- [x] **M4-B2** `posture.graphqls` + resolver changes for `manualTrust` field and `updateDeviceProfileManualTrust` mutation.
- [x] **M4-B3** Backend tests (store unit + integration + resolver).
- [x] **M4-B4** Admin GraphQL operations updated + codegen.
- [x] **M4-B5** `Switch` UI component.
- [x] **M4-B6** `CreateDeviceProfileModal.tsx` rebuilt: OS step + mockup-matched Linux panel.
- [x] **M4-B7** `Policies.tsx` page with Device Profiles tab.
- [x] **M4-B8** Routes + sidebar nav updated; old `/device-profiles` route removed.
- [x] **Build gate:** controller `go build`/`go test` + admin `codegen`/`build`, all green.
- [ ] Component tests for `Policies.tsx` and the rebuilt `CreateDeviceProfileModal.tsx` (still outstanding — Phase 1's M4-A5 test debt carries forward and now also covers this phase's new surface).
- [ ] Manual/E2E pass in a running app (not done this session — no dev server was started against a live backend).

## Post-Phase Fixes
_None yet._
