---
type: phase
member: M2
sprint: 11.2
phase: 1
title: Proto — preferred_connector_id
status: completed
commit: f88600c
depends_on: []
---

# Phase 1 — Proto: preferred_connector_id

## Goal

Add `preferred_connector_id` to `AclEntry` so the controller can signal to the
client which connector to prefer for a given resource (particularly shield routes
where the shield's current connector is known).

## Files

| File | Change |
|---|---|
| `proto/client/v1/client.proto` | Add `preferred_connector_id = 10` to `AclEntry` |

## Proto Change

```protobuf
message AclEntry {
  // ... existing fields 1–9 ...
  string preferred_connector_id = 10; // set for shield routes when the Shield's current Connector is known
}
```

## Implementation Checklist

- [x] **M2-A1** `proto/client/v1/client.proto` — add `preferred_connector_id = 10` to `AclEntry`
- [x] **M2-A2** `buf generate` — regenerate Go stubs (`controller/gen/go/proto/client/v1/client.pb.go` updated)
- [x] **Build gate:** `cd controller && go build ./...` passes
