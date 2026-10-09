use std::collections::{BTreeSet, HashMap, HashSet};

use parking_lot::RwLock;

use crate::client::v1::{AclEntry, AclSnapshot};

/// Local ACL snapshot cache. Updated from Controller heartbeat; enforces default-deny.
#[derive(Debug)]
pub struct PolicyCache {
    snapshot: RwLock<Option<AclSnapshot>>,
}

/// Resource resolved by network tuple, returned by [`PolicyCache::resolve_resource`].
pub struct ResourceAcl {
    pub resource_id: String,
    pub allowed_spiffe_ids: Vec<String>,
    pub route_type: String,
    pub shield_id: String,
}

impl PolicyCache {
    pub fn new() -> Self {
        Self {
            snapshot: RwLock::new(None),
        }
    }

    /// Replace the stored snapshot atomically.
    pub fn update(&self, snapshot: AclSnapshot) {
        *self.snapshot.write() = Some(snapshot);
    }

    /// Atomically replaces the stored snapshot and returns the (spiffe_id,
    /// resource_id) pairs that were allowed under the previous snapshot but
    /// are not allowed under the new one — exactly the live sessions that
    /// must be torn down. Empty on the first-ever snapshot (nothing to diff
    /// against — `replace` returns None). Used by the Sprint 15 ACL-diff
    /// teardown in control_stream.rs; `update` above is unchanged and still
    /// used by existing tests/call sites that don't need the diff.
    ///
    /// F-3: a session is also bound to the `(address, port, protocol)` tuple
    /// it was admitted on, and that tuple is fixed for the session's life. So
    /// when a resource keeps its `resource_id` but its tuple changes, every
    /// pair the previous snapshot allowed for that resource is revoked too —
    /// otherwise an established session keeps reaching a tuple no entry
    /// authorises any more. Name-only (and other non-tuple) changes revoke
    /// nothing.
    pub fn update_and_revoked(&self, snapshot: AclSnapshot) -> HashSet<(String, String)> {
        let new_set = allow_set(&snapshot);
        let new_tuples = tuples_by_resource(&snapshot);
        match self.snapshot.write().replace(snapshot) {
            Some(previous) => {
                let mut revoked: HashSet<(String, String)> =
                    allow_set(&previous).difference(&new_set).cloned().collect();
                let old_tuples = tuples_by_resource(&previous);
                for entry in &previous.entries {
                    let rid = entry.resource_id.as_str();
                    let (Some(old), Some(new)) = (old_tuples.get(rid), new_tuples.get(rid)) else {
                        continue; // absent from either side: the pair diff covers it
                    };
                    if old != new {
                        revoked.extend(
                            entry
                                .allowed_spiffe_ids
                                .iter()
                                .map(|spiffe| (spiffe.clone(), entry.resource_id.clone())),
                        );
                    }
                }
                revoked
            }
            None => HashSet::new(),
        }
    }

    /// Current local ACL snapshot version. Returns 0 when no snapshot is loaded.
    pub fn version(&self) -> u64 {
        self.snapshot
            .read()
            .as_ref()
            .map(|s| s.version)
            .unwrap_or(0)
    }

    /// Returns true only when the snapshot exists, the resource entry exists,
    /// `allowed_spiffe_ids` is non-empty, and `client_spiffe_id` is in the list.
    pub fn is_allowed(&self, resource_id: &str, client_spiffe_id: &str) -> bool {
        let guard = self.snapshot.read();
        let snapshot = match guard.as_ref() {
            None => return false,
            Some(s) => s,
        };
        match find_entry_by_id(snapshot, resource_id) {
            None => false,
            Some(entry) => {
                !entry.allowed_spiffe_ids.is_empty()
                    && entry
                        .allowed_spiffe_ids
                        .iter()
                        .any(|id| id == client_spiffe_id)
            }
        }
    }

    /// Return the peer Connectors that share a Remote Network with
    /// `connector_id` (including itself). Each entry is
    /// `(connector_id, connector_tunnel_addr)` — the tunnel address is
    /// `host:9092`; callers that need the Shield-facing :9091 port must
    /// derive it themselves.
    ///
    /// Returns an empty Vec when the snapshot is missing or the queried
    /// connector isn't listed in any Remote Network.
    pub fn peers_of_connector(&self, connector_id: &str) -> Vec<(String, String)> {
        let guard = self.snapshot.read();
        let Some(snapshot) = guard.as_ref() else {
            return Vec::new();
        };
        for rn in &snapshot.remote_networks {
            if rn.connectors.iter().any(|c| c.connector_id == connector_id) {
                return rn
                    .connectors
                    .iter()
                    .map(|c| (c.connector_id.clone(), c.connector_tunnel_addr.clone()))
                    .collect();
            }
        }
        Vec::new()
    }

