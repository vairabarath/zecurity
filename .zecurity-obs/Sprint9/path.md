---
type: planning
status: planned
sprint: 9
tags:
  - sprint9
  - dependencies
  - execution-path
  - team-coordination
  - rde
---

# Sprint 9 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

**RDE (Remote Device Extension)** — The client-facing data plane. End-user devices connect to the Connector's TLS/QUIC listener (`:9092`) with a device identity and destination. The Connector validates access against the local ACL snapshot delivered in Sprint 8, then routes the connection:

- **Protected resource** (has nftables rules on Shield): Connector relays through the Shield via `TunnelOpen/Data/Close` messages on the existing Control stream. Shield opens TCP locally and streams data back via `TunnelData`. nftables is bypassed because traffic enters via `zecurity0`.
- **Unprotected resource**: Connector connects directly via `copy_bidirectional`.

QUIC/UDP on the same port (`:9092`) is advertised in every `TunnelResponse` so clients can upgrade. CRL revocation checking and systemd watchdog keepalives round out Connector reliability.

> **Prerequisite:** Sprint 8 Policy Engine must be complete. Fields 8–11 on `ShieldControlMessage` are pre-reserved — Sprint 9 Day 1 activates them.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **RDE transport** | TLS listener `:9092` (TCP) + QUIC listener `:9092` (UDP) on Connector; JSON handshake `TunnelRequest`/`TunnelResponse`; protected path relays via Shield Control stream; direct path via `copy_bidirectional` |
| **QUIC advertise** | `quic_addr` in every `TunnelResponse` (even failures) — client uses this to pre-warm QUIC connection |
| **QUIC connection pool** | Client daemon maintains one QUIC connection per Connector address. Multiple resource streams share the same connection (`open_stream()` per tunnel). Never open a new connection when one already exists. |
| **Client transparent proxy** | TUN device `zecurity0` on client. smoltcp handles TCP/UDP. Routes installed per ACL snapshot resource IP. Apps see no Zecurity code. `zecurity up` / `zecurity down` replaces explicit `zecurity connect`. |
| **Client privilege** | Daemon requires `CAP_NET_ADMIN` for TUN. Set via `AmbientCapabilities` in a system-level systemd unit with `User=<enrolling_user>`. Not root. See ADR-002 and ADR-003. |
| **DNS constraint** | Sprint 9: resources accessed by IP address only. DNS split-horizon is Sprint 11. |
| **Shield field numbers** | `TunnelOpen = 8`, `TunnelOpened = 9`, `TunnelData = 10`, `TunnelClose = 11` in ShieldControlMessage oneof — reserved in Sprint 6, activated here |
| **CRL refresh** | Connector fetches `/ca.crl` from controller every 5min; revoked serial → reject with "certificate revoked" |
| **Systemd watchdog** | `READY=1` on startup; `WATCHDOG=1` every `WATCHDOG_USEC/2`; connector only |
| **Shield tunnel relay** | Shield opens local TCP to resource destination, streams data via `TunnelData` — bypasses nftables because `zecurity0` is whitelisted |
| **Access enforcement** | Connector resolves resource + client SPIFFE ID against the Sprint 8 local ACL snapshot. Missing snapshot/resource/SPIFFE means deny. No per-request Controller check in the tunnel hot path. |
| **Protected path detection** | Resource has `shield_id` set in the local resource/policy snapshot. Protected resources relay via Shield; unprotected resources connect directly. |
| **Max chunk size** | 16 KB per `TunnelData` frame — enforced on both Connector and Shield sides |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Device/client management UI (token issuance, device list, access log viewer) |
| **M2** | Go (Proto) | Activate shield.proto fields 8–11 (TunnelOpen/Opened/Data/Close) |
| **M3** | Rust (Connector) | `device_tunnel.rs`, `quic_listener.rs`, `agent_tunnel.rs` modifications, `net_util.rs`, `crl.rs`, `watchdog.rs` |
| **M4** | Rust (Shield + Client) | `shield/src/tunnel.rs`, `control_stream.rs` tunnel dispatch + client TUN device, smoltcp, QUIC pool, `zecurity up/down` |

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
| `shield/src/control_stream.rs` | M4 adds tunnel dispatch (TunnelOpen/Data/Close match arms) | M4 only. Sprint 6 discovery arms already present — add after them. |
| `shield/src/main.rs` | M4 adds `mod tunnel` | M4 only |
| `client/src/tun.rs` | M4 — new file | M4 only |
| `client/src/net_stack.rs` | M4 — new file | M4 only |
| `client/src/tunnel_pool.rs` | M4 — new file | M4 only |
| `client/src/cmd/up.rs` | M4 — new file | M4 only |
| `client/src/cmd/down.rs` | M4 — new file | M4 only |
| `client/zecurity-client.service` | M4 — already updated with `CAP_NET_ADMIN` | Do not change capabilities |

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

