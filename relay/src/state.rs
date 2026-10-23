use dashmap::DashMap;
use quinn::Connection;
use std::collections::HashSet;
use std::sync::{Arc, Mutex};
use tracing::warn;
use uuid::Uuid;

/// Canonical connector SPIFFE id, `spiffe://<td>/connector/<uuid>` with a
/// lower-case uuid — the form the controller uses (connector-revocation
/// Phase D).
pub fn canonical_connector_spiffe(trust_domain: &str, connector_id: &str) -> String {
    format!(
        "spiffe://{}/connector/{}",
        trust_domain,
        connector_id.to_ascii_lowercase()
    )
}

#[derive(Clone)]
pub struct ConnectorEntry {
    pub registration_id: Uuid,
    pub connection: Connection,
    pub spiffe_id: String,
    pub trust_domain: String,
}

pub struct RelayState {
    connectors: DashMap<String, ConnectorEntry>,
    /// Phase D (D20): connector identities the controller reported revoked.
    /// Only ever added to for the life of the process: a stale or empty
    /// heartbeat answer never unblocks an identity.
    revoked: Mutex<HashSet<String>>,
    /// Test seam: runs at the start of `insert_connector`, before the map
    /// insert, to put a revocation inside the Register check→insert window.
    #[cfg(test)]
    pub before_insert: Mutex<Option<Box<dyn Fn() + Send + Sync>>>,
}

impl RelayState {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            connectors: DashMap::new(),
            revoked: Mutex::new(HashSet::new()),
            #[cfg(test)]
            before_insert: Mutex::new(None),
        })
    }

    /// Phase D: canonical ids of the connectors registered right now.
    pub fn registered_connector_spiffe_ids(&self) -> Vec<String> {
        let mut ids: Vec<String> = self
            .connectors
            .iter()
            .map(|e| canonical_connector_spiffe(&e.value().trust_domain, e.key()))
            .collect();
        ids.sort();
        ids
    }

    /// Phase D: whether a canonical connector id is revoked. A poisoned lock
    /// fails closed.
    pub fn is_revoked(&self, canonical_spiffe: &str) -> bool {
        match self.revoked.lock() {
            Ok(set) => set.contains(canonical_spiffe),
            Err(_) => true,
        }
    }

    /// Phase D: add `ids` to the sticky revoked set, THEN close every
    /// registered connector in it (its bridged client streams end with the
    /// connection). Returns how many connections were closed.
    pub fn apply_revoked(&self, ids: &[String]) -> usize {
        if ids.is_empty() {
            return 0;
        }
        match self.revoked.lock() {
            Ok(mut set) => set.extend(ids.iter().cloned()),
            Err(poisoned) => poisoned.into_inner().extend(ids.iter().cloned()),
        }
        let mut closed = 0;
        for entry in self.connectors.iter() {
            let canonical = canonical_connector_spiffe(&entry.value().trust_domain, entry.key());
            if self.is_revoked(&canonical) {
                warn!(connector_id = %entry.key(), "revoked connector: Relay connection closed");
                entry
                    .value()
                    .connection
                    .close(0u32.into(), b"connector revoked");
                closed += 1;
            }
        }
        closed
    }
    pub fn insert_connector(
        &self,
        connector_id: String,
        connection: Connection,
        spiffe_id: String,
        trust_domain: String,
    ) -> Uuid {
        #[cfg(test)]
        if let Some(hook) = self.before_insert.lock().unwrap().as_ref() {
            hook();
        }
        let registration_id = Uuid::new_v4();

        self.connectors.insert(
            connector_id,
            ConnectorEntry {
                registration_id,
                connection,
                spiffe_id,
                trust_domain,
            },
        );

        registration_id
    }

    pub fn lookup_connector(&self, connector_id: &str) -> Option<ConnectorEntry> {
        self.connectors.get(connector_id).map(|entry| entry.clone())
    }

    pub fn connector_count(&self) -> usize {
        self.connectors.len()
    }

    pub fn remove_connector(&self, connector_id: &str, registration_id: Uuid) {
        if let Some(entry) = self.connectors.get(connector_id) {
            if entry.registration_id == registration_id {
                drop(entry);
                self.connectors.remove(connector_id);
            }
        }
    }
}
