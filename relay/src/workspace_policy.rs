//! Relay pools PR 3 (Decision Record D-40): relay-side residency enforcement.
//!
//! Every heartbeat response carries the workspace policy of the relay's pool:
//! whether the pool enforces residency, the workspaces allowed on it, a
//! version (only ever increasing, across every pool) and a lease. This module
//! holds the applied policy, persists it in `RELAY_STATE_DIR`, decides at peer
//! accept whether a workspace may use this relay, and ends the sessions of
//! workspaces that are no longer allowed.
//!
//! Rules (D-40):
//! - version 0 in a response = no policy in it: keep the current one, the
//!   lease is not renewed. An older version is ignored. The same version
//!   renews the lease. A newer version replaces the policy.
//! - Not enforced (non-residency pool): every workspace is accepted and the
//!   lease has no effect.
//! - Enforced: only allowed workspaces are accepted. A newly applied policy
//!   that drops a workspace ends its connector and client connections at
//!   once. When the lease runs out without a renewed policy, every workspace
//!   peer is refused and every existing one is ended until a fresh policy
//!   arrives.
//! - No policy at all (first start, or no usable persisted file): the relay
//!   cannot know whether its pool enforces residency, so it fails closed and
//!   refuses every workspace peer until the first successful heartbeat
//!   applies one (seconds after start).
//!
//! Every admitted peer connection (connector registrations, client lookups,
//! probes) is registered here by workspace for its whole life, so revocation
//! and lease expiry can close it. Closing a connection ends every stream
//! bridged over it (`session::pipe_streams` watches both connections).

use std::collections::{BTreeSet, HashMap};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use quinn::Connection;
use serde::{Deserialize, Serialize};
use tokio::task::JoinHandle;
use tracing::{info, warn};

use crate::relay::v1::HeartbeatResponse;

/// File in `RELAY_STATE_DIR` holding the applied policy.
pub const POLICY_FILE: &str = "workspace-policy.json";

/// QUIC close reasons for peers refused or ended by the policy.
pub const NOT_ALLOWED_REASON: &[u8] = b"workspace not allowed on this relay";
pub const LEASE_EXPIRED_REASON: &[u8] = b"workspace policy lease expired";
pub const NO_POLICY_REASON: &[u8] = b"no workspace policy yet";

/// How often lease expiry is checked independently of heartbeats, so an
/// expired lease ends sessions even while the controller is unreachable.
pub const LEASE_CHECK_PERIOD: Duration = Duration::from_secs(1);

/// Seconds since the Unix epoch (the lease is persisted as an absolute time,
/// so a restart never extends it).
pub fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// An applied policy.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Policy {
    pub version: u64,
    pub enforced: bool,
    /// Lower-case workspace ids.
    pub allowed: BTreeSet<String>,
    /// Absolute Unix time the lease ends (only meaningful when enforced).
    pub lease_expires_unix: i64,
}

impl Policy {
    fn lease_expired(&self, now: i64) -> bool {
        now >= self.lease_expires_unix
    }

    fn decide(&self, workspace_id: &str, now: i64) -> Result<(), &'static [u8]> {
        if !self.enforced {
            return Ok(());
        }
        if self.lease_expired(now) {
            return Err(LEASE_EXPIRED_REASON);
        }
        if self.allowed.contains(&workspace_id.to_ascii_lowercase()) {
            Ok(())
        } else {
            Err(NOT_ALLOWED_REASON)
        }
    }
}

/// Ok(()) = admit; Err(reason) = refuse/end with this QUIC close reason.
fn decide(policy: Option<&Policy>, workspace_id: &str, now: i64) -> Result<(), &'static [u8]> {
    match policy {
        None => Err(NO_POLICY_REASON),
        Some(policy) => policy.decide(workspace_id, now),
    }
}

/// What applying a heartbeat response did.
#[derive(Debug, PartialEq, Eq)]
pub enum Applied {
    /// The response carried no policy (version 0): nothing changed.
    Absent,
    /// Older than the applied version: ignored.
    Stale,
    /// Same version: lease renewed.
    Renewed,
    /// Newer version: policy replaced.
    Replaced,
}

