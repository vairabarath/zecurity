---
type: task
status: done
sprint: 5
member: M4
phase: 2
priority: normal
depends_on:
  - M4-Phase1 (resources.rs + SharedResourceState)
  - M3-C2 (agent_server.rs relays instructions in HeartbeatResponse)
unlocks:
  - End-to-end integration test
tags:
  - rust
  - shield
  - heartbeat
---

# M4 · Phase 2 — Shield Heartbeat Resource Ack Wiring

> Wire `resources.rs` into the heartbeat loop: receive instructions from `HeartbeatResponse`, apply nftables, send acks back in `HeartbeatRequest`.

---

## Files to Modify

| File | Action |
|------|--------|
| `shield/src/heartbeat.rs` | MODIFY — handle resources in response, send acks in request (historical — now control_stream.rs) |

---

## Checklist

### Modify `shield/src/heartbeat.rs` (historical — now `shield/src/control_stream.rs`)

The heartbeat function signature needs `Arc<SharedResourceState>` added:
```rust
pub async fn run(
    state: ShieldState,
    cfg: ShieldConfig,
    resource_state: Arc<resources::SharedResourceState>,
) -> Result<()>
```

#### On building `HeartbeatRequest`:
```rust
// Drain pending acks from resource_state
let acks = resource_state.acks.lock().unwrap().drain(..).collect::<Vec<_>>();

HeartbeatRequest {
    shield_id:      state.shield_id.clone(),
    status:         "active".to_string(),
    version:        appmeta::VERSION.to_string(),
    hostname:       util::read_hostname(),
    lan_ip:         util::detect_lan_ip().unwrap_or_default(),
    resource_acks:  acks,   // NEW — send accumulated acks
}
```

#### On receiving `HeartbeatResponse`:
```rust
// Process resource instructions
for instruction in resp.resources {
    match instruction.action.as_str() {
        "apply" => {
            // 1. Validate host
            if !resources::validate_host(&instruction.host) {
                // Push failed ack immediately
                resource_state.acks.lock().unwrap().push(ResourceAck {
                    resource_id:    instruction.resource_id.clone(),
                    status:         "failed".to_string(),
                    error:          "resource host does not match this shield's IP".to_string(),
                    verified_at:    now_unix(),
                    port_reachable: false,
                });
                continue;
            }
            // 2. Add to active resources
            resource_state.active.lock().unwrap().push(ActiveResource {
                resource_id: instruction.resource_id.clone(),
                protocol:    instruction.protocol.clone(),
                port_from:   instruction.port_from as u16,
                port_to:     instruction.port_to as u16,
            });
            // 3. Rebuild nftables chain
            let active = resource_state.active.lock().unwrap().clone();
            if let Err(e) = resources::apply_nftables(&active) {
                // Push failed ack
                resource_state.acks.lock().unwrap().push(ResourceAck {
                    resource_id:    instruction.resource_id.clone(),
                    status:         "failed".to_string(),
                    error:          e.to_string(),
                    verified_at:    now_unix(),
                    port_reachable: false,
                });
            } else {
                // Push protecting ack (health check loop will upgrade to protected)
                resource_state.acks.lock().unwrap().push(ResourceAck {
                    resource_id:    instruction.resource_id.clone(),
                    status:         "protecting".to_string(),
                    error:          String::new(),
                    verified_at:    now_unix(),
                    port_reachable: resources::check_port(instruction.port_from as u16),
                });
            }
        }
        "remove" => {
            // 1. Remove from active list
            resource_state.active.lock().unwrap()
                .retain(|r| r.resource_id != instruction.resource_id);
            // 2. Rebuild nftables without this resource
            let active = resource_state.active.lock().unwrap().clone();
            resources::apply_nftables(&active).ok();
            // 3. Push removed ack
            resource_state.acks.lock().unwrap().push(ResourceAck {
                resource_id:    instruction.resource_id.clone(),
                status:         "removed".to_string(),
                error:          String::new(),
                verified_at:    now_unix(),
                port_reachable: false,
            });
        }
        _ => {}
    }
}
```

- [ ] `HeartbeatRequest` includes `resource_acks` from drained state
- [ ] `HeartbeatResponse.resources` processed — apply/remove handled
- [ ] Host validation runs before nftables — failed ack sent if mismatch
- [ ] nftables rebuilt atomically after each instruction
- [ ] `protecting` ack sent immediately after successful nftables apply
- [ ] Health check loop (Phase 1) upgrades status to `protected` via periodic acks
- [ ] `remove` action cleans up active list + rebuilds chain + sends `removed` ack

---

## Status Flow Summary

```
Instruction received (action=apply)
  → validate_host OK?
      NO  → ack status=failed (host mismatch)
      YES → add to active + apply_nftables
              → nftables error? → ack status=failed
              → success?        → ack status=protecting
                                → health check loop (30s)
                                    → port reachable? → ack status=protected
                                    → port down?      → ack status=failed

Instruction received (action=remove)
  → remove from active + rebuild nftables
  → ack status=removed
```

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml   # must pass
```

---

## Integration Test

```bash
# 1. Start nginx on shield host: port 80
# 2. Create resource via Admin UI: host=<shield_lan_ip>, port=80, protocol=tcp
# 3. Click Protect → status=managing
# 4. Wait 60s (one heartbeat cycle)
# 5. Check: nft list ruleset → shows resource_protect chain with dport 80 rule
# 6. From another host: nc -zv <shield_lan_ip> 80 → Connection refused
# 7. Status in UI → protected
# 8. Stop nginx → wait 90s → status=failed
# 9. Start nginx → wait 90s → status=protected
# 10. Click Unprotect → wait 60s → rule removed → nc -zv succeeds
```

---

## Related

- [[Sprint5/Member4-Rust-Shield/Phase1-Resources-Module]] — depends on resources.rs
- [[Sprint5/Member3-Go-Controller/Phase2-Heartbeat-Relay]] — delivers instructions
- [[Sprint5/path.md]] — dependency map
