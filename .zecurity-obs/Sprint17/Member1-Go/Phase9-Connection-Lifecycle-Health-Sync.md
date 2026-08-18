---
type: phase
member: M1
sprint: 17
phase: 9
title: Connection Lifecycle + Identity Health + Sync Instances
status: planned
depends_on: [5]
tags: [go, identity, scim, lifecycle, health, sync-instance, pending-05]
---

# Phase 9 — Connection Lifecycle + Identity Health + Sync Instances

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §12, Addendum · [[PENDING-05-SCIM-Implementation-Plan]] P9 · [[Identity-Lifecycle-and-Ownership-Design-Review]] §8.

## Goal
Give a SCIM connection a safe lifecycle, surface sync health, and make disable→re-enable reconnects clean.

## Files
| File | Change |
| --- | --- |
| `controller/internal/idp/store.go` | connection status transitions; `last_sync_at` |
| `controller/internal/scim/sync_instance.go` | **new** — sync-instance open/stamp/reconcile |
| `controller/graph/*` | Identity Health query |

## Steps
- [ ] Connection `status: active → disabled → deleted`. **DISABLE** (reversible): new logins fail, sessions revoked, linked users **suspended**, `provisioning_owner scim→unmanaged`. **DELETE** guarded: only when `linked_users == 0` or explicit destructive confirmation.
- [ ] `last_sync_at` → **Identity Health**: Healthy / Delayed / Disconnected. (Device-trust delivery is durable via the merged outbox — no "not configured" caveat.)
- [ ] `scim_sync_instances`: open a UUID per connect; stamp `sync_instance_id` on provisioned users/external_identities; reconcile current-vs-stale on reconnect.

## Rules
- DISABLE is the reversible off-switch; DELETE never silently mass-suspends. Users are never orphaned active-with-no-login-path.

## Build gate
`go build ./...` + tests: disable→suspend→re-enable→restore; delete guard; health state.
