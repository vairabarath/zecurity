---
type: phase
member: M2
sprint: 11.2
phase: 2
title: Controller — Populate preferred_connector_id
status: completed
commit: f88600c
depends_on:
  - Sprint11.2/Member2-Go/Phase1-Proto
---

# Phase 2 — Controller: Populate preferred_connector_id

## Goal

Populate `preferred_connector_id` on `AclEntry` in the ACL compiler for shield
routes. When a Shield reports its current attached connector via heartbeat, the
controller embeds that connector's ID in the compiled ACL so the client knows
to try it first.

## Files

| File | Change |
|---|---|
| `controller/internal/policy/compiler.go` | Populate `preferred_connector_id` on shield-routed entries |
| `controller/internal/policy/store.go` | Query updates to supply preferred connector data |
| `controller/internal/shield/heartbeat.go` | Set preferred connector when shield's connector is known |
| `controller/internal/connector/control_stream.go` | Plumb preferred connector through control stream |

## Implementation Checklist

- [x] **M2-B1** `compiler.go` — populate `preferred_connector_id` on `AclEntry` for shield routes where Shield's current connector is known
- [x] **M2-B2** `store.go` — updated query to supply preferred connector ID alongside shield route data
- [x] **M2-B3** `shield/heartbeat.go` — set `preferred_connector_id` when the shield's current connector attachment is reported
- [x] **M2-B4** `connector/control_stream.go` — plumb preferred connector data through the control stream delivery path
- [x] **Build gate:** `cd controller && go build ./...` passes
