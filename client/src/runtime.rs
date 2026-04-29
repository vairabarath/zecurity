use std::sync::Arc;
use tokio::sync::RwLock;

/// All runtime state. Lives only in process memory.
#[derive(Debug, Default, Clone)]
pub struct RuntimeState {
    pub schema_version: u32,
    pub workspace: Option<WorkspaceInfo>,
    pub user: Option<UserInfo>,
    pub device: Option<DeviceInfo>,
    pub session: Option<SessionInfo>,
    pub resources: Vec<Resource>,
    pub last_sync_at: Option<i64>, // Unix timestamp
}

#[derive(Debug, Clone)]
pub struct WorkspaceInfo {
    pub id: String,
    pub name: String,
    pub slug: String,
    pub trust_domain: String,
}

#[derive(Debug, Clone)]
pub struct UserInfo {
    pub id: String,
    pub email: String,
    pub role: String,
}

#[derive(Debug, Clone)]
pub struct DeviceInfo {
    pub id: String,
    pub spiffe_id: String,
    pub certificate_pem: String,
    pub private_key_pem: String, // plaintext in memory — never written to disk
    pub ca_cert_pem: String,     // workspace CA + intermediate (concatenated)
    pub cert_expires_at: i64,    // Unix timestamp
    pub hostname: String,
    pub os: String,
}

#[derive(Debug, Clone)]
pub struct SessionInfo {
    pub access_token: String,
    pub refresh_token: String,
    pub expires_at: i64, // Unix timestamp
}

#[derive(Debug, Clone, Default)]
pub struct Resource {
    pub id: String,
    pub name: String,
    pub host: String,
    pub port: u16,
    pub protocol: String,
}

/// Shared handle used across async tasks.
pub type SharedState = Arc<RwLock<RuntimeState>>;

pub fn new_shared() -> SharedState {
    Arc::new(RwLock::new(RuntimeState {
        schema_version: crate::appmeta::SCHEMA_VERSION,
        ..Default::default()
    }))
}
