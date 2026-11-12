#[cfg(unix)]
use std::path::PathBuf;

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncRead, AsyncWrite, AsyncWriteExt, BufReader};
#[cfg(unix)]
use tokio::net::UnixStream;

// ── Wire types ───────────────────────────────────────────────────────────────

#[derive(Debug, Serialize, Deserialize)]
pub struct IpcResource {
    pub name: String,
    pub address: String,
    pub port: u32,
    pub protocol: String,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum IpcRequest {
    Status,
    Resources,
    Sync,
    Shutdown,
    LoadState,
    GetToken,
    PostLoginState {
        workspace_slug: String,
        workspace_name: String,
        workspace_id: String,
        trust_domain: String,
        user_email: String,
        access_token: String,
        refresh_token: String,
        expires_at: i64,
        device_id: String,
        spiffe_id: String,
        certificate_pem: String,
        private_key_pem: String,
        /// PENDING-17: set instead of `private_key_pem` when the device
        /// enrolled with a TPM-backed key. Opaque here — the daemon just
        /// persists whichever of the two the CLI produced.
        #[serde(default)]
        tpm_key_material: Option<String>,
        ca_cert_pem: String,
        cert_expires_at: i64,
        hostname: String,
        os: String,
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
    #[serde(skip_serializing_if = "Option::is_none")]
    pub acl_last_sync_at: Option<i64>,
    /// Number of ACL entries in the loaded snapshot. None when no snapshot is loaded.
    /// Use this (not version) to determine whether policies are configured.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub acl_entry_count: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub synced_resources: Option<usize>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub resources: Option<Vec<IpcResource>>,
    /// Sprint 19 Track 2 (PENDING-13): "re_enroll_required" / "revoked" when
    /// the controller has sent a device directive. None when active.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub device_state: Option<String>,
    /// Human-readable reason accompanying device_state. Present whenever
    /// device_state is present (may be an empty string).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub revoked_reason: Option<String>,
    /// Status: whether the tunnel is up. "Running" only means the daemon runs
    /// with a session (live run 2026-10-29, O-5).
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tunnel_up: Option<bool>,
    /// Status (FU-10): present while the daemon is bringing the tunnel back
    /// after a restart; the last attempt's error, "" before the first.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tunnel_restoring: Option<String>,
    /// Status (PENDING-14 F3): DNS-name resources, "active (N names)" or
    /// "unavailable: <reason>"; absent when there are none.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub dns_names: Option<String>,
}

// ── Socket path ──────────────────────────────────────────────────────────────

/// Path to the daemon Unix socket.
///
/// Matches `RuntimeDirectory=zecurity-client` in the systemd unit.
/// Override with `ZECURITY_DAEMON_SOCKET` for dev/testing.
#[cfg(unix)]
pub fn ipc_socket_path() -> PathBuf {
    if let Ok(p) = std::env::var("ZECURITY_DAEMON_SOCKET") {
        return PathBuf::from(p);
    }
    // Android: in the app's private files directory, reachable only by the
    // app's own processes (the UI and the VPN service hosting the daemon).
    #[cfg(target_os = "android")]
    return crate::android::files_dir().join("daemon.sock");
    #[cfg(not(target_os = "android"))]
    PathBuf::from("/run/zecurity-client/daemon.sock")
}

// ── Same-user check ──────────────────────────────────────────────────────────

/// Returns true if the peer on `stream` is the same OS user as the current process.
/// Rejects cross-user connections using SO_PEERCRED (Linux only).
#[cfg(target_os = "linux")]
pub fn check_same_user(stream: &UnixStream) -> bool {
    use std::os::unix::io::AsRawFd;
    unsafe {
        let mut cred = libc::ucred {
            pid: 0,
            uid: 0,
            gid: 0,
        };
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

#[cfg(all(unix, not(target_os = "linux")))]
pub fn check_same_user(_stream: &UnixStream) -> bool {
    true
}

// ── Windows named pipe ───────────────────────────────────────────────────────

/// The daemon's named pipe on Windows (the counterpart of the Unix socket).
/// Override with `ZECURITY_DAEMON_PIPE` for dev/testing.
#[cfg(windows)]
pub fn ipc_pipe_name() -> String {
    std::env::var("ZECURITY_DAEMON_PIPE").unwrap_or_else(|_| r"\\.\pipe\zecurity-client".into())
}

/// The enrolling user's SID, from the service command line
/// (`zecurity-client daemon --ipc-user-sid`), set once at daemon start.
#[cfg(windows)]
static PIPE_USER_SID: std::sync::OnceLock<Option<String>> = std::sync::OnceLock::new();

#[cfg(windows)]
pub fn set_pipe_user_sid(sid: Option<String>) {
    let _ = PIPE_USER_SID.set(sid);
}

#[cfg(windows)]
pub fn pipe_user_sid() -> Option<&'static str> {
    PIPE_USER_SID.get().and_then(|s| s.as_deref())
}

/// Creates one instance of the daemon's named pipe. `first` claims the name
/// (fails if another process already owns it, so the pipe can't be squatted).
#[cfg(windows)]
pub fn create_pipe_server(
    first: bool,
) -> std::io::Result<tokio::net::windows::named_pipe::NamedPipeServer> {
    // Owned by SYSTEM when running as the service: that is what the CLI
    // verifies before it sends anything (crate::win_security).
    let owner_system = crate::win_service::running_as_service();
    create_pipe_server_with_sddl(
        first,
        &crate::win_security::pipe_sddl(pipe_user_sid(), owner_system),
    )
}

#[cfg(windows)]
fn create_pipe_server_with_sddl(
    first: bool,
    sddl: &str,
) -> std::io::Result<tokio::net::windows::named_pipe::NamedPipeServer> {
    use windows_sys::Win32::Foundation::FALSE;
    use windows_sys::Win32::Security::SECURITY_ATTRIBUTES;

    let sd = crate::win_security::descriptor_from_sddl(sddl)?;
    let mut attrs = SECURITY_ATTRIBUTES {
        nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: crate::win_security::as_ptr(&sd),
        bInheritHandle: FALSE,
    };
    // SAFETY: `attrs` and the descriptor it points to (`sd`) outlive the call.
    unsafe {
        tokio::net::windows::named_pipe::ServerOptions::new()
            .first_pipe_instance(first)
            .reject_remote_clients(true)
            .create_with_security_attributes_raw(
                ipc_pipe_name(),
                &mut attrs as *mut SECURITY_ATTRIBUTES as *mut std::ffi::c_void,
            )
    }
}

// ── CLI-side helpers ─────────────────────────────────────────────────────────

/// Connect to the daemon and send one request. Fails if the socket is not present.
#[cfg(unix)]
pub async fn send_ipc(req: &IpcRequest) -> Result<IpcResponse> {
    let path = ipc_socket_path();
    let stream = UnixStream::connect(&path)
        .await
        .with_context(|| format!("connect to daemon socket {}", path.display()))?;
    exchange(stream, req).await
}

/// Connect to the daemon's named pipe and send one request. Fails if the
/// daemon is not running.
#[cfg(windows)]
pub async fn send_ipc(req: &IpcRequest) -> Result<IpcResponse> {
    use tokio::net::windows::named_pipe::ClientOptions;
    const ERROR_PIPE_BUSY: i32 = 231;

    let name = ipc_pipe_name();
    let mut attempts = 0;
    let client = loop {
        match ClientOptions::new().open(&name) {
            Ok(client) => break client,
            // Every instance is serving another client; one frees up shortly.
            Err(e) if e.raw_os_error() == Some(ERROR_PIPE_BUSY) && attempts < 20 => {
                attempts += 1;
                tokio::time::sleep(std::time::Duration::from_millis(50)).await;
            }
            Err(e) => {
                return Err(e).with_context(|| format!("connect to daemon pipe {name}"));
            }
        }
    };
    // Never send tokens or keys to a pipe someone else created.
    {
        use std::os::windows::io::AsRawHandle;
        crate::win_security::verify_pipe_server(client.as_raw_handle())?;
    }
    exchange(client, req).await
}

/// One newline-delimited JSON request and its response (ADR-002).
async fn exchange<S>(stream: S, req: &IpcRequest) -> Result<IpcResponse>
where
    S: AsyncRead + AsyncWrite,
{
    let (reader, mut writer) = tokio::io::split(stream);

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

    #[cfg(all(unix, not(target_os = "android")))]
    let _ = tokio::process::Command::new("systemctl")
        .args(["start", "zecurity-client"])
        .status()
        .await;
    // Starting a service needs rights a standard user may lack; the installer
    // sets the service to start automatically, so this is the fallback.
    #[cfg(windows)]
    let _ = tokio::process::Command::new("sc.exe")
        .args(["start", "zecurity-client"])
        .status()
        .await;

    tokio::time::sleep(std::time::Duration::from_millis(500)).await;

    // Android: the app starts the daemon with its VPN service; there is
    // nothing to start from here.
    send_ipc(req).await.context(if cfg!(target_os = "android") {
        "the Zecurity service is not running — reopen the Zecurity app"
    } else if cfg!(windows) {
        "daemon could not be started — run: Get-Service zecurity-client (start it from an elevated PowerShell with Start-Service zecurity-client)"
    } else {
        "daemon could not be started — run: systemctl status zecurity-client"
    })
}

/// Runs on Windows only: the secured pipe really carries a request and its
/// response, and its name can't be claimed twice.
#[cfg(all(test, windows))]
mod windows_pipe_tests {
    use super::*;

    /// The tests share the ZECURITY_DAEMON_PIPE variable.
    static PIPE_ENV: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

    fn use_test_pipe(suffix: &str) {
        let name = format!(r"\\.\pipe\zecurity-client-test-{}-{suffix}", std::process::id());
        std::env::set_var("ZECURITY_DAEMON_PIPE", name);
        // Admit the account running the tests, elevated or not.
        let me = crate::win_security::current_user_sid().expect("current user SID");
        set_pipe_user_sid(Some(me));
    }

    /// A second "first" instance of the same name must fail: no squatting.
    /// (Wine 8 does not enforce FILE_FLAG_FIRST_PIPE_INSTANCE; real Windows does.)
    #[tokio::test]
    async fn pipe_name_cannot_be_claimed_twice() {
        let _env = PIPE_ENV.lock().await;
        use_test_pipe("first");
        let _server = create_pipe_server(true).expect("create secured pipe");
        assert!(create_pipe_server(true).is_err());
    }

    /// Answers one Status request on `server`.
    async fn serve_one(server: tokio::net::windows::named_pipe::NamedPipeServer) {
        server.connect().await.expect("accept client");
        let (reader, mut writer) = tokio::io::split(server);
        let mut line = String::new();
        BufReader::new(reader).read_line(&mut line).await.unwrap();
        let req: IpcRequest = serde_json::from_str(line.trim()).unwrap();
        assert!(matches!(req, IpcRequest::Status));
        let resp = IpcResponse {
            ok: true,
            kind: "Status".into(),
            ..Default::default()
        };
        let mut out = serde_json::to_string(&resp).unwrap();
        out.push('\n');
        writer.write_all(out.as_bytes()).await.unwrap();
        writer.flush().await.unwrap();
    }

    /// The SDDL-secured pipe the daemon creates carries a request and its
    /// response (the transport itself, without the owner check).
    #[tokio::test]
    async fn secured_pipe_carries_a_request_and_response() {
        let _env = PIPE_ENV.lock().await;
        use_test_pipe("rt");
        let server = create_pipe_server(true).expect("create secured pipe");
        let serve = tokio::spawn(serve_one(server));
        let client = tokio::net::windows::named_pipe::ClientOptions::new()
            .open(ipc_pipe_name())
            .expect("open pipe");
        let resp = exchange(client, &IpcRequest::Status).await.expect("IPC round trip");
        assert!(resp.ok);
        assert_eq!(resp.kind, "Status");
        serve.await.unwrap();
    }

    /// An impostor's pipe (owned by an ordinary account, as any pipe made
    /// while the service is stopped would be) is refused before anything is
    /// sent to it.
    #[tokio::test]
    async fn client_refuses_a_pipe_not_owned_by_system() {
        let _env = PIPE_ENV.lock().await;
        use_test_pipe("impostor");
        let me = crate::win_security::current_user_sid().expect("current user SID");
        let sddl = format!("O:{me}D:P(A;;GA;;;WD)");
        let _impostor = create_pipe_server_with_sddl(true, &sddl).expect("create impostor pipe");
        let err = send_ipc(&IpcRequest::Status).await.expect_err("must refuse");
        assert!(
            format!("{err:#}").contains("not SYSTEM"),
            "unexpected error: {err:#}"
        );
    }
}
