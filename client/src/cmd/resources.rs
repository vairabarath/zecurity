use anyhow::Result;

use crate::ipc::{send_ipc, IpcRequest};

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Err(_) => {
            println!("Not configured. Run `zecurity-client setup --workspace <name>` first.");
            return Ok(());
        }
        Ok(_) => {}
    }

    match send_ipc(&IpcRequest::Resources).await {
        Ok(resp) if resp.ok => {
            let resources = resp.resources.unwrap_or_default();
            if resources.is_empty() {
                println!("No resources — ACL not loaded or no access policies configured.");
                println!("Run `zecurity-client login` if not connected.");
            } else {
                println!("Resources ({}):", resources.len());
                println!("{:<28} {:<24} {:<6} {}", "Name", "Address", "Port", "Protocol");
                println!("{}", "-".repeat(70));
                for r in &resources {
                    println!("{:<28} {:<24} {:<6} {}", r.name, r.address, r.port, r.protocol.to_uppercase());
                }
            }
        }
        _ => {
            println!("Daemon not running — run `zecurity-client login` first.");
        }
    }

    Ok(())
}
