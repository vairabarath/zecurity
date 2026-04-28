---
type: phase
status: done
sprint: 6
member: M4
phase: Phase1-Discovery-Module
depends_on: []
tags:
  - rust
  - shield
  - discovery
---

# M4 Phase 1 — Shield Discovery Module

> **This phase has no external dependencies — you can start immediately on Day 1.**
> Proto types are NOT needed yet for this file. Write the structs in plain Rust first.

---

## What You're Building

`shield/src/discovery.rs` — scans the local host's listening TCP ports via `/proc/net/tcp` and computes differential reports.

---

## File to Create

### `shield/src/discovery.rs`

```rust
use anyhow::Result;
use std::collections::HashSet;
use std::hash::{Hash, Hasher};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use tracing::{info, warn};

/// Ports to skip — system noise that is never interesting.
const IGNORED_PORTS: &[(u16, &str)] = &[
    (5355, "LLMNR"),
    (631,  "IPP"),
    (5353, "mDNS"),
    (9091, "zecurity-connector"),  // skip our own gRPC port
];

/// Ephemeral port range start.
const EPHEMERAL_PORT_START: u16 = 32768;

#[derive(Debug, Clone)]
pub struct DiscoveredService {
    pub protocol:     &'static str,
    pub port:         u16,
    pub bound_ip:     String,
    pub service_name: String,
}

/// Static lookup of well-known port numbers to service names.
pub fn service_from_port(port: u16) -> &'static str {
    match port {
        21    => "FTP",
        22    => "SSH",
        25    => "SMTP",
        53    => "DNS",
        80    => "HTTP",
        110   => "POP3",
        143   => "IMAP",
        443   => "HTTPS",
        445   => "SMB",
        465   => "SMTPS",
        587   => "SMTP",
        993   => "IMAPS",
        995   => "POP3S",
        1433  => "MSSQL",
        1521  => "Oracle",
        2375  => "Docker",
        2376  => "Docker TLS",
        3000  => "Dev Server",
        3306  => "MySQL",
        3389  => "RDP",
        5432  => "PostgreSQL",
        5672  => "RabbitMQ",
        5900  => "VNC",
        6379  => "Redis",
        6443  => "Kubernetes API",
        8080  => "HTTP Proxy",
        8443  => "gRPC/TLS",
        9090  => "Prometheus",
        9200  => "Elasticsearch",
        27017 => "MongoDB",
        _     => "",
    }
}

fn is_externally_listening(addr: &SocketAddr) -> bool {
    !addr.ip().is_loopback()
}

fn is_ignored_port(port: u16) -> bool {
    IGNORED_PORTS.iter().any(|(p, _)| *p == port)
}

fn should_include_port(port: u16) -> bool {
    if is_ignored_port(port) {
        return false;
    }
    // Include ephemeral ports only if they map to a well-known service
    if port >= EPHEMERAL_PORT_START {
        return !service_from_port(port).is_empty();
    }
    true
}

// ── Linux: /proc/net/tcp parser ───────────────────────────────────────────────

fn parse_proc_ipv4(hex: &str) -> Option<Ipv4Addr> {
    let n = u32::from_str_radix(hex, 16).ok()?;
    Some(Ipv4Addr::from(n.to_be()))
}

fn parse_proc_ipv6(hex: &str) -> Option<Ipv6Addr> {
    if hex.len() != 32 { return None; }
    let mut octets = [0u8; 16];
    for i in 0..4 {
        let word = u32::from_str_radix(&hex[i * 8..(i + 1) * 8], 16).ok()?;
        let bytes = word.to_be().to_le_bytes();
        octets[i * 4..i * 4 + 4].copy_from_slice(&bytes);
    }
    Some(Ipv6Addr::from(octets))
}

/// Parse /proc/net/tcp{,6} for LISTEN sockets (state 0A).
fn parse_proc_tcp(path: &str, is_v6: bool) -> Vec<SocketAddr> {
    let content = match std::fs::read_to_string(path) {
        Ok(c) => c,
        Err(_) => return vec![],
    };
    let mut results = vec![];
    for line in content.lines().skip(1) {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 4 { continue; }
        if fields[3] != "0A" { continue; }  // 0A = TCP_LISTEN
        let parts: Vec<&str> = fields[1].split(':').collect();
        if parts.len() != 2 { continue; }
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

/// Synchronous discovery scan — called via spawn_blocking.
pub fn discover_sync() -> Result<Vec<DiscoveredService>> {
    let mut addrs = parse_proc_tcp("/proc/net/tcp", false);
    addrs.extend(parse_proc_tcp("/proc/net/tcp6", true));
    info!("discovery: raw TCP listener count = {}", addrs.len());

    let mut exposed = Vec::new();
    for addr in &addrs {
        if !is_externally_listening(addr) { continue; }
        let port = addr.port();
        if !should_include_port(port) { continue; }
        exposed.push(DiscoveredService {
            protocol:     "tcp",
            port,
            bound_ip:     addr.ip().to_string(),
            service_name: service_from_port(port).to_string(),
        });
    }
    info!("discovery: externally-listening services = {}", exposed.len());
    Ok(exposed)
}

pub async fn discover_exposed_services() -> Result<Vec<DiscoveredService>> {
    tokio::task::spawn_blocking(discover_sync).await?
}

/// Hash over sorted (port, protocol) set — used to detect changes without full diff.
pub fn compute_fingerprint(ports: &HashSet<(u16, String)>) -> u64 {
    let mut sorted: Vec<_> = ports.iter().collect();
    sorted.sort();
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    for entry in &sorted {
        entry.hash(&mut hasher);
    }
    std::hash::Hasher::finish(&hasher)
}

/// Result of a discovery scan — ready to convert to proto DiscoveryReport.
pub struct DiscoveryDiff {
    pub shield_id:   String,
    pub seq:         u64,
    pub added:       Vec<DiscoveredService>,
    pub removed:     Vec<(u16, String)>,  // (port, protocol)
    pub fingerprint: u64,
    pub full_sync:   bool,
}

/// Compute a differential scan.
/// Returns `None` if nothing changed (fingerprint unchanged).
pub async fn run_discovery_diff(
    shield_id: &str,
    sent_services: &mut HashSet<(u16, String)>,
    last_fingerprint: &mut u64,
    seq: &mut u64,
) -> Result<Option<DiscoveryDiff>> {
    let services = discover_exposed_services().await?;

    let current_ports: HashSet<(u16, String)> = services
        .iter()
        .map(|s| (s.port, s.protocol.to_string()))
        .collect();

    let fingerprint = compute_fingerprint(&current_ports);
    if fingerprint == *last_fingerprint {
        return Ok(None);
    }

    let added: Vec<DiscoveredService> = services
        .iter()
        .filter(|s| !sent_services.contains(&(s.port, s.protocol.to_string())))
        .cloned()
        .collect();

    let removed: Vec<(u16, String)> = sent_services
        .iter()
        .filter(|p| !current_ports.contains(p))
        .cloned()
        .collect();

    if added.is_empty() && removed.is_empty() {
        *last_fingerprint = fingerprint;
        return Ok(None);
    }

    *seq += 1;
    for svc in &added {
        sent_services.insert((svc.port, svc.protocol.to_string()));
    }
    for key in &removed {
        sent_services.remove(key);
    }
    *last_fingerprint = fingerprint;

    Ok(Some(DiscoveryDiff {
        shield_id: shield_id.to_string(),
        seq: *seq,
        added,
        removed,
        fingerprint,
        full_sync: false,
    }))
}

/// Run a full sync — always returns a report (full_sync=true).
pub async fn run_discovery_full_sync(
    shield_id: &str,
    sent_services: &mut HashSet<(u16, String)>,
    last_fingerprint: &mut u64,
    seq: &mut u64,
) -> Result<DiscoveryDiff> {
    let services = discover_exposed_services().await?;

    let current_ports: HashSet<(u16, String)> = services
        .iter()
        .map(|s| (s.port, s.protocol.to_string()))
        .collect();

    let fingerprint = compute_fingerprint(&current_ports);
    *seq += 1;

    sent_services.clear();
    for svc in &services {
        sent_services.insert((svc.port, svc.protocol.to_string()));
    }
    *last_fingerprint = fingerprint;

    Ok(DiscoveryDiff {
        shield_id: shield_id.to_string(),
        seq: *seq,
        added: services,
        removed: vec![],
        fingerprint,
        full_sync: true,
    })
}
```

---

## Build Check

```bash
cargo build --manifest-path shield/Cargo.toml
```

Warnings OK. Errors not. The proto types (`DiscoveryReport`) are wired in Phase 2.
