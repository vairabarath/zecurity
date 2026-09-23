use anyhow::{anyhow, Context, Result};
use axum::{extract::Query, response::Html, routing::get, Router};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use rand::RngCore;
use rcgen::{CertificateParams, DistinguishedName, KeyPair};
use sha2::{Digest, Sha256};
use std::{collections::HashMap, sync::Arc};
use tokio::sync::oneshot;
use x509_parser::prelude::*;

use crate::{
    config::ClientConf,
    grpc::{client_v1::*, connect_grpc},
    runtime::{DeviceInfo, SessionInfo, UserInfo, WorkspaceInfo},
};

pub struct LoginResult {
    pub workspace: WorkspaceInfo,
    pub user: UserInfo,
    pub device: DeviceInfo,
    pub session: SessionInfo,
}

/// Picks the key backend for a newly enrolling device (PENDING-17) and
/// returns `(signing key, software PEM, TPM key material)` — exactly one of
/// the latter two is populated, mirroring `StoredDevice`'s two fields.
///
/// `ZECURITY_TPM_KEYS=1` means the device MUST be hardware-backed: the TPM is
/// used, or enrollment fails. Without it, a software key is used and no TPM
/// is consulted. This is the ONLY place that choice is made: it's baked into
/// the device's persisted state at enrollment and never re-decided, so a
/// device can't silently downgrade from hardware- to software-backed later
/// (which would also break the controller's pinned-fingerprint check, since
/// the key would differ).
fn generate_device_key() -> Result<(KeyPair, String, Option<String>)> {
    #[cfg(target_os = "linux")]
    if tpm_mode_required() {
        // Fail closed. `ZECURITY_TPM_KEYS=1` is a statement that this device
        // MUST have a hardware-backed identity, so an unreachable TPM is an
        // enrollment failure, not a cue to quietly mint a software key. The
        // silent-fallback alternative is actively dangerous: an administrator
        // would believe a fleet is hardware-backed when parts of it are not,
        // and nothing downstream — cert, CSR, or handshake — reveals the
        // difference.
        let (key_material, _public_key_raw) = crate::tpm::generate().context(
            "ZECURITY_TPM_KEYS=1 requires a TPM-backed device key, but no usable TPM 2.0 \
             device is reachable. Either this machine has no TPM, or this process cannot \
             open /dev/tpmrm0 (the enrolling user usually needs to be in the 'tss' group). \
             Refusing to enroll with a software key, which would not be hardware-backed. \
             Unset ZECURITY_TPM_KEYS to enroll with a software key deliberately.",
        )?;
        let serialized = crate::tpm::serialize_key_material(&key_material)?;
        let key_pair = crate::tpm::load(key_material)?;
        println!("Using TPM-backed device key (hardware-protected, non-exportable).");
        return Ok((key_pair, String::new(), Some(serialized)));
    }

    // TPM mode not requested: software key, as before. Capability detection
    // is deliberately NOT used to auto-upgrade here — "use TPM if one
    // happens to exist" and "this device must use TPM" are different security
    // policies, and only the explicit request carries the second meaning.
    let key_pair = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P384_SHA384)?;
    let private_key_pem = key_pair.serialize_pem();
    Ok((key_pair, private_key_pem, None))
}

/// Whether the operator has demanded a hardware-backed identity for this
/// device. Kept a single predicate so the meaning of the flag lives in one
/// place: requested means required.
///
/// Anything set that isn't explicitly falsy counts as requested. Matching
/// only the literal `"1"` would mean `ZECURITY_TPM_KEYS=true` silently
/// enrolls a software key — reintroducing, through a typo, exactly the
/// false-confidence failure this flag was made fail-closed to prevent. An
/// unrecognised value therefore errs toward requiring the TPM, which fails
/// loudly and is trivially diagnosed.
#[cfg(target_os = "linux")]
fn tpm_mode_required() -> bool {
    match std::env::var("ZECURITY_TPM_KEYS") {
        Ok(value) => !matches!(
            value.trim().to_ascii_lowercase().as_str(),
            "" | "0" | "false" | "no" | "off"
        ),
        Err(_) => false,
    }
}

#[cfg(all(test, target_os = "linux"))]
mod tpm_mode_tests {
    use super::tpm_mode_required;

