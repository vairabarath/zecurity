// util.rs — Shared utility functions used across multiple shield modules
//
// USED BY:
//   enrollment.rs — read_hostname() for EnrollRequest.hostname
//   control_stream.rs — read_hostname() + get_public_ip() for ShieldHealthReport
//   crypto.rs     — sha256_hex() is defined there (crypto concern), not here

use if_addrs::IfAddr;
use std::fs;

/// Read the system hostname.
///
/// Tries /etc/hostname first (standard on Linux/systemd systems).
/// Falls back to "unknown" if the file is missing or unreadable.
///
/// Used in EnrollRequest and ShieldHealthReport so the admin UI can
/// display a human-readable name for each shield.
pub fn read_hostname() -> String {
    fs::read_to_string("/etc/hostname")
        .map(|h| h.trim().to_owned())
        .unwrap_or_else(|_| "unknown".to_owned())
}

const VIRTUAL_PREFIXES: &[&str] = &["docker", "br-", "virbr", "veth", "lo"];
const PREFERRED_PREFIXES: &[&str] = &["eth", "en", "wlan", "wl"];

fn is_rfc1918(ip: std::net::Ipv4Addr) -> bool {
    let o = ip.octets();
    o[0] == 10 || (o[0] == 172 && (16..=31).contains(&o[1])) || (o[0] == 192 && o[1] == 168)
}

/// Detect the best RFC-1918 LAN IP on this host.
/// Non-fatal: returns None if nothing suitable is found.
pub fn detect_lan_ip() -> Option<String> {
    let ifaces = if_addrs::get_if_addrs().ok()?;
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

/// Fetch the shield's public IP address from an external echo service.
///
/// WHY:
///   The shield may be behind NAT. The connector and controller need the
///   public IP for routing and display in the admin UI.
///
/// Returns None if the request fails (no internet, timeout, etc.).
/// This is best-effort — the control stream still proceeds without a public IP.
pub async fn get_public_ip() -> Option<String> {
    // api.ipify.org returns just the IP as plain text — simple and reliable
    reqwest::get("https://api.ipify.org")
        .await
        .ok()?
        .text()
        .await
        .ok()
        .map(|s| s.trim().to_owned())
        .filter(|s| !s.is_empty())
}
