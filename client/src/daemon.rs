use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr, ToSocketAddrs};
use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;
use tokio::sync::Mutex;
use tracing::{error, info, warn};

use crate::auth;
use crate::config;
use crate::grpc::{
    self,
    client_v1::{AclConnector, AclEntry, AclRemoteNetwork, GetAclSnapshotRequest},
};
use crate::ipc::{check_same_user, ipc_socket_path, IpcRequest, IpcResource, IpcResponse};
use crate::login::LoginResult;
use crate::net_stack;
use crate::relay_pool::RelayPool;
use crate::runtime::{
    self, DeviceInfo, SessionInfo, SharedState, TunHandle, UserInfo, WorkspaceInfo,
};
use crate::state_store::{self, save_workspace_state, StoredWorkspaceState};
use crate::transport::{ClientTransport, RelayContext};
use crate::tun::{AllowedFlow, TunManager};
use crate::tunnel_pool::TunnelPool;

type TunSlot = Arc<Mutex<Option<TunManager>>>;
const ACL_REFRESH_TTL_SECS: i64 = 60;
// Early-resync backoff after a relay transport failure. The connector may still
// be re-homing, so the controller's ACL may not carry the new relay yet — retry
// with exponential backoff until the version changes, then fall back to the
// steady poll tick. Bounded so a permanently-dead relay can't spin forever.
// ~2+4+8+16+16 = 46s span, which covers the connector re-home floor (~5–15s).
const RELAY_RESYNC_BASE_SECS: u64 = 2;
const RELAY_RESYNC_MAX_SECS: u64 = 16;
const RELAY_RESYNC_MAX_ATTEMPTS: u32 = 5;
// Minimum gap between early-resync bursts. Without it, a permanently-dead relay
// plus an app that keeps retrying connections would re-arm the signal
// continuously and spin the burst back-to-back. The 60s tick is the backstop.
const RELAY_RESYNC_COOLDOWN_SECS: u64 = 30;

struct AclSyncResult {
    version: u64,
    entry_count: usize,
    synced_at: i64,
    changed: bool,
}

pub async fn run() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::new("info"))
        .init();

    info!(
        name = env!("CARGO_PKG_NAME"),
        version = env!("CARGO_PKG_VERSION"),
        "daemon starting"
    );

    let conf = config::load()
        .context("daemon requires a configured workspace — run zecurity-client setup first")?;

    let state = runtime::new_shared();

    // Load encrypted durable state on startup if present.
    if let Ok(stored) = state_store::load_workspace_state(&conf.workspace) {
        let mut s = state.write().await;
        populate_runtime(&mut s, &stored);
        info!(workspace = %conf.workspace, "durable state loaded on startup");
        drop(s);

        // Fetch ACL snapshot in background — stale token will just log a warning.
        let state_clone = Arc::clone(&state);
        let conf_clone = conf.clone();
        let ca_pem = stored.device.ca_cert_pem.clone();
        let device_id = stored.device.id.clone();
        tokio::spawn(async move {
            fetch_and_store_acl(&state_clone, &conf_clone, ca_pem, device_id).await;
        });
    }

    // Proactive session-refresh loop. Runs for the daemon's lifetime, sleeps
    // until each access token nears expiry, then rotates. See run_refresh_scheduler.
    {
        let state_clone = Arc::clone(&state);
        let conf_clone = conf.clone();
        tokio::spawn(async move {
            run_refresh_scheduler(state_clone, conf_clone).await;
        });
    }

    let socket_path = ipc_socket_path();

    // Remove stale socket from a previous run.
    if socket_path.exists() {
        std::fs::remove_file(&socket_path)
            .with_context(|| format!("remove stale socket {}", socket_path.display()))?;
    }

    // Ensure parent directory exists (non-systemd / dev mode).
    if let Some(parent) = socket_path.parent() {
        std::fs::create_dir_all(parent)
            .with_context(|| format!("create socket directory {}", parent.display()))?;
    }

    let listener = UnixListener::bind(&socket_path)
        .with_context(|| format!("bind IPC socket {}", socket_path.display()))?;

    info!(path = %socket_path.display(), "IPC socket ready");

    // Signal systemd: socket is bound, daemon is ready (required for Type=notify).
    sd_notify_ready();

    // Keep the systemd watchdog alive. Reads WATCHDOG_USEC set by systemd and
    // pings every half-interval so a transient slow tick never trips the timeout.
    sd_spawn_watchdog();

    let tun_slot: TunSlot = Arc::new(Mutex::new(None));

    // Background ACL sync loop. Fetches the snapshot every ACL_REFRESH_TTL_SECS
    // and restarts the tunnel when the version changes — the timer counterpart
    // to the action-triggered sync in handle_request. See run_acl_sync_scheduler.
    {
        let state_clone = Arc::clone(&state);
        let conf_clone = conf.clone();
        let tun_slot_clone = Arc::clone(&tun_slot);
        tokio::spawn(async move {
            run_acl_sync_scheduler(state_clone, conf_clone, tun_slot_clone).await;
        });
    }
    loop {
        match listener.accept().await {
            Ok((stream, _)) => {
                if !check_same_user(&stream) {
                    warn!("rejected IPC connection from a different user");
                    continue;
                }
                let state = Arc::clone(&state);
                let conf = conf.clone();
                let tun_slot = Arc::clone(&tun_slot);
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(stream, state, conf, tun_slot).await {
                        error!(error = %e, "IPC connection error");
                    }
                });
            }
            Err(e) => error!(error = %e, "IPC accept error"),
        }
    }
}