    /// Look up a resource by its network tuple (address + port + protocol).
    /// Returns `None` when no snapshot is present or no entry matches — callers must deny.
    pub fn resolve_resource(
        &self,
        address: &str,
        port: u16,
        protocol: &str,
    ) -> Option<ResourceAcl> {
        let guard = self.snapshot.read();
        let snapshot = guard.as_ref()?;
        let entry = snapshot.entries.iter().find(|e| {
            e.address == address
                && e.port == port as u32
                && e.protocol.eq_ignore_ascii_case(protocol)
        })?;
        Some(ResourceAcl {
            resource_id: entry.resource_id.clone(),
            allowed_spiffe_ids: entry.allowed_spiffe_ids.clone(),
            route_type: entry.route_type.clone(),
            shield_id: entry.shield_id.clone(),
        })
    }
}

fn find_entry_by_id<'a>(snapshot: &'a AclSnapshot, resource_id: &str) -> Option<&'a AclEntry> {
    snapshot
        .entries
        .iter()
        .find(|e| e.resource_id == resource_id)
}

/// Flattens a snapshot's entries into the set of (spiffe_id, resource_id)
/// pairs it currently allows. Note: this assumes `resource_id` doesn't repeat
/// across entries in one snapshot (consistent with `is_allowed`'s
/// first-match-by-id lookup) — if it ever did, the only failure mode is
/// under-cancelling an already-open session, since new connections are still
/// caught by device_tunnel.rs's post-registration recheck either way.
pub fn allow_set(snapshot: &AclSnapshot) -> HashSet<(String, String)> {
    snapshot
        .entries
        .iter()
        .flat_map(|entry| {
            entry
                .allowed_spiffe_ids
                .iter()
                .map(move |spiffe| (spiffe.clone(), entry.resource_id.clone()))
        })
        .collect()
}

