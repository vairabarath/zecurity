use std::sync::Arc;
use tokio::sync::RwLock;

use crate::grpc::client_v1::{AclSnapshot, TransportSnapshot};

/// Live TUN session handle — present while `zecurity up` is active.
pub struct TunHandle {
    /// Abort the net_stack::run task on Down.
    pub abort: tokio::task::AbortHandle,
    /// Resource IPs added as /32 routes (for cleanup logging).
    pub route_count: usize,
}

impl std::fmt::Debug for TunHandle {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TunHandle")
            .field("route_count", &self.route_count)
            .finish()
    }
}

/// Coordination state for `daemon::restart_tunnel_if_running`'s queued-oneshot
/// batch coordinator. `running` is true while a worker task is draining
/// `pending` passes; a caller pushes a oneshot sender onto `pending` and
/// starts a worker only if none is already running.
#[derive(Default)]
pub struct TunnelRestartCoordinator {
    pub running: bool,
    pub pending: Vec<tokio::sync::oneshot::Sender<std::result::Result<(), String>>>,
}

impl std::fmt::Debug for TunnelRestartCoordinator {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TunnelRestartCoordinator")
            .field("running", &self.running)
            .field("pending_count", &self.pending.len())
            .finish()
    }
}

/// All runtime state. Lives only in process memory.
#[derive(Debug, Default, Clone)]
pub struct RuntimeState {
    // pub schema_version: u32,
    pub workspace: Option<WorkspaceInfo>,
    pub user: Option<UserInfo>,
    pub device: Option<DeviceInfo>,
    pub session: Option<SessionInfo>,
    /// ACL snapshot fetched from the Controller. None = default-deny.
    pub acl_snapshot: Option<AclSnapshot>,
    /// Unix timestamp of the last successful ACL snapshot fetch.
    pub acl_last_sync_at: Option<i64>,
    /// Transport (connectivity) snapshot — per-connector relay coords keyed by
    /// remote_network_id (ADR-015 Track B). Independent of the ACL. None = fall
    /// back to the ACL's transitional relay fields (ACLConnector 4+5).
    pub transport_snapshot: Option<TransportSnapshot>,
    /// Unix timestamp of the last successful transport snapshot fetch.
    pub transport_last_sync_at: Option<i64>,
    /// Verified platform Relay CRL cache shared across tunnel restarts.
    pub relay_crl: Option<crate::crl::CrlManager>,
    /// Live TUN session. Present while `zecurity up` is active.
    pub tun_handle: Option<Arc<TunHandle>>,
    /// Ensures only one task refreshes the session tokens at a time.
    pub refresh_lock: Arc<tokio::sync::Mutex<()>>,
    /// Serializes the transport fetch/read/store operation so a concurrent
    /// scheduler tick and relay-recovery task cannot overwrite a newer transport
    /// snapshot with an older response. Held across the whole known-version →
    /// fetch → store sequence, not just the store.
    pub transport_sync_lock: Arc<tokio::sync::Mutex<()>>,
    /// Coordinates tunnel down→up restarts so concurrent triggers (relay
    /// recovery, the 60s ACL tick, IPC sync/resources, post-login) share
    /// restart passes instead of each running its own full down/up cycle.
    /// See `daemon::restart_tunnel_if_running`.
    pub tunnel_restart: Arc<tokio::sync::Mutex<TunnelRestartCoordinator>>,
    /// Signalled by the data plane (net_stack) when a managed-resource relay
    /// transport fails, so the ACL sync scheduler re-syncs early instead of
    /// waiting for the next poll tick. Coalescing: a burst of failures collapses
    /// into a single wake.
    pub relay_resync: Arc<tokio::sync::Notify>,
     /// Signalled by PostLoginState right after a successful login, so the
    /// posture scheduler collects and submits immediately instead of waiting
    /// for the next 5-minute tick.
    pub posture_resync: Arc<tokio::sync::Notify>,
}

#[derive(Debug, Clone)]
pub struct WorkspaceInfo {
    pub id: String,
    pub name: String,
    pub slug: String,
    pub trust_domain: String,
}
#[allow(dead_code)]
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

/// Shared handle used across async tasks.
pub type SharedState = Arc<RwLock<RuntimeState>>;

pub fn new_shared() -> SharedState {
    Arc::new(RwLock::new(RuntimeState {
        // schema_version: crate::appmeta::SCHEMA_VERSION,
        workspace: None,
        user: None,
        device: None,
        session: None,
        acl_snapshot: None,
        acl_last_sync_at: None,
        transport_snapshot: None,
        transport_last_sync_at: None,
        relay_crl: None,
        tun_handle: None,
        refresh_lock: Arc::new(tokio::sync::Mutex::new(())),
        transport_sync_lock: Arc::new(tokio::sync::Mutex::new(())),
        tunnel_restart: Arc::new(tokio::sync::Mutex::new(TunnelRestartCoordinator::default())),
        relay_resync: Arc::new(tokio::sync::Notify::new()),
         posture_resync: Arc::new(tokio::sync::Notify::new()),
    }))
}
