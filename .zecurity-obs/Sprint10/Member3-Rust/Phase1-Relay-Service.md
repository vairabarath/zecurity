---
type: phase
sprint: 10
member: M3
phase: 1
status: planned
---

# M3 Phase 1 — Relay Service (New `relay/` Crate)

## What You're Building

A standalone Rust binary that accepts QUIC connections from connectors and clients. It validates SPIFFE identities, maintains a state table of registered connectors, and bridges two QUIC streams together without decrypting traffic.

## Files to Create

| File | Purpose |
|------|---------|
| `relay/Cargo.toml` | New workspace member |
| `relay/src/main.rs` | Binary entry point |
| `relay/src/listener.rs` | QUIC accept loop |
| `relay/src/state.rs` | `RelayState` — connector registry |
| `relay/src/session.rs` | Per-connection handler |
| `relay/src/spiffe.rs` | SPIFFE validation (see M2 Phase 2 spec) |

## Do NOT Touch

- `controller/` anything
- `connector/src/` anything
- `client/src/` anything

---

## Step 1 — Add to Workspace

In the root `Cargo.toml`, add `"relay"` to the `members` list.

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

Entry point. Reads from environment:
- `RELAY_BIND` — bind address, default `0.0.0.0:9093`
- `RELAY_TLS_CERT` — path to PEM cert file
- `RELAY_TLS_KEY` — path to PEM key file

Load TLS config using `rustls_pemfile`, build QUIC endpoint with ALPN `ztna-relay-v1`, create shared `Arc<RelayState>`, call `listener::start()`.

---

## Step 4 — `relay/src/state.rs`

```rust
use dashmap::DashMap;
use std::sync::Arc;

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
    pub fn new() -> Self { ... }
    pub fn insert_connector(&self, connector_id: String, entry: ConnectorEntry) { ... }
    pub fn remove_connector(&self, connector_id: &str) { ... }
    pub fn lookup_connector(&self, connector_id: &str) -> Option<ConnectorEntry> { ... }
}
```

---

## Step 5 — `relay/src/session.rs`

Wire protocol (length-prefixed JSON, 4-byte big-endian length):

```rust
#[derive(Deserialize)]
#[serde(tag = "type")]
enum HandshakeMsg {
    Register { connector_id: String, spiffe_id: String },
    Lookup   { connector_id: String },
}
```

`handle_connection(conn: quinn::Connection, state: Arc<RelayState>)`:
1. Accept bi-directional stream
2. Read 4-byte length, read JSON, deserialize `HandshakeMsg`
3. Validate SPIFFE from peer cert (see `spiffe.rs`)
4. Match on variant:
   - `Register` → validate connector SPIFFE → store in `RelayState` → keep connection alive (block on `conn.closed()`) → on disconnect remove from state
   - `Lookup` → validate client SPIFFE → check trust domain matches connector → call `conn.open_bi()` on stored connector connection → send 4-byte ACK to client → `pipe_streams()` both pairs bidirectionally

`pipe_streams(send_a, recv_a, send_b, recv_b)`:
- Spawn two tasks: `recv_a → send_b` and `recv_b → send_a`
- Each reads chunks up to 16 KB and writes to the other side
- Either task finishing closes both

---

## Step 6 — `relay/src/spiffe.rs`

Implement per the M2 Phase 2 spec:

```rust
pub fn extract_spiffe_uri(cert: &[u8]) -> Option<String>
pub fn parse_trust_domain(spiffe_uri: &str) -> Option<&str>
pub fn validate_connector_spiffe(spiffe_uri: &str) -> bool  // must be /connector/<uuid>
pub fn validate_client_spiffe(spiffe_uri: &str) -> bool    // must be /client_device/<uuid>
pub fn same_workspace(connector_spiffe: &str, client_spiffe: &str) -> bool
```

Extract URI SAN from DER cert using `x509-parser` or manual ASN.1 (SubjectAltName OID 2.5.29.17, GeneralName type uniformResourceIdentifier = 6).

---

## Build Check

```bash
cd relay && cargo build
```

Must pass. Warnings OK, errors not.

---

## Post-Phase Fixes

*(Empty — add fixes here as discovered)*
