---
type: phase
sprint: 10
member: M3
phase: 3
status: planned
---

# M3 Phase 3 — Client Relay Pool + Direct Fallback

## What You're Building

Client tries direct QUIC first (2s timeout). On failure, falls back to relay. `RelayPool` is analogous to `tunnel_pool.rs` — one QUIC connection to relay, streams reused per resource.

## Files to Touch

| File | Change |
|------|--------|
| `client/src/relay_pool.rs` | NEW |
| `client/src/tunnel_pool.rs` | MODIFY — 2s timeout + relay fallback |
| `client/src/daemon.rs` | Pass relay fields from ACL snapshot to net stack |

---

## Step 1 — `client/src/relay_pool.rs`

```rust
pub struct RelayPool {
    relay_addr: String,
    connector_id: String,
    tls_config: Arc<rustls::ClientConfig>,
    connection: Mutex<Option<quinn::Connection>>,
}

impl RelayPool {
    pub fn new(relay_addr: String, connector_id: String, tls_config: Arc<rustls::ClientConfig>) -> Self

    /// Opens a QUIC stream bridged to the connector via relay.
    /// Sends LookupMsg first. Stream ready for TunnelRequest JSON after this.
    pub async fn open_relay_stream(&self) -> anyhow::Result<(quinn::SendStream, quinn::RecvStream)>

    async fn get_or_connect(&self) -> anyhow::Result<quinn::Connection>
}
```

`open_relay_stream()`:
1. Get or create QUIC connection to `relay_addr`, ALPN `ztna-relay-v1`, device mTLS cert
2. Open bi-directional stream
3. Send `LookupMsg { connector_id }` as 4-byte length + JSON
4. Read relay ACK: `{"ok": true}` or `{"ok": false, "error": "..."}`
5. Return `(send, recv)` — ready for regular `TunnelRequest`/`TunnelResponse`

---

## Step 2 — `client/src/tunnel_pool.rs`

Wrap direct connect in a 2s timeout:

```rust
match tokio::time::timeout(Duration::from_secs(2), connect_direct(&addr)).await {
    Ok(Ok(streams)) => return Ok(streams),
    _ => tracing::debug!("direct connect failed, trying relay"),
}

if let Some(relay) = relay_pool {
    return relay.open_relay_stream().await;
}

anyhow::bail!("direct connect failed and no relay configured")
```

---

## Step 3 — `client/src/daemon.rs`

When handling `Up` IPC command, build `RelayPool` from ACL snapshot:

```rust
let relay_pool = if !snapshot.relay_addr.is_empty() {
    Some(Arc::new(RelayPool::new(
        snapshot.relay_addr.clone(),
        snapshot.connector_id.clone(),
        tls_config.clone(),
    )))
} else {
    None
};
```

Thread through to net stack loop.

---

## Build Check

```bash
cd client && cargo build
```

---

## Manual Test

1. Start relay on machine C: `RELAY_BIND=0.0.0.0:9093 zecurity-relay`
2. Start connector on machine A with `RELAY_ADDR=<C>:9093` — relay logs show connector registered
3. On machine B (no direct route to A), `zecurity-client up` — traffic flows via relay
4. Block direct path — same result

---

## Post-Phase Fixes

*(Empty)*
