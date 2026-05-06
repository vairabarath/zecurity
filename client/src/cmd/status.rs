use anyhow::Result;

use crate::ipc::{send_ipc, IpcRequest};
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

    match send_ipc(&IpcRequest::Status).await {
        Ok(resp) if resp.ok => {
            let email = resp.email.as_deref().unwrap_or("unknown");
            let expires = resp
                .cert_expires_at
                .map(format_duration_until)
                .unwrap_or_else(|| "unknown".into());
            println!(
                "Status:     Running as {}, cert expires in {}",
                email, expires
            );

            if let Some(id) = &resp.device_id {
                println!("Device ID:  {}", id);
            }
            if let Some(spiffe) = &resp.spiffe_id {
                println!("SPIFFE ID:  {}", spiffe);
            }

            match (resp.acl_entry_count, resp.acl_snapshot_version) {
                (None, _) => println!("ACL:        not yet loaded"),
                (Some(0), _) => {
                    println!("ACL:        loaded (no policies configured for this workspace)")
                }
                (Some(n), Some(0)) => println!("ACL:        loaded ({} rules)", n),
                (Some(n), Some(v)) => println!("ACL:        loaded ({} rules, version {})", n, v),
                (Some(n), None) => println!("ACL:        loaded ({} rules)", n),
            }
            if let Some(last_sync) = resp.acl_last_sync_at {
                println!("ACL Sync:   {}", format_duration_since(last_sync));
            }
        }
        _ => {
            println!("Status:     Not connected — run zecurity-client login");
        }
    }

    Ok(())
}

fn format_duration_since(ts: i64) -> String {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    let secs = now.saturating_sub(ts);
    if secs < 60 {
        format!("{}s ago", secs)
    } else if secs < 3600 {
        format!("{}m ago", secs / 60)
    } else if secs < 86_400 {
        format!("{}h ago", secs / 3600)
    } else {
        format!("{}d ago", secs / 86_400)
    }
}
