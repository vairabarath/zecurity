use anyhow::Result;

use crate::grpc::{client_v1::RevokeDeviceRequest, connect_grpc};
use crate::ipc::{send_ipc, IpcRequest};

pub async fn run() -> Result<()> {
    let Ok(conf) = crate::config::load() else {
        println!("No saved session to clear.");
        return Ok(());
    };

    // Load stored state ONCE, before we touch the daemon or the state file —
    // the token + device_id live in the state we're about to erase.
    let stored = crate::state_store::load_workspace_state(&conf.workspace).ok();
    let access_token = stored
        .as_ref()
        .map(|s| s.session.access_token.clone())
        .filter(|t| !t.is_empty());
    let device_id = stored
        .as_ref()
        .map(|s| s.device.id.clone())
        .filter(|d| !d.is_empty());
    // Best-effort DEVICE revoke — sets revoked_at server-side so the SPIFFE
    // drops from the ACL and the connector closes the gate. gRPC, mirroring
    // enroll/get-ACL. Failures logged and swallowed; local logout must proceed.
    let saved_ca = stored
        .as_ref()
        .map(|s| s.device.ca_cert_pem.as_str())
        .unwrap_or_default();
    if let (Some(token), Some(dev)) = (access_token.as_ref(), device_id.as_ref()) {
        let revoked = match crate::trust::embedded_platform_root_pem() {
            Ok(root) => revoke_device(&conf, token, dev, saved_ca, root).await,
            Err(e) => Err(e),
        };
        if let Err(e) = revoked {
            eprintln!(
                "warning: server-side device revoke failed: {e:#} (local session will still be cleared)"
            );
        }
    }

    // Best-effort SESSION revoke — invalidates the refresh token in Redis.
    if let Some(token) = access_token.as_ref() {
        if let Err(e) = crate::auth::logout(&conf, saved_ca, token).await {
            eprintln!(
                "warning: server-side logout failed: {e:#} (local session will still be cleared)"
            );
        }
    }

    // Best-effort shutdown — drop runtime state from daemon memory.
    let _ = send_ipc(&IpcRequest::Shutdown).await;

    if crate::state_store::clear_workspace_state(&conf.workspace)? {
        println!("Logged out of {}.", conf.workspace);
    } else {
        println!("No saved session to clear.");
    }
    Ok(())
}
/// Certificate audit bug 1 (client): the controller connection trusts the
/// anchors saved at enrollment (checked against `platform_root_pem`), not a
/// fresh plain-HTTP `/ca.crt`. Only with nothing saved does it fall back to
/// the download, pinned to the platform root exactly like login.
pub(crate) async fn revoke_device(
    conf: &crate::config::ClientConf,
    access_token: &str,
    device_id: &str,
    saved_ca_pem: &str,
    platform_root_pem: &str,
) -> Result<()> {
    let mut grpc = if saved_ca_pem.is_empty() {
        crate::login::connect_controller(conf, platform_root_pem)
            .await?
            .0
    } else {
        crate::daemon::check_saved_trust(saved_ca_pem, platform_root_pem)?;
        connect_grpc(conf.controller(), saved_ca_pem).await?
    };
    grpc.revoke_device(RevokeDeviceRequest {
        access_token: access_token.to_string(),
        device_id: device_id.to_string(),
    })
    .await?;
    Ok(())
}
