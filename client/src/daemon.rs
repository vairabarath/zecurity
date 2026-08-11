use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr, ToSocketAddrs};
use std::sync::Arc;

use anyhow::{Context, Result};
use futures::FutureExt;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;
use tokio::sync::Mutex;
use tracing::{error, info, warn};

use crate::auth;
use crate::config;
use crate::grpc::{
    self,
    client_v1::{
        AclConnector, AclEntry, AclRemoteNetwork, DevicePostureReport, GetAclSnapshotRequest,
        GetTransportSnapshotRequest, ReportDevicePostureRequest, TransportConnector,
        TransportRemoteNetwork, TransportSnapshot,
    },
};
use crate::posture;
use uuid::Uuid;
use crate::ipc::{check_same_user, ipc_socket_path, IpcRequest, IpcResource, IpcResponse};
use crate::login::LoginResult;
use crate::net_stack;
use crate::relay_pool::RelayPool;
use crate::runtime::{
    self, DeviceInfo, SessionInfo, SharedState, TunHandle, TunnelRestartCoordinator, UserInfo,
    WorkspaceInfo,
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

        // Fetch the transport snapshot too, so relay routing is available before
        // the first background tick (ADR-015 Track B).
        let state_clone = Arc::clone(&state);
        let conf_clone = conf.clone();
        tokio::spawn(async move {
            if let Err(e) = fetch_and_store_transport(&state_clone, &conf_clone).await {
                warn!(error = %e, "startup transport snapshot fetch failed");
            }
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
    // Posture collection loop. Sibling to run_refresh_scheduler — posture
    // cadence (fixed 5-minute interval) and token-refresh cadence are
    // different concerns, so this is its own task, not merged into either.
    {
        let state_clone = Arc::clone(&state);
        let conf_clone = conf.clone();
        tokio::spawn(async move {
            run_posture_scheduler(state_clone, conf_clone).await;
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
                    {
                        let s = state.read().await;
                        s.posture_resync.notify_one();
                    }
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

    // Require an ACL snapshot with at least one entry. The transport snapshot is
    // optional — routing falls back to the ACL relay fields when it's absent.
    let (acl, transport, device) = {
        let s = state.read().await;
        (
            s.acl_snapshot.clone(),
            s.transport_snapshot.clone(),
            s.device.clone(),
        )
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

    let relay_crl = {
        let mut runtime = state.write().await;
        runtime
            .relay_crl
            .get_or_insert_with(crate::crl::CrlManager::new)
            .clone()
    };
    let relay_crl_url = format!("{}/relay.crl", conf.http_base());
    if !relay_crl.has_valid_cache() {
        if let Err(error) = relay_crl
            .refresh(&relay_crl_url, device.ca_cert_pem.as_bytes())
            .await
        {
            warn!(%error, "initial Relay CRL fetch failed; relay routing is fail-closed");
        }
    }
    relay_crl.clone().spawn_refresh(
        relay_crl_url,
        device.ca_cert_pem.as_bytes().to_vec(),
        60,
        15,
    );

    let transports = match build_transports_by_resource_with_crl(
        &allowed_entries,
        &acl.remote_networks,
        transport.as_ref(),
        &device,
        relay_crl,
    ) {
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

/// Relay recovery, the 60s ACL tick, IPC sync/resources, and post-login can
/// each call this concurrently. Requests are coalesced via
/// `register_restart_request`: everyone queued before a restart pass starts
/// shares that pass's result; anyone arriving mid-pass is deferred to the
/// next one. This avoids one full down/up cycle per concurrent caller while
/// still giving every caller the correct pass's real result.
async fn restart_tunnel_if_running(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) -> Result<()> {
    let coordinator = { state.read().await.tunnel_restart.clone() };
    let state = state.clone();
    let conf = conf.clone();
    let tun_slot = tun_slot.clone();

    let work = move || {
        let state = state.clone();
        let conf = conf.clone();
        let tun_slot = tun_slot.clone();
        async move {
            perform_tunnel_restart(&state, &conf, &tun_slot)
                .await
                .map_err(|e| e.to_string())
        }
    };

    register_restart_request(&coordinator, work)
        .await
        .map_err(|e| anyhow::anyhow!(e))
}

/// One down→up restart pass. Extracted so `run_restart_worker` can drive it
/// through the queued-oneshot coordinator, and so tests can drive the
/// coordination logic itself against a fake `work` closure.
async fn perform_tunnel_restart(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) -> Result<()> {
    if tun_slot.lock().await.is_none() {
        return Ok(());
    }

    info!("snapshot changed, restarting VPN");

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

/// Runs restart passes for one `TunnelRestartCoordinator` until its `pending`
/// queue is drained. Each pass answers exactly the requests queued at the
/// moment it starts (`batch`); requests arriving mid-pass land in `pending`
/// and are picked up by the next loop iteration. On failure, both the
/// failing batch and everything queued during that failed pass are failed
/// together — a later pass re-checking tunnel state could otherwise
/// misreport success to requests that were actually waiting on the failure.
async fn run_restart_worker<F, Fut>(coordinator: Arc<tokio::sync::Mutex<TunnelRestartCoordinator>>, work: F)
where
    F: Fn() -> Fut + Send + 'static,
    Fut: std::future::Future<Output = std::result::Result<(), String>> + Send + 'static,
{
    loop {
        let batch = {
            let mut c = coordinator.lock().await;
            if c.pending.is_empty() {
                c.running = false;
                return;
            }
            std::mem::take(&mut c.pending)
        };

        let outcome = std::panic::AssertUnwindSafe(work()).catch_unwind().await;
        match outcome {
            Ok(Ok(())) => {
                for waiter in batch {
                    let _ = waiter.send(Ok(()));
                }
                // Loop: pick up any stragglers that arrived mid-pass.
            }
            Ok(Err(e)) => {
                // Stabilize coordinator state before publishing the failure.
                // Otherwise a waiter can observe Err, immediately enqueue a
                // fresh request while running is still true, and be incorrectly
                // drained as a straggler of this failed pass.
                let stragglers = {
                    let mut c = coordinator.lock().await;
                    c.running = false;
                    std::mem::take(&mut c.pending)
                };
                for waiter in batch {
                    let _ = waiter.send(Err(e.clone()));
                }
                for waiter in stragglers {
                    let _ = waiter.send(Err(e.clone()));
                }
                return;
            }
            Err(_) => {
                // Catch the panic while `batch` is still owned here. Reset the
                // coordinator before resolving any receiver so callers cannot
                // race a fresh request against stale running=true state.
                let error = "tunnel restart worker panicked".to_string();
                let stragglers = {
                    let mut c = coordinator.lock().await;
                    c.running = false;
                    std::mem::take(&mut c.pending)
                };
                for waiter in batch {
                    let _ = waiter.send(Err(error.clone()));
                }
                for waiter in stragglers {
                    let _ = waiter.send(Err(error.clone()));
                }
                return;
            }
        }
    }
}

/// Registers a restart request with the coordinator, starting a worker if
/// none is running, and waits for the pass that answers this request.
///
/// The worker is supervised with `catch_unwind` in the same spawned task
/// that runs it, so a panic mid-pass still resets `running` and fails any
/// queued waiters instead of leaving the coordinator permanently wedged.
/// This is best-effort panic recovery, not an absolute guarantee against
/// every possible abnormal termination (e.g. process exit) — nothing
/// currently retains the worker's `JoinHandle` to observe or abort it
/// externally, so an in-task panic is the realistic failure mode covered.
async fn register_restart_request<F, Fut>(
    coordinator: &Arc<tokio::sync::Mutex<TunnelRestartCoordinator>>,
    work: F,
) -> std::result::Result<(), String>
where
    F: Fn() -> Fut + Send + 'static,
    Fut: std::future::Future<Output = std::result::Result<(), String>> + Send + 'static,
{
    let (tx, rx) = tokio::sync::oneshot::channel();
    let start_worker = {
        let mut c = coordinator.lock().await;
        c.pending.push(tx);
        if c.running {
            false
        } else {
            c.running = true;
            true
        }
    };

    if start_worker {
        let coordinator = coordinator.clone();
        // The worker catches panics from the restart operation per pass while
        // it still owns that pass's batch. This outer supervisor is defensive
        // best-effort cleanup for an unexpected panic in coordinator logic.
        tokio::spawn(async move {
            let outcome = std::panic::AssertUnwindSafe(run_restart_worker(coordinator.clone(), work))
                .catch_unwind()
                .await;
            if outcome.is_err() {
                let stragglers = {
                    let mut c = coordinator.lock().await;
                    c.running = false;
                    std::mem::take(&mut c.pending)
                };
                for waiter in stragglers {
                    let _ = waiter.send(Err("tunnel restart worker panicked".into()));
                }
            }
        });
    }

    match rx.await {
        Ok(result) => result,
        Err(_) => Err("tunnel restart worker stopped unexpectedly".into()),
    }
}

#[cfg(test)]
mod tunnel_restart_coordinator_tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    fn new_coordinator() -> Arc<tokio::sync::Mutex<TunnelRestartCoordinator>> {
        Arc::new(tokio::sync::Mutex::new(TunnelRestartCoordinator::default()))
    }

    /// A controllable fake restart operation: counts calls, and only
    /// completes each call once the test explicitly releases it via the
    /// returned `Notify`, so tests can deterministically control pass
    /// timing instead of relying on real sleeps.
    struct FakeWork {
        calls: Arc<AtomicUsize>,
        release: Arc<tokio::sync::Notify>,
        result: Arc<std::sync::Mutex<std::result::Result<(), String>>>,
    }

    impl FakeWork {
        fn new() -> Self {
            Self {
                calls: Arc::new(AtomicUsize::new(0)),
                release: Arc::new(tokio::sync::Notify::new()),
                result: Arc::new(std::sync::Mutex::new(Ok(()))),
            }
        }

        fn set_result(&self, result: std::result::Result<(), String>) {
            *self.result.lock().unwrap() = result;
        }

        fn release(&self) {
            self.release.notify_one();
        }

        fn closure(&self) -> impl Fn() -> std::pin::Pin<Box<dyn std::future::Future<Output = std::result::Result<(), String>> + Send>> + 'static
        {
            let calls = self.calls.clone();
            let release = self.release.clone();
            let result = self.result.clone();
            move || {
                let calls = calls.clone();
                let release = release.clone();
                let result = result.clone();
                Box::pin(async move {
                    calls.fetch_add(1, Ordering::SeqCst);
                    release.notified().await;
                    result.lock().unwrap().clone()
                })
            }
        }
    }

    /// Registers one restart request as its own spawned task, so it is
    /// driven independently by the runtime (not just when the test task
    /// happens to poll it directly).
    fn spawn_request<F, Fut>(
        coordinator: &Arc<tokio::sync::Mutex<TunnelRestartCoordinator>>,
        work: F,
    ) -> tokio::task::JoinHandle<std::result::Result<(), String>>
    where
        F: Fn() -> Fut + Send + 'static,
        Fut: std::future::Future<Output = std::result::Result<(), String>> + Send + 'static,
    {
        let coordinator = coordinator.clone();
        tokio::spawn(async move { register_restart_request(&coordinator, work).await })
    }

    async fn wait_until(mut condition: impl FnMut() -> bool) {
        for _ in 0..1000 {
            if condition() {
                return;
            }
            tokio::task::yield_now().await;
        }
        panic!("condition never became true");
    }

    async fn wait_for_pending(
        coordinator: &Arc<tokio::sync::Mutex<TunnelRestartCoordinator>>,
        expected: usize,
    ) {
        for _ in 0..1000 {
            if coordinator.lock().await.pending.len() == expected {
                return;
            }
            tokio::task::yield_now().await;
        }
        panic!("pending queue never reached {expected}");
    }

    #[tokio::test]
    async fn concurrent_requests_before_a_pass_share_one_pass() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();

        // Pre-populate one batch under the coordinator lock so this assertion
        // does not depend on task scheduling between two registration calls.
        let (tx1, rx1) = tokio::sync::oneshot::channel();
        let (tx2, rx2) = tokio::sync::oneshot::channel();
        {
            let mut c = coordinator.lock().await;
            c.running = true;
            c.pending.extend([tx1, tx2]);
        }
        let worker = tokio::spawn(run_restart_worker(
            coordinator.clone(),
            fake.closure(),
        ));

        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1, "only one pass should have started");
        fake.release();

        assert!(rx1.await.unwrap().is_ok());
        assert!(rx2.await.unwrap().is_ok());
        worker.await.unwrap();
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1, "both requests must be answered by the same pass");
    }

    #[tokio::test]
    async fn request_arriving_mid_pass_is_answered_by_next_pass() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();

        let r1 = spawn_request(&coordinator, fake.closure());
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;

        // Register a second request while pass 1 is still in flight.
        let r2 = spawn_request(&coordinator, fake.closure());
        wait_for_pending(&coordinator, 1).await;
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1, "straggler must not join the in-flight pass");

        // Releasing pass 1 lets the worker loop and start pass 2 for the straggler.
        fake.release();
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 2).await;
        fake.release();

        assert!(r1.await.unwrap().is_ok());
        assert!(r2.await.unwrap().is_ok());
    }

    #[tokio::test]
    async fn failing_pass_fails_its_whole_batch() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();
        fake.set_result(Err("boom".into()));

        let (tx1, rx1) = tokio::sync::oneshot::channel();
        let (tx2, rx2) = tokio::sync::oneshot::channel();
        {
            let mut c = coordinator.lock().await;
            c.running = true;
            c.pending.extend([tx1, tx2]);
        }
        let worker = tokio::spawn(run_restart_worker(
            coordinator.clone(),
            fake.closure(),
        ));
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;
        fake.release();

        assert_eq!(rx1.await.unwrap(), Err("boom".to_string()));
        assert_eq!(rx2.await.unwrap(), Err("boom".to_string()));
        worker.await.unwrap();
    }

    #[tokio::test]
    async fn stragglers_during_a_failing_pass_are_also_failed() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();
        fake.set_result(Err("boom".into()));

        let r1 = spawn_request(&coordinator, fake.closure());
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;

        let r2 = spawn_request(&coordinator, fake.closure());
        wait_for_pending(&coordinator, 1).await;

        // Only one pass ever runs: it fails, and must drain+fail the
        // straggler too instead of letting a spurious pass 2 report success.
        fake.release();
        assert_eq!(r1.await.unwrap(), Err("boom".to_string()));
        assert_eq!(r2.await.unwrap(), Err("boom".to_string()));
        assert_eq!(fake.calls.load(Ordering::SeqCst), 1);
        assert!(!coordinator.lock().await.running, "worker must reset running on failure");
    }

    #[tokio::test]
    async fn fresh_request_after_failure_starts_a_new_worker() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();
        fake.set_result(Err("boom".into()));

        let r1 = spawn_request(&coordinator, fake.closure());
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;
        fake.release();
        assert_eq!(r1.await.unwrap(), Err("boom".to_string()));
        assert!(!coordinator.lock().await.running);

        let fake2 = FakeWork::new();
        let r2 = spawn_request(&coordinator, fake2.closure());
        wait_until(|| fake2.calls.load(Ordering::SeqCst) >= 1).await;
        fake2.release();
        assert!(r2.await.unwrap().is_ok());
    }

    #[tokio::test]
    async fn dropped_receiver_does_not_break_other_waiters_in_the_batch() {
        let coordinator = new_coordinator();
        let fake = FakeWork::new();

        // r1's task (and its oneshot receiver) will be aborted before the
        // pass completes; r2 must still get its result.
        let r1 = spawn_request(&coordinator, fake.closure());
        let r2 = spawn_request(&coordinator, fake.closure());
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;

        r1.abort();
        tokio::task::yield_now().await;
        fake.release();

        assert!(
            r2.await.unwrap().is_ok(),
            "surviving waiter in the same batch must still get its result"
        );
    }

    #[tokio::test]
    async fn panic_mid_pass_resets_running_and_fails_queued_waiters() {
        let coordinator = new_coordinator();
        let calls = Arc::new(AtomicUsize::new(0));

        let work_calls = calls.clone();
        let work = move || {
            let work_calls = work_calls.clone();
            Box::pin(async move {
                work_calls.fetch_add(1, Ordering::SeqCst);
                panic!("simulated restart panic");
                #[allow(unreachable_code)]
                Ok(())
            }) as std::pin::Pin<Box<dyn std::future::Future<Output = std::result::Result<(), String>> + Send>>
        };

        let res = register_restart_request(&coordinator, work).await;
        assert_eq!(
            res,
            Err("tunnel restart worker panicked".to_string()),
            "a panicked pass must surface its explicit error, not a closed channel"
        );
        assert!(
            !coordinator.lock().await.running,
            "panic recovery must reset running so a later request can start a fresh worker"
        );
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        // Confirm the coordinator is usable again after the panic.
        let fake = FakeWork::new();
        let r2 = spawn_request(&coordinator, fake.closure());
        wait_until(|| fake.calls.load(Ordering::SeqCst) >= 1).await;
        fake.release();
        assert!(r2.await.unwrap().is_ok());
    }
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

