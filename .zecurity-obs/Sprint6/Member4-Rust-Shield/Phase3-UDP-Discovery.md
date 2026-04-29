---
type: phase
status: done
sprint: 6
member: M4
phase: Phase3-UDP-Discovery
depends_on:
  - M4-E1 (discovery.rs — Phase 1 complete)
  - M4-E2 (control_stream.rs wiring — Phase 2 complete)
tags:
  - rust
  - shield
  - discovery
  - udp
---

# M4 Phase 3 — UDP Local Discovery

---

## Why This Exists

The resource UI already allows admins to create both TCP and UDP resources. Neither has data-plane tunneling yet — Sprint 10 engineers both (WireGuard/tun). Since TCP and UDP resources are on equal footing today, discovery must cover both. The Shield reads `/proc/net/udp` the same way it reads `/proc/net/tcp` — passive, local, zero network probing.

**Rule: Discovery must follow resource capability, not tunnel capability.**

---

## Scope

One file only: `shield/src/discovery.rs`

No proto changes. No controller changes. No DB migration. No UI changes.

The proto `DiscoveredService` message already has a `protocol` field.
The DB PK `(shield_id, protocol, port)` already separates TCP and UDP rows.
The UI discovery tab already displays the protocol column.

---

## Design Decisions

| Decision | Detail |
|---|---|
| State filter for `/proc/net/udp` | State `07` (`TCP_CLOSE`) — means socket is bound and waiting for datagrams (the UDP equivalent of LISTEN) |
| Separate ignored list | `IGNORED_UDP_PORTS` — different noise profile from TCP |
| Separate service lookup | `service_from_port_udp()` — UDP services differ from TCP on same ports |
| Dedup key | Existing `HashSet<(u16, &'static str)>` handles it — `(80, "tcp")` and `(80, "udp")` are separate entries naturally |
| Ephemeral filter | Same rule as TCP: ephemeral ports (≥32768) only included if `service_from_port_udp()` returns a non-empty name |

---

## What to Add to `shield/src/discovery.rs`

### Step 1 — UDP Ignored Ports Constant

Add after the existing `IGNORED_PORTS` constant:

```rust
/// UDP-specific ports to skip — system/infrastructure noise.
const IGNORED_UDP_PORTS: &[(u16, &str)] = &[
    (67,   "DHCP Server"),
    (68,   "DHCP Client"),
    (123,  "NTP"),
    (137,  "NetBIOS-NS"),
    (138,  "NetBIOS-DGM"),
    (1900, "SSDP"),
    (5353, "mDNS"),
    (5355, "LLMNR"),
];
```

> **Why different from TCP?** UDP has more system-level noise. NTP, DHCP, NetBIOS, SSDP are UDP-only and would flood results. DNS (53) is intentionally NOT ignored — an internal DNS server on a Shield host is a valid resource to protect.

---

### Step 2 — UDP Service Name Lookup

Add after `service_from_port()`:

```rust
/// Static lookup of well-known UDP port numbers to service names.
pub fn service_from_port_udp(port: u16) -> &'static str {
    match port {
        53   => "DNS",
        69   => "TFTP",
        161  => "SNMP",
        162  => "SNMP Trap",
        500  => "IKE",
        514  => "Syslog",
        1194 => "OpenVPN",
        1812 => "RADIUS",
        1813 => "RADIUS Accounting",
        4500 => "IPSec NAT-T",
        5060 => "SIP",
        5061 => "SIP-TLS",
        _    => "",
    }
}
```

---

### Step 3 — UDP Ignored Port Helper

Add after `is_ignored_port()`:

```rust
fn is_ignored_udp_port(port: u16) -> bool {
    IGNORED_UDP_PORTS.iter().any(|(p, _)| *p == port)
}
```

---

### Step 4 — `/proc/net/udp` Parser

Add after `parse_proc_tcp()`:

