// types.rs — Shared data structures used across multiple shield modules

use std::path::Path;

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

/// One entry in the Shield's peer-Connector list. Each `ConnectorRef` is a
/// full identity pair — its own SPIFFE UUID and its own gRPC address —
/// because the Shield performs per-peer mTLS verification when dialing.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ConnectorRef {
    /// UUID of this peer Connector (canonical lowercase).
    pub connector_id: String,
    /// Shield-facing gRPC address for this peer (host:9091).
    pub connector_addr: String,
}

/// Persistent state saved to state.json after successful enrollment.
///
/// WHY WE SAVE THIS:
///   After enrollment, the shield has a certificate, a list of Connectors it
///   can attach to, and a network interface. On restart, we don't re-enroll
///   — we just load this state and resume the control stream. Re-enrollment
///   would burn the enrollment token and create a new shield record in the DB.
///
/// PEER LIST:
///   `connectors` starts as a single element (the Connector that enrolled us)
///   and is expanded automatically by piggyback `PeerConnectorList` pushes
///   from whichever Connector we're currently attached to. On Control-stream
///   failure the Shield walks the list in stored order and fails over to the
///   first peer that answers.
///
/// FILE LOCATION: <state_dir>/state.json (default: /var/lib/zecurity-shield/state.json)
#[derive(Debug, Clone, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ShieldState {
    /// UUID assigned by the controller during enrollment.
    pub shield_id: String,

    /// Workspace trust domain (e.g. "ws-acme.zecurity.in").
    pub trust_domain: String,

    /// Ordered list of peer Connectors on the Shield's Remote Network. The
    /// head is the currently-preferred target; on Control-stream failure the
    /// Shield tries the rest in order and rotates the failed head to the tail.
    /// Non-empty invariant enforced by `ShieldState::load`.
    pub connectors: Vec<ConnectorRef>,

    /// /32 IP address assigned to the zecurity0 interface (e.g. "100.64.0.5").
    pub interface_addr: String,

    /// RFC 3339 timestamp of when enrollment completed.
    pub enrolled_at: String,

    /// Unix timestamp of the certificate's NotAfter field.
    /// Updated after RenewCert so the next control stream uses fresh credentials.
    pub cert_not_after: i64,
}

/// On-disk representation with both the new `connectors` list and the legacy
/// singleton fields. Custom `Deserialize` implementation upgrades old files
/// (pre-peer-failover) to the new shape on load, and the next `save()`
/// rewrites the file with only the new shape.
///
/// Both new and old fields are `#[serde(default)]` so either shape parses;
/// the `From` impl below rejects the file only if neither yields a usable
/// Connector.
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ShieldStateOnDisk {
    shield_id: String,
    trust_domain: String,
    #[serde(default)]
    connectors: Vec<ConnectorRef>,
    #[serde(default)]
    connector_id: Option<String>,
    #[serde(default)]
    connector_addr: Option<String>,
    interface_addr: String,
    enrolled_at: String,
    cert_not_after: i64,
}

impl From<ShieldStateOnDisk> for ShieldState {
    fn from(v: ShieldStateOnDisk) -> Self {
        // Prefer the new list shape when present; fall back to the legacy
        // singleton pair. `ShieldState::load` rejects the resulting empty
        // list downstream so we don't need to panic here.
        let connectors = if !v.connectors.is_empty() {
            v.connectors
        } else if let (Some(id), Some(addr)) = (v.connector_id, v.connector_addr) {
            vec![ConnectorRef {
                connector_id: id,
                connector_addr: addr,
            }]
        } else {
            Vec::new()
        };
        ShieldState {
            shield_id: v.shield_id,
            trust_domain: v.trust_domain,
            connectors,
            interface_addr: v.interface_addr,
            enrolled_at: v.enrolled_at,
            cert_not_after: v.cert_not_after,
        }
    }
}

impl<'de> Deserialize<'de> for ShieldState {
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        ShieldStateOnDisk::deserialize(deserializer).map(ShieldState::from)
    }
}

impl ShieldState {
    /// Load ShieldState from state.json in the given state directory. Also
    /// upgrades legacy single-connector files into the new list shape.
    /// Returns an error if the file is missing, unparseable, or holds an
    /// empty connector list.
    pub fn load(state_dir: &str) -> Result<Self> {
        let path = Path::new(state_dir).join("state.json");
        let json = std::fs::read_to_string(&path)
            .with_context(|| format!("failed to read {}", path.display()))?;
        let state: ShieldState = serde_json::from_str(&json)
            .with_context(|| format!("failed to parse {}", path.display()))?;
        if state.connectors.is_empty() {
            bail!(
                "state file {} has no connectors — re-enrollment required",
                path.display()
            );
        }
        Ok(state)
    }

