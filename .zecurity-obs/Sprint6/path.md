---
type: planning
status: active
sprint: 6
tags:
  - sprint6
  - dependencies
  - execution-path
  - team-coordination
---

# Sprint 6 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

Three features land together:

**Shield Discovery** — Shield scans its own host's listening TCP ports via `/proc/net/tcp`, sends differential discovery reports up the Control stream to the Connector. Connector batches and relays to Controller. Admin sees all services running on each Shield host and can promote any of them to a resource with one click.

**Connector Network Discovery** — Admin defines a scan scope (CIDR or IP list + ports) from the UI. Controller sends a `ScanCommand` down the Control stream to the Connector. Connector TCP-pings every target (two-phase: alive check + banner grab), returns a `ScanReport`. Admin sees discovered live services across the remote network and can create resources from the results.

**RDE (Remote Device Extension)** — Devices connect to the Connector's TLS listener (`:9092`) with a token + destination. For protected resources the Connector relays through the Shield via `TunnelOpen/Data/Close` messages on the Control stream. QUIC/UDP on the same port is advertised for clients that support it. CRL revocation checking and systemd watchdog keepalives round out connector reliability.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **Shield discovery transport** | `DiscoveryReport` rides the existing `ShieldControlMessage` Control stream — no new RPCs |
| **Connector discovery transport** | `ScanCommand` / `ScanReport` ride the existing `ConnectorControlMessage` Control stream — no new RPCs |
| **Shield discovery interval** | Every 60s differential scan; full sync on first connect and whenever fingerprint gap detected |
| **Connector scan — two-phase** | Phase 1: quick 500ms TCP ping on first port across all targets to find alive hosts (`ConnectionRefused` = alive). Phase 2: banner-grab probe on alive hosts only (reads 256 bytes, identifies SSH/SMTP/HTTP/VNC/MySQL/Redis from bytes). Orders of magnitude faster than probing all ports on all targets. |
| **Connector scan limits** | Max 512 targets, max 16 ports, max 32 concurrent probes (Semaphore), 5s per-target probe timeout |
| **`reachable_from`** | Scan results carry the `connector_id` that ran the scan — visible in UI as "Via (connector)" column |
| **Discovered services DB** | `shield_discovered_services` table — upsert on (shield_id, protocol, port); `last_seen` updated each report |
| **Scan results DB** | `connector_scan_results` table — keyed by (request_id, connector_id, ip, port); TTL-purged after 24h |
| **Promote to resource** | `PromoteDiscoveredService` mutation creates a resource row with host=shield.lan_ip — same auto-match logic as Sprint 5 |
| **Shield field numbers** | `DiscoveryReport = 7`, `TunnelOpen = 8`, `TunnelOpened = 9`, `TunnelData = 10`, `TunnelClose = 11` in ShieldControlMessage oneof (pong=6 is current max) |
| **Connector field numbers** | `ShieldDiscoveryBatch = 8`, `ScanReport = 9`, `ScanCommand = 10` in ConnectorControlMessage oneof (pong=7 is current max) |
| **RDE transport** | TLS listener `:9092` (TCP) + QUIC listener `:9092` (UDP) on Connector; JSON handshake `TunnelRequest`/`TunnelResponse`; protected path relays via Shield Control stream; direct path via `copy_bidirectional` |
| **QUIC advertise** | `quic_addr` in every `TunnelResponse` (even failures) — client uses this to pre-warm QUIC connection |
| **CRL refresh** | Connector fetches `/ca.crl` from controller every 5min; revoked serial → reject with "certificate revoked" |
| **Systemd watchdog** | `READY=1` on startup; `WATCHDOG=1` every `WATCHDOG_USEC/2`; connector only |
| **Shield tunnel relay** | Shield opens local TCP to resource destination, streams data via `TunnelData` — bypasses nftables because `zecurity0` is whitelisted |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Discovery tab on Shields page, Scan UI on Remote Networks page, promote-to-resource flows |
| **M2** | Go (Proto + DB + GraphQL) | proto messages (both protos), migration 008, GraphQL schema |
| **M3** | Go+Rust (Controller + Connector) | discovery store, resolvers, connector control stream handlers, scan executor, RDE device tunnel, QUIC listener, CRL manager, watchdog, `check_access` endpoint |
| **M4** | Rust (Shield) | `shield/src/discovery.rs`, Control stream wiring, discovery interval loop, tunnel relay (`tunnel.rs`) |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/shield/v1/shield.proto` | M2 adds DiscoveryReport + DiscoveredService | M2 commits first — everyone waits for buf generate |
| `proto/connector/v1/connector.proto` | M2 adds ShieldDiscoveryBatch + ScanCommand/ScanReport | M2 commits first |
| `controller/internal/connector/control.go` | M3 adds discovery batch + scan handlers | M3 only |
| `connector/src/agent_server.rs` | M3 adds discovery buffering + relay | M3 only |
| `connector/src/control_plane.rs` | M3 adds scan command dispatch + result relay | M3 only |
| `shield/src/heartbeat.rs` | M4 adds discovery loop calls + tunnel message dispatch | M4 only |
| `connector/src/device_tunnel.rs` | M3 — new file | M3 only |
| `connector/src/quic_listener.rs` | M3 — new file | M3 only |
| `connector/src/agent_tunnel.rs` | M3 modifies TunnelHub + Control stream wiring | M3 only |
| `connector/src/main.rs` | M3 wires all listeners + watchdog | M3 only |
| `shield/src/tunnel.rs` | M4 — new file | M4 only |
| `controller/internal/device/check_access.go` | M3 — new file, `/api/device/check-access` endpoint | M3 only |

---

## Execution Timeline

### DAY 1 — Unblocking Work (Must land before anyone fans out)

- [ ] **M2-D1-A** `proto/shield/v1/shield.proto` — Add `DiscoveredService` + `DiscoveryReport` messages; add `discovery_report = 7` to `ShieldControlMessage.oneof`
- [ ] **M2-D1-B** `proto/connector/v1/connector.proto` — Add `ShieldDiscoveryBatch = 8`, `ScanCommand = 9`, `ScanReport = 10` to `ConnectorControlMessage.oneof`; add `DiscoveredResource` + `ScanResult` messages
- [ ] **M2-D1-C** `controller/migrations/008_discovery.sql` — `shield_discovered_services` table + `connector_scan_results` table + indexes
- [ ] **M2-D1-D** `controller/graph/discovery.graphqls` — `DiscoveredService` type, `ScanResult` type, queries + mutations
- [ ] **TEAM** Run `buf generate` from repo root → Go stubs updated
- [ ] **TEAM** Run `cd controller && go generate ./graph/...` → gqlgen regenerates `generated.go`

> After Day 1 checkboxes are done: M1 can start UI layout, M3 can start discovery package, M4 can start discovery.rs scaffold.

---

### PHASE A — M2 Discovery Store (Depends on: Day 1 done)

- [ ] **M2-A1** `controller/internal/discovery/store.go` — DB helpers: `UpsertDiscoveredServices`, `GetDiscoveredServices(shieldId)`, `DeleteDiscoveredService`, `UpsertScanResults`, `GetScanResults(requestId)`, `PurgeScanResults(olderThan)`
- [ ] **M2-A2** `controller/internal/discovery/config.go` — `DiscoveryConfig` struct, `ScanResultTTL` constant (24h)

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE B — M3 Resolvers (Depends on: Day 1 done + M2-A done)

- [ ] **M3-B1** `controller/graph/resolvers/discovery.resolvers.go` — `GetDiscoveredServices(shieldId)` query, `PromoteDiscoveredService(shieldId, protocol, port)` mutation, `TriggerScan(connectorId, targets, ports)` mutation, `GetScanResults(requestId)` query
- [ ] **M3-B2** `controller/graph/resolvers/helpers.go` — Add `toDiscoveredServiceGQL()` and `toScanResultGQL()` mappers

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE C — M3 Controller Control Handler (Depends on: Day 1 proto done + M3-B done)

- [ ] **M3-C1** `controller/internal/connector/control.go` — MODIFY: handle `ConnectorControlMessage.ShieldDiscoveryBatch` → call `discovery.UpsertDiscoveredServices()` for each report; handle `ConnectorControlMessage.ScanReport` → call `discovery.UpsertScanResults()`; on `TriggerScan` resolver call → inject `ScanCommand` into outbound Control stream
- [ ] **M3-C2** `controller/internal/connector/control.go` — Add `PurgeScanResults` background goroutine (runs every hour, purges results older than 24h)

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE D — M3 Connector Control Stream (Depends on: Day 1 proto done)

- [ ] **M3-D1** `connector/src/agent_server.rs` — MODIFY: when a Shield sends `ShieldControlMessage::DiscoveryReport`, buffer it in `ShieldState`; on next upstream flush (every 5s or on content change) batch all pending reports into `ConnectorControlMessage::ShieldDiscoveryBatch` and send upstream
- [ ] **M3-D2** `connector/src/control_plane.rs` — MODIFY: handle incoming `ConnectorControlMessage::ScanCommand` from Controller → parse into `ScanCommand` struct → spawn `scan::execute_scan()` → send result as `ConnectorControlMessage::ScanReport` upstream
- [ ] **M3-D3** `connector/src/discovery/` — NEW directory with modules (ported from reference):
  - `mod.rs` — pub mod declarations
  - `scan.rs` — `ScanCommand`, `DiscoveredResource`, `ScanReport`, `execute_scan()` (TCP ping with semaphore, max 32 concurrent)
  - `tcp_ping.rs` — async TCP connect with timeout (tokio + timeout)
  - `service_detect.rs` — port → service name lookup table (mirrors reference project)
  - `scope.rs` — CIDR/IP range expander, validates max 512 targets

> Build check: `cd connector && cargo build` must pass.

---

### PHASE E — M4 Shield Discovery (Depends on: Day 1 proto done)

- [ ] **M4-E1** `shield/src/discovery.rs` — NEW file:
  - `discover_sync()` — Linux: parse `/proc/net/tcp` + `/proc/net/tcp6` for LISTEN (state 0A) sockets; filter loopback, ephemeral, ignored ports (5355 LLMNR, 631 IPP, 5353 mDNS)
  - `service_from_port(port)` — well-known port → service name lookup
  - `compute_fingerprint(ports)` — hash over sorted (port, protocol) set
  - `run_discovery_diff(shield_id, sent, last_fp, seq)` — computes diff, returns `DiscoveryReport` or `None` if unchanged
  - `run_discovery_full_sync(shield_id, sent, last_fp, seq)` — full snapshot, always returns `DiscoveryReport`
- [ ] **M4-E2** `shield/src/config.rs` — Add `discovery_interval_secs: u64` (default 60)
- [ ] **M4-E3** `shield/src/heartbeat.rs` — MODIFY: after existing health report send, call `discovery::run_discovery_diff()`; on first connect call `run_discovery_full_sync()`; send result as `ShieldControlMessage::DiscoveryReport` on the Control stream
- [ ] **M4-E4** `shield/src/main.rs` — Add `mod discovery`

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---

### PHASE F — M1 Frontend (Depends on: Day 1 codegen done)

- [ ] **M1-F1** `admin/src/pages/Shields.tsx` — Add "Discovered Services" expandable panel per shield row; columns: Protocol, Port, Service Name, Bound IP, First Seen, Last Seen, Promote button
- [ ] **M1-F2** `admin/src/components/PromoteServiceModal.tsx` — Confirm modal: "Promote port 22/tcp (SSH) on 192.168.1.5 to a resource?" — prefills CreateResource form
- [ ] **M1-F3** `admin/src/pages/RemoteNetworks.tsx` — Add "Scan Network" button per network; opens `ScanModal`
- [ ] **M1-F4** `admin/src/components/ScanModal.tsx` — Form: target IPs/CIDR (textarea), ports (comma-separated); submits `TriggerScan` mutation; polls `GetScanResults(requestId)` every 3s; shows results table with Create Resource button per row
- [ ] **M1-F5** `admin/src/graphql/queries.graphql` — Add `GetDiscoveredServices`, `GetScanResults`
- [ ] **M1-F6** `admin/src/graphql/mutations.graphql` — Add `PromoteDiscoveredService`, `TriggerScan`
- [ ] **M1-F7** Run `cd admin && npm run codegen` — regenerate TypeScript hooks

> Build check: `cd admin && npm run build` must pass.

---

### PHASE G — M3 RDE Device Tunnel + Connector Reliability (Depends on: Day 1 proto done — TunnelOpen/Opened/Data/Close fields 8-11)

- [ ] **M3-G1** `connector/src/device_tunnel.rs` — NEW: TLS listener `:9092`, `TunnelRequest`/`TunnelResponse` JSON handshake, `check_access()` HTTP fallback, protected path via `AgentTunnelHub` relay, direct path via `copy_bidirectional`, `relay_udp()` 4-byte length-prefix, `emit_access_log()`
- [ ] **M3-G2** `connector/src/quic_listener.rs` — NEW: QUIC/UDP listener `:9092`, ALPN `ztna-tunnel-v1`, delegates each bidir stream to `device_tunnel::handle_stream()`
- [ ] **M3-G3** `connector/src/agent_tunnel.rs` — MODIFY: dispatch `TunnelOpened/Data/Close` from Shield Control stream into hub sessions; send `TunnelOpen` to Shield via control stream sender
- [ ] **M3-G4** `connector/src/net_util.rs` — NEW: `lan_ip()` UDP routing trick for private IP discovery
- [ ] **M3-G5** `connector/src/crl.rs` — NEW: `CrlManager` — fetch `/ca.crl` DER, cache revoked serials, background refresh every 5min
- [ ] **M3-G6** `connector/src/watchdog.rs` — NEW: `notify_ready()` + `spawn_watchdog()` for systemd sd_notify integration
- [ ] **M3-G7** `connector/src/main.rs` — MODIFY: wire all listeners in correct order, `notify_ready()`, `spawn_watchdog()`
- [ ] **M3-G8** `controller/internal/device/check_access.go` — NEW: `POST /api/device/check-access` — validate Bearer JWT, look up resource, return `{ok, shield_id, connector_id, protocol}`

> Build check: `cd connector && cargo build` and `cd controller && go build ./...` must pass.

---

### PHASE H — M4 Shield Tunnel Relay (Depends on: Day 1 proto done + M4-E done)

- [ ] **M4-H1** `shield/src/tunnel.rs` — NEW: `TunnelHub`, `handle_tunnel_open()` (connect TCP locally, register session), `handle_tunnel_data()` (forward bytes to local TCP), `handle_tunnel_close()` (drop session)
- [ ] **M4-H2** `shield/src/heartbeat.rs` — MODIFY: dispatch `TunnelOpen/Data/Close` from incoming Control stream messages to `tunnel::` handlers
- [ ] **M4-H3** `shield/src/main.rs` — Add `mod tunnel`

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---



Run these once all phases are complete:

- [ ] `buf generate` (from repo root) — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd connector && cargo build` — clean (warnings OK)
- [ ] `cargo build --manifest-path shield/Cargo.toml` — clean
- [ ] `cd admin && npm run build` — clean
- [ ] Full DB migration: `008_discovery.sql` runs on fresh DB
- [ ] Shield connects → within 60s, discovered services appear in UI for that shield
- [ ] Stop a service on Shield host → within 120s, it disappears from discovered services
- [ ] Start a new service on Shield host → within 120s, it appears in discovered services
- [ ] Click Promote → resource created with correct host/port/protocol, auto-matched shield
- [ ] Trigger Scan from UI → results appear within 10s for reachable hosts
- [ ] Scan result "Create Resource" → resource row created correctly
- [ ] Scan results purged after 24h (background goroutine)
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
       M2-D1-A (shield.proto: DiscoveryReport=7, TunnelOpen=8..TunnelClose=11)
       M2-D1-B (connector.proto: ShieldDiscoveryBatch=8, ScanReport=9, ScanCommand=10)
       M2-D1-C (008_discovery.sql)                                   Day 1 — FIRST
       M2-D1-D (graph/discovery.graphqls)
              │
              ▼
       buf generate + go generate
              │
      ┌───────┼─────────────┬──────────────┬──────────────┐
      ▼       ▼             ▼              ▼              ▼
    M2-A    M3-B          M4-E           M1-F           M3-G
  (discovery (resolvers)  (discovery.rs  (layout)       (RDE tunnel
   store)                  + wiring)                     QUIC, CRL,
      │       │                │                         watchdog)
      ▼       ▼                ▼                              │
    M2-A2   M3-C            M4-E3                             ▼
  (config)  (controller      (heartbeat               M4-H (Shield
             control.go)      wiring)                  tunnel relay)
              │
              ▼
            M3-D
          (connector
           agent_server +
           control_plane +
           discovery/)
