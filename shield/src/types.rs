// types.rs — Shared data structures used across multiple shield modules

use std::path::Path;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

/// Persistent state saved to state.json after successful enrollment.
///
/// WHY WE SAVE THIS:
///   After enrollment, the shield has a certificate, a connector address,
///   and a network interface. On restart, we don't re-enroll — we just
///   load this state and resume heartbeating. Re-enrollment would burn
///   the enrollment token and create a new shield record in the DB.
///
/// FILE LOCATION: <state_dir>/state.json (default: /var/lib/zecurity-shield/state.json)
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ShieldState {
    /// UUID assigned by the controller during enrollment.
    pub shield_id: String,

    /// Workspace trust domain (e.g. "ws-acme.zecurity.in").
    pub trust_domain: String,

    /// UUID of the connector this shield heartbeats through.
    pub connector_id: String,

    /// gRPC address of the assigned connector (e.g. "connector.example.com:9091").
    pub connector_addr: String,

    /// /32 IP address assigned to the zecurity0 interface (e.g. "100.64.0.5").
    pub interface_addr: String,

    /// RFC 3339 timestamp of when enrollment completed.
    pub enrolled_at: String,

    /// Unix timestamp of the certificate's NotAfter field.
    /// heartbeat.rs checks this to decide when to call RenewCert.
    pub cert_not_after: i64,
}

impl ShieldState {
    /// Load ShieldState from state.json in the given state directory.
    pub fn load(state_dir: &str) -> Result<Self> {
        let path = Path::new(state_dir).join("state.json");
        let json = std::fs::read_to_string(&path)
            .with_context(|| format!("failed to read {}", path.display()))?;
        serde_json::from_str(&json)
            .with_context(|| format!("failed to parse {}", path.display()))
    }

    /// Save ShieldState to state.json in the given state directory.
    pub fn save(&self, state_dir: &str) -> Result<()> {
        let dir = Path::new(state_dir);
        std::fs::create_dir_all(dir)
            .with_context(|| format!("failed to create state dir {}", dir.display()))?;
        let path = dir.join("state.json");
        let json = serde_json::to_string_pretty(self)
            .context("failed to serialize ShieldState")?;
        std::fs::write(&path, json)
            .with_context(|| format!("failed to write {}", path.display()))
    }
}