    /// Save ShieldState to state.json in the given state directory. Always
    /// writes the new shape (`connectors: [...]`); legacy singleton fields
    /// never appear in freshly-written files.
    pub fn save(&self, state_dir: &str) -> Result<()> {
        let dir = Path::new(state_dir);
        std::fs::create_dir_all(dir)
            .with_context(|| format!("failed to create state dir {}", dir.display()))?;
        let path = dir.join("state.json");
        let json = serde_json::to_string_pretty(self).context("failed to serialize ShieldState")?;
        std::fs::write(&path, json).with_context(|| format!("failed to write {}", path.display()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch_dir(label: &str) -> std::path::PathBuf {
        let mut p = std::env::temp_dir();
        p.push(format!(
            "zecurity-shield-types-{label}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .map(|d| d.as_nanos())
                .unwrap_or(0),
        ));
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    fn cleanup(dir: &std::path::Path) {
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn legacy_single_connector_file_upgrades_on_load() {
        let dir = scratch_dir("legacy-upgrade");
        let legacy_json = serde_json::json!({
            "shield_id": "aaaa-bbbb",
            "trust_domain": "ws-acme.zecurity.in",
            "connector_id": "1111-2222",
            "connector_addr": "10.0.0.5:9091",
            "interface_addr": "100.64.0.5",
            "enrolled_at": "2026-07-01T00:00:00Z",
            "cert_not_after": 1793491200
        });
        std::fs::write(dir.join("state.json"), legacy_json.to_string()).unwrap();

        let state = ShieldState::load(dir.to_str().unwrap()).unwrap();
        assert_eq!(state.connectors.len(), 1);
        assert_eq!(state.connectors[0].connector_id, "1111-2222");
        assert_eq!(state.connectors[0].connector_addr, "10.0.0.5:9091");

        // Save-then-reload rewrites in the new shape and drops the top-level
        // legacy scalar fields. Note: `"connector_id"` and `"connector_addr"`
        // *do* still appear as nested fields inside each ConnectorRef entry —
        // we're only asserting they no longer sit at the top level.
        state.save(dir.to_str().unwrap()).unwrap();
        let raw: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(dir.join("state.json")).unwrap())
                .unwrap();
        assert!(raw.get("connectors").is_some());
        assert!(raw.get("connector_id").is_none());
        assert!(raw.get("connector_addr").is_none());
        // And the nested shape is intact.
        let connectors = raw.get("connectors").unwrap().as_array().unwrap();
        assert_eq!(connectors.len(), 1);
        assert_eq!(connectors[0]["connector_id"], "1111-2222");
        assert_eq!(connectors[0]["connector_addr"], "10.0.0.5:9091");

        cleanup(&dir);
    }

    #[test]
    fn modern_list_file_roundtrips() {
        let dir = scratch_dir("modern-roundtrip");
        let original = ShieldState {
            shield_id: "sh-1".into(),
            trust_domain: "ws.zecurity.in".into(),
            connectors: vec![
                ConnectorRef {
                    connector_id: "conn-a".into(),
                    connector_addr: "10.0.0.5:9091".into(),
                },
                ConnectorRef {
                    connector_id: "conn-b".into(),
                    connector_addr: "10.0.0.6:9091".into(),
                },
            ],
            interface_addr: "100.64.0.5".into(),
            enrolled_at: "2026-07-01T00:00:00Z".into(),
            cert_not_after: 1793491200,
        };
        original.save(dir.to_str().unwrap()).unwrap();
        let loaded = ShieldState::load(dir.to_str().unwrap()).unwrap();
        assert_eq!(loaded.connectors, original.connectors);
        assert_eq!(loaded.shield_id, original.shield_id);
        cleanup(&dir);
    }

    #[test]
    fn empty_connectors_list_is_rejected() {
        let dir = scratch_dir("empty-connectors");
        let empty_json = serde_json::json!({
            "shield_id": "sh-1",
            "trust_domain": "ws.zecurity.in",
            "connectors": [],
            "interface_addr": "100.64.0.5",
            "enrolled_at": "2026-07-01T00:00:00Z",
            "cert_not_after": 1793491200
        });
        std::fs::write(dir.join("state.json"), empty_json.to_string()).unwrap();
        let err = ShieldState::load(dir.to_str().unwrap()).unwrap_err();
        assert!(
            err.to_string().contains("no connectors"),
            "expected \"no connectors\" in error, got: {err}"
        );
        cleanup(&dir);
    }

    #[test]
    fn no_connector_fields_at_all_is_rejected() {
        let dir = scratch_dir("missing-connectors");
        // Neither `connectors` nor legacy scalars — unusable file.
        let bad_json = serde_json::json!({
            "shield_id": "sh-1",
            "trust_domain": "ws.zecurity.in",
            "interface_addr": "100.64.0.5",
            "enrolled_at": "2026-07-01T00:00:00Z",
            "cert_not_after": 1793491200
        });
        std::fs::write(dir.join("state.json"), bad_json.to_string()).unwrap();
        let err = ShieldState::load(dir.to_str().unwrap()).unwrap_err();
        assert!(
            err.to_string().contains("no connectors"),
            "expected \"no connectors\" in error, got: {err}"
        );
        cleanup(&dir);
    }

    #[test]
    fn modern_list_wins_over_legacy_fields_when_both_present() {
        // A file that carries both (from a bad migration) — new list wins.
        let dir = scratch_dir("both-shapes");
        let mixed_json = serde_json::json!({
            "shield_id": "sh-1",
            "trust_domain": "ws.zecurity.in",
            "connectors": [{"connector_id": "new-1", "connector_addr": "10.0.0.9:9091"}],
            "connector_id": "old-1",
            "connector_addr": "10.0.0.99:9091",
            "interface_addr": "100.64.0.5",
            "enrolled_at": "2026-07-01T00:00:00Z",
            "cert_not_after": 1793491200
        });
        std::fs::write(dir.join("state.json"), mixed_json.to_string()).unwrap();
        let state = ShieldState::load(dir.to_str().unwrap()).unwrap();
        assert_eq!(state.connectors.len(), 1);
        assert_eq!(state.connectors[0].connector_id, "new-1");
        cleanup(&dir);
    }
}
