// util.rs — Shared utility functions used across multiple shield modules
//
// USED BY:
//   enrollment.rs — read_hostname() for EnrollRequest.hostname
//   heartbeat.rs  — read_hostname() + get_public_ip() for HeartbeatRequest
//   crypto.rs     — sha256_hex() is defined there (crypto concern), not here

use std::fs;

/// Read the system hostname.
///
/// Tries /etc/hostname first (standard on Linux/systemd systems).
/// Falls back to "unknown" if the file is missing or unreadable.
///
/// Used in EnrollRequest and HeartbeatRequest so the admin UI can
/// display a human-readable name for each shield.
pub fn read_hostname() -> String {
    fs::read_to_string("/etc/hostname")
        .map(|h| h.trim().to_owned())
        .unwrap_or_else(|_| "unknown".to_owned())
}

/// Fetch the shield's public IP address from an external echo service.
///
/// WHY:
///   The shield may be behind NAT. The connector and controller need the
///   public IP for routing and display in the admin UI.
///
/// Returns None if the request fails (no internet, timeout, etc.).
/// This is best-effort — heartbeat still proceeds without a public IP.
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
