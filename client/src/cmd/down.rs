use anyhow::Result;

use crate::ipc::{ensure_daemon_and_send, IpcRequest};

pub async fn run() -> Result<()> {
    let resp = ensure_daemon_and_send(&IpcRequest::Down).await?;
    if resp.ok {
        println!("Zecurity is down.");
    } else {
        anyhow::bail!(
            "{}",
            resp.error.unwrap_or_else(|| "unknown error".into())
        );
    }
    Ok(())
}
