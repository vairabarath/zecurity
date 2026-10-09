use anyhow::Result;

use crate::ipc::{ensure_daemon_and_send, IpcRequest};

pub async fn run() -> Result<()> {
    let resp = ensure_daemon_and_send(&IpcRequest::Up).await?;
    if resp.ok {
        if resp.synced_resources == Some(0) {
            println!("Zecurity is up. No resources are currently assigned to this device.");
        } else {
            println!("Zecurity is up.");
        }
    } else {
        anyhow::bail!("{}", resp.error.unwrap_or_else(|| "unknown error".into()));
    }
    Ok(())
}