/// On-disk form. `binding` ties the file to this relay's identity and
/// platform PKI, so a file left over from another relay id or a reset
/// controller (whose version counter restarted) is not applied.
#[derive(Serialize, Deserialize)]
struct Persisted {
    binding: String,
    policy: Policy,
}

struct Peer {
    workspace_id: String,
    connection: Connection,
}

pub struct WorkspacePolicy {
    current: Mutex<Option<Policy>>,
    path: Option<PathBuf>,
    binding: String,
    /// Admitted peer connections by registration id. Lock order: `current`,
    /// then `peers`.
    peers: Mutex<HashMap<u64, Peer>>,
    next_peer: AtomicU64,
    /// Set while an enforced lease is expired (log on transitions only).
    lease_expired: AtomicBool,
}

/// Removes an admitted peer from the registry when its session ends.
pub struct PeerGuard {
    policy: Arc<WorkspacePolicy>,
    id: u64,
}

impl Drop for PeerGuard {
    fn drop(&mut self) {
        lock(&self.policy.peers).remove(&self.id);
    }
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

impl WorkspacePolicy {
    fn with(current: Option<Policy>, path: Option<PathBuf>, binding: String) -> Arc<Self> {
        Arc::new(Self {
            current: Mutex::new(current),
            path,
            binding,
            peers: Mutex::new(HashMap::new()),
            next_peer: AtomicU64::new(1),
            lease_expired: AtomicBool::new(false),
        })
    }

    /// Loads the persisted policy from `state_dir`. A missing, unreadable,
    /// corrupt or foreign file means no policy (fail closed until the first
    /// heartbeat). A persisted policy keeps its original lease expiry.
    pub fn load(state_dir: &Path, binding: String) -> Arc<Self> {
        let path = state_dir.join(POLICY_FILE);
        let current = match read_persisted(&path, &binding) {
            Ok(Some(policy)) => {
                info!(
                    version = policy.version,
                    enforced = policy.enforced,
                    allowed_workspaces = policy.allowed.len(),
                    lease_expires_unix = policy.lease_expires_unix,
                    "loaded persisted workspace policy"
                );
                Some(policy)
            }
            Ok(None) => {
                info!("no persisted workspace policy: refusing workspace peers until the first heartbeat applies one");
                None
            }
            Err(error) => {
                warn!(
                    path = %path.display(),
                    error = format!("{error:#}"),
                    "ignoring unusable persisted workspace policy: refusing workspace peers until the first heartbeat applies one"
                );
                None
            }
        };
        Self::with(current, Some(path), binding)
    }

    /// Not persisted; no policy yet.
    #[cfg(test)]
    pub fn in_memory() -> Arc<Self> {
        Self::with(None, None, String::new())
    }

    /// Test helper: an applied non-enforcing policy (accepts everything).
    #[cfg(test)]
    pub fn allow_all() -> Arc<Self> {
        Self::with(
            Some(Policy {
                version: 1,
                enforced: false,
                allowed: BTreeSet::new(),
                lease_expires_unix: 0,
            }),
            None,
            String::new(),
        )
    }

    /// Version of the applied policy, 0 if none (reported in the heartbeat).
    pub fn applied_version(&self) -> u64 {
        lock(&self.current).as_ref().map_or(0, |p| p.version)
    }

    #[cfg(test)]
    pub fn snapshot(&self) -> Option<Policy> {
        lock(&self.current).clone()
    }

    #[cfg(test)]
    pub fn peer_count(&self) -> usize {
        lock(&self.peers).len()
    }

    /// Whether a peer of `workspace_id` may use this relay at `now`.
    #[cfg(test)]
    pub fn check(&self, workspace_id: &str, now: i64) -> Result<(), &'static [u8]> {
        decide(lock(&self.current).as_ref(), workspace_id, now)
    }