async fn handle_connection(
    stream: tokio::net::UnixStream,
    state: SharedState,
    conf: config::ClientConf,
    tun_slot: TunSlot,
) -> Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    let mut line = String::new();

    reader.read_line(&mut line).await?;

    let (response, shutdown) = match serde_json::from_str::<IpcRequest>(line.trim()) {
        Ok(req) => {
            let is_shutdown = matches!(req, IpcRequest::Shutdown);
            let resp = handle_request(req, &state, &conf, &tun_slot).await;
            (resp, is_shutdown)
        }
        Err(_) => (
            IpcResponse {
                ok: false,
                kind: "Error".into(),
                error: Some("malformed JSON request".into()),
                ..Default::default()
            },
            false,
        ),
    };

    let mut resp_line = serde_json::to_string(&response)?;
    resp_line.push('\n');
    writer.write_all(resp_line.as_bytes()).await?;
    writer.flush().await?;

    if shutdown {
        info!("shutdown requested via IPC — exiting");
        std::process::exit(0);
    }

    Ok(())
}

async fn handle_request(
    req: IpcRequest,
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) -> IpcResponse {
    match req {
        IpcRequest::Status => {
            let s = state.read().await;
            IpcResponse {
                ok: true,
                kind: "Status".into(),
                state: Some("running".into()),
                email: s.user.as_ref().map(|u| u.email.clone()),
                device_id: s.device.as_ref().map(|d| d.id.clone()),
                spiffe_id: s.device.as_ref().map(|d| d.spiffe_id.clone()),
                cert_expires_at: s.device.as_ref().map(|d| d.cert_expires_at),
                workspace: s.workspace.as_ref().map(|w| w.name.clone()),
                acl_snapshot_version: s.acl_snapshot.as_ref().map(|snap| snap.version),
                acl_last_sync_at: s.acl_last_sync_at,
                acl_entry_count: s.acl_snapshot.as_ref().map(|snap| snap.entries.len()),
                ..Default::default()
            }
        }

        IpcRequest::Sync => match sync_acl_now(state, conf).await {
            Ok(result) => {
                if result.changed {
                    info!(
                        version = result.version,
                        "ACL changed, restarting VPN or tunnel"
                    );

                    if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
                        return IpcResponse {
                            ok: false,
                            kind: "Sync".into(),
                            error: Some(e.to_string()),
                            ..Default::default()
                        };
                    }
                } else {
                    info!(
                        version = result.version,
                        "ACL unchanged, skipping VPN or tunnel restart"
                    );
                }
                IpcResponse {
                    ok: true,
                    kind: "Sync".into(),
                    acl_snapshot_version: Some(result.version),
                    acl_last_sync_at: Some(result.synced_at),
                    acl_entry_count: Some(result.entry_count),
                    synced_resources: Some(result.entry_count),
                    ..Default::default()
                }
            }
            Err(e) => IpcResponse {
                ok: false,
                kind: "Sync".into(),
                error: Some(e.to_string()),
                ..Default::default()
            },
        },

        IpcRequest::Resources => {
            match refresh_acl_if_needed(state, conf).await {
                Ok(Some(result)) if result.changed => {
                    if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
                        return IpcResponse {
                            ok: false,
                            kind: "Resources".into(),
                            error: Some(e.to_string()),
                            ..Default::default()
                        };
                    }
                }
                Ok(_) => {}
                Err(e) => {
                    return IpcResponse {
                        ok: false,
                        kind: "Resources".into(),
                        error: Some(e.to_string()),
                        ..Default::default()
                    };
                }
            }

            let s = state.read().await;
            let my_spiffe = s
                .device
                .as_ref()
                .map(|d| d.spiffe_id.as_str())
                .unwrap_or("");
            let resources = s.acl_snapshot.as_ref().map(|snap| {
                snap.entries
                    .iter()
                    .filter(|e| e.allowed_spiffe_ids.iter().any(|id| id == my_spiffe))
                    .map(|e| IpcResource {
                        name: e.name.clone(),
                        address: e.address.clone(),
                        port: e.port,
                        protocol: e.protocol.clone(),
                    })
                    .collect::<Vec<_>>()
            });
            IpcResponse {
                ok: true,
                kind: "Resources".into(),
                acl_snapshot_version: s.acl_snapshot.as_ref().map(|snap| snap.version),
                acl_last_sync_at: s.acl_last_sync_at,
                acl_entry_count: s.acl_snapshot.as_ref().map(|snap| snap.entries.len()),
                resources,
                ..Default::default()
            }
        }

        IpcRequest::Shutdown => IpcResponse {
            ok: true,
            kind: "Shutdown".into(),
            ..Default::default()
        },

        IpcRequest::LoadState => match state_store::load_workspace_state(&conf.workspace) {
            Ok(stored) => {
                let mut s = state.write().await;
                populate_runtime(&mut s, &stored);
                info!("runtime state reloaded via LoadState");
                IpcResponse {
                    ok: true,
                    kind: "LoadState".into(),
                    ..Default::default()
                }
            }
            Err(e) => IpcResponse {
                ok: false,
                kind: "LoadState".into(),
                error: Some(e.to_string()),
                ..Default::default()
            },
        },

        IpcRequest::GetToken => {
            let s = state.read().await;
            match s
                .session
                .as_ref()
                .filter(|sess| !sess.access_token.is_empty())
            {
                Some(sess) => IpcResponse {
                    ok: true,
                    kind: "GetToken".into(),
                    token: Some(sess.access_token.clone()),
                    ..Default::default()
                },
                None => IpcResponse {
                    ok: false,
                    kind: "GetToken".into(),
                    error: Some("no active session — run zecurity-client login".into()),
                    ..Default::default()
                },
            }
        }

        IpcRequest::PostLoginState {
            workspace_slug,
            workspace_name,
            workspace_id: _,
            trust_domain,
            user_email,
            access_token,
            refresh_token,
            expires_at,
            device_id,
            spiffe_id,
            certificate_pem,
            private_key_pem,
            ca_cert_pem,
            cert_expires_at,
            hostname,
            os,
        } => {
            // Reconstruct LoginResult so from_login can decode workspace_id,
            // user_id, and role from the JWT claims in access_token.
            let login_result = LoginResult {
                workspace: WorkspaceInfo {
                    id: String::new(),
                    name: workspace_name,
                    slug: workspace_slug.clone(),
                    trust_domain,
                },
                user: UserInfo {
                    id: String::new(),
                    email: user_email,
                    role: String::new(),
                },
                device: DeviceInfo {
                    id: device_id,
                    spiffe_id,
                    certificate_pem,
                    private_key_pem,
                    ca_cert_pem,
                    cert_expires_at,
                    hostname,
                    os,
                },
                session: SessionInfo {
                    access_token,
                    refresh_token,
                    expires_at,
                },
            };
            let stored = StoredWorkspaceState::from_login(login_result);

            match save_workspace_state(&workspace_slug, &stored) {
                Ok(_) => {
                    let mut s = state.write().await;
                    populate_runtime(&mut s, &stored);
                    info!("PostLoginState: durable state saved, runtime updated");
                    drop(s);

                    let result = match sync_acl_now(state, conf).await {
                        Ok(r) => r,
                        Err(e) => {
                            return IpcResponse {
                                ok: false,
                                kind: "PostLoginState".into(),
                                error: Some(format!(
                                    "login state saved, but ACL sync failde: {}",
                                    e
                                )),
                                ..Default::default()
                            };
                        }
                    };
                    info!(
                        version = result.version,
                        entries = result.entry_count,
                        "ACL syncronized after login"
                    );

                    let running = tun_slot.lock().await.is_some();
                    if !running {
                        // Automatically connect after login.
                        let up = handle_up(state, conf, tun_slot).await;

                        if !up.ok {
                            return IpcResponse {
                                ok: false,
                                kind: "PostLoginState".into(),
                                error: Some(
                                    up.error.unwrap_or_else(|| {
                                        "automatic vpn startup failed".to_string()
                                    }),
                                ),
                                ..Default::default()
                            };
                        }
                    } else {
                        if result.changed {
                            if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
                                return IpcResponse {
                                    ok: false,
                                    kind: "Sync".into(),
                                    error: Some(e.to_string()),
                                    ..Default::default()
                                };
                            }
                        } else {
                            info!(
                                version = result.version,
                                "acl unchanged after login, tunnel restart not required"
                            );
                        }
                    }

                    IpcResponse {
                        ok: true,
                        kind: "PostLoginState".into(),
                        ..Default::default()
                    }
                }
                Err(e) => IpcResponse {
                    ok: false,
                    kind: "PostLoginState".into(),
                    error: Some(e.to_string()),
                    ..Default::default()
                },
            }
        }

        IpcRequest::Up => handle_up(state, conf, tun_slot).await,

        IpcRequest::Down => handle_down(state, tun_slot).await,
    }
}

