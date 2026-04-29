---
type: decision
status: accepted
date: 2026-04-29
related:
  - "[[Decisions/ADR-002-Client-Daemon-Required]]"
  - "[[Sprint9/path]]"
tags:
  - adr
  - client
  - tun
  - transparent-proxy
  - sprint9
---

# ADR-003 — Client Transparent Proxy via TUN + smoltcp

## Context

Sprint 9 delivers the RDE data plane. The original design required an explicit `zecurity connect <resource>` command per resource. This is not the target UX — apps should connect to resources transparently without being aware of Zecurity, exactly as Twingate's client works.

The alternatives for transparent OS-level traffic interception on Linux are:

1. **Raw TUN + userspace TCP/IP stack (smoltcp)**
2. **nftables TPROXY redirect to a transparent proxy socket**
3. **SOCKS5 proxy (explicit, not transparent)**

## Decision

Use **raw TUN device + smoltcp** for transparent client-side proxying in Sprint 9.

The daemon (from Sprint 8.5) creates a TUN interface named `zecurity0`, assigns it a `/32` host address (e.g. `100.64.0.1/32`), and installs one `/32` host route per resource IP from the ACL snapshot pointing to it. No broad connected route is installed. The OS routes traffic destined for resource IPs into the TUN device. The daemon reads raw IP packets from the TUN fd, feeds them into a smoltcp interface, and for each new TCP connection or UDP datagram, opens a QUIC stream to the Connector and proxies the data.

This is the same approach Tailscale uses (their `netstack` package). It is the only option that handles both TCP and UDP correctly without requiring iptables/nftables changes at traffic time.

## Why Not TPROXY

nftables TPROXY requires the daemon to manipulate nftables rules at runtime and use `IP_TRANSPARENT` socket options. It is fragile in the OUTPUT chain and harder to reason about for UDP session tracking. It also requires `CAP_NET_ADMIN` for the nftables changes anyway. smoltcp gives cleaner separation: the TUN device is the only kernel boundary; everything else is userspace.

## Why Not SOCKS5

SOCKS5 requires apps to be configured to use the proxy. That is not transparent. Rejected.

## Architecture

```
App (curl, ssh, psql...)
  ↓  normal TCP/UDP socket
OS routing table
  → destination matches ACL snapshot IP → zecurity0 TUN
  → else → normal internet

zecurity0 TUN (kernel)
  ↓  raw IP packets
daemon (userspace, CAP_NET_ADMIN)
  smoltcp Interface
    TCP socket accepted
      → extract destination IP:port
      → look up ACL snapshot: find resource_id, find Connector address
      → TunnelPool::open_stream(connector_addr)
          → reuse existing QUIC connection, or connect new
          → open QUIC bidirectional stream
      → send TunnelRequest JSON { spiffe_id, destination, port, protocol }
      → read TunnelResponse JSON { ok, quic_addr }
      → copy_bidirectional(smoltcp_socket ↔ QUIC stream)
    UDP datagram received
      → session table lookup (src_ip:port + dst_ip:port → QUIC stream)
      → new session: open QUIC stream, send TunnelRequest
      → relay datagram with 4-byte length prefix
      → idle timeout: 30 seconds
```

## QUIC Connection Pool

One QUIC connection per Connector address. Multiple tunnel streams share the same connection. This is mandatory: without pooling, a browser opening 20 simultaneous connections to the same resource would open 20 QUIC connections.

```rust
pub struct TunnelPool {
    connections: Arc<Mutex<HashMap<SocketAddr, Arc<quinn::Connection>>>>,
    tls_config:  Arc<rustls::ClientConfig>,
}
```

`open_stream(connector_addr)` returns a bidirectional QUIC stream on an existing connection, or establishes a new connection if none exists.

## TUN Interface Configuration

```text
Interface name:  zecurity0
Address:         100.64.0.1/32   (/32 host address — no broad connected route installed)
Routes:          one /32 host route per resource IP from ACL snapshot
Lifecycle:       created on `zecurity up`, removed on `zecurity down` or daemon exit
```

The interface address is configurable. Default `100.64.0.1` uses the RFC 6598 CGNAT range but this can conflict with enterprise, mobile, or VPN networks that also use `100.64.0.0/10`. The installation should detect conflicts before bringing the TUN up.

Using `/32` on the interface (not `/10`) means no connected route is added for the whole CGNAT block. Only the explicit per-resource `/32` routes from the ACL snapshot are installed. This is the correct approach: route exactly what the ACL allows, nothing more.

## Protocol Layers

Keep these two layers distinct in code and documentation:

| Layer | Protocol | Description |
|-------|----------|-------------|
| **Outer transport** | QUIC over UDP | Between client daemon and Connector `:9092`. Multiplexed streams. mTLS device cert. |
| **Inner payload** | TCP stream or UDP datagrams | The actual resource protocol. Carried inside QUIC streams. |

"QUIC/UDP" is ambiguous. Use "outer transport: QUIC" and "inner protocol: TCP or UDP resource".

## New CLI Commands

| Command | What it does |
|---------|-------------|
| `zecurity up` | CLI sends `Up` IPC to daemon. Daemon creates TUN, loads routes from ACL snapshot, starts smoltcp loop. Apps can now reach resources. |
| `zecurity down` | CLI sends `Down` IPC to daemon. Daemon removes TUN and all routes. |

The previous `zecurity connect <resource>` explicit model is **not implemented**. `up`/`down` replace it entirely.

## Cargo Dependencies (client)

```toml
tun     = "0.6"          # TUN device creation and fd management
smoltcp = { version = "0.11", features = ["proto-ipv4", "socket-tcp", "socket-udp"] }
quinn   = "0.11"         # QUIC connection pool to Connector
```

## DNS Constraint

Sprint 9 resources are accessed by **IP address** from the ACL snapshot `address` field. The TUN routing table contains IP routes only.

DNS split-horizon (intercepting DNS queries, resolving resource names like `db.internal` to private IPs, returning them to apps) is **Sprint 11**. Until then, apps must connect to resource IPs directly, or admins must configure `/etc/hosts` entries manually.

This is not a bug — it is a documented scope constraint.

## Privilege Model

See ADR-002 Privilege Model section. `CAP_NET_ADMIN` is set in `client/zecurity-client.service`. The daemon creates the TUN device in response to the `Up` IPC message using the ambient capability. No other operations require elevation.

## Consequences

- Apps work transparently — no proxy configuration required.
- Multiple resources accessible simultaneously through one QUIC connection (streams multiplexed).
- UDP resources work with session-mapped relay.
- Matches Twingate's data plane UX on Linux.
- DNS requires a follow-on sprint.
- macOS requires replacing TUN+smoltcp with NetworkExtension (Sprint 11+).

## When To Revisit

- **macOS support**: NetworkExtension framework replaces this approach. Different API, same concept.
- **Privileged helper**: Replace `AmbientCapabilities` with a small helper binary that creates TUN and passes fd via `SCM_RIGHTS`. Removes the capability from the long-running daemon process entirely.
- **Kernel WireGuard**: If Zecurity eventually ships a kernel module, the userspace smoltcp approach can be replaced. Not planned.
