use dashmap::DashMap;
use quinn::Connection;
use std::sync::Arc;
use uuid::Uuid;

#[derive(Clone)]
pub struct ConnectorEntry {
    pub registration_id: Uuid,
    pub connection: Connection,
    pub spiffe_id: String,
    pub trust_domain: String,
}

pub struct RelayState {
    connectors: DashMap<String, ConnectorEntry>,
}

impl RelayState {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            connectors: DashMap::new(),
        })
    }
    pub fn insert_connector(
        &self,
        connector_id: String,
        connection: Connection,
        spiffe_id: String,
        trust_domain: String,
    ) -> Uuid {
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

    pub fn remove_connector(&self, connector_id: &str, registration_id: Uuid) {
        if let Some(entry) = self.connectors.get(connector_id) {
            if entry.registration_id == registration_id {
                drop(entry);
                self.connectors.remove(connector_id);
            }
        }
    }
}
