---
type: phase
status: pending
sprint: 6
member: M4
phase: Phase2-Control-Stream-Wiring
depends_on:
  - M4-E1 (discovery.rs)
  - M2-D1-A (shield.proto DiscoveryReport)
  - buf generate
tags:
  - rust
  - shield
  - control-stream
  - wiring
---

# M4 Phase 2 — Control Stream Discovery Wiring

---

## What You're Building

Wire `discovery.rs` into the existing shield heartbeat / Control stream loop. Shield sends a full sync on first connect, then diffs every `discovery_interval_secs`.

---

## Files to Touch

### 1. `shield/src/config.rs` (MODIFY)

Add one field:

```rust
pub discovery_interval_secs: u64,
```

Default value: `60`.

Wire from figment/env: `DISCOVERY_INTERVAL_SECS` env var, fallback 60.

---

### 2. `shield/src/heartbeat.rs` (MODIFY)

Add discovery state variables before the heartbeat loop:

```rust
use std::collections::HashSet;

let mut discovery_sent: HashSet<(u16, String)> = HashSet::new();
let mut discovery_fingerprint: u64 = 0;
let mut discovery_seq: u64 = 0;
let mut first_connect = true;

let discovery_interval = tokio::time::Duration::from_secs(cfg.discovery_interval_secs);
let mut discovery_tick = tokio::time::interval(discovery_interval);
discovery_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
```

**Inside the Control stream send loop**, after sending `ShieldHealthReport`, add:

```rust
// On first connect: full sync; every interval: diff
let diff_opt = if first_connect {
    first_connect = false;
    match discovery::run_discovery_full_sync(
        &shield_id,
        &mut discovery_sent,
        &mut discovery_fingerprint,
        &mut discovery_seq,
    ).await {
        Ok(diff) => Some(diff),
        Err(e) => { warn!("discovery: full sync error: {}", e); None }
    }
} else {
    // Only run diff on discovery tick
    if discovery_tick.tick().now_or_never().is_some() {
        match discovery::run_discovery_diff(
            &shield_id,
            &mut discovery_sent,
            &mut discovery_fingerprint,
            &mut discovery_seq,
        ).await {
            Ok(opt) => opt,
            Err(e) => { warn!("discovery: diff error: {}", e); None }
        }
    } else {
        None
    }
};

if let Some(diff) = diff_opt {
    // Convert DiscoveryDiff → proto DiscoveryReport
    let proto_report = shieldpb::DiscoveryReport {
        shield_id:   diff.shield_id.clone(),
        seq:         diff.seq,
        fingerprint: diff.fingerprint,
        full_sync:   diff.full_sync,
        added: diff.added.iter().map(|s| shieldpb::DiscoveredService {
            protocol:     s.protocol.to_string(),
            port:         s.port as u32,
            bound_ip:     s.bound_ip.clone(),
            service_name: s.service_name.clone(),
        }).collect(),
        removed: diff.removed.iter().map(|(port, proto)| shieldpb::DiscoveredService {
            protocol:     proto.clone(),
            port:         *port as u32,
            bound_ip:     String::new(),
            service_name: String::new(),
        }).collect(),
    };

    let msg = shieldpb::ShieldControlMessage {
        body: Some(shieldpb::shield_control_message::Body::DiscoveryReport(proto_report)),
    };

    if let Err(e) = stream_tx.send(msg).await {
        warn!("discovery: failed to send report: {}", e);
    } else {
        info!("discovery: sent seq={} added={} removed={} full_sync={}",
            diff.seq, diff.added.len(), diff.removed.len(), diff.full_sync);
    }
}
```

---

### 3. `shield/src/main.rs` (MODIFY)

Add:

```rust
mod discovery;
```

---

## Important Notes

- `discovery_tick.tick().now_or_never()` polls the interval non-blockingly — it fires only when the interval has elapsed. This prevents discovery from blocking the heartbeat loop.
- On reconnect (if the heartbeat loop restarts), reset `first_connect = true` and clear `discovery_sent` so a fresh full sync is sent.
- The Control stream sender (`stream_tx`) is the same channel used for `ShieldHealthReport` and `ResourceAck` — just send another message on it.

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
```