/// Raw GetACLSnapshot RPC. Returns Ok(None) when the controller reports
/// up_to_date (client's known_version matches — keep the cached snapshot).
async fn fetch_acl_snapshot(
    conf: &config::ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
    known_version: u64,
) -> Result<Option<crate::grpc::client_v1::AclSnapshot>> {
    let mut client = grpc::connect_grpc(conf.controller(), ca_pem).await?;
    let resp = client
        .get_acl_snapshot(GetAclSnapshotRequest {
            access_token: access_token.to_string(),
            device_id: device_id.to_string(),
            known_version,
        })
        .await?
        .into_inner();
    if resp.up_to_date {
        return Ok(None);
    }
    resp.snapshot
        .ok_or_else(|| anyhow::anyhow!("controller returned empty ACL snapshot"))
        .map(Some)
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
    known_version: u64,
) -> Result<Option<crate::grpc::client_v1::AclSnapshot>> {
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

    match fetch_acl_snapshot(conf, ca_pem, &access_token, device_id, known_version).await {
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

            match fetch_acl_snapshot(conf, ca_pem, &access_token, device_id, known_version).await {
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

            fetch_acl_snapshot(conf, ca_pem, &new_tokens.access_token, device_id, known_version)
                .await
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

/// Single-flight + cooldown state for the transport-recovery task, shared
/// between the scheduler's select! loop and the spawned recovery task.
#[derive(Debug, Default)]
struct TransportRecoveryState {
    /// True while a recovery task is in flight.
    running: bool,
    /// When the last recovery completed. The cooldown is measured from here (not
    /// from when recovery started) so a long recovery can't immediately re-burst.
    last_finished_at: Option<tokio::time::Instant>,
}

/// Decide whether a new transport-recovery burst may start, and reserve the slot
/// if so. Pure so it is unit-testable without spawning tasks or mocking time:
/// returns false when a recovery is already running or the cooldown since the
/// last completion has not elapsed; otherwise marks `running` and returns true.
fn may_start_transport_recovery(
    recovery: &mut TransportRecoveryState,
    now: tokio::time::Instant,
    cooldown: std::time::Duration,
) -> bool {
    if recovery.running {
        return false;
    }
    if recovery
        .last_finished_at
        .is_some_and(|finished| now.duration_since(finished) < cooldown)
    {
        return false;
    }
    recovery.running = true;
    true
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
async fn run_acl_sync_scheduler(state: SharedState, conf: config::ClientConf, tun_slot: TunSlot) {
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

    // Single-flight + cooldown state for transport recovery. The recovery task
    // runs off this select! thread so the 60s ACL tick keeps firing during a
    // relay recovery (revocation polling must not stall). The cooldown is
    // measured from completion (see TransportRecoveryState) so a permanently-dead
    // relay can't spin bursts back-to-back.
    let recovery_state =
        std::sync::Arc::new(tokio::sync::Mutex::new(TransportRecoveryState::default()));

    loop {
        tokio::select! {
            _ = ticker.tick() => {
                sync_and_restart_if_changed(&state, &conf, &tun_slot).await;
            }
            _ = resync.notified() => {
                // A relay transport just failed. The relay change lives in the
                // TRANSPORT snapshot (Track B), so re-poll transport — not ACL.
                // Single check-and-set under one lock: skip if a recovery is
                // already running, or if the cooldown since the last completion
                // hasn't elapsed (the 60s tick remains the backstop).
                let should_start = {
                    let mut recovery = recovery_state.lock().await;
                    may_start_transport_recovery(
                        &mut recovery,
                        tokio::time::Instant::now(),
                        std::time::Duration::from_secs(RELAY_RESYNC_COOLDOWN_SECS),
                    )
                };
                if !should_start {
                    continue;
                }

                // Run the retry-until-changed loop as a SEPARATE task so this
                // select! keeps servicing the ACL tick during relay recovery.
                info!("relay failure signalled — early transport resync (background)");
                let state = state.clone();
                let conf = conf.clone();
                let tun_slot = tun_slot.clone();
                let recovery_state = recovery_state.clone();
                tokio::spawn(async move {
                    run_transport_recovery(&state, &conf, &tun_slot).await;
                    // Mark complete and stamp the finish time so the cooldown is
                    // measured from completion, not from when recovery started.
                    let mut recovery = recovery_state.lock().await;
                    recovery.running = false;
                    recovery.last_finished_at = Some(tokio::time::Instant::now());
                });
            }
        }
    }
}
/// Posture collection + submission loop. Sibling to run_refresh_scheduler and
/// run_acl_sync_scheduler — posture cadence is a fixed 5-minute interval, a
/// different concern from token-refresh or ACL-poll cadence.
///
/// Lifecycle:
///   - No session in state (fresh daemon before login, or post-logout): sleep
///     and recheck — same "not logged in yet" handling as run_refresh_scheduler.
///   - PostLoginState signals `posture_resync` right after a successful login,
///     so a freshly-logged-in device reports posture immediately rather than
///     waiting for the first 5-minute tick.
///   - Unlike run_acl_sync_scheduler, this does NOT pre-consume the ticker's
///     immediate first tick — there is no separate startup-time posture fetch
///     elsewhere (unlike ACL's fetch_and_store_acl spawned in run()), so the
///     interval's natural immediate first tick IS the "collect at startup"
///     trigger the spec asks for.
async fn run_posture_scheduler(state: SharedState, conf: config::ClientConf) {
    const POSTURE_COLLECT_INTERVAL_SECS: u64 = 300;
    const NO_SESSION_POLL_SECS: u64 = 60;

    let resync = { state.read().await.posture_resync.clone() };

    let mut ticker =
        tokio::time::interval(std::time::Duration::from_secs(POSTURE_COLLECT_INTERVAL_SECS));
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    loop {
        let has_session = { state.read().await.session.is_some() };
        if !has_session {
            tokio::time::sleep(std::time::Duration::from_secs(NO_SESSION_POLL_SECS)).await;
            continue;
        }

        tokio::select! {
            _ = ticker.tick() => {}
            _ = resync.notified() => {}
        }

        collect_and_submit_posture(&state, &conf).await;
    }
}

/// Collects one posture report via the real gRPC submitter. Splitting
/// collection (here) from assembly+retry (submit_with_retry) lets tests
/// inject a fake submitter and drive the retry path without a live
/// controller or real system posture collection.
async fn collect_and_submit_posture(state: &SharedState, conf: &config::ClientConf) {
    let (ca_pem, device_id) = {
        let s = state.read().await;
        match s.device.as_ref() {
            Some(d) => (d.ca_cert_pem.clone(), d.id.clone()),
            None => return,
        }
    };

    let collected = posture::collect().await;
    let submitter = GrpcPostureSubmitter {
        state,
        conf,
        ca_pem: &ca_pem,
        device_id: &device_id,
    };
    submit_with_retry(&submitter, collected).await;
}

/// Abstracts "submit this assembled report" so the retry-with-same-report_id
/// logic in submit_with_retry can be unit-tested without a live controller —
/// mirrors the AclFetcher/TransportFetcher pattern used by sync_acl_now_with /
/// fetch_and_store_transport_with.
#[async_trait::async_trait]
trait PostureSubmitter {
    async fn submit(&self, report: DevicePostureReport) -> Result<()>;
}

/// Real submitter: submits via the gRPC RPC, transparently refreshing the
/// access token on Unauthenticated (see submit_posture_report).
struct GrpcPostureSubmitter<'a> {
    state: &'a SharedState,
    conf: &'a config::ClientConf,
    ca_pem: &'a str,
    device_id: &'a str,
}

#[async_trait::async_trait]
impl PostureSubmitter for GrpcPostureSubmitter<'_> {
    async fn submit(&self, report: DevicePostureReport) -> Result<()> {
        submit_posture_report(self.conf, self.ca_pem, self.state, self.device_id, report).await
    }
}

/// Assembles the DevicePostureReport (one fresh report_id for this cycle) and
/// submits it, retrying once with the SAME report_id on failure so the
/// controller's UNIQUE(report_id) constraint makes the retry idempotent
/// rather than creating a duplicate row. Gives up after the retry — the next
/// scheduled tick collects fresh data anyway.
async fn submit_with_retry(submitter: &impl PostureSubmitter, collected: posture::PostureReport) {
    let report = DevicePostureReport {
        report_id: Uuid::new_v4().to_string(),
        reported_at: now_unix(),
        client_version: env!("CARGO_PKG_VERSION").to_string(),
        os_name: collected.os_name,
        os_version: collected.os_version,
        checks: collected.checks,
    };

    if let Err(e) = submitter.submit(report.clone()).await {
        warn!(error = %e, "posture submission failed, retrying once with same report_id");
        if let Err(e) = submitter.submit(report).await {
            warn!(error = %e, "posture submission retry failed, will try again next cycle");
        }
    }
}

/// Submit a device posture report, transparently refreshing the access token
/// on Unauthenticated — same shape as fetch_acl_snapshot_with_refresh (see
/// its doc comment for the full flow this mirrors).
async fn submit_posture_report(
    conf: &config::ClientConf,
    ca_pem: &str,
    state: &SharedState,
    device_id: &str,
    report: DevicePostureReport,
) -> Result<()> {
    let refresh_lock = { state.read().await.refresh_lock.clone() };
    let (access_token, _refresh_token) = {
        let s = state.read().await;
        let sess = s.session.as_ref().ok_or_else(|| {
            anyhow::anyhow!("no session in state — run zecurity-client login first")
        })?;
        (sess.access_token.clone(), sess.refresh_token.clone())
    };

    match report_device_posture(conf, ca_pem, &access_token, device_id, report.clone()).await {
        Ok(()) => Ok(()),
        Err(err) => {
            if !is_grpc_unauthenticated(&err) {
                return Err(err);
            }
            info!("posture submit returned Unauthenticated; refreshing session");
            let _guard = refresh_lock.lock().await;

            let (access_token, refresh_token) = {
                let s = state.read().await;
                let sess = s
                    .session
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("no session in state"))?;
                (sess.access_token.clone(), sess.refresh_token.clone())
            };

            match report_device_posture(conf, ca_pem, &access_token, device_id, report.clone())
                .await
            {
                Ok(()) => return Ok(()),
                Err(err) if is_grpc_unauthenticated(&err) => {
                    // Still expired. Continue to refresh below.
                }
                Err(err) => return Err(err),
            }

            let new_tokens = auth::refresh_access_token(conf, &access_token, &refresh_token)
                .await
                .map_err(|e| match e {
                    auth::RefreshError::SessionDead => {
                        anyhow::anyhow!("session expired; re-login required")
                    }
                    auth::RefreshError::Transient(inner) => inner.context("refresh access token"),
                })?;

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

            report_device_posture(conf, ca_pem, &new_tokens.access_token, device_id, report).await
        }
    }
}

/// Raw ReportDevicePosture RPC.
async fn report_device_posture(
    conf: &config::ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
    report: DevicePostureReport,
) -> Result<()> {
    let mut client = grpc::connect_grpc(conf.controller(), ca_pem).await?;
    let resp = client
        .report_device_posture(ReportDevicePostureRequest {
            access_token: access_token.to_string(),
            device_id: device_id.to_string(),
            report: Some(report),
        })
        .await?
        .into_inner();
    if !resp.accepted {
        anyhow::bail!(
            "controller rejected posture report: {}",
            resp.rejection_reason
        );
    }
    Ok(())
}
/// Early transport re-poll after a relay failure, run as its own task so it
/// never blocks the ACL sync tick (revocations keep polling). Retries with
/// backoff until the transport version changes (then restarts the tunnel to
/// adopt the new relay) or attempts are exhausted — the 60s tick is the backstop.
async fn run_transport_recovery(
    state: &SharedState,
    conf: &config::ClientConf,
    tun_slot: &TunSlot,
) {
    let mut backoff = std::time::Duration::from_secs(RELAY_RESYNC_BASE_SECS);
    for _ in 0..RELAY_RESYNC_MAX_ATTEMPTS {
        let has_identity = {
            let s = state.read().await;
            s.device.is_some() && s.session.is_some()
        };
        if !has_identity {
            break;
        }
        match fetch_and_store_transport(state, conf).await {
            Ok(true) => {
                info!("early transport resync: version changed, restarting tunnel");
                if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
                    warn!(error = %e, "early transport resync: tunnel restart failed");
                }
                break;
            }
            Ok(false) => {
                // Controller hasn't seen the connector re-home yet.
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(std::time::Duration::from_secs(RELAY_RESYNC_MAX_SECS));
            }
            Err(e) => {
                warn!(error = %e, "early transport resync failed — keeping cached snapshot");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(std::time::Duration::from_secs(RELAY_RESYNC_MAX_SECS));
            }
        }
    }
}