async fn handle_up(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) -> IpcResponse {
    // Reject if already up.
    if tun_slot.lock().await.is_some() {
        return IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some("already up".into()),
            ..Default::default()
        };
    }

    if let Err(e) = refresh_acl_if_needed(state, conf).await {
        return IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some(format!("ACL sync failed: {}", e)),
            ..Default::default()
        };
    }

    // Require an ACL snapshot with at least one entry.
    let (acl, device) = {
        let s = state.read().await;
        (s.acl_snapshot.clone(), s.device.clone())
    };

    let acl = match acl {
        None => {
            return IpcResponse {
                ok: false,
                kind: "Up".into(),
                error: Some(
                    "no ACL snapshot — run zecurity-client status to check daemon state".into(),
                ),
                ..Default::default()
            }
        }
        Some(a) if a.entries.is_empty() => {
            return IpcResponse {
                ok: false,
                kind: "Up".into(),
                error: Some("ACL snapshot has no entries — no resources to route".into()),
                ..Default::default()
            }
        }
        Some(a) => Arc::new(a),
    };

    let device = match device {
        None => {
            return IpcResponse {
                ok: false,
                kind: "Up".into(),
                error: Some("no device identity — run zecurity-client login first".into()),
                ..Default::default()
            }
        }
        Some(d) => d,
    };

    // Filter to only entries this device is permitted to access.
    let my_spiffe = device.spiffe_id.clone();
    let allowed_entries: Vec<AclEntry> = acl
        .entries
        .iter()
        .filter(|e| {
            e.allowed_spiffe_ids
                .iter()
                .any(|id| id == my_spiffe.as_str())
        })
        .cloned()
        .collect();

    if allowed_entries.is_empty() {
        return IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some("no accessible resources for this device — check group membership".into()),
            ..Default::default()
        };
    }

    let transports =
        match build_transports_by_resource(&allowed_entries, &acl.remote_networks, &device) {
            Ok(t) => Arc::new(t),
            Err(e) => {
                return IpcResponse {
                    ok: false,
                    kind: "Up".into(),
                    error: Some(format!("failed to build client transport: {}", e)),
                    ..Default::default()
                }
            }
        };

    // Create TUN device.
    let mut mgr = match TunManager::create().await {
        Ok(m) => m,
        Err(e) => {
            return IpcResponse {
                ok: false,
                kind: "Up".into(),
                error: Some(format!("failed to create TUN device: {}", e)),
                ..Default::default()
            }
        }
    };

    // Mark only the allowed TCP destination flows into the Zecurity route table.
    // Other ports on the same IP stay on the normal kernel route.
    let allowed_flows: Vec<AllowedFlow> = allowed_entries
        .iter()
        .filter(|e| e.protocol.to_lowercase() == "tcp" || e.protocol.is_empty())
        .filter_map(|e| {
            let IpAddr::V4(ip) = e.address.parse::<IpAddr>().ok()? else {
                return None;
            };
            Some(AllowedFlow {
                ip,
                port: e.port as u16,
            })
        })
        .collect();

    if allowed_flows.is_empty() {
        return IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some("no TCP resources available for this device".into()),
            ..Default::default()
        };
    }

    if let Err(e) = mgr.configure_allowed_flows(&allowed_flows) {
        return IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some(format!("failed to configure split-tunnel routes: {}", e)),
            ..Default::default()
        };
    }

    let route_count = allowed_flows.len();
    let dev = match mgr.take_device() {
        Some(d) => d,
        None => {
            return IpcResponse {
                ok: false,
                kind: "Up".into(),
                error: Some("TUN device unavailable".into()),
                ..Default::default()
            }
        }
    };

    let relay_resync = { state.read().await.relay_resync.clone() };
    let task = tokio::spawn(async move {
        if let Err(e) = net_stack::run(dev, allowed_entries, transports, relay_resync).await {
            error!(error = %e, "net_stack exited with error");
        }
    });
    let abort = task.abort_handle();

    // Store TunManager (for route cleanup) and AbortHandle (for task cancel).
    *tun_slot.lock().await = Some(mgr);
    state.write().await.tun_handle = Some(Arc::new(TunHandle { abort, route_count }));

    info!(routes = route_count, "zecurity0 up");
    IpcResponse {
        ok: true,
        kind: "Up".into(),
        ..Default::default()
    }
}

