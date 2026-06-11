---
type: phase
sprint: 10
member: M3
phase: 1
status: planned
---

# M3 Phase 1 — Relay Service (New `relay/` Crate)

## What You're Building

Standalone Rust binary. Accepts QUIC connections from connectors and clients, validates SPIFFE identities, maintains a connector registry, and bridges two QUIC streams without decrypting traffic.

## Files to Create

| File | Purpose |
|------|---------|
| `relay/Cargo.toml` | New workspace member |
| `relay/src/main.rs` | Binary entry point |
| `relay/src/listener.rs` | QUIC accept loop |
| `relay/src/state.rs` | `RelayState` — connector registry |
| `relay/src/session.rs` | Per-connection handler |
| `relay/src/spiffe.rs` | SPIFFE validation (per M2 Phase 2 spec) |

## Do NOT Touch

- `controller/` anything
- `connector/src/` anything
- `client/src/` anything

---

## Step 1 — Add to Workspace

In root `Cargo.toml`, add `"relay"` to the `members` list.

---

## Step 2 — `relay/Cargo.toml`

```toml
[package]
name = "relay"
version = "1.0.0"
edition = "2021"

[[bin]]
name = "zecurity-relay"
path = "src/main.rs"

[dependencies]
quinn = "0.11"
rustls = { version = "0.23", features = ["ring"] }
rustls-pemfile = "2"
tokio = { version = "1", features = ["full"] }
serde = { version = "1", features = ["derive"] }
serde_json = "1"
tracing = "0.1"
tracing-subscriber = "0.3"
anyhow = "1"
bytes = "1"
dashmap = "6"
```

---

## Step 3 — `relay/src/main.rs`

Reads from env:
- `RELAY_BIND` — bind address, default `0.0.0.0:9093`
- `RELAY_TLS_CERT` — path to PEM cert
- `RELAY_TLS_KEY` — path to PEM key

Load TLS, build QUIC endpoint with ALPN `ztna-relay-v1`, create `Arc<RelayState>`, call `listener::start()`.

---

## Step 4 — `relay/src/state.rs`

```rust
#[derive(Clone)]
pub struct RelayState {
    pub connectors: Arc<DashMap<String, ConnectorEntry>>,
}

pub struct ConnectorEntry {
    pub connection: quinn::Connection,
    pub spiffe_id: String,
    pub trust_domain: String,
}

impl RelayState {
    pub fn new() -> Self
    pub fn insert_connector(&self, connector_id: String, entry: ConnectorEntry)
    pub fn remove_connector(&self, connector_id: &str)
    pub fn lookup_connector(&self, connector_id: &str) -> Option<ConnectorEntry>
}
```

---

## Step 5 — `relay/src/session.rs`

Wire protocol: 4-byte big-endian length prefix + JSON body.

```rust
#[derive(Deserialize)]
#[serde(tag = "type")]
enum HandshakeMsg {
    Register { connector_id: String, spiffe_id: String },
    Lookup   { connector_id: String },
}
```

`handle_connection(conn, state)`:
1. Accept bi-directional stream
2. Read length-prefixed JSON → deserialize `HandshakeMsg`
3. Validate SPIFFE from peer cert
4. `Register` → store in `RelayState`, block on `conn.closed()`, remove on disconnect
5. `Lookup` → validate client SPIFFE + workspace match → `conn.open_bi()` on stored connector → `pipe_streams()` both pairs

`pipe_streams(send_a, recv_a, send_b, recv_b)`:
- Two tasks: `recv_a → send_b` and `recv_b → send_a`
- Read chunks up to 16 KB, write to other side
- Either task finishing closes both

---

## Step 6 — `relay/src/spiffe.rs`

Implement per M2 Phase 2 spec. Extract URI SAN from DER cert using `x509-parser` crate (add to Cargo.toml).

---

## Build Check

```bash
cd relay && cargo build
```

---

## Post-Phase Fixes

*(Empty)*
