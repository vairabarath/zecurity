use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;
use tokio::net::UnixStream;

use crate::runtime::SharedState;

const SOCK_PATH: &str = "/run/zecurity-client.sock";

pub fn sock_path() -> String {
    if std::path::Path::new("/run").exists()
        && std::fs::metadata("/run").map(|m| !m.permissions().readonly()).unwrap_or(false)
    {
        SOCK_PATH.to_string()
    } else {
        let uid = unsafe { libc::getuid() };
        format!("/tmp/zecurity-client-{}.sock", uid)
    }
}

// ── Server (runs inside connect daemon) ───────────────────────────────────

pub async fn serve_ipc(state: SharedState) {
    let path = sock_path();
    let _ = std::fs::remove_file(&path);
    let listener = match UnixListener::bind(&path) {
        Ok(l) => l,
        Err(e) => { eprintln!("IPC socket error: {}", e); return; }
    };

    loop {
        let Ok((stream, _)) = listener.accept().await else { continue };
        let state = state.clone();
        tokio::spawn(handle_ipc_conn(stream, state));
    }
}

#[derive(Deserialize)]
struct IpcRequest { cmd: String }

#[derive(Serialize)]
#[serde(untagged)]
enum IpcResponse {
    Status {
        email:              String,
        workspace:          String,
        spiffe_id:          String,
        session_expires_at: i64,
        connected:          bool,
    },
    Token  { access_token: String },
    Ok     { ok: bool },
    Error  { error: String },
}

async fn handle_ipc_conn(stream: UnixStream, state: SharedState) {
    let (reader, mut writer) = stream.into_split();
    let mut lines = BufReader::new(reader).lines();

    while let Ok(Some(line)) = lines.next_line().await {
        let resp = match serde_json::from_str::<IpcRequest>(&line) {
            Err(_) => IpcResponse::Error { error: "invalid request".into() },
            Ok(req) => {
                let st = state.read().await;
                match req.cmd.as_str() {
                    "status" => IpcResponse::Status {
                        email:              st.user.as_ref().map(|u| u.email.clone()).unwrap_or_default(),
                        workspace:          st.workspace.as_ref().map(|w| w.slug.clone()).unwrap_or_default(),
                        spiffe_id:          st.device.as_ref().map(|d| d.spiffe_id.clone()).unwrap_or_default(),
                        session_expires_at: st.session.as_ref().map(|s| s.expires_at).unwrap_or(0),
                        connected:          st.device.is_some(),
                    },
                    "token" => match st.session.as_ref() {
                        Some(s) => IpcResponse::Token { access_token: s.access_token.clone() },
                        None    => IpcResponse::Error { error: "not authenticated".into() },
                    },
                    "logout" => {
                        drop(st);
                        let mut st = state.write().await;
                        *st = Default::default();
                        IpcResponse::Ok { ok: true }
                    }
                    _ => IpcResponse::Error { error: "unknown command".into() },
                }
            }
        };
        let mut out = serde_json::to_string(&resp).unwrap_or_default();
        out.push('\n');
        if writer.write_all(out.as_bytes()).await.is_err() { break; }
    }
}

// ── Client (runs in status/invite/logout subcommands) ─────────────────────

async fn ipc_call(cmd: &str) -> Result<String> {
    let path = sock_path();
    let mut stream = UnixStream::connect(&path).await
        .map_err(|_| anyhow!("Daemon not running. Start with `zecurity-client connect`."))?;
    let req = format!("{{\"cmd\":\"{}\"}}\n", cmd);
    stream.write_all(req.as_bytes()).await?;
    let mut lines = BufReader::new(stream).lines();
    lines.next_line().await?.ok_or_else(|| anyhow!("no response from daemon"))
}

pub async fn query_daemon_status() -> Result<serde_json::Value> {
    let raw = ipc_call("status").await?;
    Ok(serde_json::from_str(&raw)?)
}

pub async fn query_daemon_token() -> Result<String> {
    let raw = ipc_call("token").await?;
    let v: serde_json::Value = serde_json::from_str(&raw)?;
    v["access_token"].as_str().map(|s| s.to_string())
        .ok_or_else(|| anyhow!("no token in response"))
}

pub async fn signal_daemon_logout() -> Result<()> {
    ipc_call("logout").await?;
    Ok(())
}
