// util.rs — Shared utility functions used across multiple connector modules.

use std::fs;

/// Read the system hostname from /etc/hostname.
/// Falls back to "unknown" if the file is missing or unreadable.
pub fn read_hostname() -> String {
    fs::read_to_string("/etc/hostname")
        .map(|h| h.trim().to_string())
        .unwrap_or_else(|_| "unknown".to_string())
}
