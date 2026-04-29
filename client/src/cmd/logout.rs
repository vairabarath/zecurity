

use anyhow::Result;

pub async fn run() -> Result<()> {
    crate::ipc::signal_daemon_logout().await
        .map_err(|_| anyhow::anyhow!("Daemon not running."))?;
    println!("Session cleared. Daemon will reconnect on next `connect`.");
    Ok(())
}