async fn handle_down(state: &SharedState, tun_slot: &TunSlot) -> IpcResponse {
    let handle = state.write().await.tun_handle.take();
    if let Some(h) = handle {
        h.abort.abort();
    }

    let mgr = tun_slot.lock().await.take();
    if let Some(m) = mgr {
        if let Err(e) = m.cleanup().await {
            warn!(error = %e, "error cleaning up TUN routes");
        }
    }

    info!("zecurity0 down");
    IpcResponse {
        ok: true,
        kind: "Down".into(),
        ..Default::default()
    }
}

async fn restart_tunnel_if_running(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) -> Result<()> {
    let running = tun_slot.lock().await.is_some();

    if !running {
        return Ok(());
    }

    info!("ACL changed, restarting VPN");

    let down = handle_down(state, tun_slot).await;
    if !down.ok {
        anyhow::bail!(
            "{}",
            down.error.unwrap_or_else(|| "failed to stop VPN".into())
        );
    }

    let up = handle_up(state, conf, tun_slot).await;
    if !up.ok {
        anyhow::bail!(
            "{}",
            up.error.unwrap_or_else(|| "failed to start VPN".into())
        );
    }

    Ok(())
}

fn populate_runtime(s: &mut runtime::RuntimeState, stored: &StoredWorkspaceState) {
    s.workspace = Some(WorkspaceInfo::from(stored));
    s.user = Some(UserInfo::from(stored));
    s.device = Some(DeviceInfo::from(stored));
    s.session = Some(SessionInfo::from(stored));
}

fn sd_notify_ready() {
    let Ok(path) = std::env::var("NOTIFY_SOCKET") else {
        return;
    };
    let _ =
        std::os::unix::net::UnixDatagram::unbound().and_then(|s| s.send_to(b"READY=1\n", &path));
}

fn sd_spawn_watchdog() {
    let Ok(usec_str) = std::env::var("WATCHDOG_USEC") else {
        return;
    };
    let Ok(usec) = usec_str.parse::<u64>() else {
        return;
    };
    let Ok(path) = std::env::var("NOTIFY_SOCKET") else {
        return;
    };
    let interval = tokio::time::Duration::from_micros(usec / 2);
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        loop {
            ticker.tick().await;
            let _ = std::os::unix::net::UnixDatagram::unbound()
                .and_then(|s| s.send_to(b"WATCHDOG=1\n", &path));
        }
    });
}

