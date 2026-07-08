---
type: phase
sprint: 10
member: M3
phase: 2
status: done
---

# M3 Phase 2 — Connector Relay Client

## What You're Building

A background task in the connector that opens a persistent QUIC connection to the relay and sends a `RegisterMsg`. If the connection drops, it reconnects automatically. The connector's mTLS cert is used — the relay validates the SPIFFE from it.

## Files to Touch

| File | Change |
|------|--------|
| `connector/src/relay_client.rs` | NEW — relay registration logic |
| `connector/src/main.rs` | Spawn relay registration task on startup |
| `connector/src/config.rs` | Add `relay_addr: Option<String>` |

## Do NOT Touch

- `relay/` (Phase 1 owns it)
- `client/src/` anything
- `controller/` anything

---

## Step 1 — Config

In `connector/src/config.rs`, add:

```rust
pub struct Config {
    // ... existing fields ...
    pub relay_addr: Option<String>,  // from RELAY_ADDR env var
}
```

Read from env in the constructor:
```rust
relay_addr: std::env::var("RELAY_ADDR").ok(),
```

---

## Step 2 — `connector/src/relay_client.rs`

Wire protocol matches the relay session.rs spec — 4-byte length-prefixed JSON.

```rust
pub struct RelayClient {
    connector_id: String,
    relay_addr: String,
    tls_config: Arc<rustls::ClientConfig>,
}

impl RelayClient {
    pub fn new(connector_id: String, relay_addr: String, tls_config: Arc<rustls::ClientConfig>) -> Self

    /// Connects to relay and sends RegisterMsg. Returns when connection drops.
    pub async fn connect_and_register(&self) -> anyhow::Result<()>

    /// Reconnect loop — runs forever, reconnects after 5s backoff on failure.
    pub async fn maintain_registration(self: Arc<Self>)
}
```

`connect_and_register()`:
1. Connect QUIC to `relay_addr` with ALPN `ztna-relay-v1` — use connector mTLS cert
2. Open bi-directional stream
3. Serialize and send `RegisterMsg { connector_id, spiffe_id }` as 4-byte length + JSON
4. Block on `connection.closed()` — relay will keep this open until disconnect

`maintain_registration()`:
```rust
loop {
    match self.connect_and_register().await {
        Ok(_) => tracing::info!("relay registration ended"),
        Err(e) => tracing::warn!("relay registration error: {e}"),
    }
    tokio::time::sleep(Duration::from_secs(5)).await;
}
```

The SPIFFE ID to send is `self_spiffe_id()` — read from the connector's own cert URI SAN (same technique as SPIFFE interceptor in the controller).

---

## Step 3 — `connector/src/main.rs`

After existing listeners are started, add:

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

This is fire-and-forget — connector operation does not depend on relay registration succeeding.

---

## Build Check

```bash
cd connector && cargo build
```

---

## Post-Phase Fixes

### Fix: Wire Registered Connection to Inner-mTLS Relay Handler

**Issue:** Relay client and handler modules compiled, but Connector startup did
not launch registration or accept streams opened by Relay.

**Fix Applied:**
- Added `RELAY_ADDR` and exact `RELAY_SPIFFE_ID` Connector configuration.
- Connector startup constructs `RelayHandler` and launches persistent
  registration without blocking normal Connector operation.
- After registration succeeds, the same outer QUIC connection is passed to
  `RelayHandler::run`.
- Relay hostnames are resolved again on every reconnect.