> See [[Sprint9/Member2-Go-Proto/Phase1-Tunnel-Proto]] for full field specs.

---

### PHASE B — M3 RDE Device Tunnel (Depends on: Day 1 done)

- [ ] **M3-B1** `connector/src/device_tunnel.rs` — NEW: TLS listener `:9092`, `TunnelRequest`/`TunnelResponse` JSON handshake, local ACL snapshot enforcement, protected path via `AgentTunnelHub` relay, direct path via `copy_bidirectional`, `relay_udp()` 4-byte length-prefix, `emit_access_log()`
- [ ] **M3-B2** `connector/src/quic_listener.rs` — NEW: QUIC/UDP listener `:9092`, ALPN `ztna-tunnel-v1`, delegates each bidir stream to `device_tunnel::handle_stream()`
- [ ] **M3-B3** `connector/src/agent_tunnel.rs` — MODIFY: dispatch `TunnelOpened/Data/Close` from Shield Control stream into hub sessions; send `TunnelOpen` to Shield via control stream sender
- [ ] **M3-B4** `connector/src/net_util.rs` — NEW: `lan_ip()` UDP routing trick for private IP discovery

> Build check: `cd connector && cargo build` must pass.

---

### PHASE C — M3 Connector Access Enforcement (Depends on: Sprint 8 ACL cache + Day 1 done)

- [ ] **M3-C1** `connector/src/device_tunnel.rs` — enforce Sprint 8 policy cache before routing: missing snapshot, unknown resource, or missing client SPIFFE ID must deny.
- [ ] **M3-C2** Access logging — emit local/Controller-bound access events after local decision.

> Build check: `cd connector && cargo build` must pass.

---

### PHASE D — M3 Connector Reliability (Depends on: M3-B done)

- [ ] **M3-D1** `connector/src/crl.rs` — NEW: `CrlManager` — fetch `/ca.crl` DER, cache revoked serials, background refresh every 5min
- [ ] **M3-D2** `connector/src/watchdog.rs` — NEW: `notify_ready()` + `spawn_watchdog()` for systemd sd_notify integration
- [ ] **M3-D3** `connector/src/main.rs` — MODIFY: wire all listeners in correct order, `notify_ready()`, `spawn_watchdog()`

> Build check: `cd connector && cargo build` must pass.

---

### PHASE E — M4 Shield Tunnel Relay (Depends on: Day 1 done + Sprint 6 M4-E done)

- [ ] **M4-E1** `shield/src/tunnel.rs` — NEW: `TunnelHub`, `handle_tunnel_open()` (connect TCP locally, register session), `handle_tunnel_data()` (forward bytes to local TCP), `handle_tunnel_close()` (drop session)
- [ ] **M4-E2** `shield/src/control_stream.rs` — MODIFY: add match arms for `TunnelOpen/Data/Close` from incoming Control stream messages → dispatch to `tunnel::` handlers. Add after existing Sprint 6 discovery arms.
- [ ] **M4-E3** `shield/src/main.rs` — Add `mod tunnel`

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---

### PHASE F — M4 Client Transparent Proxy (Depends on: Sprint 8.5 daemon + M4-E done + M3-B done)

> See [[Sprint9/Member4-Rust-Client/Phase1-Client-TUN]].

- [ ] **M4-F1** `client/src/tun.rs` — Create `zecurity0` TUN interface; assign configurable host address (default `100.64.0.1/32`); `add_route(ip)` per ACL snapshot entry; detect route conflicts before `up`; `cleanup()` removes routes and interface on shutdown.
- [ ] **M4-F2** `client/src/net_stack.rs` — smoltcp integration: read IP packets from TUN, accept TCP connections, dispatch UDP datagrams; for each connection open QUIC stream via pool, send `TunnelRequest`/`TunnelResponse` JSON, relay bidirectionally.
- [ ] **M4-F3** `client/src/tunnel_pool.rs` — QUIC connection pool: one connection per Connector address using device mTLS cert from daemon RuntimeState; `open_stream()` reuses existing connection.
- [ ] **M4-F4** `client/src/cmd/up.rs` + `client/src/cmd/down.rs` — `zecurity up` / `zecurity down` IPC commands. `Up` creates TUN and starts smoltcp loop. `Down` teardown with route cleanup.
- [ ] **M4-F5** Wire `Up`/`Down` IPC handlers in `client/src/daemon.rs`; add `Up`/`Down` subcommands in `client/src/main.rs`; add to `client/src/ipc.rs` message enum.
- [ ] **M4-F6** `client/Cargo.toml` — add `tun = "0.6"`, `smoltcp = "0.11"`, `quinn = "0.11"`.

> Build check: `cd client && cargo build` passes.
> Manual: `zecurity up` creates `zecurity0`, routes appear, app connects to resource IP transparently. `zecurity down` cleans up.

