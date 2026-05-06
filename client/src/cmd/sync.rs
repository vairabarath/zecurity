use anyhow::Result;

use crate::ipc::{ensure_daemon_and_send, IpcRequest};

pub async fn run() -> Result<()> {
    let resp = ensure_daemon_and_send(&IpcRequest::Sync).await?;
    if !resp.ok {
        anyhow::bail!("{}", resp.error.unwrap_or_else(|| "unknown error".into()));
    }

    println!("ACL synced.");
    if let Some(version) = resp.acl_snapshot_version {
        println!("Version: {}", version);
    }
    println!("Resources: {}", resp.synced_resources.unwrap_or(0));
    Ok(())
}