async fn fetch_acl_snapshot(
    conf: &config::ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
) -> Result<crate::grpc::client_v1::AclSnapshot> {
    let mut client = grpc::connect_grpc(conf.controller(), ca_pem).await?;
    let resp = client
        .get_acl_snapshot(GetAclSnapshotRequest {
            access_token: access_token.to_string(),
            device_id: device_id.to_string(),
        })
        .await?
        .into_inner();
    resp.snapshot
        .ok_or_else(|| anyhow::anyhow!("controller returned empty ACL snapshot"))
}

/// Fetch the ACL snapshot, transparently refreshing the access token on 401.
/// This is the entry point every control-plane call site should use — it
/// keeps the retry logic and token persistence in one place so callers do
/// not each reinvent it.
///
/// Flow:
///   1. Read current session tokens from in-memory state.
///   2. Attempt GetACLSnapshot.
///   3. If tonic::Status::Unauthenticated (server rejected the JWT), call
///      /auth/refresh to rotate both tokens.
///   4. Persist the new pair to disk (state_store) AND to in-memory state
///      atomically so any concurrent reader sees consistent tokens.
///   5. Retry GetACLSnapshot with the new access token — once.
///
/// A second Unauthenticated on the retry means the session is dead. This
/// is treated as an error the caller must surface — the user must log in
/// again. We do NOT clear state here; that is a policy decision left to
/// the caller (background sync should keep trying quietly; an interactive
/// IPC sync should prompt the user).
async fn fetch_acl_snapshot_with_refresh(
    conf: &config::ClientConf,
    ca_pem: &str,
    state: &SharedState,
    device_id: &str,
) -> Result<crate::grpc::client_v1::AclSnapshot> {
    let refresh_lock = {
        let s = state.read().await;
        s.refresh_lock.clone()
    };
    let (access_token, _refresh_token) = {
        let s = state.read().await;
        let sess = s.session.as_ref().ok_or_else(|| {
            anyhow::anyhow!("no session in state — run zecurity-client login first")
        })?;
        (sess.access_token.clone(), sess.refresh_token.clone())
    };

    match fetch_acl_snapshot(conf, ca_pem, &access_token, device_id).await {
        Ok(snap) => Ok(snap),
        Err(err) => {
            if !is_grpc_unauthenticated(&err) {
                return Err(err);
            }
            info!("ACL fetch returned Unauthenticated; refreshing session");
            // Serialize token refreshes across the daemon.
            let _guard = refresh_lock.lock().await;

            let (access_token, refresh_token) = {
                let s = state.read().await;

                let sess = s
                    .session
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("no session in state"))?;

                (sess.access_token.clone(), sess.refresh_token.clone())
            };

            match fetch_acl_snapshot(conf, ca_pem, &access_token, device_id).await {
                Ok(snapshot) => {
                    info!("ACL fetch succeeded after another task refreshed the session");
                    return Ok(snapshot);
                }

                Err(err) if is_grpc_unauthenticated(&err) => {
                    // Still expired.
                    // Continue to refresh below.
                }

                Err(err) => {
                    return Err(err);
                }
            }

            let new_tokens = auth::refresh_access_token(conf, &access_token, &refresh_token)
                .await
                .map_err(|e| match e {
                    auth::RefreshError::SessionDead => {
                        anyhow::anyhow!("session expired; re-login required")
                    }
                    auth::RefreshError::Transient(inner) => inner.context("refresh access token"),
                })?;

            // Persist the rotated pair BEFORE updating in-memory state so a
            // crash between save and in-memory update leaves the disk as the
            // source of truth for the next boot.
            state_store::save_rotated_tokens(
                &conf.workspace,
                new_tokens.access_token.clone(),
                new_tokens.refresh_token.clone(),
                new_tokens.expires_at,
            )
            .context("persist rotated tokens")?;

            {
                let mut s = state.write().await;
                if let Some(sess) = s.session.as_mut() {
                    sess.access_token = new_tokens.access_token.clone();
                    sess.refresh_token = new_tokens.refresh_token;
                    sess.expires_at = new_tokens.expires_at;
                }
            }

            fetch_acl_snapshot(conf, ca_pem, &new_tokens.access_token, device_id).await
        }
    }
}

/// True when an error surfaced from a gRPC control-plane call is the
/// controller telling us the access token is expired / revoked. We only
/// retry with refresh in this case — network errors and 5xx bubble up as
/// transient failures for the caller to handle.
fn is_grpc_unauthenticated(err: &anyhow::Error) -> bool {
    err.downcast_ref::<tonic::Status>()
        .map(|s| s.code() == tonic::Code::Unauthenticated)
        .unwrap_or(false)
}