    /// Admits a peer and registers its connection for the life of the
    /// returned guard. Decision and registration happen under the policy
    /// lock, so a concurrently applied policy either sees this peer in its
    /// sweep or was already used for the decision.
    pub fn admit(
        self: &Arc<Self>,
        workspace_id: &str,
        connection: &Connection,
        now: i64,
    ) -> Result<PeerGuard, &'static [u8]> {
        let current = lock(&self.current);
        decide(current.as_ref(), workspace_id, now)?;
        let id = self.next_peer.fetch_add(1, Ordering::Relaxed);
        lock(&self.peers).insert(
            id,
            Peer {
                workspace_id: workspace_id.to_ascii_lowercase(),
                connection: connection.clone(),
            },
        );
        drop(current);
        Ok(PeerGuard {
            policy: self.clone(),
            id,
        })
    }

    /// Applies the policy of a heartbeat response, persists it, and ends the
    /// sessions it no longer allows.
    pub fn apply(&self, response: &HeartbeatResponse, now: i64) -> Applied {
        let version = response.workspace_policy_version;
        if version == 0 {
            return Applied::Absent;
        }
        let lease_expires_unix =
            now.saturating_add(i64::from(response.workspace_policy_lease_seconds));
        let mut current = lock(&self.current);
        let applied = current.as_ref().map_or(0, |p| p.version);
        let outcome = if version < applied {
            warn!(version, applied, "ignoring older workspace policy version");
            return Applied::Stale;
        } else if version == applied {
            let policy = current.as_mut().expect("applied version > 0 has a policy");
            policy.lease_expires_unix = lease_expires_unix;
            Applied::Renewed
        } else {
            let policy = Policy {
                version,
                enforced: response.workspace_policy_enforced,
                allowed: response
                    .allowed_workspace_ids
                    .iter()
                    .map(|id| id.to_ascii_lowercase())
                    .collect(),
                lease_expires_unix,
            };
            info!(
                version,
                previous_version = applied,
                enforced = policy.enforced,
                allowed_workspaces = policy.allowed.len(),
                lease_seconds = response.workspace_policy_lease_seconds,
                "applied workspace policy"
            );
            *current = Some(policy);
            Applied::Replaced
        };
        let snapshot = current.clone().expect("policy applied");
        drop(current);

        if self.lease_expired.swap(false, Ordering::Relaxed) {
            info!(
                version,
                "workspace policy lease renewed: serving allowed workspaces again"
            );
        }
        // Renewals of a non-enforcing policy change nothing that matters.
        if outcome == Applied::Replaced || snapshot.enforced {
            self.persist(&snapshot);
        }
        self.enforce(now);
        outcome
    }

    /// Closes every registered peer the current policy refuses at `now`
    /// (dropped workspaces, an expired lease). Returns how many it closed.
    pub fn enforce(&self, now: i64) -> usize {
        let current = lock(&self.current);
        let Some(policy) = current.as_ref() else {
            return 0;
        };
        if !policy.enforced {
            return 0;
        }
        if policy.lease_expired(now) && !self.lease_expired.swap(true, Ordering::Relaxed) {
            warn!(
                version = policy.version,
                lease_expires_unix = policy.lease_expires_unix,
                "workspace policy lease expired: refusing workspace peers and ending their sessions until a fresh policy arrives"
            );
        }
        let peers = lock(&self.peers);
        let mut closed = 0;
        for peer in peers.values() {
            if peer.connection.close_reason().is_some() {
                continue; // already ending; its guard drops shortly
            }
            if let Err(reason) = policy.decide(&peer.workspace_id, now) {
                warn!(
                    workspace_id = %peer.workspace_id,
                    remote = %peer.connection.remote_address(),
                    reason = %String::from_utf8_lossy(reason),
                    version = policy.version,
                    "ending Relay session: workspace no longer allowed by the workspace policy"
                );
                peer.connection.close(0u32.into(), reason);
                closed += 1;
            }
        }
        closed
    }

    /// Checks the lease every `period`, independently of heartbeats.
    pub fn spawn_lease_watch(self: Arc<Self>, period: Duration) -> JoinHandle<()> {
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(period);
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            loop {
                tick.tick().await;
                self.enforce(unix_now());
            }
        })
    }

    fn persist(&self, policy: &Policy) {
        let Some(path) = &self.path else { return };
        let result = serde_json::to_vec(&Persisted {
            binding: self.binding.clone(),
            policy: policy.clone(),
        })
        .context("encode workspace policy")
        .and_then(|data| {
            crate::cert_manager::write_atomic(path, &data, 0o600)
                .with_context(|| format!("write {}", path.display()))
        });
        if let Err(error) = result {
            // The policy still applies in memory; a restart falls back to the
            // last persisted one (or none) and the next heartbeat.
            warn!(
                error = format!("{error:#}"),
                "failed to persist workspace policy"
            );
        }
    }
}