```rust
/// Parse /proc/net/udp or /proc/net/udp6 for bound sockets (state 07).
/// State 07 (TCP_CLOSE) is how the kernel represents a bound-but-unconnected
/// UDP socket — the connectionless equivalent of LISTEN.
fn parse_proc_udp(path: &str, is_v6: bool) -> Vec<SocketAddr> {
    let content = match std::fs::read_to_string(path) {
        Ok(c) => c,
        Err(e) => {
            warn!(path, error = %e, "could not read proc udp file");
            return vec![];
        }
    };
    let mut results = vec![];
    for line in content.lines().skip(1) {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 4 {
            continue;
        }
        if fields[3] != "07" {
            continue; // only bound UDP sockets
        }
        let parts: Vec<&str> = fields[1].split(':').collect();
        if parts.len() != 2 {
            continue;
        }
        let port = match u16::from_str_radix(parts[1], 16) {
            Ok(p) => p,
            Err(_) => continue,
        };
        let ip: IpAddr = if is_v6 {
            match parse_proc_ipv6(parts[0]) {
                Some(v6) => IpAddr::V6(v6),
                None => continue,
            }
        } else {
            match parse_proc_ipv4(parts[0]) {
                Some(v4) => IpAddr::V4(v4),
                None => continue,
            }
        };
        results.push(SocketAddr::new(ip, port));
    }
    results
}
```

---

### Step 5 — Extend `discover_sync()`

The current `discover_sync()` ends after processing TCP. Add the UDP block **after** the TCP block, before the final `info!` log and `Ok(exposed)`.

The `seen` HashSet already uses `(port, &'static str)` as the key — adding `"udp"` entries is naturally separate from `"tcp"` entries.

```rust
// ── UDP ──────────────────────────────────────────────────────────────────────
let mut udp_addrs = parse_proc_udp("/proc/net/udp", false);
udp_addrs.extend(parse_proc_udp("/proc/net/udp6", true));
info!("discovery: raw UDP listener count = {}", udp_addrs.len());

for addr in &udp_addrs {
    if !is_externally_listening(addr) {
        continue;
    }
    let port = addr.port();
    if is_ignored_udp_port(port) {
        continue;
    }
    if port >= EPHEMERAL_PORT_START && service_from_port_udp(port).is_empty() {
        continue;
    }
    let Some(bound_ip) = normalize_bound_ip(addr.ip(), lan_ip) else {
        warn!(port, raw_ip = %addr.ip(), "skipping UDP wildcard — LAN IP not detected");
        continue;
    };
    if !seen.insert((port, "udp")) {
        continue; // dedup across /proc/net/udp and /proc/net/udp6
    }
    exposed.push(DiscoveredService {
        protocol:     "udp",
        port,
        bound_ip:     bound_ip.to_string(),
        service_name: service_from_port_udp(port).to_string(),
    });
}
```

---

## What Does NOT Change

| Component | Reason |
|---|---|
| `proto/shield/v1/shield.proto` | `DiscoveredService.protocol` field already exists |
| `controller/internal/discovery/store.go` | PK is `(shield_id, protocol, port)` — UDP rows stored separately already |
| `controller/graph/resolvers/discovery.resolvers.go` | No changes |
| `controller/migrations/010_discovery.sql` | Schema already supports any `protocol` string |
| `admin/src/pages/Shields.tsx` | Protocol column already rendered — UDP rows appear automatically |
| `connector/src/discovery/scan.rs` | Connector network scanner stays TCP-only — UDP network scanning is a different problem, deferred to Sprint 10 |

---

## What the UI Shows After This Ships

The Shields page discovery tab will show rows like:

| Protocol | Port | Service | Bound IP |
|---|---|---|---|
| TCP | 22 | SSH | 192.168.1.5 |
| TCP | 443 | HTTPS | 192.168.1.5 |
| UDP | 53 | DNS | 192.168.1.5 |
| UDP | 5060 | SIP | 192.168.1.5 |

Admins can then promote any UDP service to a resource. The resource is stored with `protocol=udp`. Tunneling enforcement is Sprint 10.

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
```

Warnings OK. Errors not.

---

## Verification

After deploying the updated Shield binary:

1. Run `ss -ulnp` on the Shield host — note which UDP ports are externally bound
2. Within 60s, those ports appear in the Shields discovery tab with `protocol=udp`
3. Ports in `IGNORED_UDP_PORTS` (67, 68, 123, 137, 138, 1900, 5353, 5355) must NOT appear
4. A UDP port bound only to `127.0.0.1` or `::1` must NOT appear
5. Click Promote on a UDP service — resource created with `protocol=udp` and correct `bound_ip`
