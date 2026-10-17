//! Auth session management for the client daemon.
//!
//! The daemon holds a short-lived access JWT and a long-lived refresh token,
//! both minted by the controller during OAuth login. The access JWT expires
//! every 15 minutes; the refresh token lives 7 days on a rolling idle window.
//!
//! This module exposes the mechanics of exchanging an expired access token
//! plus a valid refresh token for a fresh pair, and of ending the session.
//! Both go over the gRPC channel pinned to the device's saved anchors
//! (`ClientService.RefreshSession` / `Logout`). They used to go to the
//! controller's plain-HTTP `/auth/refresh` and `/auth/logout`, which put the
//! access token and the refresh token on the wire in clear (certificate audit
//! follow-up). The controller shares the session rules with the browser's
//! cookie path; see `controller/internal/auth/refresh.go`.

use anyhow::{anyhow, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use serde::Deserialize;

use crate::config::ClientConf;
use crate::grpc::client_v1::{LogoutRequest, RefreshSessionRequest};
use crate::grpc::connect_grpc;

/// The result of a successful refresh. `expires_at` is the Unix
/// timestamp at which the new access token expires — derived from the JWT's
/// `exp` claim, not from a separate server field, so the client can never be
/// out of sync with the token itself.
#[derive(Debug, Clone)]
pub struct RefreshedTokens {
    pub access_token: String,
    pub refresh_token: String,
    pub expires_at: i64,
}

/// Distinguish a dead session (refresh rejected — user must re-login) from
/// a transient failure (network / server error — caller can retry later).
/// The caller acts differently in each case: a dead session clears local
/// state and prompts login; a transient failure keeps existing state and
/// tries again on the next tick.
#[derive(Debug, thiserror::Error)]
pub enum RefreshError {
    #[error("refresh session dead — user must sign in again")]
    SessionDead,
    #[error("transient refresh failure: {0}")]
    Transient(#[from] anyhow::Error),
}

/// Exchange the current (possibly expired) access token plus refresh token
/// for a fresh pair, over the channel pinned to `ca_pem` (the saved anchors:
/// Workspace CA + Intermediate). Signature is verified server-side; expired
/// access tokens are accepted for identity extraction only. The controller
/// rotates the refresh token on every call — the returned refresh_token must
/// replace the caller's stored value atomically before any subsequent
/// refresh, or the next call will be rejected as a replay.
pub async fn refresh_access_token(
    conf: &ClientConf,
    ca_pem: &str,
    access_token: &str,
    refresh_token: &str,
) -> Result<RefreshedTokens, RefreshError> {
    let mut client = connect_grpc(conf.controller(), ca_pem)
        .await
        .context("connect to controller for RefreshSession")
        .map_err(RefreshError::Transient)?;
    let response = match client
        .refresh_session(RefreshSessionRequest {
            access_token: access_token.to_string(),
            refresh_token: refresh_token.to_string(),
        })
        .await
    {
        Ok(response) => response.into_inner(),
        Err(status) if status.code() == tonic::Code::Unauthenticated => {
            return Err(RefreshError::SessionDead)
        }
        Err(status) => {
            return Err(RefreshError::Transient(anyhow!(
                "RefreshSession failed: {}",
                status.message()
            )))
        }
    };

    if response.access_token.is_empty() || response.refresh_token.is_empty() {
        return Err(RefreshError::Transient(anyhow!(
            "RefreshSession returned empty tokens"
        )));
    }

    let expires_at = extract_exp(&response.access_token)
        .context("read exp claim from refreshed access token")
        .map_err(RefreshError::Transient)?;

    Ok(RefreshedTokens {
        access_token: response.access_token,
        refresh_token: response.refresh_token,
        expires_at,
    })
}

/// Best-effort server-side logout over the channel pinned to `ca_pem`.
/// Invalidates the caller's refresh session in the controller so a leaked
/// refresh token cannot be replayed later. The controller treats logout as
/// idempotent; only a transport failure returns Err, for the caller to log —
/// the caller must still proceed to clear local state.
pub async fn logout(conf: &ClientConf, ca_pem: &str, access_token: &str) -> Result<()> {
    let mut client = connect_grpc(conf.controller(), ca_pem)
        .await
        .context("connect to controller for Logout")?;
    client
        .logout(LogoutRequest {
            access_token: access_token.to_string(),
        })
        .await
        .context("Logout")?;
    Ok(())
}

/// Extract the `exp` claim from a JWT without verifying its signature —
/// the token was just minted by the controller and the transport was TLS,
/// so signature verification here is redundant. Signature is enforced on
/// the server side of every subsequent API call.
fn extract_exp(access_token: &str) -> Result<i64> {
    #[derive(Deserialize)]
    struct ExpClaim {
        exp: i64,
    }
    let payload = access_token
        .split('.')
        .nth(1)
        .ok_or_else(|| anyhow!("access token is not a JWT"))?;
    let decoded = URL_SAFE_NO_PAD
        .decode(payload)
        .context("base64 decode JWT payload")?;
    let claim: ExpClaim = serde_json::from_slice(&decoded).context("parse JWT payload JSON")?;
    Ok(claim.exp)
}
