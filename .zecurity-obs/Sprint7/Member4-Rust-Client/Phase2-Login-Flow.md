---
type: phase
status: done
sprint: 7
member: M4
phase: Phase2-Login-Flow
depends_on:
  - M4-Phase1 (CLI scaffold done)
  - M3-Phase1 (ClientService gRPC live on Controller)
tags:
  - rust
  - cli
  - grpc
  - oauth
  - pki
---

# M4 Phase 2 — Login Flow (PKCE + gRPC + Device Enrollment)

---

## What You're Building

The authentication and enrollment logic that runs at the start of `connect`. Everything stays in `RuntimeState` — nothing is written to disk.

Flow:
1. Read `client.conf` → workspace slug + controller address
2. gRPC `GetAuthConfig` → Google OAuth parameters
3. PKCE + local callback server → capture Google auth code
4. gRPC `TokenExchange` → Zecurity JWT + refresh token (in memory)
5. Generate P-384 ECDSA keypair in memory → CSR → gRPC `EnrollDevice` → certificate (in memory)
6. Populate `RuntimeState` and return it to the caller (`connect.rs`)

The private key is generated fresh every time `connect` runs. It never leaves the process.

---

## Proto Stubs in Rust

### `client/build.rs`

```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        .build_server(false)
        .build_client(true)
        .compile(
            &["../proto/client/v1/client.proto"],
            &["../proto"],
        )?;
    Ok(())
}
```

---

## `client/src/grpc.rs`

```rust
use tonic::transport::{Channel, ClientTlsConfig};
use anyhow::Result;

pub mod client_v1 {
    tonic::include_proto!("client.v1");
}
pub use client_v1::client_service_client::ClientServiceClient;

pub async fn connect_grpc(controller_address: &str) -> Result<ClientServiceClient<Channel>> {
    let tls = ClientTlsConfig::new();
    let channel = Channel::from_shared(format!("https://{}", controller_address))?
        .tls_config(tls)?
        .connect()
        .await?;
    Ok(ClientServiceClient::new(channel))
}
```

---

## `client/src/login.rs`

This is a library module, not a command. Called from `connect.rs` at startup.

```rust
use anyhow::{anyhow, Result};
use axum::{extract::Query, response::Html, routing::get, Router};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use rand::RngCore;
use rcgen::{CertificateParams, DistinguishedName, KeyPair};
use sha2::{Digest, Sha256};
use std::{collections::HashMap, sync::Arc};
use tokio::sync::oneshot;

use crate::{
    config::ClientConf,
    grpc::{connect_grpc, client_v1::*},
    runtime::{DeviceInfo, SessionInfo, UserInfo, WorkspaceInfo},
};

pub struct LoginResult {
    pub workspace: WorkspaceInfo,
    pub user:      UserInfo,
    pub device:    DeviceInfo,
    pub session:   SessionInfo,
}

pub async fn run(conf: &ClientConf, invite_token: Option<String>) -> Result<LoginResult> {
    let mut grpc = connect_grpc(conf.controller()).await?;

    // 1. Get auth config
    let auth_cfg = grpc
        .get_auth_config(GetAuthConfigRequest {
            workspace_slug: conf.workspace.clone(),
        })
        .await?
        .into_inner();

    // 2. PKCE
    let mut verifier_bytes = [0u8; 32];
    rand::thread_rng().fill_bytes(&mut verifier_bytes);
    let code_verifier = URL_SAFE_NO_PAD.encode(verifier_bytes);
    let code_challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(code_verifier.as_bytes()));

    // 3. Local callback server
    let (tx, rx) = oneshot::channel::<String>();
    let tx = Arc::new(tokio::sync::Mutex::new(Some(tx)));
    let tx_clone = tx.clone();

    let app = Router::new().route("/callback", get(move |Query(params): Query<HashMap<String, String>>| {
        let tx = tx_clone.clone();
        async move {
            if let Some(code) = params.get("code") {
                if let Some(sender) = tx.lock().await.take() {
                    let _ = sender.send(code.clone());
                }
            }
            Html("<html><body><h2>Authentication complete. You can close this tab.</h2></body></html>")
        }
    }));

    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
    let port = listener.local_addr()?.port();
    let redirect_uri = format!("http://127.0.0.1:{}/callback", port);
    tokio::spawn(async move { axum::serve(listener, app).await.ok(); });

    // 4. Open browser
    let oauth_url = format!(
        "{}?client_id={}&redirect_uri={}&response_type=code\
         &scope=openid%20email&code_challenge={}&code_challenge_method=S256",
        auth_cfg.auth_endpoint,
        auth_cfg.google_client_id,
        urlencoding::encode(&redirect_uri),
        code_challenge,
    );
    println!("Opening browser for authentication...");
    println!("If the browser doesn't open, visit:\n{}", oauth_url);
    open::that(&oauth_url).ok();

    // 5. Wait for code (5 min timeout)
    let code = tokio::time::timeout(
        std::time::Duration::from_secs(300), rx,
    ).await
        .map_err(|_| anyhow!("Login timed out after 5 minutes"))??;

    // 6. TokenExchange
    println!("Exchanging token...");
    let tok = grpc.token_exchange(TokenExchangeRequest {
        workspace_slug: conf.workspace.clone(),
        code,
        code_verifier,
        redirect_uri,
        invite_token: invite_token.unwrap_or_default(),
    }).await?.into_inner();

    // 7. Generate P-384 keypair in memory
    println!("Generating device certificate...");
    let key_pair = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P384_SHA384)?;
    let private_key_pem = key_pair.serialize_pem();  // kept in memory only

    let hostname = hostname::get().unwrap_or_default().to_string_lossy().to_string();
    let os = std::env::consts::OS.to_string();

    let mut params = CertificateParams::default();
    params.distinguished_name = DistinguishedName::new();
    params.distinguished_name.push(rcgen::DnType::CommonName, &hostname);
    let csr_pem = params.serialize_request(&key_pair)?.pem()?;

    // 8. EnrollDevice
    let enroll = grpc.enroll_device(EnrollDeviceRequest {
        access_token: tok.access_token.clone(),
        csr_pem,
        device_name: hostname.clone(),
        os: os.clone(),
    }).await?.into_inner();

    // 9. Build CA chain (concatenate workspace CA + intermediate)
    let ca_cert_pem = format!("{}\n{}", enroll.workspace_ca_pem, enroll.intermediate_ca_pem);

    use std::time::{SystemTime, UNIX_EPOCH};
    let now = SystemTime::now().duration_since(UNIX_EPOCH)?.as_secs() as i64;

    Ok(LoginResult {
        workspace: WorkspaceInfo {
            id:           String::new(),  // populated from TokenExchange response if Controller adds it
            name:         conf.workspace.clone(),
            slug:         conf.workspace.clone(),
            trust_domain: extract_trust_domain(&enroll.spiffe_id),
        },
        user: UserInfo {
            id:    String::new(),
            email: tok.email.clone(),
            role:  String::new(),  // populated from JWT claims if needed
        },
        device: DeviceInfo {
            id:              String::new(),  // from enroll response
            spiffe_id:       enroll.spiffe_id,
            certificate_pem: enroll.certificate_pem,
            private_key_pem,             // never written to disk
            ca_cert_pem,
            cert_expires_at: now + 7 * 24 * 3600,  // 7-day cert TTL
            hostname,
            os,
        },
        session: SessionInfo {
            access_token:  tok.access_token,
            refresh_token: tok.refresh_token,
            expires_at:    now + tok.expires_in,
        },
    })
}

fn extract_trust_domain(spiffe_id: &str) -> String {
    // "spiffe://ws-slug.zecurity.in/client/uuid" → "ws-slug.zecurity.in"
    spiffe_id
        .strip_prefix("spiffe://")
        .and_then(|s| s.split('/').next())
        .unwrap_or("")
        .to_string()
}
```

