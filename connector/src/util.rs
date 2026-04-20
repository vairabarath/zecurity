// util.rs — Shared utility functions used across multiple connector modules.

use std::fs;
use if_addrs::IfAddr;

/// Read the system hostname from /etc/hostname.
/// Falls back to "unknown" if the file is missing or unreadable.
pub fn read_hostname() -> String {
    fs::read_to_string("/etc/hostname")
        .map(|h| h.trim().to_string())
        .unwrap_or_else(|_| "unknown".to_string())
}

const VIRTUAL_PREFIXES: &[&str] = &["docker", "br-", "virbr", "veth", "lo"];
const PREFERRED_PREFIXES: &[&str] = &["eth", "en", "wlan", "wl"];

fn is_rfc1918(ip: std::net::Ipv4Addr) -> bool {
    let o = ip.octets();
    o[0] == 10
        || (o[0] == 172 && (16..=31).contains(&o[1]))
        || (o[0] == 192 && o[1] == 168)
}

/// Detect the best RFC-1918 LAN IP on this host.
///
/// Prefers physical interfaces (eth*, en*, wlan*, wl*) over others.
/// Skips virtual/Docker bridges (docker*, br-*, virbr*, veth*, lo).
/// Returns None if no suitable address is found (non-fatal).
pub fn detect_lan_ip() -> Option<String> {
    let ifaces = if_addrs::get_if_addrs().ok()?;
    // Two passes: preferred interface names first, then any RFC-1918
    for prefer_named in [true, false] {
        for iface in &ifaces {
            if VIRTUAL_PREFIXES.iter().any(|p| iface.name.starts_with(p)) {
                continue;
            }
            if let IfAddr::V4(ref v4) = iface.addr {
                if !is_rfc1918(v4.ip) {
                    continue;
                }
                if prefer_named {
                    if PREFERRED_PREFIXES.iter().any(|p| iface.name.starts_with(p)) {
                        return Some(v4.ip.to_string());
                    }
                } else {
                    return Some(v4.ip.to_string());
                }
            }
        }
    }
    None
}