/// One steady-cadence sync of BOTH planes: refresh the ACL and the transport
/// snapshot; if either version changed, restart the tunnel to rebuild
/// transports (which read both planes). No-op when not logged in.
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

    let acl_changed = match sync_acl_now(state, conf).await {
        Ok(result) => result.changed,
        Err(e) => {
            warn!(error = %e, "background ACL sync failed — keeping cached snapshot");
            false
        }
    };
    let transport_changed = match fetch_and_store_transport(state, conf).await {
        Ok(changed) => changed,
        Err(e) => {
            warn!(error = %e, "background transport sync failed — keeping cached snapshot");
            false
        }
    };

    if acl_changed || transport_changed {
        info!(
            acl_changed,
            transport_changed, "background sync: version changed, restarting tunnel"
        );
        if let Err(e) = restart_tunnel_if_running(state, conf, tun_slot).await {
            warn!(error = %e, "background sync: tunnel restart failed");
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
    // known_version = 0: startup has no cached snapshot, so always request the
    // full ACL (the controller never reports up_to_date for known_version == 0).
    match fetch_acl_snapshot_with_refresh(conf, &ca_pem, state, &device_id, 0).await {
        Ok(Some(snapshot)) => {
            let version = snapshot.version;
            let synced_at = now_unix();
            let mut s = state.write().await;
            s.acl_snapshot = Some(snapshot);
            s.acl_last_sync_at = Some(synced_at);
            info!(version, "ACL snapshot stored");
        }
        Ok(None) => {
            // Contract violation: up_to_date for known_version == 0. Keep whatever
            // we have rather than acting on an empty response.
            warn!("ACL fetch reported up_to_date for known_version=0 — unexpected; keeping cached snapshot");
        }
        Err(e) => {
            warn!(error = %e, "ACL snapshot fetch failed — default-deny in effect");
        }
    }
}

// ── Transport plane (ADR-015 Track B) ───────────────────────────────────────

/// Raw GetTransportSnapshot RPC. Returns Ok(None) when the controller reports
/// up_to_date (client's known_version matches — keep the cached snapshot).
async fn fetch_transport_snapshot(
    conf: &config::ClientConf,
    ca_pem: &str,
    access_token: &str,
    device_id: &str,
    known_version: u64,
) -> Result<Option<TransportSnapshot>> {
    let mut client = grpc::connect_grpc(conf.controller(), ca_pem).await?;
    let resp = client
        .get_transport_snapshot(GetTransportSnapshotRequest {
            access_token: access_token.to_string(),
            device_id: device_id.to_string(),
            known_version,
        })
        .await?
        .into_inner();
    if resp.up_to_date {
        return Ok(None);
    }
    resp.snapshot
        .ok_or_else(|| anyhow::anyhow!("controller returned empty transport snapshot"))
        .map(Some)
}

/// Fetch the transport snapshot, transparently refreshing the access token on
/// Unauthenticated — mirrors fetch_acl_snapshot_with_refresh. Ok(None) means
/// up_to_date (keep cached).
async fn fetch_transport_snapshot_with_refresh(
    conf: &config::ClientConf,
    ca_pem: &str,
    state: &SharedState,
    device_id: &str,
    known_version: u64,
) -> Result<Option<TransportSnapshot>> {
    let refresh_lock = {
        let s = state.read().await;
        s.refresh_lock.clone()
    };
    let access_token = {
        let s = state.read().await;
        let sess = s.session.as_ref().ok_or_else(|| {
            anyhow::anyhow!("no session in state — run zecurity-client login first")
        })?;
        sess.access_token.clone()
    };

    match fetch_transport_snapshot(conf, ca_pem, &access_token, device_id, known_version).await {
        Ok(snap) => Ok(snap),
        Err(err) => {
            if !is_grpc_unauthenticated(&err) {
                return Err(err);
            }
            info!("transport fetch returned Unauthenticated; refreshing session");
            let _guard = refresh_lock.lock().await;

            let (access_token, refresh_token) = {
                let s = state.read().await;
                let sess = s
                    .session
                    .as_ref()
                    .ok_or_else(|| anyhow::anyhow!("no session in state"))?;
                (sess.access_token.clone(), sess.refresh_token.clone())
            };

            // Another task may have refreshed while we waited for the lock.
            match fetch_transport_snapshot(conf, ca_pem, &access_token, device_id, known_version)
                .await
            {
                Ok(snap) => return Ok(snap),
                Err(err) if is_grpc_unauthenticated(&err) => {}
                Err(err) => return Err(err),
            }

            let new_tokens = auth::refresh_access_token(conf, &access_token, &refresh_token)
                .await
                .map_err(|e| match e {
                    auth::RefreshError::SessionDead => {
                        anyhow::anyhow!("session expired; re-login required")
                    }
                    auth::RefreshError::Transient(inner) => inner.context("refresh access token"),
                })?;

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

            fetch_transport_snapshot(
                conf,
                ca_pem,
                &new_tokens.access_token,
                device_id,
                known_version,
            )
            .await
        }
    }
}

/// Abstracts the transport-snapshot fetch (given the client's known_version)
/// so the store/serialization logic in fetch_and_store_transport_with can be
/// unit-tested without a live controller. Ok(None) means up_to_date.
#[async_trait::async_trait]
trait TransportFetcher {
    async fn fetch(&self, known_version: u64) -> Result<Option<TransportSnapshot>>;
}

/// Real fetcher: reads the device identity from state and calls the gRPC RPC.
struct GrpcTransportFetcher<'a> {
    state: &'a SharedState,
    conf: &'a config::ClientConf,
}