Wire into `main.rs`:
```rust
mod login;
mod grpc;
```

---

## Build Check

```bash
cd client && cargo build
```

Integration test (requires running Controller):
```bash
./target/debug/zecurity-client connect
# Should open browser, complete OAuth, print "Logged in as user@example.com"
```

---

## Post-Phase Fixes

### Fix: rustls CryptoProvider panic during `zecurity-client login`
**Issue:** `zecurity-client login` panicked before opening OAuth:

```text
Could not automatically determine the process-level CryptoProvider from Rustls crate features.
```

**Root Cause:** The client dependency graph enabled both rustls crypto providers. `tonic`/`tokio-rustls` pulled `ring`, while the direct `rustls` dependency requested `aws_lc_rs`. With more than one provider compiled, rustls requires the process to choose one explicitly.

**Fix Applied:**
```rust
// client/src/main.rs
rustls::crypto::ring::default_provider()
    .install_default()
    .expect("failed to install default crypto provider");
```

Also changed `client/Cargo.toml` to request the `ring` rustls feature directly and disable `tokio-rustls` default features so `aws_lc_rs` is not enabled by the direct client dependency.

### Fix: controller gRPC TLS failed with `UnknownIssuer`
**Issue:** After the rustls provider fix, `zecurity-client login` failed while dialing the controller:

```text
invalid peer certificate: UnknownIssuer
```

**Root Cause:** The controller generates its gRPC server certificate in memory and signs it with Zecurity's intermediate CA. The client used `ClientTlsConfig::new()` with default public roots, so it could not trust the internal CA.

**Fix Applied:**
```rust
// client/src/login.rs
let ca_pem = fetch_controller_ca(conf).await?;
let mut grpc = connect_grpc(conf.controller(), &ca_pem).await?;

// client/src/grpc.rs
let tls = ClientTlsConfig::new()
    .ca_certificate(Certificate::from_pem(ca_pem.as_bytes()))
    .domain_name(controller_host(controller_address));
```

The CLI now fetches the existing public `GET /ca.crt` endpoint before `GetAuthConfig`, uses that PEM as the tonic root CA, and derives the expected TLS server name from `controller_address`. Added `setup --http-base` for dev configs where the HTTP API is not the default `http://<controller-host>:8080`.