/// Proactive session-refresh loop. The access token has a 15-minute TTL;
/// waiting for it to expire would cause a user-visible request failure on
/// the first call after 15 minutes. Instead we rotate ~60s before the
/// stamped `expires_at` so every observable call has a valid token in hand.
///
/// Runs for the daemon's lifetime as a spawned tokio task. The 401-retry
/// path in fetch_acl_snapshot_with_refresh is the safety net for cases this
/// scheduler misses (transient network failure at rotation time, clock
/// skew, first request after resume-from-suspend).
///
/// Lifecycle:
///   - No session in state (fresh daemon before login, or post-logout):
///     sleep the recheck interval and poll again.
///   - Session exists but expires_at is in the past: refresh immediately.
///   - Session exists and expires_at is in the future: sleep until
///     `expires_at - REFRESH_LEAD_SECS`, then refresh.
///
/// On SessionDead the loop does not exit. It sleeps and keeps polling so a
/// later PostLoginState can install a fresh session without requiring a daemon
/// restart.
async fn run_refresh_scheduler(state: SharedState, conf: config::ClientConf) {
    const REFRESH_LEAD_SECS: i64 = 60;
    const NO_SESSION_POLL_SECS: u64 = 60;
    const TRANSIENT_RETRY_SECS: u64 = 30;

    loop {
        // Snapshot expiry outside any long-held lock.
        let expires_at = {
            let s = state.read().await;
            s.session.as_ref().map(|sess| sess.expires_at)
        };

        let expires_at = match expires_at {
            Some(exp) => exp,
            None => {
                // Not logged in yet (or just logged out). Recheck later —
                // login/logout is an infrequent event, 60s polling is fine.
                tokio::time::sleep(std::time::Duration::from_secs(NO_SESSION_POLL_SECS)).await;
                continue;
            }
        };

        let sleep_secs = (expires_at - REFRESH_LEAD_SECS - now_unix()).max(0) as u64;
        if sleep_secs > 0 {
            tokio::time::sleep(std::time::Duration::from_secs(sleep_secs)).await;
        }

        
        // Re-read tokens right before the network call — the session may
        // have been rotated by a concurrent 401-retry in the meantime.
        let refresh_lock = {
            let s = state.read().await;
            s.refresh_lock.clone()
        };

        let _guard = refresh_lock.lock().await;
        let (access_token, refresh_token) = {
            let s = state.read().await;

            match s.session.as_ref() {
                Some(sess) => (sess.access_token.clone(), sess.refresh_token.clone()),
                None => continue,
            }
        };
        match auth::refresh_access_token(&conf, &access_token, &refresh_token).await {
            Ok(new_tokens) => {
                if let Err(e) = state_store::save_rotated_tokens(
                    &conf.workspace,
                    new_tokens.access_token.clone(),
                    new_tokens.refresh_token.clone(),
                    new_tokens.expires_at,
                ) {
                    // Server has already rotated but we could not persist —
                    // the new tokens are only in memory. In-memory update
                    // still happens below so the daemon keeps working; a
                    // crash before the next successful save loses them.
                    warn!(error = %e, "persist rotated tokens failed");
                }
                {
                    let mut s = state.write().await;
                    if let Some(sess) = s.session.as_mut() {
                        sess.access_token = new_tokens.access_token;
                        sess.refresh_token = new_tokens.refresh_token;
                        sess.expires_at = new_tokens.expires_at;
                    }
                }
                info!(
                    next_expiry = new_tokens.expires_at,
                    "session refreshed proactively"
                );
            }
            Err(auth::RefreshError::SessionDead) => {
                warn!("refresh session dead — user must sign in again; scheduler polling");
                tokio::time::sleep(std::time::Duration::from_secs(NO_SESSION_POLL_SECS)).await;
                continue;
            }
            Err(auth::RefreshError::Transient(e)) => {
                warn!(error = %e, "transient refresh failure; retry in {}s", TRANSIENT_RETRY_SECS);
                tokio::time::sleep(std::time::Duration::from_secs(TRANSIENT_RETRY_SECS)).await;
            }
        }
    }
}

