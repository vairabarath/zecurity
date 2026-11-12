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

/// True for a well-formed SID string (`S-1-...`, digits and dashes only).
/// Anything else is refused, so the configured value can never add an access
/// rule of its own to the pipe's security descriptor.
#[cfg_attr(not(windows), allow(dead_code))]
pub fn is_valid_sid(sid: &str) -> bool {
    let Some(rest) = sid.strip_prefix("S-1-") else {
        return false;
    };
    !rest.is_empty()
        && rest.split('-').all(|part| !part.is_empty() && part.bytes().all(|b| b.is_ascii_digit()))
}

/// The pipe's security descriptor (SDDL): protected DACL, full access for
/// SYSTEM and Administrators, read/write for the enrolling user. This is the
/// Windows form of the Linux same-user check: the daemon runs as SYSTEM, the
/// CLI as the enrolling user. Without a valid user SID it fails closed to
/// SYSTEM and Administrators only.
#[cfg_attr(not(windows), allow(dead_code))]
pub fn pipe_sddl(user_sid: &str) -> String {
    let mut sddl = String::from("D:P(A;;GA;;;SY)(A;;GA;;;BA)");
    if is_valid_sid(user_sid) {
        sddl.push_str(&format!("(A;;GRGW;;;{user_sid})"));
    }
    sddl
}

/// Creates one instance of the daemon's named pipe. `first` claims the name
/// (fails if another process already owns it, so the pipe can't be squatted).
#[cfg(windows)]
pub fn create_pipe_server(
    first: bool,
    user_sid: &str,
) -> std::io::Result<tokio::net::windows::named_pipe::NamedPipeServer> {
    use windows_sys::Win32::Foundation::{LocalFree, FALSE};
    use windows_sys::Win32::Security::Authorization::{
        ConvertStringSecurityDescriptorToSecurityDescriptorW, SDDL_REVISION_1,
    };
    use windows_sys::Win32::Security::{PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES};

    if !user_sid.is_empty() && !is_valid_sid(user_sid) {
        tracing::warn!("ipc_user_sid in client.conf is not a valid SID; the IPC pipe allows only SYSTEM and Administrators");
    }
    let sddl: Vec<u16> = pipe_sddl(user_sid).encode_utf16().chain(Some(0)).collect();
    let mut descriptor: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
    // SAFETY: `sddl` is NUL-terminated UTF-16; on success `descriptor` is a
    // LocalAlloc'd buffer that is freed below after the pipe is created.
    let ok = unsafe {
        ConvertStringSecurityDescriptorToSecurityDescriptorW(
            sddl.as_ptr(),
            SDDL_REVISION_1,
            &mut descriptor,
            std::ptr::null_mut(),
        )
    };
    if ok == 0 {
        return Err(std::io::Error::last_os_error());
    }
    let mut attrs = SECURITY_ATTRIBUTES {
        nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
        lpSecurityDescriptor: descriptor,
        bInheritHandle: FALSE,
    };
    // SAFETY: `attrs` and the descriptor it points to outlive the call.
    let result = unsafe {
        tokio::net::windows::named_pipe::ServerOptions::new()
            .first_pipe_instance(first)
            .reject_remote_clients(true)
            .create_with_security_attributes_raw(
                ipc_pipe_name(),
                &mut attrs as *mut SECURITY_ATTRIBUTES as *mut std::ffi::c_void,
            )
    };
    // SAFETY: allocated by ConvertStringSecurityDescriptorToSecurityDescriptorW.
    unsafe { LocalFree(descriptor as _) };
    result
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

    #[cfg(unix)]
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

    send_ipc(req).await.context(if cfg!(windows) {
        "daemon could not be started — run: Get-Service zecurity-client (start it from an elevated PowerShell with Start-Service zecurity-client)"
    } else {
        "daemon could not be started — run: systemctl status zecurity-client"
    })
}

#[cfg(test)]
mod tests {
    use super::{is_valid_sid, pipe_sddl};

    #[test]
    fn sid_validation_accepts_only_plain_sid_strings() {
        assert!(is_valid_sid("S-1-5-21-1004336348-1177238915-682003330-1001"));
        assert!(is_valid_sid("S-1-5-18"));
        for bad in [
            "",
            "S-1-",
            "S-1-5-",
            "S-1-5--18",
            "s-1-5-18",
            "S-1-5-18)(A;;GA;;;WD",
            "S-1-5-18 ",
            "WD",
        ] {
            assert!(!is_valid_sid(bad), "{bad:?} must be rejected");
        }
    }

    #[test]
    fn pipe_sddl_admits_system_admins_and_the_enrolling_user_only() {
        assert_eq!(
            pipe_sddl("S-1-5-21-1-2-3-1001"),
            "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GRGW;;;S-1-5-21-1-2-3-1001)"
        );
    }

    #[test]
    fn pipe_sddl_fails_closed_without_a_valid_sid() {
        assert_eq!(pipe_sddl(""), "D:P(A;;GA;;;SY)(A;;GA;;;BA)");
        // An injection attempt adds nothing: Everyone (WD) is never granted.
        assert_eq!(
            pipe_sddl("S-1-5-18)(A;;GA;;;WD"),
            "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
        );
    }
}
