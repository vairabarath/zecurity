---
type: phase
member: M1
sprint: 17
phase: 3
title: Break-Glass Permission Primitive
status: planned
depends_on: []
tags: [go, identity, authz, permission, break-glass, pending-05]
---

# Phase 3 — Break-Glass Permission Primitive

> Independent (parallel with 1/2). Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §3.2 · [[PENDING-05-SCIM-Implementation-Plan]] P3.
> **Locked (Q2): this is a DEDICATED permission — ADMIN alone must NOT satisfy it.**

## Goal
The smallest fine-grained permission primitive needed to represent and check
`identity.mapping.break_glass`. Not a permission-system rewrite; not an ADMIN shortcut.

## Files
| File | Change |
| --- | --- |
| `controller/migrations/<TBD>.sql` | (fold into Phase-1 migration) `workspace_permissions` table |
| `controller/internal/permission/permission.go` | **new** — `HasPermission`, grant/revoke |
| `controller/graph/*` | grant/revoke mutations (audited) |

## Steps
- [ ] `workspace_permissions(workspace_id, user_id, permission, granted_by, granted_at)`.
- [ ] `HasPermission(ctx, workspace, user, "identity.mapping.break_glass") bool` — explicit possession only.
- [ ] Grant/revoke mutations (an ADMIN may grant, but possession is an explicit record, not implied by role); every grant/revoke audited.
- [ ] MFA hook present at the break-glass call site; enforced only where auth infra supports it (PENDING-06) — no-op stub until then.

## Rules
- ADMIN without an explicit grant → **denied**. Never widen ADMIN to cover this.

## Build gate
`go build ./...` + tests: explicit possession; ADMIN-without-grant denied; grant/revoke audited.
