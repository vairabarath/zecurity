use anyhow::Result;
use std::collections::HashSet;
use std::hash::Hash;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use tracing::{info, warn};

/// Ports to skip — system noise that is never interesting.
const IGNORED_PORTS: &[(u16, &str)] = &[
    (5355, "LLMNR"),
    (631,  "IPP"),
    (5353, "mDNS"),
    (9091, "zecurity-connector"),
];

/// UDP-specific ports to skip — infrastructure/system noise.
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

/// Ephemeral port range start (Linux default).
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

fn is_externally_listening(addr: &SocketAddr) -> bool {
    !addr.ip().is_loopback()
}

/// Normalize a raw bound IP from /proc/net/tcp[6]:
/// - `0.0.0.0` / `::` (wildcard)       → shield LAN IP (reachable on all interfaces)
/// - `::ffff:x.x.x.x` (IPv4-mapped)    → strip to plain IPv4
/// - `::ffff:0.0.0.0` (mapped wildcard) → shield LAN IP
/// - anything else                      → unchanged
/// Returns `None` if the address is a wildcard but no LAN IP could be detected.
fn normalize_bound_ip(ip: IpAddr, lan_ip: Option<IpAddr>) -> Option<IpAddr> {
    match ip {
        IpAddr::V4(v4) if v4.is_unspecified() => lan_ip,
        IpAddr::V4(_) => Some(ip),
        IpAddr::V6(v6) => {
            if let Some(v4) = v6.to_ipv4_mapped() {
                // IPv4-mapped: ::ffff:x.x.x.x
                if v4.is_unspecified() { lan_ip } else { Some(IpAddr::V4(v4)) }
            } else if v6.is_unspecified() {
                // Pure IPv6 wildcard: ::
                lan_ip
            } else {
                // Genuine IPv6 unicast — keep as-is
                Some(ip)
            }
        }
    }
}

fn is_ignored_port(port: u16) -> bool {
    IGNORED_PORTS.iter().any(|(p, _)| *p == port)
}

fn is_ignored_udp_port(port: u16) -> bool {
    IGNORED_UDP_PORTS.iter().any(|(p, _)| *p == port)
}

fn should_include_port(port: u16) -> bool {
    if is_ignored_port(port) {
        return false;
    }
    // Include ephemeral ports only if they map to a well-known service.
    if port >= EPHEMERAL_PORT_START {
        return !service_from_port(port).is_empty();
    }
    true
}

// ── Linux: /proc/net parsers ──────────────────────────────────────────────────

fn parse_proc_ipv4(hex: &str) -> Option<Ipv4Addr> {
    let n = u32::from_str_radix(hex, 16).ok()?;
    // /proc/net/tcp stores addresses in little-endian host byte order.
    Some(Ipv4Addr::from(n.to_be()))
}

fn parse_proc_ipv6(hex: &str) -> Option<Ipv6Addr> {
    if hex.len() != 32 {
        return None;
    }
    let mut octets = [0u8; 16];
    for i in 0..4 {
        let word = u32::from_str_radix(&hex[i * 8..(i + 1) * 8], 16).ok()?;
        // /proc/net/tcp6 prints each 4-byte group as a native (LE) u32 via %08X.
        // to_le_bytes() recovers the original network-order bytes directly.
        let bytes = word.to_le_bytes();
        octets[i * 4..i * 4 + 4].copy_from_slice(&bytes);
    }
    Some(Ipv6Addr::from(octets))
}

/// Parse /proc/net/tcp or /proc/net/tcp6 for LISTEN sockets (state 0A).
fn parse_proc_tcp(path: &str, is_v6: bool) -> Vec<SocketAddr> {
    let content = match std::fs::read_to_string(path) {
        Ok(c) => c,
        Err(e) => {
            warn!(path, error = %e, "could not read proc tcp file");
            return vec![];
        }
    };
    let mut results = vec![];
    for line in content.lines().skip(1) {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 4 {
            continue;
        }
        if fields[3] != "0A" {
            continue; // only TCP_LISTEN
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

/// Parse /proc/net/udp or /proc/net/udp6 for bound sockets (state 07).
/// State 07 (TCP_CLOSE) is how the kernel marks a UDP socket that has called
/// bind() and is waiting for datagrams — the connectionless equivalent of LISTEN.
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

/// Synchronous discovery scan — called via spawn_blocking from async context.
pub fn discover_sync() -> Result<Vec<DiscoveredService>> {
    // Detect LAN IP once — used to replace wildcard bound addresses.
    let lan_ip: Option<IpAddr> = crate::util::detect_lan_ip()
        .and_then(|s| s.parse().ok());

    let mut addrs = parse_proc_tcp("/proc/net/tcp", false);
    addrs.extend(parse_proc_tcp("/proc/net/tcp6", true));
    info!("discovery: raw TCP listener count = {}", addrs.len());

    let mut exposed: Vec<DiscoveredService> = Vec::new();
    // Dedup by (port, protocol): a dual-stack wildcard service appears in both
    // /proc/net/tcp and /proc/net/tcp6 — keep only the first entry per port.
    let mut seen: HashSet<(u16, &'static str)> = HashSet::new();

    for addr in &addrs {
        if !is_externally_listening(addr) {
            continue;
        }
        let port = addr.port();
        if !should_include_port(port) {
            continue;
        }
        // Normalize wildcard / IPv4-mapped addresses. Skip if we cannot
        // resolve a wildcard to a concrete LAN IP (no usable bound_ip to store).
        let Some(bound_ip) = normalize_bound_ip(addr.ip(), lan_ip) else {
            warn!(port, raw_ip = %addr.ip(), "skipping wildcard listener — LAN IP not detected");
            continue;
        };
        if !seen.insert((port, "tcp")) {
            continue; // already added this port from the other ip-family file
        }
        exposed.push(DiscoveredService {
            protocol:     "tcp",
            port,
            bound_ip:     bound_ip.to_string(),
            service_name: service_from_port(port).to_string(),
        });
    }
    // ── UDP ──────────────────────────────────────────────────────────────────
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

    info!("discovery: externally-listening services = {}", exposed.len());
    Ok(exposed)
}

pub async fn discover_exposed_services() -> Result<Vec<DiscoveredService>> {
    tokio::task::spawn_blocking(discover_sync).await?
}

/// Hash over sorted (port, protocol) set — used to detect changes without a full diff.
pub fn compute_fingerprint(ports: &HashSet<(u16, String)>) -> u64 {
    let mut sorted: Vec<_> = ports.iter().collect();
    sorted.sort();
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    for entry in &sorted {
        entry.hash(&mut hasher);
    }
    std::hash::Hasher::finish(&hasher)
}

/// Result of a discovery scan — ready to be converted to proto DiscoveryReport in Phase 2.
pub struct DiscoveryDiff {
    pub shield_id:   String,
    pub seq:         u64,
    pub added:       Vec<DiscoveredService>,
    pub removed:     Vec<(u16, String)>, // (port, protocol)
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
        .filter(|p| !current_ports.contains(*p))
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
/// Called on first connect and whenever a fingerprint gap is detected.
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