/// The `(address, port, protocol)` tuples each resource is admitted on.
/// Protocol is lowercased because `resolve_resource` matches it
/// case-insensitively; address is compared exactly, as `resolve_resource` does.
/// A set (not a single tuple) so a repeated `resource_id` compares
/// order-independently and any difference counts as a change.
fn tuples_by_resource(snapshot: &AclSnapshot) -> HashMap<String, BTreeSet<(String, u32, String)>> {
    let mut out: HashMap<String, BTreeSet<(String, u32, String)>> = HashMap::new();
    for entry in &snapshot.entries {
        out.entry(entry.resource_id.clone()).or_default().insert((
            entry.address.clone(),
            entry.port,
            entry.protocol.to_ascii_lowercase(),
        ));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn snapshot_with(entries: Vec<AclEntry>) -> AclSnapshot {
        AclSnapshot {
            version: 1,
            workspace_id: "ws-test".into(),
            generated_at: 0,
            entries,
            ..Default::default()
        }
    }

    fn entry(resource_id: &str, allowed: Vec<&str>) -> AclEntry {
        AclEntry {
            preferred_connector_id: "".to_string(),
            remote_network_id: "".to_string(),
            resource_id: resource_id.into(),
            name: resource_id.into(),
            address: "10.0.0.1".into(),
            port: 443,
            protocol: "tcp".into(),
            allowed_spiffe_ids: allowed.into_iter().map(String::from).collect(),
            route_type: "shield".into(),
            shield_id: "shield-test".into(),
        }
    }

    #[test]
    fn deny_when_no_snapshot() {
        let cache = PolicyCache::new();
        assert!(!cache.is_allowed("res-1", "spiffe://ws/client/device-1"));
    }

    #[test]
    fn deny_unknown_resource() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry(
            "res-1",
            vec!["spiffe://ws/client/device-1"],
        )]));
        assert!(!cache.is_allowed("res-unknown", "spiffe://ws/client/device-1"));
    }

    #[test]
    fn deny_missing_spiffe_id() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry(
            "res-1",
            vec!["spiffe://ws/client/device-1"],
        )]));
        assert!(!cache.is_allowed("res-1", "spiffe://ws/client/device-OTHER"));
    }

    #[test]
    fn deny_empty_allowed_spiffe_ids() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry("res-1", vec![])]));
        assert!(!cache.is_allowed("res-1", "spiffe://ws/client/device-1"));
    }

    #[test]
    fn allow_known_resource_with_matching_spiffe() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry(
            "res-1",
            vec!["spiffe://ws/client/device-1"],
        )]));
        assert!(cache.is_allowed("res-1", "spiffe://ws/client/device-1"));
    }

    #[test]
    fn resolve_resource_returns_none_without_snapshot() {
        let cache = PolicyCache::new();
        assert!(cache.resolve_resource("10.0.0.1", 443, "tcp").is_none());
    }

    #[test]
    fn resolve_resource_matches_network_tuple() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry(
            "res-1",
            vec!["spiffe://ws/client/device-1"],
        )]));
        let result = cache.resolve_resource("10.0.0.1", 443, "tcp");
        assert!(result.is_some());
        assert_eq!(result.unwrap().resource_id, "res-1");
    }

    fn snapshot_with_remote_networks(
        entries: Vec<AclEntry>,
        remote_networks: Vec<crate::client::v1::AclRemoteNetwork>,
    ) -> AclSnapshot {
        AclSnapshot {
            version: 1,
            workspace_id: "ws-test".into(),
            generated_at: 0,
            entries,
            remote_networks,
            ..Default::default()
        }
    }

    fn rn(rn_id: &str, connectors: Vec<(&str, &str)>) -> crate::client::v1::AclRemoteNetwork {
        crate::client::v1::AclRemoteNetwork {
            remote_network_id: rn_id.into(),
            name: rn_id.into(),
            connectors: connectors
                .into_iter()
                .map(|(id, addr)| crate::client::v1::AclConnector {
                    connector_id: id.into(),
                    connector_tunnel_addr: addr.into(),
                    connector_spiffe: format!("spiffe://ws/connector/{id}"),
                    relay_addr: String::new(),
                    relay_spiffe_id: String::new(),
                })
                .collect(),
        }
    }

    #[test]
    fn peers_of_connector_returns_all_peers_in_same_rn() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with_remote_networks(
            vec![],
            vec![rn(
                "rn-a",
                vec![("conn-1", "10.0.0.5:9092"), ("conn-2", "10.0.0.6:9092")],
            )],
        ));
        let peers = cache.peers_of_connector("conn-1");
        assert_eq!(peers.len(), 2);
        assert!(peers
            .iter()
            .any(|(id, addr)| id == "conn-1" && addr == "10.0.0.5:9092"));
        assert!(peers
            .iter()
            .any(|(id, addr)| id == "conn-2" && addr == "10.0.0.6:9092"));
    }

    #[test]
    fn peers_of_connector_empty_when_snapshot_missing() {
        let cache = PolicyCache::new();
        assert!(cache.peers_of_connector("conn-1").is_empty());
    }

    #[test]
    fn peers_of_connector_empty_when_self_absent() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with_remote_networks(
            vec![],
            vec![rn("rn-a", vec![("conn-99", "10.0.0.99:9092")])],
        ));
        assert!(cache.peers_of_connector("conn-1").is_empty());
    }

    #[test]
    fn peers_of_connector_filters_to_own_rn_only() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with_remote_networks(
            vec![],
            vec![
                rn("rn-a", vec![("conn-1", "10.0.0.5:9092")]),
                rn("rn-b", vec![("conn-2", "10.0.0.6:9092")]),
            ],
        ));
        let peers = cache.peers_of_connector("conn-1");
        assert_eq!(peers.len(), 1);
        assert_eq!(peers[0].0, "conn-1");
    }

    #[test]
    fn resolve_resource_returns_none_on_port_mismatch() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry(
            "res-1",
            vec!["spiffe://ws/client/device-1"],
        )]));
        assert!(cache.resolve_resource("10.0.0.1", 80, "tcp").is_none());
    }

    // ── F-3: update_and_revoked tuple-change invalidation ──────────────────

    const DEV_1: &str = "spiffe://ws/client/device-1";
    const DEV_2: &str = "spiffe://ws/client/device-2";

    fn tuple_entry(
        resource_id: &str,
        address: &str,
        port: u32,
        protocol: &str,
        allowed: Vec<&str>,
    ) -> AclEntry {
        AclEntry {
            address: address.into(),
            port,
            protocol: protocol.into(),
            ..entry(resource_id, allowed)
        }
    }

    fn pairs(items: &[(&str, &str)]) -> HashSet<(String, String)> {
        items
            .iter()
            .map(|(s, r)| (s.to_string(), r.to_string()))
            .collect()
    }

    /// Installs `before`, then returns what `update_and_revoked(after)` revokes.
    fn revoked_between(before: Vec<AclEntry>, after: Vec<AclEntry>) -> HashSet<(String, String)> {
        let cache = PolicyCache::new();
        assert!(cache.update_and_revoked(snapshot_with(before)).is_empty());
        cache.update_and_revoked(snapshot_with(after))
    }

    #[test]
    fn revokes_on_port_change_same_resource_id() {
        let revoked = revoked_between(
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
            vec![tuple_entry("res-1", "10.0.0.1", 9090, "tcp", vec![DEV_1])],
        );
        assert_eq!(revoked, pairs(&[(DEV_1, "res-1")]));
    }

    #[test]
    fn revokes_on_protocol_change_same_resource_id() {
        let revoked = revoked_between(
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "udp", vec![DEV_1])],
        );
        assert_eq!(revoked, pairs(&[(DEV_1, "res-1")]));
    }

    #[test]
    fn revokes_on_address_change_same_resource_id() {
        let revoked = revoked_between(
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
            vec![tuple_entry("res-1", "10.0.0.2", 8080, "tcp", vec![DEV_1])],
        );
        assert_eq!(revoked, pairs(&[(DEV_1, "res-1")]));
    }

    #[test]
    fn name_only_change_revokes_nothing() {
        let before = tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1]);
        let after = AclEntry {
            name: "renamed".into(),
            ..before.clone()
        };
        assert!(revoked_between(vec![before], vec![after]).is_empty());
    }

    #[test]
    fn protocol_case_change_revokes_nothing() {
        // resolve_resource compares protocol case-insensitively, so a
        // TCP → tcp edit admits exactly the same sessions.
        let revoked = revoked_between(
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "TCP", vec![DEV_1])],
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
        );
        assert!(revoked.is_empty());
    }

    #[test]
    fn tuple_change_does_not_touch_unrelated_resource() {
        let revoked = revoked_between(
            vec![
                tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1]),
                tuple_entry("res-2", "10.0.0.1", 7070, "tcp", vec![DEV_1]),
            ],
            vec![
                tuple_entry("res-1", "10.0.0.1", 9090, "tcp", vec![DEV_1]),
                tuple_entry("res-2", "10.0.0.1", 7070, "tcp", vec![DEV_1]),
            ],
        );
        assert_eq!(revoked, pairs(&[(DEV_1, "res-1")]));
    }

    #[test]
    fn tuple_change_revokes_all_previously_allowed_spiffes() {
        let revoked = revoked_between(
            vec![
                tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1, DEV_2]),
                tuple_entry("res-2", "10.0.0.3", 443, "tcp", vec![DEV_1, DEV_2]),
            ],
            vec![
                tuple_entry("res-1", "10.0.0.1", 9090, "tcp", vec![DEV_1, DEV_2]),
                tuple_entry("res-2", "10.0.0.3", 443, "tcp", vec![DEV_1, DEV_2]),
            ],
        );
        assert_eq!(revoked, pairs(&[(DEV_1, "res-1"), (DEV_2, "res-1")]));
    }

    #[test]
    fn access_removal_still_revokes_pair() {
        let revoked = revoked_between(
            vec![tuple_entry(
                "res-1",
                "10.0.0.1",
                8080,
                "tcp",
                vec![DEV_1, DEV_2],
            )],
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
        );
        assert_eq!(revoked, pairs(&[(DEV_2, "res-1")]));

        let removed_entirely = revoked_between(
            vec![tuple_entry("res-1", "10.0.0.1", 8080, "tcp", vec![DEV_1])],
            vec![],
        );
        assert_eq!(removed_entirely, pairs(&[(DEV_1, "res-1")]));
    }

    #[test]
    fn first_snapshot_revokes_nothing() {
        let cache = PolicyCache::new();
        let revoked = cache.update_and_revoked(snapshot_with(vec![tuple_entry(
            "res-1",
            "10.0.0.1",
            8080,
            "tcp",
            vec![DEV_1],
        )]));
        assert!(revoked.is_empty());
    }
}
