---
type: phase
sprint: 10
member: M3
phase: 3
status: planned
---

# M3 Phase 3 — Client Relay Pool + Direct Fallback

## What You're Building

Client tries direct QUIC first. If it fails within 2s, it falls back to the relay. The relay pool is analogous to `tunnel_pool.rs` — one QUIC connection to the relay, multiple streams reused per resource.

## Files to Touch

| File | Change |
|------|--------|
| `client/src/relay_pool.rs` | NEW — relay QUIC pool |
| `client/src/tunnel_pool.rs` | MODIFY — 2s timeout + relay fallback |
| `client/src/daemon.rs` | Pass relay fields from ACL snapshot to smoltcp loop |

## Do NOT Touch

- `relay/` anything
- `connector/src/` anything
- `controller/` anything
- `shield/` anything

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

    /// Returns a QUIC stream pair bridged to the given connector via relay.
    /// Sends LookupMsg first. Stream is ready for TunnelRequest JSON after this.
    pub async fn open_relay_stream(&self) -> anyhow::Result<(quinn::SendStream, quinn::RecvStream)>

    async fn get_or_connect(&self) -> anyhow::Result<quinn::Connection>
}
```

`open_relay_stream()`:
1. Get or create QUIC connection to `relay_addr` with ALPN `ztna-relay-v1`, using device mTLS cert
2. Open bi-directional stream
3. Send `LookupMsg { connector_id }` as 4-byte length + JSON
4. Read relay ACK (4-byte length + JSON `{"ok": true}` or `{"ok": false, "error": "..."}`)
5. Return `(send_stream, recv_stream)` — now ready for regular `TunnelRequest`/`TunnelResponse` JSON

---

## Step 2 — `client/src/tunnel_pool.rs`

Find where a new QUIC stream to the connector is opened. Wrap the direct connection attempt in a timeout:

```rust
pub async fn open_stream(
    &self,
    connector_addr: &str,
    relay_pool: Option<&Arc<RelayPool>>,
) -> anyhow::Result<(quinn::SendStream, quinn::RecvStream)> {
    // Try direct first
    match tokio::time::timeout(Duration::from_secs(2), self.open_direct_stream(connector_addr)).await {
        Ok(Ok(streams)) => return Ok(streams),
        Ok(Err(e)) => tracing::debug!("direct connect failed: {e}, trying relay"),
        Err(_) => tracing::debug!("direct connect timed out, trying relay"),
    }

    // Fall back to relay
    if let Some(relay) = relay_pool {
        return relay.open_relay_stream().await;
    }

    anyhow::bail!("direct connect failed and no relay configured")
}
```

---

## Step 3 — `client/src/daemon.rs`

When the daemon receives `Up` IPC command, it reads the ACL snapshot. Pass `relay_addr` and `connector_id` through to wherever `TunnelPool::open_stream` is called (likely in `net_stack.rs`):

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

Thread `relay_pool` through to the smoltcp / net stack loop.

---

## Build Check

```bash
cd client && cargo build
```

---

## Manual Test

1. Start relay on machine C: `RELAY_BIND=0.0.0.0:9093 RELAY_TLS_CERT=... RELAY_TLS_KEY=... zecurity-relay`
2. Start connector on machine A with `RELAY_ADDR=<C-ip>:9093` — verify relay logs show `connector registered`
3. On machine B (no direct route to A), set `RELAY_ADDR=<C-ip>:9093`, run `zecurity-client up`
4. Verify traffic flows to resource — relay logs show `bridged client → connector`
5. Block direct path (firewall rule) — same result

---

## Post-Phase Fixes

*(Empty — add fixes here as discovered)*