---

### PHASE G — M1 Frontend (Depends on: M3-C done + codegen done)

- [ ] **M1-F1** Device/client management UI — TBD based on Sprint 9 kickoff. At minimum: access log viewer showing `connector_log` events from RDE connections.

> Build check: `cd admin && npm run build` must pass.

---

Run these once all phases are complete:

- [ ] `buf generate` (from repo root) — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd connector && cargo build` — clean (warnings OK)
- [ ] `cargo build --manifest-path shield/Cargo.toml` — clean
- [ ] `cd admin && npm run build` — clean
**TCP gate (Sprint 9 completion criteria — must all pass):**
- [ ] `zecurity up` creates `zecurity0` TUN interface with configurable host address (default `100.64.0.1/32`)
- [ ] Routes for ACL snapshot resource IPs appear in routing table on client machine
- [ ] App connects to resource IP directly → traffic intercepted by TUN → flows through Connector → reaches resource (no `zecurity connect` needed)
- [ ] Protected resource (behind Shield nftables): traffic relays via Shield tunnel relay — still reachable through TUN transparent proxy
- [ ] Unprotected resource: traffic routes via `copy_bidirectional` on Connector — reachable through TUN transparent proxy
- [ ] Multiple simultaneous connections to different resources work (QUIC stream multiplexing — not multiple QUIC connections)
- [ ] Device cert is revoked → Connector rejects with "certificate revoked"
- [ ] Client SPIFFE ID not in ACL snapshot → Connector denies access
- [ ] `zecurity down` removes `zecurity0` and all routes cleanly
- [ ] QUIC `quic_addr` present in every `TunnelResponse` (even rejections)
- [ ] Systemd watchdog: `WATCHDOG=1` notifications appear in `journalctl` for connector service
- [ ] Daemon exit (SIGTERM) cleans up TUN and routes — no dangling `zecurity0` after daemon stops

**UDP stretch goal (Sprint 9 if time allows, else Sprint 10):**
- [ ] App sends UDP to resource IP → intercepted by TUN → relayed via `relay_udp()` with 4-byte length prefix
- [ ] UDP session idle timeout (30s) cleans up stale sessions

---

## Dependency Graph (Visual)

```
Sprint 8 ACL Snapshot + Sprint 8.5 Daemon + M2-D1-A (shield.proto TunnelOpen/Opened/Data/Close)
              │
              ▼
       buf generate + go generate + npm codegen
              │
      ┌───────┼──────────────┬──────────────┐
      ▼       ▼              ▼              ▼
    M3-B    M3-C           M4-E           M1-F
  (device   (local ACL     (Shield        (device UI)
   tunnel,   enforcement)   tunnel.rs
   QUIC,                    relay)
   agent_tunnel)
      │                       │
      ▼                       ▼
    M3-D                    M4-F
  (crl.rs,               (Client TUN
   watchdog.rs,           smoltcp
   main.rs wiring)        QUIC pool
                          zecurity up/down)
```

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Before touching any file, confirm dependency checkboxes are checked.
2. **Proto field numbers are permanent.** Sprint 9 activates ShieldControlMessage fields 8–11 (reserved in Sprint 6). Never reuse or renumber.
3. **Tunnel messages ride the existing Shield Control stream.** No new RPCs. Connector sends TunnelOpen; Shield replies TunnelOpened/Data/Close on the same stream.
4. **Sprint 6 control_stream.rs already has discovery arms** — Sprint 9 M4-E2 adds additional match arms after them. Do not remove or reorder existing arms.
5. **RDE access checks.** Connector MUST use the Sprint 8 local ACL snapshot. Do not call the Controller per request.
6. **RDE protected path.** For resources with `shield_id` set, Connector MUST relay via `AgentTunnelHub` → Shield Control stream. Direct connect will fail due to nftables.
7. **QUIC is on same port as TLS.** `:9092` — UDP for QUIC, TCP for TLS. OS demuxes by transport protocol.
8. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.
9. **Max chunk size is 16 KB** per `TunnelData` frame — enforced on both sides.

See individual member phase files for detailed specs:
- [[Sprint9/Member2-Go-Proto/Phase1-Tunnel-Proto]]
- [[Sprint9/Member3-Go-Connector/Phase1-RDE-Device-Tunnel]]
- [[Sprint9/Member3-Go-Connector/Phase2-Connector-Extras]]
- [[Sprint9/Member4-Rust-Shield/Phase1-Shield-Tunnel-Relay]]
- [[Sprint9/Member4-Rust-Client/Phase1-Client-TUN]]

See architecture decisions:
- [[Decisions/ADR-003-Client-TUN-Transparent-Proxy]]
- [[Decisions/ADR-002-Client-Daemon-Required]]
