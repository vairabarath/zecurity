use std::sync::RwLock;

use crate::client::v1::{AclEntry, AclSnapshot};

/// Local ACL snapshot cache. Updated from Controller heartbeat; enforces default-deny.
pub struct PolicyCache {
    snapshot: RwLock<Option<AclSnapshot>>,
}

/// Resource resolved by network tuple, returned by [`PolicyCache::resolve_resource`].
pub struct ResourceAcl {
    pub resource_id: String,
    pub allowed_spiffe_ids: Vec<String>,
}

impl PolicyCache {
    pub fn new() -> Self {
        Self {
            snapshot: RwLock::new(None),
        }
    }

    /// Replace the stored snapshot atomically.
    pub fn update(&self, snapshot: AclSnapshot) {
        *self.snapshot.write().unwrap() = Some(snapshot);
    }

    /// Returns true only when the snapshot exists, the resource entry exists,
    /// `allowed_spiffe_ids` is non-empty, and `client_spiffe_id` is in the list.
    pub fn is_allowed(&self, resource_id: &str, client_spiffe_id: &str) -> bool {
        let guard = self.snapshot.read().unwrap();
        let snapshot = match guard.as_ref() {
            None => return false,
            Some(s) => s,
        };
        match find_entry_by_id(snapshot, resource_id) {
            None => false,
            Some(entry) => {
                !entry.allowed_spiffe_ids.is_empty()
                    && entry.allowed_spiffe_ids.iter().any(|id| id == client_spiffe_id)
            }
        }
    }

    /// Look up a resource by its network tuple (address + port + protocol).
    /// Returns `None` when no snapshot is present or no entry matches — callers must deny.
    pub fn resolve_resource(&self, address: &str, port: u16, protocol: &str) -> Option<ResourceAcl> {
        let guard = self.snapshot.read().unwrap();
        let snapshot = guard.as_ref()?;
        let entry = snapshot.entries.iter().find(|e| {
            e.address == address && e.port == port as u32 && e.protocol == protocol
        })?;
        Some(ResourceAcl {
            resource_id: entry.resource_id.clone(),
            allowed_spiffe_ids: entry.allowed_spiffe_ids.clone(),
        })
    }
}

fn find_entry_by_id<'a>(snapshot: &'a AclSnapshot, resource_id: &str) -> Option<&'a AclEntry> {
    snapshot.entries.iter().find(|e| e.resource_id == resource_id)
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
        }
    }

    fn entry(resource_id: &str, allowed: Vec<&str>) -> AclEntry {
        AclEntry {
            resource_id: resource_id.into(),
            address: "10.0.0.1".into(),
            port: 443,
            protocol: "tcp".into(),
            allowed_spiffe_ids: allowed.into_iter().map(String::from).collect(),
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
        cache.update(snapshot_with(vec![entry("res-1", vec!["spiffe://ws/client/device-1"])]));
        assert!(!cache.is_allowed("res-unknown", "spiffe://ws/client/device-1"));
    }

    #[test]
    fn deny_missing_spiffe_id() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry("res-1", vec!["spiffe://ws/client/device-1"])]));
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
        cache.update(snapshot_with(vec![entry("res-1", vec!["spiffe://ws/client/device-1"])]));
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
        cache.update(snapshot_with(vec![entry("res-1", vec!["spiffe://ws/client/device-1"])]));
        let result = cache.resolve_resource("10.0.0.1", 443, "tcp");
        assert!(result.is_some());
        assert_eq!(result.unwrap().resource_id, "res-1");
    }

    #[test]
    fn resolve_resource_returns_none_on_port_mismatch() {
        let cache = PolicyCache::new();
        cache.update(snapshot_with(vec![entry("res-1", vec!["spiffe://ws/client/device-1"])]));
        assert!(cache.resolve_resource("10.0.0.1", 80, "tcp").is_none());
    }
}
