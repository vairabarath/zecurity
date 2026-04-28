---
type: task
status: done
sprint: 5
member: M3
phase: 2
priority: normal
depends_on:
  - M2-D1-A/B (proto ResourceInstruction/Ack messages)
  - M2-D1-C (007_resources migration)
  - M2-A2 (resource store functions)
  - M3-B1 (resolvers done — shared DB patterns)
  - buf generate done
unlocks:
  - M4 can test full heartbeat delivery end-to-end
tags:
  - go
  - rust
  - heartbeat
  - relay
---

# M3 · Phase 2 — Heartbeat Resource Relay

> This phase wires the delivery mechanism: Controller injects resource instructions into Connector heartbeat responses, Connector caches and forwards to Shield, Shield acks come back the same way.

---

## Files to Modify

| File | Action |
|------|--------|
| `controller/internal/connector/heartbeat.go` | MODIFY — inject resources + process acks |
| `connector/src/agent_server.rs` | MODIFY — cache + relay per shield |
| `connector/src/heartbeat.rs` | MODIFY — forward acks to Controller (historical — now control_stream.rs) |

---

## Checklist

### 1. Modify `controller/internal/connector/heartbeat.go`

After processing the existing heartbeat (connector status + shield health updates), add:

#### Inject pending resource instructions into HeartbeatResponse:
```go
// For each active shield in req.Shields:
//   pending, err := resource.GetPendingForShield(ctx, db, shieldID)
//   if len(pending) > 0:
//     resp.ShieldResources[shieldID] = toProtoInstructions(pending)

func toProtoInstructions(resources []resource.Resource) *connectorpb.ShieldResourceInstructions {
    // map each resource to ResourceInstruction proto
    // action = "apply" if status == "managing"
    // action = "remove" if status == "removing"
}
```

#### Process ResourceAck from Connector HeartbeatRequest:
```go
// For each ack in req.ResourceAcks:
//   resource.RecordAck(ctx, db, ResourceAck{
//     ResourceID:    ack.ResourceId,
//     Status:        ack.Status,
//     Error:         ack.Error,
//     VerifiedAt:    ack.VerifiedAt,
//     PortReachable: ack.PortReachable,
//   })
```

- [ ] `HeartbeatResponse` now includes `shield_resources` map
- [ ] `req.ResourceAcks` processed and persisted via `resource.RecordAck`
- [ ] Status transitions: `managing → protecting/failed`, `removing → deleted/failed`
- [ ] Only active shields (in `req.Shields`) get resource instructions injected

### 2. Modify `connector/src/agent_server.rs`

Add resource instruction cache to `ShieldServer`:

```rust
pub struct ShieldServer {
    // existing fields...
    resource_instructions: Arc<Mutex<HashMap<String, Vec<ResourceInstruction>>>>,
    // shield_id → pending instructions received from Controller heartbeat
    pending_acks: Arc<Mutex<Vec<ResourceAck>>>,
    // collected from all shield heartbeats, drained on Connector heartbeat
}
```

#### In Shield `Heartbeat` handler:
```rust
// 1. Look up cached instructions for this shield_id
//    let instructions = resource_instructions.lock().get(shield_id).cloned()
// 2. Put instructions into HeartbeatResponse.resources
// 3. Collect ResourceAck from HeartbeatRequest.resource_acks
//    → push into pending_acks
```

#### New method `update_resource_instructions(shield_id, instructions)`:
```rust
// Called by connector heartbeat when Controller response has shield_resources
// Updates the cache for the given shield_id
```

#### New method `drain_resource_acks() → Vec<ResourceAck>`:
```rust
// Called by connector heartbeat — drains and returns all pending acks
// Clears the vec after draining
```

- [ ] `ShieldServer` has `resource_instructions` + `pending_acks` fields
- [ ] Shield HeartbeatResponse includes cached instructions for that shield
- [ ] Shield ResourceAcks collected into `pending_acks`
- [ ] `update_resource_instructions` and `drain_resource_acks` public methods

### 3. Modify `connector/src/heartbeat.rs` (historical — now `connector/src/control_stream.rs`)

After receiving `HeartbeatResponse` from Controller:
```rust
// Process shield_resources from response:
for (shield_id, instructions) in resp.shield_resources {
    shield_server.update_resource_instructions(&shield_id, instructions.instructions);
}

// Before building HeartbeatRequest:
let resource_acks = shield_server.drain_resource_acks();
// Include in HeartbeatRequest.resource_acks
```

- [ ] `HeartbeatResponse.shield_resources` processed and cached in ShieldServer
- [ ] `HeartbeatRequest.resource_acks` populated from drained acks
- [ ] No acks lost between heartbeat cycles (drain is atomic)

---

## Build Check

```bash
cd controller && go build ./...     # must pass
cd connector && cargo build         # must pass (warnings OK)
```

---

## Notes

- The Connector acts as a **transparent relay** — it does not interpret resource instructions, just caches and forwards.
- Acks are drained once per Connector heartbeat cycle (60s). This means worst-case Controller knows about Shield status within 60s of Shield applying nftables.
- If Connector restarts, cached instructions are lost — Controller will re-send them on next heartbeat since status is still `managing`.

---

## Related

- [[Sprint5/Member3-Go-Controller/Phase1-Resolvers]] — depends on resource store
- [[Sprint5/Member4-Rust-Shield/Phase1-Resources-Module]] — consumes delivered instructions
- [[Sprint5/path.md]] — dependency map
