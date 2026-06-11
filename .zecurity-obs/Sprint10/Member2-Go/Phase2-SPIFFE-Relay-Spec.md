---
type: phase
sprint: 10
member: M2
phase: 2
status: planned
---

# M2 Phase 2 — SPIFFE Relay Validation Spec

## What You're Building

Design ownership phase — M2 owns the security spec that the relay must enforce. M3 implements it in `relay/src/spiffe.rs`. M2 reviews that implementation.

---

## SPIFFE Validation Rules for Relay

### Connector Certificate (RegisterMsg side)

- URI SAN MUST match: `spiffe://<trust_domain>/connector/<uuid>`
- Reject if: no URI SAN, wrong path prefix, non-UUID ID

### Client Device Certificate (LookupMsg side)

- URI SAN MUST match: `spiffe://<trust_domain>/client_device/<uuid>`
- Reject if: no URI SAN, wrong path prefix

### Workspace Isolation Rule

When client sends `LookupMsg { connector_id }`:
1. Extract `trust_domain` from client SPIFFE URI
2. Extract `trust_domain` from stored connector SPIFFE URI
3. They MUST be equal — different trust domains = different workspaces = reject
4. Error: `"workspace mismatch: client and connector are in different workspaces"`

### Functions in `relay/src/spiffe.rs`

```rust
pub fn extract_spiffe_uri(cert: &[u8]) -> Option<String>
pub fn parse_trust_domain(spiffe_uri: &str) -> Option<&str>
pub fn validate_connector_spiffe(spiffe_uri: &str) -> bool
pub fn validate_client_spiffe(spiffe_uri: &str) -> bool
pub fn same_workspace(connector_spiffe: &str, client_spiffe: &str) -> bool
```

All return `false`/`None` on malformed input — never panic.

### What Relay Does NOT Check

- Does NOT verify client SPIFFE is in any ACL (Connector handles this)
- Does NOT call the controller
- Does NOT validate certificate expiry (mTLS handles this)

---

## Review Checklist (M2 reviews M3's implementation)

- [ ] `validate_connector_spiffe` rejects `spiffe://domain/shield/id`
- [ ] `validate_client_spiffe` rejects `spiffe://domain/connector/id`
- [ ] `same_workspace` returns false for different trust domains
- [ ] Functions return false for empty strings — no panics
- [ ] No logging of full certificate contents

---

## Post-Phase Fixes

*(Empty)*
