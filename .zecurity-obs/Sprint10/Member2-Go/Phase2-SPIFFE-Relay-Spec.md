---
type: phase
sprint: 10
member: M2
phase: 2
status: planned
---

# M2 Phase 2 — SPIFFE Relay Validation Spec

## What You're Building

This is a **design ownership phase** — M2 owns the security spec that the relay must enforce. M3 implements it in `relay/src/spiffe.rs`. M2 reviews that implementation before Phase E is checked off.

## Files to Touch

| File | Change |
|------|--------|
| This file | Write the spec below |

---

## SPIFFE Validation Rules for Relay

### Connector Certificate (RegisterMsg side)

- Peer certificate MUST have a URI SAN
- URI SAN MUST match format: `spiffe://<trust_domain>/connector/<uuid>`
- `<trust_domain>` is the workspace trust domain (derived from the `ConnectorSPIFFE` stored in `RelayState`)
- Reject if: no URI SAN, wrong prefix path, non-UUID ID portion

### Client Device Certificate (LookupMsg side)

- Peer certificate MUST have a URI SAN
- URI SAN MUST match format: `spiffe://<trust_domain>/client_device/<uuid>`
- Reject if: no URI SAN, wrong prefix path

### Workspace Isolation Rule

When a client sends `LookupMsg { connector_id }`:
1. Relay looks up the connector entry by `connector_id`
2. Extract `trust_domain` from client SPIFFE URI
3. Extract `trust_domain` from stored connector SPIFFE URI
4. **They MUST be equal.** Different trust domains = different workspaces = reject.
5. Error message: `"workspace mismatch: client and connector are in different workspaces"`

### Implementation in `relay/src/spiffe.rs`

```rust
pub fn extract_spiffe_uri(cert: &rustls::Certificate) -> Option<String>
pub fn parse_trust_domain(spiffe_uri: &str) -> Option<&str>
pub fn validate_connector_spiffe(spiffe_uri: &str) -> bool
pub fn validate_client_spiffe(spiffe_uri: &str) -> bool
pub fn same_workspace(connector_spiffe: &str, client_spiffe: &str) -> bool
```

All functions return `false`/`None` on malformed input — never panic, never log full cert contents.

### What Relay Does NOT Check

- Relay does NOT verify that the client SPIFFE ID is in any ACL
- Relay does NOT call the controller
- Relay does NOT validate certificate expiry (mTLS handles this)
- Access control is handled by the Connector when the tunnel is established (same as direct path)

---

## Review Checklist (M2 reviews M3's implementation)

- [ ] `validate_connector_spiffe` rejects `spiffe://domain/shield/id` (wrong role)
- [ ] `validate_client_spiffe` rejects `spiffe://domain/connector/id` (wrong role)
- [ ] `same_workspace` returns false for `spiffe://workspace-a/...` vs `spiffe://workspace-b/...`
- [ ] Functions return false for empty strings — no panics
- [ ] No logging of full certificate contents (PII/security risk)

---

## Post-Phase Fixes

*(Empty — add fixes here as discovered)*
