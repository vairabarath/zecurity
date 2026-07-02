---
type: phase
member: M3
sprint: 11.2
phase: 1
title: Connector — Framed Tunnel Handshake
status: completed
commits: f88600c, 8f9235f
depends_on:
  - Sprint11.2/Member1-Client/Phase1-MultiTransport
---

# Phase 1 — Connector: Framed Tunnel Handshake

## Goal

Replace the connector's raw JSON tunnel handshake read/write with the same
4-byte length-prefixed framing the client now uses. Prevents partial-read
bugs and aligns both sides of the protocol.

## Files

| File | Change |
|---|---|
| `connector/src/device_tunnel.rs` | Replace raw read/write with `read_framed_json` / `write_framed_json`; raise max size to 16 KB |
| `connector/src/policy/mod.rs` | Supporting change for preferred connector routing |

## device_tunnel.rs

### Before (raw read, unbounded partial-read risk)

```rust
const MAX_HANDSHAKE_SIZE: usize = 4096;

let mut buf = vec![0u8; MAX_HANDSHAKE_SIZE];
let n = stream.read(&mut buf).await?;
let handshake = String::from_utf8(buf[..n].to_vec())?;
let req: TunnelRequest = serde_json::from_str(handshake.trim())?;
```

### After (framed read)

```rust
const MAX_TUNNEL_HANDSHAKE_SIZE: usize = 16 * 1024;

let req: TunnelRequest = read_framed_json(&mut stream).await
    .map_err(|e| anyhow!("invalid tunnel request: {}", e))?;
```

### send_response (before → after)

```rust
// Before:
let json = serde_json::to_string(response)?;
stream.write_all(json.as_bytes()).await?;

// After:
write_framed_json(stream, response).await
```

### Helpers added (mirrors client-side)

```rust
async fn write_framed_json<W, T>(writer: &mut W, value: &T) -> Result<()>
async fn read_framed_json<R, T>(reader: &mut R) -> Result<T>
```

Both use 4-byte big-endian length prefix + JSON body, max `MAX_TUNNEL_HANDSHAKE_SIZE` (16 KB).

## Implementation Checklist

- [x] **M3-D1** `device_tunnel.rs` — replace raw JSON read with `read_framed_json`
- [x] **M3-D2** `device_tunnel.rs` — replace raw JSON write in `send_response` with `write_framed_json`
- [x] **M3-D3** `device_tunnel.rs` — add `write_framed_json` / `read_framed_json` helpers
- [x] **M3-D4** `device_tunnel.rs` — `MAX_TUNNEL_HANDSHAKE_SIZE = 16 * 1024` (was `MAX_HANDSHAKE_SIZE = 4096`)
- [x] **M3-D5** `policy/mod.rs` — supporting change for preferred connector routing
- [x] **Build gate:** `cd connector && cargo build` passes
