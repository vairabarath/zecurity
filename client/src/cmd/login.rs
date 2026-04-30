use anyhow::Result;

use crate::{
    config::load,
    ipc::{ensure_daemon_and_send, IpcRequest},
    login,
};

pub async fn run() -> Result<()> {
    let conf = load()?;

    println!("Authenticating...");
    let result = login::run(&conf, None).await?;

    let email = result.user.email.clone();
    let workspace = result.workspace.name.clone();
    let device_id = result.device.id.clone();

    ensure_daemon_and_send(&IpcRequest::PostLoginState {
        workspace_slug:  conf.workspace.clone(),
        workspace_name:  result.workspace.name,
        workspace_id:    result.workspace.id,
        trust_domain:    result.workspace.trust_domain,
        user_email:      result.user.email,
        access_token:    result.session.access_token,
        refresh_token:   result.session.refresh_token,
        expires_at:      result.session.expires_at,
        device_id:       result.device.id,
        spiffe_id:       result.device.spiffe_id,
        certificate_pem: result.device.certificate_pem,
        private_key_pem: result.device.private_key_pem,
        ca_cert_pem:     result.device.ca_cert_pem,
        cert_expires_at: result.device.cert_expires_at,
        hostname:        result.device.hostname,
        os:              result.device.os,
    })
    .await?;

    println!("Logged in as {}", email);
    println!("Workspace: {}", workspace);
    println!("Device ID: {}", device_id);
    Ok(())
}
