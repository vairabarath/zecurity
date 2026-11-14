//! `zecurity-client auth` — step-up authentication (PENDING-06 M-5).
//!
//! A resource policy can require a recent sign-in, optionally with MFA. When
//! the device's last authentication is too old (or its risk score demands a
//! new one), the controller leaves those resources out of the device's ACL
//! and reports how many are waiting (`auth_required_count`). This command
//! asks the controller for a step-up URL, opens it in the browser and waits
//! until the resources appear. The device is not re-enrolled and the tunnel
//! stays up.

use std::time::Duration;

use anyhow::{anyhow, Context, Result};

use crate::grpc::{client_v1::StartReauthRequest, connect_grpc};
use crate::ipc::{send_ipc, IpcRequest, IpcResponse};

/// How long to wait for the browser flow before giving up.
const WAIT_TIMEOUT: Duration = Duration::from_secs(300);
const POLL_INTERVAL: Duration = Duration::from_secs(2);

/// The line `status` and `resources` print when resources are waiting for an
/// authentication. None when nothing is waiting or the count is unknown.
pub fn auth_required_line(count: Option<u32>) -> Option<String> {
    match count {
        Some(1) => Some(
            "Sign-in:    1 resource needs you to authenticate again — run `zecurity-client auth`"
                .into(),
        ),
        Some(n) if n > 1 => Some(format!(
            "Sign-in:    {n} resources need you to authenticate again — run `zecurity-client auth`"
        )),
        _ => None,
    }
}

pub async fn run() -> Result<()> {
    let Ok(conf) = crate::config::load() else {
        println!("Not configured. Run `zecurity-client setup --workspace <name>` first.");
        return Ok(());
    };

    // A sync first: it refreshes the access token if it expired and tells us
    // how many resources are waiting.
    let before = sync_count().await;
    let status = send_ipc(&IpcRequest::Status)
        .await
        .map_err(|_| anyhow!("the Zecurity daemon is not running — run `zecurity-client login`"))?;
    if let Some(marker) = status.device_state.as_deref() {
        return Err(anyhow!(
            "this device can't authenticate ({marker}) — run `zecurity-client login`"
        ));
    }
    let device_id = status
        .device_id
        .filter(|d| !d.is_empty())
        .ok_or_else(|| anyhow!("no enrolled device — run `zecurity-client login`"))?;
    let token = send_ipc(&IpcRequest::GetToken)
        .await
        .ok()
        .and_then(|r| r.token)
        .filter(|t| !t.is_empty())
        .ok_or_else(|| anyhow!("no active session — run `zecurity-client login`"))?;

    // The controller connection trusts the anchors saved at enrollment.
    let stored = crate::state_store::load_workspace_state(&conf.workspace)
        .context("load saved device state")?;
    let saved_ca = stored.device.ca_cert_pem.as_str();
    if saved_ca.is_empty() {
        return Err(anyhow!("no saved controller trust — run `zecurity-client login`"));
    }
    crate::daemon::check_saved_trust(saved_ca, crate::trust::embedded_platform_root_pem()?)?;
    let mut grpc = connect_grpc(conf.controller(), saved_ca).await?;
    let started = grpc
        .start_reauth(StartReauthRequest {
            access_token: token,
            device_id,
        })
        .await
        .map_err(|s| match s.code() {
            tonic::Code::Unimplemented => {
                anyhow!("this controller does not support step-up authentication")
            }
            tonic::Code::FailedPrecondition | tonic::Code::PermissionDenied => {
                anyhow!("{} — run `zecurity-client login`", s.message())
            }
            _ => anyhow!("could not start authentication: {}", s.message()),
        })?
        .into_inner();

    println!("Opening browser to authenticate...");
    println!("If the browser doesn't open, visit:\n{}", started.auth_url);
    #[cfg(any(target_os = "android", target_os = "ios"))]
    crate::mobile::open_url(&started.auth_url);
    #[cfg(not(any(target_os = "android", target_os = "ios")))]
    open::that(&started.auth_url).ok();

    // Nothing was waiting: there is no signal to wait for. The controller
    // pushes the change to the daemon when the browser flow completes.
    if !matches!(before, Some(n) if n > 0) {
        println!("Finish in the browser. Your resources update on their own.");
        return Ok(());
    }

    println!("Waiting for you to finish in the browser...");
    let deadline = tokio::time::Instant::now() + WAIT_TIMEOUT;
    loop {
        tokio::time::sleep(POLL_INTERVAL).await;
        match sync_count().await {
            Some(0) => {
                println!("Authenticated. Run `zecurity-client resources` to see what you can reach.");
                return Ok(());
            }
            // A partial result (for example MFA skipped) still counts as done.
            Some(n) if Some(n) < before => {
                println!(
                    "Authenticated. {n} resource(s) still need a second factor — run `zecurity-client auth` again to set one up."
                );
                return Ok(());
            }
            _ => {}
        }
        if tokio::time::Instant::now() >= deadline {
            println!("Still waiting. Finish in the browser, then run `zecurity-client status`.");
            return Ok(());
        }
    }
}

/// Asks the daemon to sync the ACL and returns how many resources wait for
/// an authentication (None when the daemon can't say).
async fn sync_count() -> Option<u32> {
    match send_ipc(&IpcRequest::Sync).await {
        Ok(IpcResponse {
            ok: true,
            auth_required_count,
            ..
        }) => auth_required_count,
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::auth_required_line;

    #[test]
    fn line_only_when_resources_wait() {
        assert_eq!(auth_required_line(None), None);
        assert_eq!(auth_required_line(Some(0)), None);
        assert!(auth_required_line(Some(1)).unwrap().contains("1 resource needs"));
        assert!(auth_required_line(Some(3)).unwrap().contains("3 resources need"));
    }
}
