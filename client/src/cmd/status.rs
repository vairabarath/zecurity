use anyhow::Result;

use crate::ipc::{ensure_daemon_and_send, IpcRequest};
use crate::state_store::format_duration_until;

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Err(_) => {
            println!("Status:    Not configured");
            println!("Run `zecurity-client setup --workspace <name>` first.");
            return Ok(());
        }
        Ok(conf) => {
            println!("Workspace:  {}", conf.workspace);
            println!("Controller: {}", conf.controller());
        }
    }

    match ensure_daemon_and_send(&IpcRequest::Status).await {
        Ok(resp) if resp.ok => {
            let email = resp.email.as_deref().unwrap_or("unknown");
            let expires = resp
                .cert_expires_at
                .map(format_duration_until)
                .unwrap_or_else(|| "unknown".into());
            println!("Status:     Running as {}, cert expires in {}", email, expires);

            if let Some(id) = &resp.device_id {
                println!("Device ID:  {}", id);
            }
            if let Some(spiffe) = &resp.spiffe_id {
                println!("SPIFFE ID:  {}", spiffe);
            }

            let acl_ver = resp.acl_snapshot_version.unwrap_or(0);
            if acl_ver > 0 {
                println!("ACL:        version {}", acl_ver);
            } else {
                println!("ACL:        not yet loaded");
            }
        }
        _ => {
            println!("Status:     Not connected — run zecurity-client login");
        }
    }

    Ok(())
}
