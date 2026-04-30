use std::path::PathBuf;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;

// ── Wire types ───────────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct IpcResource {
    pub name:     String,
    pub address:  String,
    pub port:     u32,
    pub protocol: String,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum IpcRequest {
    Status,
    Resources,
    Shutdown,
    LoadState,
    GetToken,
    PostLoginState {
        workspace_slug:  String,
        workspace_name:  String,
        workspace_id:    String,
        trust_domain:    String,
        user_email:      String,
        access_token:    String,
        refresh_token:   String,
        expires_at:      i64,
        device_id:       String,
        spiffe_id:       String,
        certificate_pem: String,
        private_key_pem: String,
        ca_cert_pem:     String,
        cert_expires_at: i64,
        hostname:        String,
        os:              String,
    },
    /// Stub — implemented in Sprint 9 Phase F.
    Up,
    /// Stub — implemented in Sprint 9 Phase F.
    Down,
}

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct IpcResponse {
    pub ok: bool,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub state: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub email: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub device_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub spiffe_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cert_expires_at: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub workspace: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub acl_snapshot_version: Option<u64>,
    /// Number of ACL entries in the loaded snapshot. None when no snapshot is loaded.
    /// Use this (not version) to determine whether policies are configured.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub acl_entry_count: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resources: Option<Vec<IpcResource>>,
}

// ── Socket path ──────────────────────────────────────────────────────────────

/// Path to the daemon Unix socket.
///
/// Matches `RuntimeDirectory=zecurity-client` in the systemd unit.
/// Override with `ZECURITY_DAEMON_SOCKET` for dev/testing.
pub fn ipc_socket_path() -> PathBuf {
    if let Ok(p) = std::env::var("ZECURITY_DAEMON_SOCKET") {
        return PathBuf::from(p);
    }
    PathBuf::from("/run/zecurity-client/daemon.sock")
}

// ── Same-user check ──────────────────────────────────────────────────────────

/// Returns true if the peer on `stream` is the same OS user as the current process.
/// Rejects cross-user connections using SO_PEERCRED (Linux only).
#[cfg(target_os = "linux")]
pub fn check_same_user(stream: &UnixStream) -> bool {
    use std::os::unix::io::AsRawFd;
    unsafe {
        let mut cred = libc::ucred { pid: 0, uid: 0, gid: 0 };
        let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut _ as *mut libc::c_void,
            &mut len,
        ) == 0
            && cred.uid == libc::geteuid()
    }
}

#[cfg(not(target_os = "linux"))]
pub fn check_same_user(_stream: &UnixStream) -> bool {
    true
}

// ── CLI-side helpers ─────────────────────────────────────────────────────────

/// Connect to the daemon and send one request. Fails if the socket is not present.
pub async fn send_ipc(req: &IpcRequest) -> Result<IpcResponse> {
    let path = ipc_socket_path();
    let stream = UnixStream::connect(&path)
        .await
        .with_context(|| format!("connect to daemon socket {}", path.display()))?;

    let (reader, mut writer) = stream.into_split();

    let mut line = serde_json::to_string(req)?;
    line.push('\n');
    writer.write_all(line.as_bytes()).await?;

    let mut buf = String::new();
    BufReader::new(reader).read_line(&mut buf).await?;
    serde_json::from_str(buf.trim()).context("parse daemon response")
}

/// Try IPC; if the socket is absent, start the daemon via systemctl and retry once.
/// Fails closed if the second attempt also fails.
pub async fn ensure_daemon_and_send(req: &IpcRequest) -> Result<IpcResponse> {
    if let Ok(resp) = send_ipc(req).await {
        return Ok(resp);
    }

    let _ = tokio::process::Command::new("systemctl")
        .args(["start", "zecurity-client"])
        .status()
        .await;

    tokio::time::sleep(std::time::Duration::from_millis(500)).await;

    send_ipc(req)
        .await
        .context("daemon could not be started — run: systemctl status zecurity-client")
}
