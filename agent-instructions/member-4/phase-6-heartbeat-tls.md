# Phase 6 — Rust Connector: Heartbeat + TLS

Implement the mTLS heartbeat loop and SPIFFE-based controller verification.

---

## Files to Create

```
connector/src/heartbeat.rs
connector/src/tls.rs
```

---

## `connector/src/heartbeat.rs` — Heartbeat Loop

1. Build mTLS config: client cert + key, trust root = workspace CA chain
2. Post-handshake: call `verify_controller_spiffe(peer_cert_der)` from `tls.rs`
3. Create tonic channel + `ConnectorServiceClient`
4. Loop every `HEARTBEAT_INTERVAL_SECS`:
   - Send `HeartbeatRequest { connector_id, version, hostname, public_ip }`
   - On success: reset failure counter, log if `re_enroll` is true (no action yet), log if new version available
   - On failure: exponential backoff (5s, 10s, 20s, 40s, 60s cap)

---

## `connector/src/tls.rs` — SPIFFE Verification

`verify_controller_spiffe(cert_der)` — parse X.509, find SAN URI matching `appmeta::SPIFFE_CONTROLLER_ID`.

**Reject if SPIFFE ID doesn't match** — prevents a rogue server signed by the same CA from impersonating the controller.

---

## Important Rules

1. **`re_enroll` handling:** The `HeartbeatResponse.re_enroll` field will always be `false` this sprint. Your Rust code should read it, log a warning if `true`, but take no action. This prepares for next sprint's auto-renewal without requiring a proto change.
2. **Needs enrollment working first** (Phase 5) — you need the client cert and key.

---

## Phase 6 Checklist

```
✓ heartbeat.rs implements mTLS heartbeat loop
✓ TLS config built with client cert + key + CA chain
✓ Controller SPIFFE ID verified post-handshake
✓ Heartbeat requests sent at configured interval
✓ Exponential backoff on failures (5s → 60s cap)
✓ re_enroll field read and logged (no action this sprint)
✓ tls.rs verifies controller SPIFFE SAN URI
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 7 (auto-updater).
