---
type: phase
status: done
sprint: 8
member: M4
phase: Phase1-ACL-Snapshot-Handling
depends_on:
  - M2-Phase1-Policy-Schema
  - M3 Compiler Output Contract documented
tags:
  - rust
  - client
  - connector
  - policy-engine
  - acl
---

# M4 Phase 1 — Connector ACL Snapshot Handling

---

## What You're Building

Wire ACL snapshots into the Rust connector. This sprint does not build RDE tunneling; it builds the local policy state RDE will depend on later.

Client active runtime state is daemon-required and moves to Sprint 8.5. See [[Decisions/ADR-002-Client-Daemon-Required]].

---

## Connector Work

Receive ACL snapshot from Connector heartbeat response and keep it locally.

Read the Compiler Output Contract in [[Sprint8/Member3-Go-Controller/Phase1-Policy-Compiler]] before implementing `is_allowed()` or resource resolution helpers.

Create a small policy cache module with helpers like:

```rust
pub fn is_allowed(&self, resource_id: &str, client_spiffe_id: &str) -> bool;
pub fn resolve_resource(&self, address: &str, port: u16, protocol: &str) -> Option<ResourceAcl>;
```

Rules:

- Missing snapshot = deny.
- Missing resource = deny.
- Missing SPIFFE ID = deny.
- Empty `allowed_spiffe_ids` = deny.
- M4 depends on M2's generated proto types and the documented compiler contract, not on M3's compiler implementation being complete.

Files likely touched:

- `connector/src/policy/`
- `connector/src/heartbeat.rs` or equivalent heartbeat client module
- `connector/src/main.rs`

---

## Build Check

```bash
cd connector && cargo build
```

## Deferred To Sprint 8.5

Client work moves to the daemon foundation:

- daemon-required IPC
- systemd user unit
- command refactor
- `GetACLSnapshot` fetch into daemon runtime state
- no optional direct-state fallback