fn read_persisted(path: &Path, binding: &str) -> Result<Option<Policy>> {
    let data = match std::fs::read(path) {
        Ok(data) => data,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error).with_context(|| format!("read {}", path.display())),
    };
    let persisted: Persisted = serde_json::from_slice(&data).context("parse workspace policy")?;
    if persisted.binding != binding {
        anyhow::bail!("workspace policy belongs to another relay identity or platform CA");
    }
    if persisted.policy.version == 0 {
        anyhow::bail!("workspace policy has version 0");
    }
    Ok(Some(persisted.policy))
}

/// Binding of the persisted policy: this relay's id plus the SHA-256 of the
/// platform Intermediate CA it trusts.
pub fn binding_for(relay_id: &str, intermediate_ca_pem: &[u8]) -> String {
    use sha2::Digest;
    format!(
        "{}:{}",
        relay_id.to_ascii_lowercase(),
        hex::encode(sha2::Sha256::digest(intermediate_ca_pem))
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::quic_pair;

    const A: &str = "aaaaaaaa-0000-0000-0000-000000000001";
    const B: &str = "bbbbbbbb-0000-0000-0000-000000000002";
    const NOW: i64 = 1_800_000_000;

    fn response(version: u64, enforced: bool, allowed: &[&str], lease: u32) -> HeartbeatResponse {
        HeartbeatResponse {
            workspace_policy_version: version,
            workspace_policy_enforced: enforced,
            allowed_workspace_ids: allowed.iter().map(|s| s.to_string()).collect(),
            workspace_policy_lease_seconds: lease,
            ..Default::default()
        }
    }

    fn state_dir() -> PathBuf {
        let dir =
            std::env::temp_dir().join(format!("relay-policy-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn no_policy_refuses_every_workspace() {
        let p = WorkspacePolicy::in_memory();
        assert_eq!(p.applied_version(), 0);
        assert_eq!(p.check(A, NOW), Err(NO_POLICY_REASON));
    }

    #[test]
    fn version_zero_is_ignored_without_renewing_the_lease() {
        let p = WorkspacePolicy::in_memory();
        assert_eq!(p.apply(&response(0, true, &[A], 600), NOW), Applied::Absent);
        assert_eq!(p.snapshot(), None, "version 0 never applies a policy");

        p.apply(&response(5, true, &[A], 600), NOW);
        assert_eq!(
            p.apply(&response(0, false, &[], 600), NOW + 300),
            Applied::Absent
        );
        let policy = p.snapshot().unwrap();
        assert_eq!(policy.version, 5);
        assert!(policy.enforced);
        assert_eq!(policy.lease_expires_unix, NOW + 600, "lease not renewed");
    }

    #[test]
    fn older_version_is_ignored() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(7, true, &[A], 600), NOW);
        assert_eq!(
            p.apply(&response(6, false, &[B], 600), NOW + 30),
            Applied::Stale
        );
        let policy = p.snapshot().unwrap();
        assert_eq!(policy.version, 7);
        assert!(policy.enforced);
        assert_eq!(policy.lease_expires_unix, NOW + 600);
        assert_eq!(p.check(B, NOW + 30), Err(NOT_ALLOWED_REASON));
    }

    #[test]
    fn equal_version_renews_the_lease() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(7, true, &[A], 600), NOW);
        assert_eq!(
            p.apply(&response(7, true, &[A], 600), NOW + 30),
            Applied::Renewed
        );
        assert_eq!(p.snapshot().unwrap().lease_expires_unix, NOW + 630);
        assert_eq!(p.check(A, NOW + 610), Ok(()), "renewed lease still valid");
    }

    #[test]
    fn newer_version_replaces_with_lease_from_response() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(7, true, &[A], 600), NOW);
        assert_eq!(
            p.apply(&response(9, true, &[B], 120), NOW + 30),
            Applied::Replaced
        );
        assert_eq!(
            p.snapshot().unwrap(),
            Policy {
                version: 9,
                enforced: true,
                allowed: BTreeSet::from([B.to_owned()]),
                lease_expires_unix: NOW + 150,
            }
        );
        assert_eq!(p.applied_version(), 9);
        assert_eq!(p.check(A, NOW + 30), Err(NOT_ALLOWED_REASON));
        assert_eq!(p.check(B, NOW + 30), Ok(()));
    }

    #[test]
    fn not_enforced_accepts_every_workspace_and_ignores_the_lease() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(3, false, &[], 600), NOW);
        assert_eq!(p.check(A, NOW), Ok(()));
        assert_eq!(p.check(B, NOW + 100_000), Ok(()), "no lease effect");
    }

    #[test]
    fn enforced_allows_only_listed_workspaces_until_the_lease_expires() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(3, true, &[&A.to_ascii_uppercase()], 600), NOW);
        assert_eq!(p.check(A, NOW), Ok(()), "ids compare case-insensitively");
        assert_eq!(p.check(B, NOW), Err(NOT_ALLOWED_REASON));
        assert_eq!(
            p.check(A, NOW + 599),
            Ok(()),
            "missed heartbeats within the lease"
        );
        assert_eq!(p.check(A, NOW + 600), Err(LEASE_EXPIRED_REASON));
        p.apply(&response(3, true, &[A], 600), NOW + 700);
        assert_eq!(
            p.check(A, NOW + 700),
            Ok(()),
            "fresh policy restores service"
        );
    }

    #[test]
    fn persistence_round_trip_keeps_the_original_lease_expiry() {
        let dir = state_dir();
        let p = WorkspacePolicy::load(&dir, "binding".into());
        p.apply(&response(4, true, &[A, B], 600), NOW);
        let mode = std::os::unix::fs::PermissionsExt::mode(
            &std::fs::metadata(dir.join(POLICY_FILE))
                .unwrap()
                .permissions(),
        );
        assert_eq!(mode & 0o777, 0o600);

        let restarted = WorkspacePolicy::load(&dir, "binding".into());
        let policy = restarted.snapshot().expect("persisted policy loaded");
        assert_eq!(policy, p.snapshot().unwrap());
        assert_eq!(
            policy.lease_expires_unix,
            NOW + 600,
            "restart keeps the lease"
        );
        assert_eq!(restarted.check(A, NOW + 600), Err(LEASE_EXPIRED_REASON));

        // A renewal is persisted too.
        p.apply(&response(4, true, &[A, B], 600), NOW + 30);
        let restarted = WorkspacePolicy::load(&dir, "binding".into());
        assert_eq!(restarted.snapshot().unwrap().lease_expires_unix, NOW + 630);
    }

    #[test]
    fn corrupt_or_foreign_file_means_no_policy() {
        let dir = state_dir();
        std::fs::write(dir.join(POLICY_FILE), b"{not json").unwrap();
        let p = WorkspacePolicy::load(&dir, "binding".into());
        assert_eq!(p.snapshot(), None);
        assert_eq!(p.check(A, NOW), Err(NO_POLICY_REASON));

        let dir = state_dir();
        WorkspacePolicy::load(&dir, "relay-1".into()).apply(&response(4, false, &[], 600), NOW);
        let other = WorkspacePolicy::load(&dir, "relay-2".into());
        assert_eq!(
            other.snapshot(),
            None,
            "another identity's file is not applied"
        );
    }

    #[test]
    fn binding_covers_relay_id_and_ca() {
        assert_eq!(binding_for("ABC", b"ca"), binding_for("abc", b"ca"));
        assert_ne!(binding_for("abc", b"ca"), binding_for("abc", b"other"));
        assert_ne!(binding_for("abc", b"ca"), binding_for("abd", b"ca"));
    }

    #[tokio::test]
    async fn admit_refuses_disallowed_workspace_without_registering() {
        let p = WorkspacePolicy::in_memory();
        let (_peer, conn) = quic_pair().await;
        assert_eq!(p.admit(A, &conn, NOW).err(), Some(NO_POLICY_REASON));
        p.apply(&response(2, true, &[A], 600), NOW);
        assert_eq!(p.admit(B, &conn, NOW).err(), Some(NOT_ALLOWED_REASON));
        assert_eq!(p.peer_count(), 0);
        let guard = p.admit(A, &conn, NOW).unwrap();
        assert_eq!(p.peer_count(), 1);
        drop(guard);
        assert_eq!(p.peer_count(), 0, "guard unregisters the peer");
    }

    async fn closed_with(conn: &Connection) -> Vec<u8> {
        match tokio::time::timeout(Duration::from_secs(5), conn.closed())
            .await
            .expect("connection must be closed")
        {
            quinn::ConnectionError::ApplicationClosed(close) => close.reason.to_vec(),
            other => panic!("unexpected close: {other}"),
        }
    }

    /// D-40 revocation: a newly applied policy that drops B ends B's
    /// connections (connector and client alike) at once; A's stay.
    #[tokio::test]
    async fn dropping_a_workspace_closes_its_connections_and_keeps_others() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(2, true, &[A, B], 600), NOW);
        let (a_peer, a_conn) = quic_pair().await;
        let (b_connector, b_conn1) = quic_pair().await;
        let (b_client, b_conn2) = quic_pair().await;
        let _a = p.admit(A, &a_conn, NOW).unwrap();
        let _b1 = p.admit(B, &b_conn1, NOW).unwrap();
        let _b2 = p.admit(B, &b_conn2, NOW).unwrap();

        assert_eq!(
            p.apply(&response(3, true, &[A], 600), NOW + 30),
            Applied::Replaced
        );
        assert_eq!(closed_with(&b_connector).await, NOT_ALLOWED_REASON);
        assert_eq!(closed_with(&b_client).await, NOT_ALLOWED_REASON);
        assert!(
            a_peer.close_reason().is_none(),
            "allowed workspace untouched"
        );
        assert!(a_conn.close_reason().is_none());
    }

    /// Turning enforcement on ends every workspace not on the list.
    #[tokio::test]
    async fn enforcement_turned_on_closes_unlisted_workspaces() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(2, false, &[], 600), NOW);
        let (a_peer, a_conn) = quic_pair().await;
        let (b_peer, b_conn) = quic_pair().await;
        let _a = p.admit(A, &a_conn, NOW).unwrap();
        let _b = p.admit(B, &b_conn, NOW).unwrap();
        p.apply(&response(3, true, &[A], 600), NOW);
        assert_eq!(closed_with(&b_peer).await, NOT_ALLOWED_REASON);
        assert!(a_peer.close_reason().is_none());
    }

    /// Lease expiry ends existing sessions and refuses new ones; a fresh
    /// policy restores service. A failed heartbeat (no `apply`) changes
    /// nothing before the lease ends.
    #[tokio::test]
    async fn lease_expiry_closes_sessions_and_a_fresh_policy_restores_service() {
        let p = WorkspacePolicy::in_memory();
        p.apply(&response(2, true, &[A], 600), NOW);
        let (a_peer, a_conn) = quic_pair().await;
        let _a = p.admit(A, &a_conn, NOW).unwrap();

        assert_eq!(p.enforce(NOW + 599), 0, "still within the lease");
        assert!(a_peer.close_reason().is_none());

        assert_eq!(p.enforce(NOW + 600), 1);
        assert_eq!(closed_with(&a_peer).await, LEASE_EXPIRED_REASON);
        let (_new_peer, new_conn) = quic_pair().await;
        assert_eq!(
            p.admit(A, &new_conn, NOW + 601).err(),
            Some(LEASE_EXPIRED_REASON)
        );

        p.apply(&response(2, true, &[A], 600), NOW + 700);
        assert!(p.admit(A, &new_conn, NOW + 700).is_ok(), "service restored");
    }

    /// The background watch ends sessions at lease expiry without any
    /// heartbeat.
    #[tokio::test]
    async fn lease_watch_closes_sessions_without_a_heartbeat() {
        let p = WorkspacePolicy::in_memory();
        // A lease that is already over by the next real-clock tick.
        p.apply(&response(2, true, &[A], 1), unix_now() - 5);
        let (a_peer, a_conn) = quic_pair().await;
        // Registered directly: admit would (correctly) refuse it now.
        lock(&p.peers).insert(
            0,
            Peer {
                workspace_id: A.to_owned(),
                connection: a_conn,
            },
        );
        let watch = p.clone().spawn_lease_watch(Duration::from_millis(20));
        assert_eq!(closed_with(&a_peer).await, LEASE_EXPIRED_REASON);
        watch.abort();
    }
}
