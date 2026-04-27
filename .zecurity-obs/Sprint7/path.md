---
type: planning
status: planned
sprint: 7
tags:
  - sprint7
  - dependencies
  - execution-path
  - team-coordination
  - rde
---

# Sprint 7 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

**RDE (Remote Device Extension)** — The client-facing data plane. End-user devices connect to the Connector's TLS/QUIC listener (`:9092`) with a token and destination. The Connector validates access via the Controller, then routes the connection:

- **Protected resource** (has nftables rules on Shield): Connector relays through the Shield via `TunnelOpen/Data/Close` messages on the existing Control stream. Shield opens TCP locally and streams data back via `TunnelData`. nftables is bypassed because traffic enters via `zecurity0`.
- **Unprotected resource**: Connector connects directly via `copy_bidirectional`.

QUIC/UDP on the same port (`:9092`) is advertised in every `TunnelResponse` so clients can upgrade. CRL revocation checking and systemd watchdog keepalives round out Connector reliability.

> **Prerequisite:** Sprint 6 must be merged. Fields 8–11 on `ShieldControlMessage` are pre-reserved — Sprint 7 Day 1 activates them.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **RDE transport** | TLS listener `:9092` (TCP) + QUIC listener `:9092` (UDP) on Connector; JSON handshake `TunnelRequest`/`TunnelResponse`; protected path relays via Shield Control stream; direct path via `copy_bidirectional` |
| **QUIC advertise** | `quic_addr` in every `TunnelResponse` (even failures) — client uses this to pre-warm QUIC connection |
| **Shield field numbers** | `TunnelOpen = 8`, `TunnelOpened = 9`, `TunnelData = 10`, `TunnelClose = 11` in ShieldControlMessage oneof — reserved in Sprint 6, activated here |
| **CRL refresh** | Connector fetches `/ca.crl` from controller every 5min; revoked serial → reject with "certificate revoked" |
| **Systemd watchdog** | `READY=1` on startup; `WATCHDOG=1` every `WATCHDOG_USEC/2`; connector only |
| **Shield tunnel relay** | Shield opens local TCP to resource destination, streams data via `TunnelData` — bypasses nftables because `zecurity0` is whitelisted |
| **Protected path detection** | Resource has `shield_id` set. Connector resolves via local policy cache first; falls back to `POST /api/device/check-access` on Controller. |
| **Max chunk size** | 16 KB per `TunnelData` frame — enforced on both Connector and Shield sides |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Device/client management UI (token issuance, device list, access log viewer) |
| **M2** | Go (Proto) | Activate shield.proto fields 8–11 (TunnelOpen/Opened/Data/Close) |
| **M3** | Go+Rust (Controller + Connector) | `device_tunnel.rs`, `quic_listener.rs`, `agent_tunnel.rs` modifications, `net_util.rs`, `crl.rs`, `watchdog.rs`, `check_access.go` |
| **M4** | Rust (Shield) | `shield/src/tunnel.rs`, `heartbeat.rs` tunnel dispatch |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/shield/v1/shield.proto` | M2 adds TunnelOpen/Opened/Data/Close (fields 8–11) | M2 commits first — everyone waits for buf generate |
| `connector/src/device_tunnel.rs` | M3 — new file | M3 only |
| `connector/src/quic_listener.rs` | M3 — new file | M3 only |
| `connector/src/agent_tunnel.rs` | M3 modifies TunnelHub + Control stream wiring | M3 only |
| `connector/src/net_util.rs` | M3 — new file | M3 only |
| `connector/src/crl.rs` | M3 — new file | M3 only |
| `connector/src/watchdog.rs` | M3 — new file | M3 only |
| `connector/src/main.rs` | M3 wires all listeners + watchdog | M3 only |
| `shield/src/tunnel.rs` | M4 — new file | M4 only |
| `shield/src/heartbeat.rs` | M4 adds tunnel dispatch (TunnelOpen/Data/Close match arms) | M4 only. Sprint 6 discovery arms already present — add after them. |
| `shield/src/main.rs` | M4 adds `mod tunnel` | M4 only |
| `controller/internal/device/check_access.go` | M3 — new file, `/api/device/check-access` endpoint | M3 only |

---

## Execution Timeline

### DAY 1 — Unblocking Work (Must land before anyone fans out)

- [ ] **M2-D1-A** `proto/shield/v1/shield.proto` — Add tunnel messages and activate reserved fields 8–11 in `ShieldControlMessage.oneof`:
  - `TunnelOpen { connection_id, destination, port, protocol }` — field 8, Connector → Shield
  - `TunnelOpened { connection_id, ok, error }` — field 9, Shield → Connector
  - `TunnelData { connection_id, data bytes }` — field 10, bidirectional
  - `TunnelClose { connection_id, error }` — field 11, bidirectional
- [ ] **TEAM** Run `buf generate` from repo root → Go stubs updated
- [ ] **TEAM** Run `cd controller && go generate ./graph/...` → gqlgen regenerates `generated.go`
- [ ] **TEAM** Run `cd admin && npm run codegen`

> After Day 1: M3 can start device_tunnel.rs scaffold; M4 can start tunnel.rs.

---

### PHASE A — M2 Proto Schema (Day 1 = Phase A for this sprint)

> See [[Sprint7/Member2-Go-Proto/Phase1-Tunnel-Proto]] for full field specs.

---

### PHASE B — M3 RDE Device Tunnel (Depends on: Day 1 done)

- [ ] **M3-B1** `connector/src/device_tunnel.rs` — NEW: TLS listener `:9092`, `TunnelRequest`/`TunnelResponse` JSON handshake, `check_access()` HTTP fallback, protected path via `AgentTunnelHub` relay, direct path via `copy_bidirectional`, `relay_udp()` 4-byte length-prefix, `emit_access_log()`
- [ ] **M3-B2** `connector/src/quic_listener.rs` — NEW: QUIC/UDP listener `:9092`, ALPN `ztna-tunnel-v1`, delegates each bidir stream to `device_tunnel::handle_stream()`
- [ ] **M3-B3** `connector/src/agent_tunnel.rs` — MODIFY: dispatch `TunnelOpened/Data/Close` from Shield Control stream into hub sessions; send `TunnelOpen` to Shield via control stream sender
- [ ] **M3-B4** `connector/src/net_util.rs` — NEW: `lan_ip()` UDP routing trick for private IP discovery

> Build check: `cd connector && cargo build` must pass.

---

### PHASE C — M3 Controller Check-Access Endpoint (Depends on: Day 1 done)

- [ ] **M3-C1** `controller/internal/device/check_access.go` — NEW: `POST /api/device/check-access` — validate Bearer JWT, look up resource, return `{ok, shield_id, connector_id, protocol}`

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE D — M3 Connector Reliability (Depends on: M3-B done)

- [ ] **M3-D1** `connector/src/crl.rs` — NEW: `CrlManager` — fetch `/ca.crl` DER, cache revoked serials, background refresh every 5min
- [ ] **M3-D2** `connector/src/watchdog.rs` — NEW: `notify_ready()` + `spawn_watchdog()` for systemd sd_notify integration
- [ ] **M3-D3** `connector/src/main.rs` — MODIFY: wire all listeners in correct order, `notify_ready()`, `spawn_watchdog()`

> Build check: `cd connector && cargo build` must pass.

---

### PHASE E — M4 Shield Tunnel Relay (Depends on: Day 1 done + Sprint 6 M4-E done)

- [ ] **M4-E1** `shield/src/tunnel.rs` — NEW: `TunnelHub`, `handle_tunnel_open()` (connect TCP locally, register session), `handle_tunnel_data()` (forward bytes to local TCP), `handle_tunnel_close()` (drop session)
- [ ] **M4-E2** `shield/src/heartbeat.rs` — MODIFY: add match arms for `TunnelOpen/Data/Close` from incoming Control stream messages → dispatch to `tunnel::` handlers. Add after existing Sprint 6 discovery arms.
- [ ] **M4-E3** `shield/src/main.rs` — Add `mod tunnel`

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---

### PHASE F — M1 Frontend (Depends on: M3-C done + codegen done)

- [ ] **M1-F1** Device/client management UI — TBD based on Sprint 7 kickoff. At minimum: access log viewer showing `connector_log` events from RDE connections.

> Build check: `cd admin && npm run build` must pass.

---

Run these once all phases are complete:

- [ ] `buf generate` (from repo root) — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd connector && cargo build` — clean (warnings OK)
- [ ] `cargo build --manifest-path shield/Cargo.toml` — clean
- [ ] `cd admin && npm run build` — clean
- [ ] Device connects to `:9092` with valid token → reaches resource through Shield tunnel relay
- [ ] Device connects to `:9092` with valid token → reaches unprotected resource directly
- [ ] Device connects with revoked certificate → rejected with "certificate revoked"
- [ ] Device connects with invalid token → rejected with HTTP 401 from `check_access`
- [ ] QUIC `quic_addr` present in every `TunnelResponse`
- [ ] Systemd watchdog: `WATCHDOG=1` notifications appear in `journalctl` for connector service
- [ ] Shield host: TCP to a port the Shield nftables blocks → still reachable through tunnel relay (via `zecurity0` interface)

