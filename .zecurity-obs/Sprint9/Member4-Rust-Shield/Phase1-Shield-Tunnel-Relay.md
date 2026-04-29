---
type: phase
status: pending
sprint: 9
member: M4
phase: Phase1-Shield-Tunnel-Relay
depends_on:
  - M2-D1-A (TunnelOpen/Opened/Data/Close in shield.proto — Sprint 9 Day 1)
  - buf generate
  - Sprint 6 M4-E (discovery control stream wiring — control_stream.rs already has discovery arms)
tags:
  - rust
  - shield
  - tunnel
  - rde
---

# M4 Phase 1 — Shield Tunnel Relay

---

## What You're Building

When a device connects to the Connector's RDE listener (`:9092`) and targets a resource running on a Shield host, the Connector cannot connect directly — the Shield host has nftables rules blocking LAN access on that port. Instead, the Connector sends a `TunnelOpen` message via the existing Control stream to the Shield. The Shield then opens a TCP connection locally (through `zecurity0` or direct loopback) and streams data back and forth using `TunnelData` messages.

---

## Protocol Flow

```
Device → Connector :9092 (TLS)
  Connector ──TunnelOpen──► Shield (via Control stream)
  Shield opens TCP to resource (e.g. 127.0.0.1:22 or via zecurity0)
  Shield ──TunnelOpened{ok:true}──► Connector
  Connector ◄──TunnelData──► Shield  (bidirectional)
  Either side sends TunnelClose to terminate
```

Proto messages (added by M2 Sprint 9 Day 1):
- `TunnelOpen { connection_id, destination, port, protocol }` → field 8, Connector → Shield
- `TunnelOpened { connection_id, ok, error }` → field 9, Shield → Connector
- `TunnelData { connection_id, data }` → field 10, bidirectional
- `TunnelClose { connection_id, error }` → field 11, bidirectional

---

## Files to Touch

### 1. `shield/src/tunnel.rs` (NEW)

```rust
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, Mutex};
use tokio::time::timeout;

use crate::proto::shield::v1::{
    ShieldControlMessage, TunnelClose, TunnelData, TunnelOpened,
    shield_control_message::Body,
};

const MAX_CHUNK: usize = 16 * 1024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

struct TunnelSession {
    inbound_tx: mpsc::Sender<Bytes>,
}

pub type TunnelHub = Arc<Mutex<HashMap<String, TunnelSession>>>;

pub fn new_hub() -> TunnelHub {
    Arc::new(Mutex::new(HashMap::new()))
}

pub async fn handle_tunnel_open(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
    _protocol: String,
    upstream_tx: mpsc::Sender<ShieldControlMessage>,
) {
    let addr = format!("{destination}:{port}");
    let conn_id = connection_id.clone();

    tokio::spawn(async move {
        let stream = match timeout(CONNECT_TIMEOUT, TcpStream::connect(&addr)).await {
            Ok(Ok(s)) => s,
            Ok(Err(e)) => {
                let _ = upstream_tx.send(tunnel_opened_msg(&conn_id, false, &e.to_string())).await;
                return;
            }
            Err(_) => {
                let _ = upstream_tx.send(tunnel_opened_msg(&conn_id, false, "connect timeout")).await;
                return;
            }
        };

        let (inbound_tx, mut inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.lock().await.insert(conn_id.clone(), TunnelSession { inbound_tx });

        if upstream_tx.send(tunnel_opened_msg(&conn_id, true, "")).await.is_err() {
            hub.lock().await.remove(&conn_id);
            return;
        }

        let (mut reader, mut writer) = stream.into_split();
        let hub_clone = hub.clone();
        let tx_clone = upstream_tx.clone();
        let conn_id_read = conn_id.clone();

        let read_task = tokio::spawn(async move {
            let mut buf = vec![0u8; MAX_CHUNK];
            loop {
                match reader.read(&mut buf).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        let msg = ShieldControlMessage {
                            body: Some(Body::TunnelData(TunnelData {
                                connection_id: conn_id_read.clone(),
                                data: buf[..n].to_vec(),
                            })),
                        };
                        if tx_clone.send(msg).await.is_err() { break; }
                    }
                }
            }
            let _ = tx_clone.send(ShieldControlMessage {
                body: Some(Body::TunnelClose(TunnelClose {
                    connection_id: conn_id_read.clone(),
                    error: String::new(),
                })),
            }).await;
            hub_clone.lock().await.remove(&conn_id_read);
        });

        let write_task = tokio::spawn(async move {
            while let Some(data) = inbound_rx.recv().await {
                if writer.write_all(&data).await.is_err() { break; }
            }
        });

        let _ = tokio::join!(read_task, write_task);
    });
}

pub async fn handle_tunnel_data(hub: TunnelHub, connection_id: &str, data: Vec<u8>) {
    let guard = hub.lock().await;
    if let Some(session) = guard.get(connection_id) {
        let _ = session.inbound_tx.try_send(Bytes::from(data));
    }
}

pub async fn handle_tunnel_close(hub: TunnelHub, connection_id: &str) {
    hub.lock().await.remove(connection_id);
}

fn tunnel_opened_msg(connection_id: &str, ok: bool, error: &str) -> ShieldControlMessage {
    ShieldControlMessage {
        body: Some(Body::TunnelOpened(TunnelOpened {
            connection_id: connection_id.to_string(),
            ok,
            error: error.to_string(),
        })),
    }
}
```

---

### 2. `shield/src/control_stream.rs` (MODIFY)

Add match arms for the three tunnel message types **after the existing Sprint 6 discovery arms**. Do not remove or reorder existing arms.

> **Note:** Discovery and tunnel logic live in the same `match` body in `control_stream.rs` (`run_once` function). There is no separate `heartbeat.rs`.

Initialize the hub once (in `run_once` or passed from `main.rs`):

```rust
let tunnel_hub = tunnel::new_hub();
```

> `upstream_tx` is the channel already used to send `ShieldControlMessage` frames back to the Connector. No new streams needed.

---

### 3. `shield/src/main.rs` (MODIFY)

```rust
mod tunnel;
```

---

## Notes

- **Max chunk size is 16 KB** — matches the proto spec. Do not send larger chunks.
- **Back-pressure**: `inbound_tx.try_send()` drops data silently if the local writer is slow. Acceptable for Sprint 9 — full flow-control is deferred.
- **UDP tunneling from Shield**: Not implemented. If `protocol == "udp"` arrives, send `TunnelOpened{ok: false, error: "udp not supported"}` and return.
- **`destination` field**: The Connector populates this with the resource `host` field. The Shield connects to exactly what the Connector sends — no substitution needed.

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
```
