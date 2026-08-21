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
                println!(
                    "{:<24} {:<17} {:<26} {:<6} {}",
                    "Name", "Address", "Hostname", "Port", "Protocol"
                );
                println!("{}", "-".repeat(82));
                for r in &resources {
                    // `Address` is what you connect to: the pinned IP for an
                    // IP-addressed resource, or the locally-allocated synthetic IP
                    // for a name-addressed one. `Hostname` is the name that
                    // synthetic IP stands in for — the operator needs both to map
                    // the name locally.
                    println!(
                        "{:<24} {:<17} {:<26} {:<6} {}",
                        r.name,
                        dash_if_empty(&r.address),
                        dash_if_empty(&r.hostname),
                        r.port,
                        r.protocol.to_uppercase()
                    );
                }
                if resources.iter().any(|r| !r.hostname.trim().is_empty()) {
                    println!();
                    println!(
                        "Name-addressed resources reach a synthetic address allocated on this \
                         device.\nMap the hostname to it locally, e.g. in /etc/hosts."
                    );
                }
            }
        }
        Ok(resp) => {
            println!(
                "Could not load resources: {}",
                resp.error.unwrap_or_else(|| "unknown error".into())
            );
        }
        _ => {
            println!("Could not load resources — run `zecurity-client login` first.");
        }
    }

    Ok(())
}

/// Render an em dash for an absent value rather than leaving the column blank —
/// a blank cell reads as a rendering bug, which is exactly what it used to be.
fn dash_if_empty(v: &str) -> &str {
    if v.trim().is_empty() {
        "—"
    } else {
        v
    }
}