```

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Before touching any file, confirm dependency checkboxes are checked.
2. **Proto field numbers are permanent.** Never reuse or renumber. Sprint 6 assigned: ShieldControlMessage 7–11; ConnectorControlMessage 8–10.
3. **Discovery rides existing streams.** No new RPCs. DiscoveryReport on Shield Control stream; ShieldDiscoveryBatch/ScanCommand/ScanReport on Connector Control stream.
4. **Tunnel messages ride existing streams.** No new RPCs. TunnelOpen=8..TunnelClose=11 on Shield Control stream. Connector initiates, Shield relays.
5. **Shield scans only its own host.** `/proc/net/tcp` — no network scanning from Shield.
6. **Connector scanner is network-wide.** Controller triggers via ScanCommand; connector TCP-pings targets.
7. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.
8. **Scan limits are hard caps.** Max 512 targets, 16 ports, 32 concurrent probes — enforced in `scope.rs` and `scan.rs`.
9. **RDE protected path.** For resources with `shield_id` set, Connector MUST relay via `AgentTunnelHub` → Shield Control stream. Direct connect will fail due to nftables.
10. **QUIC is on same port as TLS.** `:9092` — UDP for QUIC, TCP for TLS. OS demuxes by transport protocol.

See individual member phase files for detailed specs:
- [[Sprint6/Member1-Frontend/Phase1-Discovery-Tab]]
- [[Sprint6/Member1-Frontend/Phase2-Scan-UI]]
- [[Sprint6/Member2-Go-Proto-DB/Phase1-Proto-Schema]]
- [[Sprint6/Member2-Go-Proto-DB/Phase2-Discovery-Store]]
- [[Sprint6/Member3-Go-Connector/Phase1-Discovery-Resolvers]]
- [[Sprint6/Member3-Go-Connector/Phase2-Controller-Control-Handler]]
- [[Sprint6/Member3-Go-Connector/Phase3-Connector-Discovery]]
- [[Sprint6/Member3-Go-Connector/Phase4-RDE-Device-Tunnel]]
- [[Sprint6/Member3-Go-Connector/Phase5-Connector-Extras]]
- [[Sprint6/Member4-Rust-Shield/Phase1-Discovery-Module]]
- [[Sprint6/Member4-Rust-Shield/Phase2-Control-Stream-Wiring]]
- [[Sprint6/Member4-Rust-Shield/Phase3-Shield-Tunnel-Relay]]
