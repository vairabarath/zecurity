---
type: task
status: pending
sprint: 4
member: M4
phase: 4
depends_on:
  - Phase3-Enrollment (state.json exists, connector_addr known)
  - "M3 Phase 5: connector/src/agent_server.rs live on :9091"
unlocks:
  - Shield shows ACTIVE in dashboard
  - Cert renewal works end-to-end
tags:
  - rust
  - shield
  - heartbeat
  - renewal
  - mtls
---

# M4 · Phase 4 — Heartbeat + Renewal

**Depends on: enrollment works + M3's agent_server.rs is live on Connector :9091.**

---

## Goal

Implement the Shield heartbeat loop (mTLS to Connector :9091) and cert renewal (RenewCert proxied through Connector → Controller).

---

## Files to Create

| File | Purpose |
|------|---------|
| `shield/src/heartbeat.rs` | mTLS heartbeat loop to Connector :9091 |
| `shield/src/renewal.rs` | RenewCert flow (proof-of-possession CSR) |

---

## Checklist

### heartbeat.rs

- [ ] `pub async fn run(state: ShieldState, cfg: ShieldConfig) -> anyhow::Result<()>`
- [ ] Build mTLS config:
  - Client cert: `shield.crt`
  - Client key: `shield.key`
  - Trust root: `workspace_ca.crt`
  - Post-handshake: `tls::verify_connector_spiffe(peer_cert, expected_spiffe_id)`
    ```rust
    let expected = format!("spiffe://{}/{}/{}",
        state.trust_domain,
        appmeta::SPIFFE_ROLE_CONNECTOR,
        state.connector_id);
    ```
- [ ] Connect to `state.connector_addr` (e.g. `192.168.1.10:9091`)
- [ ] Create `ShieldServiceClient::new(channel)`
- [ ] Loop every `cfg.shield_heartbeat_interval_secs`:
  ```rust
  let req = HeartbeatRequest {
      shield_id: state.shield_id.clone(),    // logging on Connector side
      version:   env!("CARGO_PKG_VERSION").to_string(),
      hostname:  util::read_hostname(),
      public_ip: util::get_public_ip().unwrap_or_default(),
  };
  ```
- [ ] On `Ok(resp)`:
  - Reset `consecutive_failures = 0`
  - If `resp.re_enroll` → call `renewal::renew_cert(&mut state, &cfg).await`
- [ ] On `Err(e)`:
  - `consecutive_failures += 1`
  - `backoff = min(5 * 2^(failures-1), 60)` seconds
  - `warn!("heartbeat to connector failed attempt={}", failures)`
  - `tokio::time::sleep(Duration::from_secs(backoff)).await`
- [ ] `pub async fn goodbye(state: &ShieldState, cfg: &ShieldConfig)` — best-effort Goodbye call on SIGTERM

### renewal.rs

- [ ] `pub async fn renew_cert(state: &mut ShieldState, cfg: &ShieldConfig) -> anyhow::Result<()>`
- [ ] Step 1: Read `shield.key` from disk
- [ ] Step 2: Build CSR with same SPIFFE SAN (proof of key possession, same keypair):
  - `crypto::build_csr(key, cn, spiffe_uri)`
  - Uses existing private key — does NOT generate a new keypair
- [ ] Step 3: Call `RenewCert` on Connector :9091 (uses same mTLS channel):
  ```rust
  let req = RenewCertRequest {
      shield_id: state.shield_id.clone(),
      csr_der:   csr,
  };
  ```
- [ ] Step 4: Receive `RenewCertResponse`
  - Save `shield.crt` (new cert, same key)
  - Save `workspace_ca.crt` (updated chain)
- [ ] Step 5: Parse new `cert_not_after` from new cert PEM
- [ ] Step 6: Update `state.cert_not_after` + save `state.json`
- [ ] Step 7: Rebuild mTLS channel in heartbeat loop with new cert
  - Return updated state to heartbeat.rs which rebuilds channel
- [ ] Log `info!("cert renewed shield_id={} new_expiry={}", ...)`

---

## Integration Test

```bash
# With Connector running (M3's agent_server on :9091):
# Shield should appear ACTIVE in dashboard within 30s of starting

# Test disconnect:
# kill -9 <shield-pid>
# Dashboard should show DISCONNECTED within 120s
```

---

## Notes

- The heartbeat loop reconnects after TLS errors — it does NOT exit. The `consecutive_failures` counter is for backoff only.
- Renewal does NOT change the keypair — the private key never leaves the device. Only the cert validity window is extended.
- After renewal, rebuild the tonic channel by returning the new `ShieldState` to the heartbeat loop caller (or use `Arc<Mutex<ShieldState>>` if shared).

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase5-AgentServer-Rust]] — server-side of this heartbeat
- [[Sprint4/Member4-Rust-Shield-CI/Phase2-Core-Modules]] — tls.rs verify_connector_spiffe used here
