---
type: phase
member: M1
sprint: 17
phase: 7
title: SCIM Groups
status: planned
depends_on: [5]
tags: [go, identity, scim, groups, policy, pending-05]
---

# Phase 7 — SCIM Groups

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §6, §8 · [[PENDING-05-SCIM-Implementation-Plan]] P7.

## Goal
Sync directory groups + membership so the policy engine reads directory-accurate membership — making
"groups are hints, not authorization" operationally true.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/groups.go` | **new** — POST/PUT/PATCH group handlers + membership sync |
| `controller/internal/idp` / groups store | origin-aware group lookup/create |

## Steps
- [ ] `scim`-origin groups keyed on connection-scoped `external_id` (never display name); coexist with `manual`/`system` groups of the same name.
- [ ] Sync `group_members` from SCIM membership ops; call `policy.Notifier.NotifyPolicyChange` on change (same as a manual edit).
- [ ] Out-of-order membership (member references a not-yet-provisioned user) → **`404`**, no dangling membership; provider retries.
- [ ] All access-rule / membership references use an **origin-aware identifier**.

## Rules
- SCIM-group membership is directory-authoritative; manual-group membership stays admin-authoritative. No cross-origin auto-merge.

## Build gate
`go build ./...` + tests: group create/sync, out-of-order 404, ACL snapshot invalidation.