#[async_trait::async_trait]
impl TransportFetcher for GrpcTransportFetcher<'_> {
    async fn fetch(&self, known_version: u64) -> Result<Option<TransportSnapshot>> {
        let (ca_pem, device_id) = {
            let s = self.state.read().await;
            let device = s
                .device
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("no device identity"))?;
            (device.ca_cert_pem.clone(), device.id.clone())
        };
        fetch_transport_snapshot_with_refresh(
            self.conf,
            &ca_pem,
            self.state,
            &device_id,
            known_version,
        )
        .await
    }
}

/// Fetch and store the transport snapshot. Uses known_version so an unchanged
/// snapshot returns up_to_date (no re-store). On failure keeps the cached
/// snapshot. Returns true when the stored version changed.
async fn fetch_and_store_transport(state: &SharedState, conf: &config::ClientConf) -> Result<bool> {
    fetch_and_store_transport_with(state, &GrpcTransportFetcher { state, conf }).await
}

/// Inner: holds transport_sync_lock across the whole known-version → fetch →
/// store sequence. Split out so tests can inject a fetcher and exercise both the
/// up_to_date path and the serialization guarantee.
///
/// Serialization matters: the 60s scheduler tick and the spawned relay-recovery
/// task can both call this at once; because the state lock is dropped during the
/// network fetch, without the sync lock an older response could land last and
/// move transport_snapshot backwards (stale relay coords). Holding the guard for
/// the whole operation makes a second caller read a fresh known_version only
/// after the first has stored, so versions progress monotonically.
async fn fetch_and_store_transport_with(
    state: &SharedState,
    fetcher: &impl TransportFetcher,
) -> Result<bool> {
    let sync_lock = { state.read().await.transport_sync_lock.clone() };
    let _sync_guard = sync_lock.lock().await;

    let known_version = {
        let s = state.read().await;
        s.transport_snapshot.as_ref().map(|t| t.version).unwrap_or(0)
    };

    match fetcher.fetch(known_version).await? {
        None => {
            // up_to_date — refresh the sync timestamp, keep the cached snapshot.
            let mut s = state.write().await;
            s.transport_last_sync_at = Some(now_unix());
            Ok(false)
        }
        Some(snapshot) => {
            let version = snapshot.version;
            let changed = known_version != version;
            let mut s = state.write().await;
            s.transport_snapshot = Some(snapshot);
            s.transport_last_sync_at = Some(now_unix());
            info!(version, "transport snapshot stored");
            Ok(changed)
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

/// Abstracts the ACL-snapshot fetch (given the client's known_version) so the
/// version-comparison / up_to_date logic in sync_acl_now_with can be unit-tested
/// without a live controller. Ok(None) means up_to_date.
#[async_trait::async_trait]
trait AclFetcher {
    async fn fetch(&self, known_version: u64) -> Result<Option<crate::grpc::client_v1::AclSnapshot>>;
}

/// Real fetcher: reads the device identity from state and calls the gRPC RPC.
struct GrpcAclFetcher<'a> {
    state: &'a SharedState,
    conf: &'a config::ClientConf,
}

#[async_trait::async_trait]
impl AclFetcher for GrpcAclFetcher<'_> {
    async fn fetch(&self, known_version: u64) -> Result<Option<crate::grpc::client_v1::AclSnapshot>> {
        let (ca_pem, device_id) = {
            let s = self.state.read().await;
            let device = s.device.as_ref().ok_or_else(|| {
                anyhow::anyhow!("no device identity — run zecurity-client login first")
            })?;
            (device.ca_cert_pem.clone(), device.id.clone())
        };
        fetch_acl_snapshot_with_refresh(self.conf, &ca_pem, self.state, &device_id, known_version)
            .await
    }
}

async fn sync_acl_now(state: &SharedState, conf: &config::ClientConf) -> Result<AclSyncResult> {
    sync_acl_now_with(state, &GrpcAclFetcher { state, conf }).await
}

/// Inner: reads the cached version, fetches, and applies the result. Split out so
/// tests can drive the up_to_date (keep cached, changed=false) and changed paths
/// without a controller.
async fn sync_acl_now_with(
    state: &SharedState,
    fetcher: &impl AclFetcher,
) -> Result<AclSyncResult> {
    let old_version = {
        let s = state.read().await;
        s.acl_snapshot.as_ref().map(|a| a.version)
    };
    let snapshot = fetcher.fetch(old_version.unwrap_or(0)).await?;

    match snapshot {
        // up_to_date — the controller confirms our cached version is current.
        // This branch is only reachable when we sent a non-zero known_version,
        // which requires a cached snapshot, so acl_snapshot is guaranteed Some.
        None => {
            let synced_at = now_unix();
            let mut s = state.write().await;
            s.acl_last_sync_at = Some(synced_at);
            let cached = s.acl_snapshot.as_ref().ok_or_else(|| {
                anyhow::anyhow!("controller reported ACL up_to_date but no cached snapshot")
            })?;
            let result = AclSyncResult {
                version: cached.version,
                entry_count: cached.entries.len(),
                synced_at,
                changed: false,
            };
            info!(version = result.version, "ACL up_to_date — kept cached snapshot");
            Ok(result)
        }
        Some(snapshot) => {
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
    }
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
// ConnCoords is a connector's connectivity (tunnel + relay) coordinates,
// sourced from either the transport snapshot (Track B, preferred) or the ACL's
// transitional relay fields (fallback). ACLConnector and TransportConnector
// carry identical fields.
pub(crate) struct ConnCoords {
    pub(crate) connector_id: String,
    pub(crate) connector_tunnel_addr: String,
    pub(crate) connector_spiffe: String,
    pub(crate) relay_addr: String,
    pub(crate) relay_spiffe_id: String,
}

fn coords_from_acl(c: &AclConnector) -> ConnCoords {
    ConnCoords {
        connector_id: c.connector_id.clone(),
        connector_tunnel_addr: c.connector_tunnel_addr.clone(),
        connector_spiffe: c.connector_spiffe.clone(),
        relay_addr: c.relay_addr.clone(),
        relay_spiffe_id: c.relay_spiffe_id.clone(),
    }
}

fn coords_from_transport(c: &TransportConnector) -> ConnCoords {
    ConnCoords {
        connector_id: c.connector_id.clone(),
        connector_tunnel_addr: c.connector_tunnel_addr.clone(),
        connector_spiffe: c.connector_spiffe.clone(),
        relay_addr: c.relay_addr.clone(),
        relay_spiffe_id: c.relay_spiffe_id.clone(),
    }
}

// ordered_transport_connectors_for_entry mirrors ordered_connectors_for_entry
// for the transport plane: the ACL entry's preferred connector first.
fn ordered_transport_connectors_for_entry<'a>(
    entry: &AclEntry,
    rn: &'a TransportRemoteNetwork,
) -> Vec<&'a TransportConnector> {
    let mut ordered = Vec::new();
    let preferred = entry.preferred_connector_id.as_str();
    if !preferred.is_empty() {
        if let Some(c) = rn.connectors.iter().find(|c| c.connector_id == preferred) {
            ordered.push(c);
        }
    }
    for c in &rn.connectors {
        if c.connector_id != preferred {
            ordered.push(c);
        }
    }
    ordered
}

// resolve_entry_coords picks a connector's connectivity coordinates for an ACL
// entry: from the transport plane when that entry's remote_network_id is present
// there (Track B, preferred), otherwise falling back per-RN to the ACL's
// transitional relay fields. Returns empty when neither plane has the RN. Pure
// (no cert/network work) so the routing decision is unit-testable.
pub(crate) fn resolve_entry_coords(
    entry: &AclEntry,
    rn_by_id: &HashMap<&str, &AclRemoteNetwork>,
    trn_by_id: &HashMap<&str, &TransportRemoteNetwork>,
) -> Vec<ConnCoords> {
    match trn_by_id.get(entry.remote_network_id.as_str()) {
        Some(trn) => ordered_transport_connectors_for_entry(entry, trn)
            .into_iter()
            .map(coords_from_transport)
            .collect(),
        None => match rn_by_id.get(entry.remote_network_id.as_str()) {
            Some(rn) => ordered_connectors_for_entry(entry, rn)
                .into_iter()
                .map(coords_from_acl)
                .collect(),
            None => Vec::new(),
        },
    }
}

// Build a transport map keyed by (Ipv4Addr, port) for every ACL entry.
//
// Authorization comes from the ACL (which entries exist, their remote_network).
// Connectivity (tunnel + relay coords) comes from the transport snapshot keyed
// by remote_network_id when present; otherwise it falls back per-RN to the ACL's
// transitional relay fields (ACLConnector 4+5) so rollout is non-breaking.
//
// Three cases at lookup time (enforced in net_stack):
//   Some(Some(t)) — managed resource, connector online  → tunnel via QUIC
//   Some(None)    — managed resource, connector offline → fail closed
//   None (absent) — unmanaged traffic, not in ACL       → no tunnel route
#[cfg(test)]
pub(crate) fn build_transports_by_resource(
    entries: &[AclEntry],
    remote_networks: &[AclRemoteNetwork],
    transport: Option<&TransportSnapshot>,
    device: &DeviceInfo,
) -> Result<HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>> {
    build_transports_by_resource_with_crl(
        entries,
        remote_networks,
        transport,
        device,
        crate::crl::CrlManager::new(),
    )
}

fn build_transports_by_resource_with_crl(
    entries: &[AclEntry],
    remote_networks: &[AclRemoteNetwork],
    transport: Option<&TransportSnapshot>,
    device: &DeviceInfo,
    relay_crl: crate::crl::CrlManager,
) -> Result<HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>> {
    let mut rn_by_id: HashMap<&str, &AclRemoteNetwork> = HashMap::new();
    for rn in remote_networks {
        rn_by_id.insert(rn.remote_network_id.as_str(), rn);
    }
    // Transport plane, keyed by remote_network_id. Empty/absent for an RN →
    // fall back to the ACL relay fields for that RN.
    let mut trn_by_id: HashMap<&str, &TransportRemoteNetwork> = HashMap::new();
    if let Some(t) = transport {
        for trn in &t.remote_networks {
            trn_by_id.insert(trn.remote_network_id.as_str(), trn);
        }
    }

    let mut out: HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>> = HashMap::new();
    let mut transport_cache: HashMap<String, Arc<ClientTransport>> = HashMap::new();
    for entry in entries {
        let Ok(ip) = entry.address.parse::<IpAddr>() else {
            continue;
        };
        let IpAddr::V4(v4) = ip else { continue };

        let coords = resolve_entry_coords(entry, &rn_by_id, &trn_by_id);

        let mut transports = Vec::new();
        for c in &coords {
            let cache_key = if c.connector_id.is_empty() {
                format!("{}:{}", entry.remote_network_id, c.connector_tunnel_addr)
            } else {
                c.connector_id.clone()
            };
            let transport = match transport_cache.get(&cache_key) {
                Some(t) => t.clone(),
                None => {
                    let t = build_transport_from_coords(c, device, relay_crl.clone())?;
                    transport_cache.insert(cache_key, t.clone());
                    t
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

fn build_transport_from_coords(
    c: &ConnCoords,
    device: &DeviceInfo,
    relay_crl: crate::crl::CrlManager,
) -> Result<Arc<ClientTransport>> {
    let connector_addr = if !c.connector_tunnel_addr.is_empty() {
        c.connector_tunnel_addr.clone()
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

    // Empty relay coords → this connector has no relay assignment (direct only).
    let relay = if !c.relay_addr.is_empty()
        && !c.relay_spiffe_id.is_empty()
        && !c.connector_id.is_empty()
        && !c.connector_spiffe.is_empty()
    {
        let pool = Arc::new(RelayPool::new(
            &device.certificate_pem,
            &device.private_key_pem,
            &device.ca_cert_pem,
            &c.relay_spiffe_id,
            relay_crl,
        )?);
        Some(RelayContext {
            pool,
            relay_addr: c.relay_addr.clone(),
            connector_id: c.connector_id.clone(),
            connector_spiffe: c.connector_spiffe.clone(),
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

#[cfg(test)]
mod recovery_tests {
    use super::*;

    #[test]
    fn recovery_is_single_flight() {
        let now = tokio::time::Instant::now();
        let mut state = TransportRecoveryState::default();

        // First call reserves the slot.
        assert!(may_start_transport_recovery(
            &mut state,
            now,
            std::time::Duration::from_secs(10),
        ));
        // Second call while running is refused.
        assert!(!may_start_transport_recovery(
            &mut state,
            now,
            std::time::Duration::from_secs(10),
        ));
    }

    #[test]
    fn cooldown_is_measured_after_recovery_finishes() {
        let finished = tokio::time::Instant::now();
        let mut state = TransportRecoveryState {
            running: false,
            last_finished_at: Some(finished),
        };

        // 9s after completion: still inside the 10s cooldown → refused.
        assert!(!may_start_transport_recovery(
            &mut state,
            finished + std::time::Duration::from_secs(9),
            std::time::Duration::from_secs(10),
        ));
        // 10s after completion: cooldown elapsed → allowed.
        assert!(may_start_transport_recovery(
            &mut state,
            finished + std::time::Duration::from_secs(10),
            std::time::Duration::from_secs(10),
        ));
    }
}

#[cfg(test)]
mod fetch_tests {
    use super::*;
    use crate::grpc::client_v1::AclSnapshot;

    // --- Transport: up_to_date keeps the cached snapshot and reports unchanged ---
    struct UpToDateTransport;
    #[async_trait::async_trait]
    impl TransportFetcher for UpToDateTransport {
        async fn fetch(&self, _known: u64) -> Result<Option<TransportSnapshot>> {
            Ok(None)
        }
    }

    #[tokio::test]
    async fn transport_up_to_date_keeps_cached_and_reports_unchanged() {
        let state = crate::runtime::new_shared();
        state.write().await.transport_snapshot = Some(TransportSnapshot {
            version: 5,
            ..Default::default()
        });

        let changed = fetch_and_store_transport_with(&state, &UpToDateTransport)
            .await
            .unwrap();

        assert!(!changed, "up_to_date must report unchanged");
        let s = state.read().await;
        assert_eq!(
            s.transport_snapshot.as_ref().unwrap().version,
            5,
            "cached snapshot must be kept"
        );
        assert!(s.transport_last_sync_at.is_some(), "sync timestamp refreshed");
    }

    // --- Transport: concurrent fetches are serialized (Fix 1 regression guard) ---
    // Each fetch returns `known_version + 1` and yields at its await point. WITHOUT
    // the sync lock, both concurrent calls would read the same known_version (0) and
    // the newer result would be lost. WITH the lock, the second call reads the
    // first's stored version, so versions advance monotonically (0 -> 1 -> 2).
    struct MonotonicTransport {
        seen: Arc<Mutex<Vec<u64>>>,
    }
    #[async_trait::async_trait]
    impl TransportFetcher for MonotonicTransport {
        async fn fetch(&self, known: u64) -> Result<Option<TransportSnapshot>> {
            self.seen.lock().await.push(known);
            // Force scheduling points so an unserialized impl would interleave.
            tokio::task::yield_now().await;
            tokio::task::yield_now().await;
            Ok(Some(TransportSnapshot {
                version: known + 1,
                ..Default::default()
            }))
        }
    }

    #[tokio::test]
    async fn transport_sync_serializes_concurrent_fetches() {
        let state = crate::runtime::new_shared();
        let seen = Arc::new(Mutex::new(Vec::<u64>::new()));
        let fetcher = MonotonicTransport { seen: seen.clone() };

        let (r1, r2) = tokio::join!(
            fetch_and_store_transport_with(&state, &fetcher),
            fetch_and_store_transport_with(&state, &fetcher),
        );
        r1.unwrap();
        r2.unwrap();

        let mut seen = seen.lock().await.clone();
        seen.sort_unstable();
        assert_eq!(
            seen,
            vec![0, 1],
            "each fetch must observe the prior fetch's stored version (serialized)"
        );
        assert_eq!(
            state.read().await.transport_snapshot.as_ref().unwrap().version,
            2,
            "final stored version must be the newest — no stale overwrite"
        );
    }

    // --- ACL: up_to_date keeps the cached snapshot and reports unchanged ---
    struct UpToDateAcl;
    #[async_trait::async_trait]
    impl AclFetcher for UpToDateAcl {
        async fn fetch(&self, _known: u64) -> Result<Option<AclSnapshot>> {
            Ok(None)
        }
    }

    #[tokio::test]
    async fn acl_up_to_date_keeps_cached_and_reports_unchanged() {
        let state = crate::runtime::new_shared();
        state.write().await.acl_snapshot = Some(AclSnapshot {
            version: 7,
            entries: vec![AclEntry::default(), AclEntry::default(), AclEntry::default()],
            ..Default::default()
        });

        let result = sync_acl_now_with(&state, &UpToDateAcl).await.unwrap();

        assert!(!result.changed, "up_to_date must report unchanged");
        assert_eq!(result.version, 7, "result reflects the cached version");
        assert_eq!(result.entry_count, 3, "result reflects the cached entry count");
        assert_eq!(
            state.read().await.acl_snapshot.as_ref().unwrap().version,
            7,
            "cached ACL snapshot must be kept"
        );
    }

    // --- ACL: a newer version is stored and reported changed ---
    struct NewAcl;
    #[async_trait::async_trait]
    impl AclFetcher for NewAcl {
        async fn fetch(&self, _known: u64) -> Result<Option<AclSnapshot>> {
            Ok(Some(AclSnapshot {
                version: 8,
                entries: vec![AclEntry::default()],
                ..Default::default()
            }))
        }
    }

    #[tokio::test]
    async fn acl_new_version_is_stored_and_reported_changed() {
        let state = crate::runtime::new_shared();
        state.write().await.acl_snapshot = Some(AclSnapshot {
            version: 7,
            ..Default::default()
        });

        let result = sync_acl_now_with(&state, &NewAcl).await.unwrap();

        assert!(result.changed, "a new version must report changed");
        assert_eq!(result.version, 8);
        assert_eq!(
            state.read().await.acl_snapshot.as_ref().unwrap().version,
            8,
            "new ACL snapshot must be stored"
        );
    }
}

#[cfg(test)]
mod posture_scheduler_tests {
    use super::*;

    // --- posture_resync wiring: notify_one() actually wakes a waiter ---
    #[tokio::test]
    async fn posture_resync_wakes_a_waiting_task() {
        let state = crate::runtime::new_shared();
        let resync = state.read().await.posture_resync.clone();

        let waiter = {
            let resync = resync.clone();
            tokio::spawn(async move {
                resync.notified().await;
            })
        };

        // Give the spawned task a chance to start waiting before notifying.
        tokio::task::yield_now().await;
        resync.notify_one();

        tokio::time::timeout(std::time::Duration::from_secs(1), waiter)
            .await
            .expect("waiter task timed out — posture_resync did not wake it")
            .expect("waiter task panicked");
    }

    // --- submit_with_retry: fake submitter records every report_id it saw ---
    struct FakeSubmitter {
        report_ids_seen: Mutex<Vec<String>>,
        fail_first_n: usize,
    }

    impl FakeSubmitter {
        fn new(fail_first_n: usize) -> Self {
            Self {
                report_ids_seen: Mutex::new(Vec::new()),
                fail_first_n,
            }
        }
    }

    #[async_trait::async_trait]
    impl PostureSubmitter for FakeSubmitter {
        async fn submit(&self, report: DevicePostureReport) -> Result<()> {
            let mut seen = self.report_ids_seen.lock().await;
            seen.push(report.report_id.clone());
            if seen.len() <= self.fail_first_n {
                anyhow::bail!("simulated transient failure");
            }
            Ok(())
        }
    }

    fn fake_collected() -> posture::PostureReport {
        posture::PostureReport {
            checks: Vec::new(),
            os_name: "TestOS".into(),
            os_version: "1.0".into(),
        }
    }

    #[tokio::test]
    async fn submit_with_retry_reuses_report_id_on_failure() {
        let submitter = FakeSubmitter::new(1); // first call fails, second succeeds
        submit_with_retry(&submitter, fake_collected()).await;

        let seen = submitter.report_ids_seen.lock().await;
        assert_eq!(seen.len(), 2, "expected exactly one retry");
        assert_eq!(
            seen[0], seen[1],
            "retry must reuse the same report_id, not generate a new one"
        );
    }

    #[tokio::test]
    async fn submit_with_retry_gives_up_after_one_retry() {
        let submitter = FakeSubmitter::new(usize::MAX); // always fails
        submit_with_retry(&submitter, fake_collected()).await;

        let seen = submitter.report_ids_seen.lock().await;
        assert_eq!(
            seen.len(),
            2,
            "must attempt exactly once plus one retry, then give up — not loop forever"
        );
    }
}
