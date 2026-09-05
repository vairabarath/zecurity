---
type: phase
member: M1
sprint: 17
phase: 8
title: Identity Conflict Workflow
status: done
depends_on: [5]
tags: [go, identity, scim, conflict, governance, pending-05]
---

# Phase 8 — Identity Conflict Workflow

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §4.1, §9 · [[PENDING-05-SCIM-Implementation-Plan]] P8.

## Goal
When SCIM hits an existing JIT/manual identity for the same Canonical Identity Key, do **not** take over
silently — raise a `409` and hold a pending conflict for an admin to resolve. (This is the safe,
audited version of contractor→employee; full linking/conversion remains Stage 4 / ADR-026.)

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/conflict.go` | **new** — conflict detection + resolution |
| `controller/graph/*` | Provisioning-Conflicts admin queries/mutations |

## Steps
- [ ] On collision → `409 identity_conflict`; create one **pending** `scim_identity_conflicts` row per `(workspace, connection, canonical_identity_key)` (unique partial index); reuse existing pending on retry.
- [ ] Consistent across POST/PUT/PATCH/DELETE while pending/rejected; never mutate the unrelated JIT/manual user; a rejected conflict never auto-approves.
- [ ] **Accept-Link** (admin-authorized): atomic — confirm `external_identities` link + set `provisioning_owner→scim` (keep immutable `provisioned_by`), preserve local roles/policies/devices, make directory attrs SCIM-controlled, write `scim.user.conflict_approved` in the same tx.
- [ ] **Reject** / **Reopen** (audited: `scim.user.conflict_reopened`).

## Rules
- Never resolve by email. Accept-Link requires explicit admin authorization.

## Build gate
`go build ./...` + tests: 409 on collision, uniqueness, "rejected never auto-approves", Accept-Link atomicity.
