---
type: task
status: pending
sprint: 4
member: M3
phase: 5
depends_on:
  - "M2 Phase 1: proto/shield/v1/shield.proto committed"
  - "M4 has NOT committed connector/src/agent_server.rs (M3 owns this file)"
unlocks:
  - M4 Phase 7 (connector/src/main.rs starts the server M3 wrote)
  - M4 Phase 4 (Shield heartbeat target exists)
tags:
  - rust
  - grpc
  - connector
  - agent-server
---

# M3 · Phase 5 — connector/src/agent_server.rs (Shield-facing gRPC Server)

> **Ownership note:** M3 writes `agent_server.rs`. M4 writes `connector/src/main.rs` to START it.
> Agree on the `ShieldServer::new()` public signature before starting.

**Depends on: shield.proto committed + connector.proto's ShieldHealth defined.**

---

## Goal

Implement the Shield-facing gRPC server that runs on Connector `:9091`. This server handles post-enrollment Shield communication: Heartbeat, RenewCert, Goodbye. It does NOT handle Enroll (Shields enroll directly with Controller).

---

## File to Create

`connector/src/agent_server.rs`

---

## Checklist

### Module setup

- [ ] Add `pub mod agent_server;` to `connector/src/main.rs` (coordinate with M4)
- [ ] Import shield proto: `include_proto!("shield.v1")` in the module
- [ ] Add tonic `ShieldService` trait implementation

### ShieldServer struct

- [ ] `pub struct ShieldServer` with:
  ```rust
  pub struct ShieldServer {
      // shield_id → ShieldEntry { status, version, last_seen: Instant, cert_not_after: DateTime }
      shields: Arc<Mutex<HashMap<String, ShieldEntry>>>,
      // mTLS channel to Controller for forwarding RenewCert
      controller_channel: Channel,
      // Connector's trust domain (to validate Shield SPIFFE certs)
      trust_domain: String,
      // Connector's own ID (for validating Shields belong to this Connector)
      connector_id: String,
      // Renewal window — return re_enroll=true when cert < this duration remaining
      renewal_window_secs: u64,
  }
  ```
- [ ] `pub fn new(controller_channel: Channel, trust_domain: String, connector_id: String) -> Self`
- [ ] `pub fn get_alive_shields(&self) -> Vec<ShieldHealth>` — returns snapshot for Connector's HeartbeatRequest

### Heartbeat handler

- [ ] Extract Shield SPIFFE identity from mTLS peer cert:
  - Verify: `spiffe://<trust_domain>/shield/<shield_id>`
  - Verify `trust_domain` matches Connector's own trust domain
  - Return `PERMISSION_DENIED` if mismatch
- [ ] Update `shields` map: `shield_id → ShieldEntry { last_seen: now(), status: "active" }`
- [ ] Check peer cert `not_after` — if within `renewal_window_secs` → `re_enroll = true`
- [ ] Return `HeartbeatResponse { ok: true, re_enroll, latest_version: "" }`

### RenewCert handler

- [ ] Verify Shield SPIFFE identity (same as Heartbeat)
- [ ] Forward `RenewCertRequest` to Controller via `controller_channel` (existing mTLS channel)
- [ ] Return Controller's `RenewCertResponse` to Shield
- [ ] The Connector is a transparent proxy — no PKI work here

### Goodbye handler

- [ ] Verify Shield SPIFFE identity
- [ ] Remove Shield from `shields` map (it will be absent from next ShieldHealth batch)
- [ ] Return `GoodbyeResponse { ok: true }`

### Enroll handler (UNIMPLEMENTED)

- [ ] Return `Status::unimplemented("Shield enrolls directly with Controller, not through Connector")`

### get_alive_shields()

- [ ] Returns `Vec<ShieldHealth>` for use in `heartbeat.rs`:
  ```rust
  pub fn get_alive_shields(&self) -> Vec<ShieldHealth> {
      self.shields.lock().unwrap()
          .iter()
          .map(|(id, entry)| ShieldHealth {
              shield_id:         id.clone(),
              status:            entry.status.clone(),
              version:           entry.version.clone(),
              last_heartbeat_at: entry.last_seen.timestamp(),
          })
          .collect()
  }
  ```

### mTLS setup (for the :9091 server itself)

- [ ] Server uses Connector's own cert (`connector.crt`) for mTLS
- [ ] Trust root: `workspace_ca.crt` (same CA that signed Shield certs)
- [ ] Require client auth — Shield must present its SPIFFE cert

---

## Build Check

```bash
cd connector && cargo build
# agent_server module compiles cleanly
# ShieldService trait fully implemented
```

---

## Coordination with M4

Before M3 starts this file, agree with M4 on:
1. `ShieldServer::new()` signature (M4 calls it in `main.rs`)
2. `get_alive_shields()` return type (M4 uses it in `heartbeat.rs`)
3. Module path: `crate::agent_server::ShieldServer`

---

## Notes

- The `shields` HashMap is in-memory only. On Connector restart, the map is empty. Shields will re-populate within one heartbeat interval (30s).
- `cert_not_after` for checking renewal: extract from the peer cert during TLS handshake using the `x509-parser` crate.
- The Connector does not persist Shield state to disk. Persistence is the Controller's job.

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase7-CI-Connector-Main]] — starts this server
- [[Sprint4/Member4-Rust-Shield-CI/Phase4-Heartbeat-Renewal]] — Shield heartbeat target
- [[Services/Connector]] — update after this phase
