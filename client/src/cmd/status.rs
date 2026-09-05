use anyhow::Result;

use crate::ipc::{send_ipc, IpcRequest};
use crate::state_store::format_duration_until;

/// Sprint 19 Track 3 (PENDING-13, ADR-028 D4): below this, renewal
/// (run_cert_renewal_scheduler in daemon.rs, which normally succeeds around
/// 2.8 days of remaining life) should already have happened. Still counting
/// down this low means renewal has likely been silently failing — a
/// deliberately smaller/later threshold than the scheduler's own renewal
/// window, since this one means "something is wrong," not "time to renew."
const CERT_EXPIRY_WARNING_SECS: i64 = 24 * 3_600;

pub async fn run() -> Result<()> {
    let conf = match crate::config::load() {
        Err(_) => {
            println!("Status:    Not configured");
            println!("Run `zecurity-client setup --workspace <name>` first.");
            return Ok(());
        }
        Ok(conf) => {
            println!("Workspace:  {}", conf.workspace);
            println!("Controller: {}", conf.controller());
            conf
        }
    };

    match send_ipc(&IpcRequest::Status).await {
        Ok(resp) if resp.ok => {
            // Sprint 19 Track 2 (PENDING-13): REVOKED/RE_ENROLL_REQUIRED take
            // over the whole status line instead of the normal running status
            // — the cert has been wiped, so "cert expires in ..." etc. no
            // longer mean anything.
            if let Some(marker) = resp.device_state.as_deref() {
                print_device_directive_status(marker, resp.revoked_reason.as_deref());
                return Ok(());
            }

            let email = resp.email.as_deref().unwrap_or("unknown");
            let expires = resp
                .cert_expires_at
                .map(format_duration_until)
                .unwrap_or_else(|| "unknown".into());
            println!(
                "Status:     Running as {}, cert expires in {}",
                email, expires
            );
            if let Some(cert_expires_at) = resp.cert_expires_at {
                if seconds_until(cert_expires_at) <= CERT_EXPIRY_WARNING_SECS {
                    println!(
                        "WARNING:    certificate renewal appears to be failing — \
                         expires in {}. If this persists, try `zecurity-client login`.",
                        expires
                    );
                }
            }

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
            // The daemon may not be reachable because it exited after a
            // REVOKED directive (Sprint 19 Track 2 / PENDING-13,
            // react_to_device_directive) — fall back to the on-disk marker so
            // this still reports "revoked" instead of a misleading generic
            // "not connected".
            let marker = crate::state_store::load_workspace_state(&conf.workspace)
                .ok()
                .map(|stored| stored.device.device_state)
                .filter(|m| !m.is_empty());

            match marker {
                Some(marker) => print_device_directive_status(&marker, None),
                None => println!("Status:     Not connected — run zecurity-client login"),
            }
        }
    }

    Ok(())
}

/// Prints the REVOKED / RE_ENROLL_REQUIRED status line (Sprint 19 Track 2 /
/// PENDING-13, D-D — the two messages are deliberately different: REVOKED is
/// terminal ("contact your admin"), RE_ENROLL_REQUIRED is recoverable
/// ("sign in again"). `reason` is only available when read live via IPC —
/// the on-disk marker alone (used when the daemon isn't running) carries no
/// reason text.
fn print_device_directive_status(marker: &str, reason: Option<&str>) {
    match marker {
        "revoked" => println!("Status:     Revoked — contact your admin"),
        "re_enroll_required" => println!("Status:     Sign in again to re-register this device"),
        other => println!("Status:     Unrecognized device state ({})", other),
    }
    if let Some(reason) = reason.filter(|r| !r.is_empty()) {
        println!("Reason:     {}", reason);
    }
}

/// Seconds remaining until `ts` (negative if already past). Same
/// SystemTime/UNIX_EPOCH computation as format_duration_since, kept separate
/// since this returns a signed count for comparison, not a display string.
fn seconds_until(ts: i64) -> i64 {
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);
    ts - now
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
