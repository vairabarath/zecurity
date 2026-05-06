---
type: task
status: done
sprint: 5
member: M4
phase: 1
priority: normal
depends_on:
  - M2-D1-A (shield.proto ResourceInstruction/Ack messages)
  - buf generate done (Rust stubs via build.rs)
unlocks:
  - M4-Phase2 (heartbeat ack wiring)
tags:
  - rust
  - shield
  - nftables
  - resources
---

# M4 · Phase 1 — Shield Resources Module

> Shield receives resource instructions from Connector, validates host IP, applies nftables rules, checks port liveness every 30s, reports acks.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `shield/src/resources.rs` | CREATE — full resource protection logic |
| `shield/src/config.rs` | MODIFY — add `resource_check_interval_secs` |
| `shield/src/main.rs` | MODIFY — register resources module + spawn health loop |

---

## Checklist

### 1. Create `shield/src/resources.rs`

#### `validate_host(resource_host: &str) → bool`
```rust
// Check if resource_host matches this Shield's LAN IP
// Uses detect_lan_ip() from util.rs
// Also allow "127.0.0.1" (explicit loopback)
pub fn validate_host(resource_host: &str) -> bool {
    if resource_host == "127.0.0.1" {
        return true;
    }
    match util::detect_lan_ip() {
        Some(my_ip) => my_ip == resource_host,
        None => false,
    }
}
```

#### `check_port(port: u16) → bool`
```rust
// Non-blocking TCP connect to localhost to verify port is listening
pub fn check_port(port: u16) -> bool {
    use std::net::TcpStream;
    use std::time::Duration;
    TcpStream::connect_timeout(
        &format!("127.0.0.1:{}", port).parse().unwrap(),
        Duration::from_secs(2),
    ).is_ok()
}
```

#### `apply_nftables(resources: &[ActiveResource]) → Result<()>`
```rust
// Atomically flush + rebuild chain resource_protect
// Steps:
// 1. nft flush chain inet zecurity resource_protect
//    (or: nft add chain inet zecurity resource_protect { type filter hook input priority 0\; }
//         if chain doesn't exist yet)
// 2. For each resource:
//    allow iifname "lo" {protocol} dport {port_from}-{port_to}
//    allow ip saddr 127.0.0.0/8 {protocol} dport {port_from}-{port_to}
//    drop {protocol} dport {port_from}-{port_to}
// 3. Rebuild entire chain atomically — never append incrementally

// Use nftables crate or shell out to `nft` binary
```

#### `remove_resource_from_nftables(resource_id: &str, resources: &[ActiveResource]) → Result<()>`
```rust
// Remove resource from active list, then call apply_nftables on remaining
// (rebuild the whole chain without this resource)
```

#### `ActiveResource` struct:
```rust
pub struct ActiveResource {
    pub resource_id: String,
    pub protocol:   String,   // "tcp", "udp", "any"
    pub port_from:  u16,
    pub port_to:    u16,
}
```

#### `SharedResourceState` — thread-safe state:
```rust
pub struct SharedResourceState {
    pub active:  Mutex<Vec<ActiveResource>>,
    pub acks:    Mutex<Vec<ResourceAck>>,  // pending acks to send in next heartbeat
}
```

#### `run_health_check_loop(interval_secs: u64, state: Arc<SharedResourceState>)`
```rust
// Every interval_secs (30s):
// For each active resource:
//   reachable = check_port(resource.port_from)
//   push ResourceAck {
//     resource_id: resource.resource_id,
//     status: if reachable { "protected" } else { "failed" },
//     error: if !reachable { "port not listening" } else { "" },
//     verified_at: SystemTime::now() unix timestamp,
//     port_reachable: reachable,
//   }
//   into state.acks (replacing any previous ack for same resource_id)
```

- [ ] `validate_host` implemented using `detect_lan_ip()`
- [ ] `check_port` uses 2s timeout connect to `127.0.0.1:{port}`
- [ ] `apply_nftables` flushes + rebuilds `chain resource_protect` atomically
- [ ] `ActiveResource` and `SharedResourceState` structs defined
- [ ] `run_health_check_loop` runs every 30s, pushes acks into shared state

### 2. Modify `shield/src/config.rs`

- [ ] Add field:
  ```rust
  #[serde(default = "default_resource_check_interval")]
  pub resource_check_interval_secs: u64,
  ```
- [ ] Add default function:
  ```rust
  fn default_resource_check_interval() -> u64 { 30 }
  ```

### 3. Modify `shield/src/main.rs`

- [ ] Add `mod resources;`
- [ ] Create `Arc<SharedResourceState>` shared between heartbeat + health check loop
- [ ] Spawn health check loop:
  ```rust
  let state_clone = Arc::clone(&resource_state);
  tokio::spawn(resources::run_health_check_loop(
      cfg.resource_check_interval_secs,
      state_clone,
  ));
  ```
- [ ] Pass `resource_state` into `heartbeat::run()`

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml   # must pass
```

---

## Notes

- `chain resource_protect` is separate from `chain input` (which handles connector access). They coexist in `table inet zecurity`.
- Flushing + rebuilding the chain on every instruction change ensures no stale rules accumulate — even if Shield restarts mid-apply.
- The health check loop runs regardless of whether any resources are active — it's a no-op when `active` is empty.
- Port range: if `port_from == port_to`, use single port in nftables rule. If different, use range `{port_from}-{port_to}`.

---

## Post-Phase Fixes

### Fix: Resource firewall should allow Shield-local relay, not Shield TUN
**Issue:** The per-resource chain allowed `iifname "zecurity0"` before dropping the protected port, but the current protected-resource dataplane uses a Shield-local socket opened from `TunnelOpen`.

**Root Cause:** The original resource firewall rule assumed packet-forwarded tunnel traffic entered through the Shield interface. Sprint 9 moved protected access to Connector → Shield Control-stream relay.

**Fix Applied (`shield/src/resources.rs`):**
```text
allow iifname "lo" <proto> dport <port>
allow ip saddr 127.0.0.0/8 <proto> dport <port>
drop  <proto> dport <port>
```

---

## Related

- [[Sprint5/Member4-Rust-Shield/Phase2-Heartbeat-Ack]] — next phase
- [[Sprint5/Member3-Go-Controller/Phase2-Heartbeat-Relay]] — delivers instructions to this module
- [[Sprint5/path.md]] — dependency map
