use std::sync::Arc;

use anyhow::{Context, Result};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;
use tracing::{error, info, warn};

use crate::config;
use crate::grpc::{self, client_v1::GetAclSnapshotRequest};
use crate::ipc::{check_same_user, ipc_socket_path, IpcRequest, IpcResponse};
use crate::login::LoginResult;
use crate::runtime::{self, DeviceInfo, SessionInfo, SharedState, UserInfo, WorkspaceInfo};
use crate::state_store::{self, save_workspace_state, StoredWorkspaceState};

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
        let access_token = stored.session.access_token.clone();
        let device_id = stored.device.id.clone();
        tokio::spawn(async move {
            fetch_and_store_acl(&state_clone, &conf_clone, ca_pem, access_token, device_id).await;
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

    loop {
        match listener.accept().await {
            Ok((stream, _)) => {
                if !check_same_user(&stream) {
                    warn!("rejected IPC connection from a different user");
                    continue;
                }
                let state = Arc::clone(&state);
                let conf = conf.clone();
                tokio::spawn(async move {
                    if let Err(e) = handle_connection(stream, state, conf).await {
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
) -> Result<()> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    let mut line = String::new();

    reader.read_line(&mut line).await?;

    let (response, shutdown) = match serde_json::from_str::<IpcRequest>(line.trim()) {
        Ok(req) => {
            let is_shutdown = matches!(req, IpcRequest::Shutdown);
            let resp = handle_request(req, &state, &conf).await;
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
                acl_snapshot_version: Some(s.acl_snapshot.as_ref().map(|snap| snap.version).unwrap_or(0)),
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
                IpcResponse { ok: true, kind: "LoadState".into(), ..Default::default() }
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
            match s.session.as_ref().filter(|sess| !sess.access_token.is_empty()) {
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
                user: UserInfo { id: String::new(), email: user_email, role: String::new() },
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
                session: SessionInfo { access_token, refresh_token, expires_at },
            };
            let stored = StoredWorkspaceState::from_login(login_result);

            match save_workspace_state(&workspace_slug, &stored) {
                Ok(_) => {
                    let mut s = state.write().await;
                    populate_runtime(&mut s, &stored);
                    info!("PostLoginState: durable state saved, runtime updated");
                    drop(s);

                    // Fetch ACL snapshot in background — does not block IPC response.
                    let state_clone = Arc::clone(state);
                    let conf_clone = conf.clone();
                    let ca_pem = stored.device.ca_cert_pem.clone();
                    let access_token = stored.session.access_token.clone();
                    let device_id = stored.device.id.clone();
                    tokio::spawn(async move {
                        fetch_and_store_acl(&state_clone, &conf_clone, ca_pem, access_token, device_id).await;
                    });

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

        IpcRequest::Up => IpcResponse {
            ok: false,
            kind: "Up".into(),
            error: Some("not implemented".into()),
            ..Default::default()
        },

        IpcRequest::Down => IpcResponse {
            ok: false,
            kind: "Down".into(),
            error: Some("not implemented".into()),
            ..Default::default()
        },
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
    let _ = std::os::unix::net::UnixDatagram::unbound()
        .and_then(|s| s.send_to(b"READY=1\n", &path));
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

/// Fetch and store the ACL snapshot. On failure, keeps the existing snapshot
/// (never reverts to None on a transient error).
async fn fetch_and_store_acl(
    state: &SharedState,
    conf: &config::ClientConf,
    ca_pem: String,
    access_token: String,
    device_id: String,
) {
    match fetch_acl_snapshot(conf, &ca_pem, &access_token, &device_id).await {
        Ok(snapshot) => {
            let version = snapshot.version;
            let mut s = state.write().await;
            s.acl_snapshot = Some(snapshot);
            info!(version, "ACL snapshot stored");
        }
        Err(e) => {
            warn!(error = %e, "ACL snapshot fetch failed — default-deny in effect");
        }
    }
}
