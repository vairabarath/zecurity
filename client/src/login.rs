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

pub async fn run(conf: &ClientConf, invite_token: Option<String>) -> Result<LoginResult> {
    let ca_pem = fetch_controller_ca(conf).await?;
    let mut grpc = connect_grpc(conf.controller(), &ca_pem).await?;

    // CLI-Controller PKCE — CLI generates this pair.
    // code_challenge is sent to the controller in InitiateAuth.
    // code_verifier is kept locally and sent in TokenExchange.
    // The controller verifies SHA256(code_verifier) == code_challenge.
    let mut verifier_bytes = [0u8; 32]; // create empty 32 sized array
    rand::thread_rng().fill_bytes(&mut verifier_bytes); // fills the randome 0's and 1's bytes there
    let code_verifier = URL_SAFE_NO_PAD.encode(verifier_bytes); // creates bash64 encode
    let code_challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(code_verifier.as_bytes())); // create again a base64 encode for the code verifier this is code challenge

    // Local callback server — receives the ctrl_code from the controller's
    // redirect after it handles the Google OAuth callback server-side.
    let (tx, rx) = oneshot::channel::<String>();
    let tx = Arc::new(tokio::sync::Mutex::new(Some(tx)));
    let tx_clone = tx.clone();

    let app = Router::new().route( // definition only for the rout handler
        "/callback",
        get(move |Query(params): Query<HashMap<String, String>>| {
            let tx = tx_clone.clone();
            async move {
                if let Some(code) = params.get("code") { // http://127.0.0.1:53721/callback?code=XYZ then 'code = "XYZ"'
                    if let Some(sender) = tx.lock().await.take() {
                        let _ = sender.send(code.clone());
                    }
                }
                Html(
                    "<html><body><h2>Authentication complete. \
                     You can close this tab.</h2></body></html>", // after the login complete the message is showed to the client
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
    let initiated = grpc //this is send to the controller for the browser to open the login flow
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

    // Generate P-384 keypair in memory — never written to disk.
    println!("Generating device certificate...");
    let key_pair = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P384_SHA384)?; // generates private key and public key
    let private_key_pem = key_pair.serialize_pem();

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

async fn fetch_controller_ca(conf: &ClientConf) -> Result<String> {
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
