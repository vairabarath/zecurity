---
type: phase
sprint: 10
member: M3
phase: 2
status: planned
---

# M3 Phase 2 — Connector Relay Client

## What You're Building

Background task in the connector that opens a persistent QUIC connection to the relay and sends `RegisterMsg`. Reconnects automatically on disconnect.

## Files to Touch

| File | Change |
|------|--------|
| `connector/src/relay_client.rs` | NEW |
| `connector/src/main.rs` | Spawn relay registration task |
| `connector/src/config.rs` | Add `relay_addr: Option<String>` |

---

## Step 1 — Config

```rust
pub relay_addr: Option<String>,  // from RELAY_ADDR env var
```

---

## Step 2 — `connector/src/relay_client.rs`

```rust
pub struct RelayClient {
    connector_id: String,
    relay_addr: String,
    tls_config: Arc<rustls::ClientConfig>,
}

impl RelayClient {
    pub fn new(connector_id: String, relay_addr: String, tls_config: Arc<rustls::ClientConfig>) -> Self

    /// Connects and sends RegisterMsg. Returns when connection drops.
    pub async fn connect_and_register(&self) -> anyhow::Result<()>

    /// Reconnect loop — runs forever with 5s backoff.
    pub async fn maintain_registration(self: Arc<Self>)
}
```

`connect_and_register()`:
1. Connect QUIC to `relay_addr` with ALPN `ztna-relay-v1` using connector mTLS cert
2. Open bi-directional stream
3. Send `RegisterMsg { connector_id, spiffe_id }` as 4-byte length + JSON
4. Block on `connection.closed()`

`maintain_registration()`:
```rust
loop {
    if let Err(e) = self.connect_and_register().await {
        tracing::warn!("relay registration error: {e}");
    }
    tokio::time::sleep(Duration::from_secs(5)).await;
}
```

SPIFFE ID = connector's own cert URI SAN.

---

## Step 3 — `connector/src/main.rs`

After existing listeners are started:

```rust
if let Some(relay_addr) = &cfg.relay_addr {
    let relay_client = Arc::new(relay_client::RelayClient::new(
        connector_id.clone(),
        relay_addr.clone(),
        tls_config.clone(),
    ));
    tokio::spawn(async move { relay_client.maintain_registration().await });
}
```

Fire-and-forget — connector does not depend on relay registration succeeding.

---

## Build Check

```bash
cd connector && cargo build
```

---

## Post-Phase Fixes

*(Empty)*
