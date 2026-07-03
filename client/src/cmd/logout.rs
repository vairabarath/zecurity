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
    if let (Some(token), Some(dev)) = (access_token.as_ref(), device_id.as_ref()) {
        if let Err(e) = revoke_device(&conf, token, dev).await {
            eprintln!("warning: server-side device revoke failed: {e:#} (local session will still be cleared)");
        }
    }

    // Best-effort SESSION revoke — invalidates the refresh token in Redis.
    if let Some(token) = access_token.as_ref() {
        if let Err(e) = crate::auth::logout(&conf, token).await {
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
async fn revoke_device(
    conf: &crate::config::ClientConf,
    access_token: &str,
    device_id: &str,
) -> Result<()> {
    let ca_pem = crate::login::fetch_controller_ca(conf).await?;
    let mut grpc = connect_grpc(conf.controller(), &ca_pem).await?;
    grpc.revoke_device(RevokeDeviceRequest {
        access_token: access_token.to_string(),
        device_id: device_id.to_string(),
    })
    .await?;
    Ok(())
}
