---
type: decision
status: accepted
date: 2026-06-23
related:
  - "[[Decisions/ADR-003-Client-TUN-Transparent-Proxy]]"
  - "[[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]"
  - "[[Sprint8/Member4-Rust-Client-Connector/Phase1-ACL-Snapshot-Handling]]"
tags:
  - adr
  - client
  - tun
  - split-tunneling
  - nftables
  - acl
---

# ADR-009 — Client Port-Based Split Tunneling via nftables + Route Table 105

## Context

ADR-003 specified that the client installs one `/32` host route per resource IP from the ACL snapshot into the kernel's main routing table. This worked for IP-level routing but had two deficiencies:

1. **All ports on a resource IP were captured.** A `/32` route for `10.1.0.5` routes every port (22, 80, 443, 5432, …) into `zecurity0`. smoltcp only has listeners for ACL-defined ports. Non-ACL ports hit smoltcp, find no listener, and receive RST — even if the destination is legitimately reachable on the local network for an unrelated service.

2. **No SPIFFE-filtered routing.** Routes were installed for all ACL entries regardless of whether the device had permission. Devices in no group would still have resource IPs routed through the TUN.

These make `/32`-based routing incorrect for true split tunneling, where only the exact permitted `(IP, port)` flows should enter the Zecurity data plane.

## Decision

Replace per-IP `/32` main-table routes with a **two-layer kernel mechanism**:

1. **nftables OUTPUT chain** — marks only permitted `(daddr, dport)` TCP flows with `SO_MARK 0x5a` at the point of packet generation, before route lookup.
2. **Custom route table 105** — holds `/32` host routes for resource IPs pointing at `zecurity0`. Only marked packets use this table via an ip rule at priority 49.

Unmarked packets (non-ACL ports on a resource IP, or IPs not in the ACL at all) use the normal kernel main table and reach the network directly.

### Kernel configuration applied on `zecurity up`

```
nft add table inet zecurity_client
nft add chain inet zecurity_client output { type route hook output priority mangle; policy accept; }
# one rule per (ip, port) from allowed_entries:
nft add rule inet zecurity_client output ip daddr <IP> tcp dport <PORT> meta mark set 0x5a

ip rule add fwmark 0x5a lookup 105 priority 49
# one route per unique resource IP:
ip route replace <IP>/32 dev zecurity0 table 105
```

### Cleanup on `zecurity down`

```
nft delete table inet zecurity_client
ip rule del fwmark 0x5a lookup 105 priority 49
ip route flush table 105
```

## SPIFFE-Filtered Allowed Entries

Before configuring kernel policy, `handle_up` in `daemon.rs` filters the ACL snapshot entries to only those where the device's own SPIFFE ID appears in `allowed_spiffe_ids`:

```rust
let allowed_entries: Vec<AclEntry> = acl.entries.iter()
    .filter(|e| e.allowed_spiffe_ids.iter().any(|id| id == my_spiffe.as_str()))
    .cloned()
    .collect();
```

`allowed_entries` is then passed to:
- `configure_allowed_flows()` — builds the nftables + ip rule + ip route kernel policy
- `build_transports_by_resource()` — constructs the `(Ipv4Addr, u16) → Option<Arc<ClientTransport>>` transport map
- `net_stack::run()` — sets up smoltcp listeners only for allowed ports

This means routes, listeners, and transports are all scoped to exactly what this device is permitted to access.

## Three-Way Transport Map

`build_transports_by_resource` returns `HashMap<(Ipv4Addr, u16), Option<Arc<ClientTransport>>>`:

| Map entry | Meaning | Behavior |
|---|---|---|
| `Some(Some(transport))` | Managed resource, connector online | Tunnel via QUIC |
| `Some(None)` | Managed resource, connector offline | Fail closed (RST) |
| absent | Should not occur (smoltcp only listens on allowed ports) | Fail closed |

The previous `None → bypass` branch (via `relay_tcp_bypass`) has been removed. Bypass is now handled entirely at the kernel level by the nftables marking mechanism — non-ACL traffic is never marked, never enters route table 105, and never reaches the TUN.

## Why Not Main-Table /32 Routes

The original approach routed entire IPs. This cannot distinguish ports at the kernel routing layer — routing decisions are made on destination IP only. Port-level decisions require either a firewall/mangle hook (nftables OUTPUT) or policy routing keyed on a packet mark. This ADR uses both.

## Why Not TPROXY

TPROXY (ADR-003 "Why Not TPROXY") was rejected for broad transparent proxying. The conclusion holds here too. The nftables OUTPUT mark approach used in this ADR is simpler: it runs at packet generation time, marks exactly the flows that should enter the TUN, and requires no TPROXY socket or `IP_TRANSPARENT`. It is closer to the WireGuard/Tailscale model of using a mark + policy route.

## Data Structures

```rust
// tun.rs
pub struct AllowedFlow {
    pub ip:   Ipv4Addr,
    pub port: u16,
}

// daemon.rs — passed to configure_allowed_flows
let allowed_flows: Vec<AllowedFlow> = allowed_entries.iter()
    .filter(|e| e.protocol.to_lowercase() == "tcp" || e.protocol.is_empty())
    .filter_map(|e| {
        let IpAddr::V4(ip) = e.address.parse().ok()? else { return None; };
        Some(AllowedFlow { ip, port: e.port as u16 })
    })
    .collect();
```

## Constants (tun.rs)

| Constant | Value | Purpose |
|---|---|---|
| `ZECURITY_TABLE` | `zecurity_client` | nft table name |
| `ZECURITY_CHAIN` | `output` | nft chain name |
| `ZECURITY_MARK` | `0x5a` | fwmark value |
| `ZECURITY_ROUTE_TABLE` | `105` | ip rule/route table id |
| `ZECURITY_RULE_PRIORITY` | `49` | ip rule priority (below default 32766) |

Table 105 is not reserved by IANA or the kernel. If a conflict arises in a future deployment, this value should become a daemon configuration option.

## Privilege Model

No change from ADR-002 / ADR-003. `CAP_NET_ADMIN` is required to run `nft` and `ip rule`/`ip route`. These are called from the daemon process which already holds the capability via `AmbientCapabilities` in the systemd unit.

## Consequences

- Non-ACL ports on a resource IP now bypass the TUN and reach the network normally — true port-based split tunneling.
- Devices with no group membership get no routes and no smoltcp listeners.
- The `relay_tcp_bypass` function and explicit `socket2` dependency are removed from `net_stack.rs`.
- nftables must be present on the host. This is standard on all modern Linux distros (kernel 3.13+, nft userspace tool). Add a preflight check if targeting minimal container images.
- UDP resources are unaffected by this ADR — UDP split tunneling is out of scope and deferred.

## When To Revisit

- **UDP split tunneling**: When UDP resources are added, a similar mechanism is needed for UDP flows. The same nftables table can gain UDP rules.
- **Table 105 conflict**: Make `ZECURITY_ROUTE_TABLE` a daemon config option if enterprise environments use table 105 for other policy routing.
- **macOS**: nftables and `ip rule` do not exist on macOS. The equivalent is `pfctl` + `utun` + `route add`. Needs a platform-abstracted `TunManager`.