/// Background ACL sync loop. The action-triggered paths (up / sync / resources)
/// only converge when the user does something; this task guarantees an idle
/// daemon with a long-lived tunnel still picks up policy changes within one
/// ACL_REFRESH_TTL_SECS interval.
///
/// Skips ticks when no session/device exists (pre-login or post-logout).
/// On version change it reuses the exact same path as the 
/// sync_acl_now + restart_tunnel_if_running. Transient fetch failures keep the
/// cached snapshot (fail-open on staleness, consistent with refresh_acl_if_needed).
async fn run_acl_sync_scheduler(
    state: SharedState,
    conf: config::ClientConf,
    tun_slot: TunSlot,
) {
    // Shared with net_stack (via handle_up): the data plane fires this when a
    // managed-resource relay transport fails, so we re-sync early rather than
    // waiting out the 60s tick.
    let resync = { state.read().await.relay_resync.clone() };

    let mut ticker =
        tokio::time::interval(std::time::Duration::from_secs(ACL_REFRESH_TTL_SECS as u64));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    // First tick completes immediately — consume it so we don't double-fetch
    // right after the startup fetch_and_store_acl.
    ticker.tick().await;

    // End time of the last early-resync burst; enforces RELAY_RESYNC_COOLDOWN so
    // a permanently-dead relay can't spin bursts back-to-back. None = never run.
    let mut last_burst_end: Option<tokio::time::Instant> = None;

    loop {
        tokio::select! {
            _ = ticker.tick() => {
                sync_and_restart_if_changed(&state, &conf, &tun_slot).await;
            }
            _ = resync.notified() => {
                // A relay transport just failed. Consume this wake; if we ran a
                // burst too recently, skip — the 60s tick remains the backstop.
                if let Some(end) = last_burst_end {
                    if end.elapsed() < std::time::Duration::from_secs(RELAY_RESYNC_COOLDOWN_SECS) {
                        continue;
                    }
                }
                // The connector may still be re-homing to a new relay, so the
                // controller's ACL may not carry it yet — retry with backoff until
                // the version changes (then restart), or attempts run out (fall
                // back to the steady tick). Notify coalesces bursts into one wake.
                info!("relay failure signalled — early ACL resync");
                let mut backoff = std::time::Duration::from_secs(RELAY_RESYNC_BASE_SECS);
                for _ in 0..RELAY_RESYNC_MAX_ATTEMPTS {
                    let has_identity = {
                        let s = state.read().await;
                        s.device.is_some() && s.session.is_some()
                    };
                    if !has_identity {
                        break;
                    }
                    match sync_acl_now(&state, &conf).await {
                        Ok(result) if result.changed => {
                            info!(
                                version = result.version,
                                "early ACL resync: version changed, restarting tunnel"
                            );
                            if let Err(e) = restart_tunnel_if_running(&state, &conf, &tun_slot).await {
                                warn!(error = %e, "early ACL resync: tunnel restart failed");
                            }
                            break;
                        }
                        Ok(_) => {
                            // Controller hasn't seen the connector re-home yet.
                            tokio::time::sleep(backoff).await;
                            backoff = (backoff * 2)
                                .min(std::time::Duration::from_secs(RELAY_RESYNC_MAX_SECS));
                        }
                        Err(e) => {
                            warn!(error = %e, "early ACL resync failed — keeping cached snapshot");
                            tokio::time::sleep(backoff).await;
                            backoff = (backoff * 2)
                                .min(std::time::Duration::from_secs(RELAY_RESYNC_MAX_SECS));
                        }
                    }
                }
                last_burst_end = Some(tokio::time::Instant::now());
            }
        }
    }
}

/// One steady-cadence ACL sync: fetch, and if the snapshot version changed,
/// restart the tunnel to rebuild transports. No-op when not logged in or when
/// the version is unchanged.
async fn sync_and_restart_if_changed(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) {
    let has_identity = {
        let s = state.read().await;
        s.device.is_some() && s.session.is_some()
    };
    if !has_identity {
        return;
    }
    match sync_acl_now(state, conf).await {
        Ok(result) if result.changed => {
            info!(
                version = result.version,
                "background ACL sync: version changed, restarting tunnel"
            );
            if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
                warn!(error = %e, "background ACL sync: tunnel restart failed");
            }
        }
        Ok(_) => {} // unchanged — quiet, this fires every 60s
        Err(e) => {
            warn!(error = %e, "background ACL sync failed — keeping cached snapshot");
        }
    }
}


/// Fetch and store the ACL snapshot. On failure, keeps the existing snapshot
/// (never reverts to None on a transient error). Access-token expiry is
/// handled transparently by fetch_acl_snapshot_with_refresh.
async fn fetch_and_store_acl(
    state: &SharedState,
    conf: &config::ClientConf,
    ca_pem: String,
    device_id: String,
) {
    match fetch_acl_snapshot_with_refresh(conf, &ca_pem, state, &device_id).await {
        Ok(snapshot) => {
            let version = snapshot.version;
            let synced_at = now_unix();
            let mut s = state.write().await;
            s.acl_snapshot = Some(snapshot);
            s.acl_last_sync_at = Some(synced_at);
            info!(version, "ACL snapshot stored");
        }
        Err(e) => {
            warn!(error = %e, "ACL snapshot fetch failed — default-deny in effect");
        }
    }
}

async fn refresh_acl_if_needed(
    state: &SharedState,
    conf: &config::ClientConf,
) -> Result<Option<AclSyncResult>> {
    let should_refresh = {
        let s = state.read().await;
        match s.acl_last_sync_at {
            Some(last) if s.acl_snapshot.is_some() => {
                now_unix().saturating_sub(last) >= ACL_REFRESH_TTL_SECS
            }
            _ => true,
        }
    };

    if !should_refresh {
        return Ok(None);
    }

    match sync_acl_now(state, conf).await {
        Ok(result) => Ok(Some(result)),
        Err(e) => {
            if state.read().await.acl_snapshot.is_some() {
                warn!(error = %e, "ACL refresh failed — using cached snapshot");
                Ok(None)
            } else {
                Err(e)
            }
        }
    }
}

async fn sync_acl_now(state: &SharedState, conf: &config::ClientConf) -> Result<AclSyncResult> {
    let (ca_pem, device_id) = {
        let s = state.read().await;
        let device = s.device.as_ref().ok_or_else(|| {
            anyhow::anyhow!("no device identity — run zecurity-client login first")
        })?;
        // Session presence is checked inside fetch_acl_snapshot_with_refresh —
        // we only need CA + device id here.
        (device.ca_cert_pem.clone(), device.id.clone())
    };
    let old_version = {
        let s = state.read().await;
        s.acl_snapshot.as_ref().map(|a| a.version)
    };
    let snapshot = fetch_acl_snapshot_with_refresh(conf, &ca_pem, state, &device_id).await?;

    let changed = match old_version {
        Some(v) => v != snapshot.version,
        None => true,
    };
    let result = AclSyncResult {
        version: snapshot.version,
        entry_count: snapshot.entries.len(),
        synced_at: now_unix(),
        changed,
    };

    let mut s = state.write().await;
    s.acl_snapshot = Some(snapshot);
    s.acl_last_sync_at = Some(result.synced_at);
    info!(
        version = result.version,
        entries = result.entry_count,
        "ACL snapshot synced"
    );
    Ok(result)
}