    /// Pins the flag contract. The asymmetry is deliberate: only explicitly
    /// falsy values opt out, so a mistyped "truthy" value still demands a
    /// TPM rather than quietly producing a software identity.
    #[test]
    fn only_explicitly_falsy_values_disable_required_tpm_mode() {
        for requested in ["1", "true", "TRUE", "yes", "on", " 1 ", "enabled"] {
            std::env::set_var("ZECURITY_TPM_KEYS", requested);
            assert!(
                tpm_mode_required(),
                "{requested:?} should require a TPM-backed key"
            );
        }
        for not_requested in ["0", "false", "FALSE", "no", "off", ""] {
            std::env::set_var("ZECURITY_TPM_KEYS", not_requested);
            assert!(
                !tpm_mode_required(),
                "{not_requested:?} should not require a TPM-backed key"
            );
        }
        std::env::remove_var("ZECURITY_TPM_KEYS");
        assert!(!tpm_mode_required(), "unset must not require a TPM");
    }
}

pub async fn run(conf: &ClientConf, invite_token: Option<String>) -> Result<LoginResult> {
    let ca_pem = fetch_controller_ca(conf).await?;
    let mut grpc = connect_grpc(conf.controller(), &ca_pem).await?;

    // CLI-Controller PKCE — CLI generates this pair.
    // code_challenge is sent to the controller in InitiateAuth.
    // code_verifier is kept locally and sent in TokenExchange.
    // The controller verifies SHA256(code_verifier) == code_challenge.
    let mut verifier_bytes = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut verifier_bytes);
    let code_verifier = URL_SAFE_NO_PAD.encode(verifier_bytes);
    let code_challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(code_verifier.as_bytes()));

    // Local callback server — receives the ctrl_code from the controller's
    // redirect after it handles the Google OAuth callback server-side.
    let (tx, rx) = oneshot::channel::<String>();
    let tx = Arc::new(tokio::sync::Mutex::new(Some(tx)));
    let tx_clone = tx.clone();

    let app = Router::new().route(
        "/callback",
        get(move |Query(params): Query<HashMap<String, String>>| {
            let tx = tx_clone.clone();
            async move {
                if let Some(code) = params.get("code") {
                    if let Some(sender) = tx.lock().await.take() {
                        let _ = sender.send(code.clone());
                    }
                }
                Html(
                    "<html><body><h2>Authentication complete. \
                     You can close this tab.</h2></body></html>",
                )
            }
        }),
    );

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let port = listener.local_addr()?.port();
    let local_redirect_uri = format!("http://127.0.0.1:{}/callback", port);
    tokio::spawn(async move {
        axum::serve(listener, app).await.ok();
    });

    // InitiateAuth — controller builds the Google OAuth URL, stores the
    // PKCE session, and returns the full auth_url. The CLI never constructs
    // the Google URL directly. The controller's fixed /api/clients/callback
    // is embedded in auth_url as the redirect_uri.
    println!("Initiating authentication...");
    let initiated = grpc
        .initiate_auth(InitiateAuthRequest {
            workspace_slug: conf.workspace.clone(),
            code_challenge,
            local_redirect_uri,
        })
        .await?
        .into_inner();

    // Open browser with the controller-built auth URL.
    println!("Opening browser for authentication...");
    println!(
        "If the browser doesn't open, visit:\n{}",
        initiated.auth_url
    );
    open::that(&initiated.auth_url).ok();

    // Wait for ctrl_code delivered by the controller's callback redirect
    // to our local server (5 minute timeout).
    let ctrl_code = tokio::time::timeout(std::time::Duration::from_secs(300), rx)
        .await
        .map_err(|_| anyhow!("Login timed out after 5 minutes"))??;

    // TokenExchange — presents session_id, ctrl_code, and code_verifier.
    // The controller verifies ctrl_code matches its session record and that
    // SHA256(code_verifier) == code_challenge from InitiateAuth (PKCE).
    println!("Exchanging token...");
    let tok = grpc
        .token_exchange(TokenExchangeRequest { // validtates the ctrl_code in service.go
            session_id: initiated.session_id,
            ctrl_code,
            code_verifier,
            invite_token: invite_token.unwrap_or_default(),
        })
        .await?
        .into_inner(); //accesstoken, refreshtoken, expiresin, email

    println!("Generating device certificate...");
    // PENDING-17: with TPM mode requested, the private half never exists
    // outside the chip, so a stolen state file (or a root-level disk read)
    // can't impersonate this device. Both backends converge on an
    // rcgen::KeyPair, so everything downstream — CSR building, enrollment,
    // later renewal — is identical.
    let (key_pair, private_key_pem, tpm_key_material) = generate_device_key()?;

    let hostname = hostname::get()
        .unwrap_or_default()
        .to_string_lossy()
        .to_string(); // gets the device name and make this as owner of the value
    let os = std::env::consts::OS.to_string();

    let mut params = CertificateParams::default(); // creates empty certificate request parameters
    params.distinguished_name = DistinguishedName::new(); // creates empty x.509 identity fields initialy cn = "", o="", ou = "" all empty
    params
        .distinguished_name
        .push(rcgen::DnType::CommonName, &hostname); // adds cn = desktop-abc123 'the host name'
    let csr_pem = params.serialize_request(&key_pair)?.pem()?;// creates the signing request contains public key and cn and signature using the private key

    // EnrollDevice — unchanged from the original flow.
    let enroll = grpc
        .enroll_device(EnrollDeviceRequest {
            access_token: tok.access_token.clone(),
            csr_pem,
            device_name: hostname.clone(),
            os: os.clone(),
        })/*{
                                                "access_token": "jwt...",
                                                "csr_pem": "-----BEGIN CERTIFICATE REQUEST-----",
                                                "device_name": "DESKTOP-ABC123",
                                                "os": "linux"
                                                } */
        .await?
        .into_inner();

    // Build CA chain — concatenate workspace CA + intermediate.
    let ca_cert_pem = format!(
        "{}\n{}",
        enroll.workspace_ca_pem, enroll.intermediate_ca_pem
    );
    let cert_expires_at = certificate_not_after_unix(&enroll.certificate_pem)?;

    use std::time::{SystemTime, UNIX_EPOCH};
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;

    Ok(LoginResult {
        workspace: WorkspaceInfo {
            id: String::new(),
            name: conf.workspace.clone(),
            slug: conf.workspace.clone(),
            trust_domain: extract_trust_domain(&enroll.spiffe_id),
        },
        user: UserInfo {
            id: String::new(),
            email: tok.email.clone(),
            role: String::new(),
        },
        device: DeviceInfo {
            id: enroll.device_id,
            spiffe_id: enroll.spiffe_id,
            certificate_pem: enroll.certificate_pem,
            private_key_pem,
            tpm_key_material,
            ca_cert_pem,
            cert_expires_at,
            hostname,
            os,
        },
        session: SessionInfo {
            access_token: tok.access_token,
            refresh_token: tok.refresh_token,
            expires_at: now + tok.expires_in,
        },
    })
}