---

## Dependency Graph (Visual)

```
       M2-D1-A (shield.proto: TunnelOpen=8, TunnelOpened=9, TunnelData=10, TunnelClose=11)
              │
              ▼
       buf generate + go generate + npm codegen
              │
      ┌───────┼──────────────┬──────────────┐
      ▼       ▼              ▼              ▼
    M3-B    M3-C           M4-E           M1-F
  (device   (check_access  (tunnel.rs     (device UI)
   tunnel,   endpoint)      heartbeat
   QUIC,                    wiring)
   agent_tunnel)
      │
      ▼
    M3-D
  (crl.rs,
   watchdog.rs,
   main.rs wiring)
```

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Before touching any file, confirm dependency checkboxes are checked.
2. **Proto field numbers are permanent.** Sprint 7 activates ShieldControlMessage fields 8–11 (reserved in Sprint 6). Never reuse or renumber.
3. **Tunnel messages ride the existing Shield Control stream.** No new RPCs. Connector sends TunnelOpen; Shield replies TunnelOpened/Data/Close on the same stream.
4. **Sprint 6 heartbeat.rs is already modified** — Sprint 7 M4-E2 adds additional match arms after the existing discovery arms. Do not remove or reorder existing arms.
5. **RDE protected path.** For resources with `shield_id` set, Connector MUST relay via `AgentTunnelHub` → Shield Control stream. Direct connect will fail due to nftables.
6. **QUIC is on same port as TLS.** `:9092` — UDP for QUIC, TCP for TLS. OS demuxes by transport protocol.
7. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.
8. **Max chunk size is 16 KB** per `TunnelData` frame — enforced on both sides.

See individual member phase files for detailed specs:
- [[Sprint7/Member2-Go-Proto/Phase1-Tunnel-Proto]]
- [[Sprint7/Member3-Go-Connector/Phase1-RDE-Device-Tunnel]]
- [[Sprint7/Member3-Go-Connector/Phase2-Connector-Extras]]
- [[Sprint7/Member4-Rust-Shield/Phase1-Shield-Tunnel-Relay]]