pub(crate) fn ordered_connectors_for_entry<'a>(
    entry: &AclEntry,
    rn: &'a AclRemoteNetwork,
) -> Vec<&'a AclConnector> {
    let mut ordered = Vec::new();
    let preferred = entry.preferred_connector_id.as_str();

    if !preferred.is_empty() {
        if let Some(connector) = rn
            .connectors
            .iter()
            .find(|connector| connector.connector_id == preferred)
        {
            ordered.push(connector);
        }
    }
    for connector in &rn.connectors {
        if connector.connector_id != preferred {
            ordered.push(connector);
        }
    }
    ordered
}

// Build a transport map keyed by (Ipv4Addr, port) for every ACL entry.
//
// Three cases at lookup time (enforced in net_stack):
//   Some(Some(t)) — managed resource, connector online  → tunnel via QUIC
//   Some(None)    — managed resource, connector offline → fail closed
//   None (absent) — unmanaged traffic, not in ACL       → no tunnel route
pub(crate) fn build_transports_by_resource(
    entries: &[AclEntry],
    remote_networks: &[AclRemoteNetwork],
    device: &DeviceInfo,
) -> Result<HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>> {
    // Build a remote_network_id lookup. Each entry picks its preferred connector
    // lazily so one bad unused connector does not poison every resource in the RN.
    let mut rn_by_id: HashMap<&str, &AclRemoteNetwork> = HashMap::new();
    for rn in remote_networks {
        rn_by_id.insert(rn.remote_network_id.as_str(), rn);
    }

    // Fan out: map each resource (Ipv4Addr, port) to its preferred connector's
    // transport when present, otherwise the RN fallback transport.
    // Resources in RNs absent from rn_by_id (shouldn't happen) are omitted.
    let mut out: HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>> = HashMap::new();
    let mut transport_cache: HashMap<String, Arc<ClientTransport>> = HashMap::new();
    for entry in entries {
        let Ok(ip) = entry.address.parse::<IpAddr>() else {
            continue;
        };
        let IpAddr::V4(v4) = ip else { continue };
        let Some(rn) = rn_by_id.get(entry.remote_network_id.as_str()) else {
            continue;
        };

        let mut transports = Vec::new();
        for connector in ordered_connectors_for_entry(entry, rn) {
            let cache_key = if connector.connector_id.is_empty() {
                format!(
                    "{}:{}",
                    rn.remote_network_id,
                    connector.connector_tunnel_addr.clone()
                )
            } else {
                connector.connector_id.clone()
            };
            let transport = match transport_cache.get(&cache_key) {
                Some(t) => t.clone(),
                None => {
                    let transport = build_transport_for_connector(connector, device)?;
                    transport_cache.insert(cache_key, transport.clone());
                    transport
                }
            };
            transports.push(transport);
        }
        let slot = if transports.is_empty() {
            None
        } else {
            Some(transports)
        };
        out.insert((v4, entry.port as u16), slot);
    }
    Ok(out)
}

fn build_transport_for_connector(
    connector: &AclConnector,
    device: &DeviceInfo,
) -> Result<Arc<ClientTransport>> {
    let connector_addr = if !connector.connector_tunnel_addr.is_empty() {
        connector.connector_tunnel_addr.clone()
    } else {
        info!(
            connector_addr = crate::appmeta::DEFAULT_CONNECTOR_ADDRESS.to_string(),
            "using default connector address"
        );
        crate::appmeta::DEFAULT_CONNECTOR_ADDRESS.to_string()
    };
    let connector_socket: SocketAddr = connector_addr
        .to_socket_addrs()
        .with_context(|| format!("resolve connector tunnel address {connector_addr}"))?
        .next()
        .with_context(|| {
            format!("connector tunnel address {connector_addr} resolved to no addresses")
        })?;

    let direct = Arc::new(TunnelPool::new(
        &device.certificate_pem,
        &device.private_key_pem,
        &device.ca_cert_pem,
    )?);

    // Relay coords are per-connector (ACLConnector fields 4+5).
    // Empty relay_addr means this connector has no relay assignment — direct only.
    let relay = if !connector.relay_addr.is_empty()
        && !connector.relay_spiffe_id.is_empty()
        && !connector.connector_id.is_empty()
        && !connector.connector_spiffe.is_empty()
    {
        let pool = Arc::new(RelayPool::new(
            &device.certificate_pem,
            &device.private_key_pem,
            &device.ca_cert_pem,
            &connector.relay_spiffe_id,
        )?);
        Some(RelayContext {
            pool,
            relay_addr: connector.relay_addr.clone(),
            connector_id: connector.connector_id.clone(),
            connector_spiffe: connector.connector_spiffe.clone(),
        })
    } else {
        None
    };

    Ok(Arc::new(ClientTransport::new(
        direct,
        connector_socket,
        relay,
    )))
}

fn now_unix() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}