pub async fn fetch_controller_ca(conf: &ClientConf) -> Result<String> {
    let url = format!("{}/ca.crt", conf.http_base());
    let response = reqwest::get(&url)
        .await
        .with_context(|| format!("fetch controller CA from {}", url))?;

    if !response.status().is_success() {
        return Err(anyhow!(
            "fetch controller CA from {}: HTTP {}",
            url,
            response.status()
        ));
    }

    let ca_pem = response
        .text()
        .await
        .with_context(|| format!("read controller CA from {}", url))?;

    if !ca_pem.contains("BEGIN CERTIFICATE") {
        return Err(anyhow!("controller CA response from {} was not PEM", url));
    }

    Ok(ca_pem)
}

fn extract_trust_domain(spiffe_id: &str) -> String {
    // "spiffe://ws-slug.zecurity.in/client/uuid" → "ws-slug.zecurity.in"
    spiffe_id
        .strip_prefix("spiffe://")
        .and_then(|s| s.split('/').next())
        .unwrap_or("")
        .to_string()
}

fn certificate_not_after_unix(certificate_pem: &str) -> Result<i64> {
    let (_, pem) = parse_x509_pem(certificate_pem.as_bytes())
        .map_err(|err| anyhow!("parse issued certificate PEM: {err}"))?;
    let (_, cert) = X509Certificate::from_der(&pem.contents)
        .map_err(|err| anyhow!("parse issued certificate DER: {err}"))?;
    Ok(cert.validity().not_after.timestamp())
}
