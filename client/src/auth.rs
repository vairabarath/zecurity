//! Auth session management for the client daemon.
//!
//! The daemon holds a short-lived access JWT and a long-lived refresh token,
//! both minted by the controller during OAuth login. The access JWT expires
//! every 15 minutes; the refresh token lives 7 days on a rolling idle window.
//!
//! This module exposes the mechanics of exchanging an expired access token
//! plus a valid refresh token for a fresh pair. The controller's browser flow
//! uses an httpOnly cookie for the refresh token — the CLI cannot receive
//! cookies cleanly, so we send the refresh token via the `X-Refresh-Token`
//! header and read the rotated value from the JSON response body. The
//! controller-side handler is shared with the browser path; see
//! `controller/internal/auth/refresh.go`.

use anyhow::{anyhow, Context, Result};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use reqwest::StatusCode;
use serde::Deserialize;

use crate::config::ClientConf;

/// The result of a successful `/auth/refresh` call. `expires_at` is the Unix
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
/// for a fresh pair. Signature is verified server-side; expired access
/// tokens are accepted for identity extraction only. The controller rotates
/// the refresh token on every call — the returned refresh_token must
/// replace the caller's stored value atomically before any subsequent
/// refresh, or the next call will be rejected as a replay.
pub async fn refresh_access_token(
    conf: &ClientConf,
    access_token: &str,
    refresh_token: &str,
) -> Result<RefreshedTokens, RefreshError> {
    let url = format!("{}/auth/refresh", conf.http_base());

    // Reqwest defaults trust the platform certificate store, which is fine
    // for HTTPS deployments fronted by a real CA. For dev / plaintext HTTP
    // deployments the transport is trivially compromisable regardless of
    // this client — that's a deployment concern, not a refresh-flow one.
    let response = reqwest::Client::new()
        .post(&url)
        .bearer_auth(access_token)
        .header("X-Refresh-Token", refresh_token)
        .send()
        .await
        .map_err(|e| RefreshError::Transient(e.into()))?;

    match response.status() {
        StatusCode::OK => {}
        StatusCode::UNAUTHORIZED => return Err(RefreshError::SessionDead),
        other => {
            return Err(RefreshError::Transient(anyhow!(
                "unexpected /auth/refresh status: {}",
                other
            )))
        }
    }

    let body: RefreshResponse = response
        .json()
        .await
        .context("decode /auth/refresh response")
        .map_err(RefreshError::Transient)?;

    if body.access_token.is_empty() || body.refresh_token.is_empty() {
        return Err(RefreshError::Transient(anyhow!(
            "/auth/refresh returned empty tokens"
        )));
    }

    let expires_at = extract_exp(&body.access_token)
        .context("read exp claim from refreshed access token")
        .map_err(RefreshError::Transient)?;

    Ok(RefreshedTokens {
        access_token: body.access_token,
        refresh_token: body.refresh_token,
        expires_at,
    })
}

/// JSON body shape returned by the controller's /auth/refresh endpoint when
/// invoked via the X-Refresh-Token header path (see controller-side handler).
#[derive(Debug, Deserialize)]
struct RefreshResponse {
    access_token: String,
    #[serde(default)]
    refresh_token: String,
}

/// Best-effort server-side logout. Invalidates the caller's refresh session
/// in the controller so a leaked refresh token cannot be replayed later.
/// Returns Ok even when the controller answers non-2xx (204 is the success
/// contract; 401 means the token was already dead — either way, we've done
/// what we can). Only returns Err on a transport failure the caller may
/// want to log — the caller must still proceed to clear local state.
pub async fn logout(conf: &ClientConf, access_token: &str) -> Result<()> {
    let url = format!("{}/auth/logout", conf.http_base());
    reqwest::Client::new()
        .post(&url)
        .bearer_auth(access_token)
        .send()
        .await
        .with_context(|| format!("POST {}", url))?;
    // We intentionally do not inspect status — server treats logout as
    // idempotent and returns 204 on both "was signed in" and "was not".
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
