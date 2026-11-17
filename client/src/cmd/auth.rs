//! `zecurity-client auth` — step-up authentication (PENDING-06 M-5).
//!
//! A resource policy can require a recent sign-in, optionally with MFA. When
//! the device's last authentication is too old (or its risk score demands a
//! new one), the controller leaves those resources out of the device's ACL
//! and reports how many are waiting (`auth_required_count`). This command
//! asks the controller for a step-up URL, opens it in the browser, receives
//! the controller's one-time code on a loopback address and redeems it
//! (`CompleteReauth`), so only this client can finish its own step-up. The device is not re-enrolled and the tunnel
//! stays up.

use std::time::Duration;

use anyhow::{anyhow, Context, Result};

use crate::grpc::{
    client_v1::{CompleteReauthRequest, StartReauthRequest},
    connect_grpc,
};
use crate::ipc::{send_ipc, IpcRequest, IpcResponse};

/// How long to wait for the browser flow before giving up.
const WAIT_TIMEOUT: Duration = Duration::from_secs(300);

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

    // A sync first: it refreshes the access token if it expired.
    let _ = sync_count().await;
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

    // Bind the browser flow to this client, like sign-in: the controller
    // sends its one-time code to our loopback address only, and nothing is
    // stamped until we redeem it with the PKCE verifier.
    let (code_verifier, code_challenge) = crate::login::new_pkce();
    let (local_redirect_uri, rx) = crate::login::start_loopback().await?;

    let started = grpc
        .start_reauth(StartReauthRequest {
            access_token: token.clone(),
            device_id: device_id.clone(),
            code_challenge,
            local_redirect_uri,
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

    println!("Waiting for you to finish in the browser...");
    let ctrl_code = match tokio::time::timeout(WAIT_TIMEOUT, rx).await {
        Ok(Ok(code)) => code,
        _ => {
            return Err(anyhow!(
                "authentication timed out — run `zecurity-client auth` again"
            ))
        }
    };

    // The browser step can outlast the token taken before it: sync again
    // (the daemon refreshes an expired token, keeping the session) and use
    // the current token. Fall back to the earlier one if that fails.
    let _ = sync_count().await;
    let token = send_ipc(&IpcRequest::GetToken)
        .await
        .ok()
        .and_then(|r| r.token)
        .filter(|t| !t.is_empty())
        .unwrap_or(token);

    let done = grpc
        .complete_reauth(CompleteReauthRequest {
            access_token: token,
            device_id,
            session_id: started.session_id,
            ctrl_code,
            code_verifier,
        })
        .await
        .map_err(|s| match s.code() {
            tonic::Code::FailedPrecondition => {
                anyhow!("{} — run `zecurity-client login`", s.message())
            }
            _ => anyhow!("authentication failed: {}", s.message()),
        })?
        .into_inner();

    // Pull the new ACL now rather than waiting for the next change hint.
    let after = sync_count().await.unwrap_or(done.auth_required_count);
    match after {
        0 => println!("Authenticated. Run `zecurity-client resources` to see what you can reach."),
        n if !done.mfa => println!(
            "Authenticated. {n} resource(s) still need a second factor — run `zecurity-client auth` again to set one up."
        ),
        n => println!("Authenticated. {n} resource(s) are still waiting — run `zecurity-client status`."),
    }
    Ok(())
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
