---
type: phase
member: M4
sprint: 11
phase: 1
title: Relay Telemetry & Probe Responder
depends_on:
  - Sprint11/Member2-Go/Phase1-Proto
---

# Phase 1 — Relay Telemetry & Probe Responder

## Goal

The relay tracks active bridged client streams, reports count in heartbeat, and responds to lightweight probe connections.

## Files

| File | Change |
|---|---|
| `relay/src/config.rs` | Add `RELAY_MAX_CONNECTIONS` env var |
| `relay/src/session.rs` | Atomic active-stream counter; probe responder |
| `relay/src/heartbeat.rs` | Include `connection_count` and `max_connections` in heartbeat payload |

## Active Stream Counter

```rust
// relay/src/session.rs
// Global or service-level atomic:
static ACTIVE_STREAMS: AtomicU32 = AtomicU32::new(0);

// On client lookup bridge start:
ACTIVE_STREAMS.fetch_add(1, Ordering::Relaxed);

// On bridge end (drop/close):
ACTIVE_STREAMS.fetch_sub(1, Ordering::Relaxed);
```

This counts **bridged client relay streams** only — not registered connector connections.

## Probe Responder

After QUIC mTLS handshake, detect whether the first message is a `ProbeRequest` or the normal `RegisterMsg`/`LookupMsg`:

```rust
// Probe path:
// 1. Read framed ProbeRequest
// 2. Validate connector_id is non-empty
// 3. Write ProbeResponse { connection_count, capacity, request_id: req.request_id }
// 4. Close connection — do NOT register the connector

// Rate limits:
// - Max RELAY_MAX_PROBE_RATE per connector per minute (default 10)
// - Max RELAY_MAX_CONCURRENT_PROBES concurrent probe connections (default 20)
// - Per-probe timeout: RELAY_PROBE_TIMEOUT_MS (default 2000ms)
// - Log warning if same connector exceeds rate limit
```

## Heartbeat

```rust
// relay/src/heartbeat.rs
RelayHeartbeat {
    // existing fields ...
    connection_count: ACTIVE_STREAMS.load(Ordering::Relaxed),
    max_connections:  config.max_connections,
}
```

## Config

```
RELAY_MAX_CONNECTIONS       = 1000   # capacity ceiling reported in heartbeat + probe
RELAY_MAX_PROBE_RATE        = 10     # max probe requests per connector per minute
RELAY_MAX_CONCURRENT_PROBES = 20     # max simultaneous probe connections
RELAY_PROBE_TIMEOUT_MS      = 2000   # per-probe deadline
```

## Build Check

```bash
cd relay && cargo build
cd relay && cargo test
```
