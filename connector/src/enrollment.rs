// enrollment.rs — Full 10-step enrollment flow for the ZECURITY connector
//
// Exchanges a JWT enrollment token for a signed SPIFFE certificate.
//
// Flow:
//   1. Parse JWT payload (base64url-decode, no signature verification)
//   2. Extract connector_id, workspace_id, trust_domain, ca_fingerprint
//   3. Fetch /ca.crt from controller HTTP endpoint
//   4. Verify CA fingerprint (SHA-256) against JWT claim
//   5. Generate EC P-384 keypair, save to connector.key (mode 0600)
//   6. Build CSR with correct CN and SPIFFE SAN URI
//   7. Connect to controller gRPC (plaintext for now)
//   8. Call Enroll RPC
//   9. Save artifacts (connector.crt, workspace_ca.crt, state.json)
//  10. Return EnrollmentResult

use std::fs;
use std::path::Path;

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use time::OffsetDateTime;
use tonic::transport::Endpoint;
use tracing::{info, warn};

use crate::appmeta;
use crate::config::ConnectorConfig;
use crate::crypto;
use crate::proto;

// ── Data structures ─────────────────────────────────────────────────────────

/// Claims extracted from the enrollment JWT payload.
/// The connector does NOT verify the JWT signature — it has no JWT_SECRET.
/// Trust is established by verifying the CA certificate fingerprint.
#[derive(Debug, Deserialize)]
struct JwtClaims {
    #[serde(rename = "jti")]
    _jti: String,
    connector_id: String,
    workspace_id: String,
    trust_domain: String,
    ca_fingerprint: String,
}

/// Persistent enrollment state saved to state.json after successful enrollment.
#[derive(Debug, Serialize, Deserialize)]
pub struct EnrollmentState {
    pub connector_id: String,
    pub trust_domain: String,
    pub workspace_id: String,
    pub enrolled_at: String,
}

/// Result returned to main.rs after enrollment succeeds.
pub struct EnrollmentResult {
    pub connector_id: String,
    pub trust_domain: String,
}

// ── Public entry point ──────────────────────────────────────────────────────

/// Run the full enrollment flow.
///
/// Called from main.rs when state.json does not exist.
/// Exits the process on fatal errors (e.g., MITM detection).
pub async fn enroll(cfg: &ConnectorConfig) -> Result<EnrollmentResult> {
    let token = cfg
        .enrollment_token
        .as_deref()
        .context("ENROLLMENT_TOKEN is required for first-run enrollment")?;

    // Step 1: Parse JWT payload
    let claims = parse_jwt_payload(token)?;
    info!(
        connector_id = %claims.connector_id,
        workspace_id = %claims.workspace_id,
        trust_domain = %claims.trust_domain,
        "parsed enrollment token"
    );

    // Step 3: Fetch /ca.crt
    let http_addr = cfg
        .controller_http_addr
        .clone()
        .unwrap_or_else(|| derive_http_addr(&cfg.controller_addr));

    let ca_pem = fetch_ca_cert(&http_addr).await?;
    info!("fetched CA certificate from controller");

    // Step 4: Verify CA fingerprint
    verify_ca_fingerprint(&ca_pem, &claims.ca_fingerprint)?;
    info!("CA fingerprint verified");

    // Step 5: Generate keypair and save private key
    let key_pair = crypto::generate_keypair()?;
    let key_path = Path::new(&cfg.state_dir).join("connector.key");
    crypto::save_private_key(&key_pair, &key_path)?;
    info!(path = %key_path.display(), "saved private key");

    // Step 6: Build CSR
    let cn = format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, claims.connector_id);
    let spiffe_uri = appmeta::connector_spiffe_id(&claims.trust_domain, &claims.connector_id);
    let csr_der = crypto::build_csr(&key_pair, &cn, &spiffe_uri)?;
    info!(cn = %cn, san = %spiffe_uri, "built CSR");

    // Step 7: Connect to controller gRPC
    let grpc_addr = format!("http://{}", cfg.controller_addr);
    let channel = Endpoint::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid gRPC address: {}", grpc_addr))?
        .connect()
        .await
        .with_context(|| format!("failed to connect to {}", grpc_addr))?;
    let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);
    info!(addr = %grpc_addr, "connected to controller gRPC");

    // Step 8: Call Enroll RPC
    let hostname = read_hostname();
    let request = tonic::Request::new(proto::EnrollRequest {
        enrollment_token: token.to_string(),
        csr_der,
        version: env!("CARGO_PKG_VERSION").to_string(),
        hostname,
    });

    let response = client
        .enroll(request)
        .await
        .context("Enroll RPC call failed")?
        .into_inner();
    info!("enrollment successful");

    // Step 9: Save artifacts
    let state_dir = Path::new(&cfg.state_dir);
    fs::create_dir_all(state_dir)
        .with_context(|| format!("failed to create state directory: {}", state_dir.display()))?;

    // Save leaf cert
    let cert_path = state_dir.join("connector.crt");
    fs::write(&cert_path, &response.certificate_pem)
        .with_context(|| format!("failed to write {}", cert_path.display()))?;
    info!(path = %cert_path.display(), "saved connector certificate");

    // Save CA chain (workspace CA + intermediate CA concatenated)
    let ca_chain_path = state_dir.join("workspace_ca.crt");
    let workspace_ca = String::from_utf8(response.workspace_ca_pem)
        .context("workspace_ca_pem is not valid UTF-8")?;
    let intermediate_ca = String::from_utf8(response.intermediate_ca_pem)
        .context("intermediate_ca_pem is not valid UTF-8")?;
    let ca_chain = format!("{}\n{}", workspace_ca, intermediate_ca);
    fs::write(&ca_chain_path, &ca_chain)
        .with_context(|| format!("failed to write {}", ca_chain_path.display()))?;
    info!(path = %ca_chain_path.display(), "saved CA chain");

    // Save state.json
    let enrolled_at = OffsetDateTime::now_utc()
        .format(&time::format_description::well_known::Rfc3339)
        .context("failed to format RFC 3339 timestamp")?;

    let state = EnrollmentState {
        connector_id: claims.connector_id.clone(),
        trust_domain: claims.trust_domain.clone(),
        workspace_id: claims.workspace_id.clone(),
        enrolled_at,
    };

    let state_path = state_dir.join("state.json");
    let state_json = serde_json::to_string_pretty(&state)
        .context("failed to serialize enrollment state")?;
    fs::write(&state_path, state_json)
        .with_context(|| format!("failed to write {}", state_path.display()))?;
    info!(path = %state_path.display(), "saved enrollment state");

    // Step 10: Return result
    Ok(EnrollmentResult {
        connector_id: claims.connector_id,
        trust_domain: claims.trust_domain,
    })
}

// ── Internal helpers ────────────────────────────────────────────────────────

/// Step 1: Parse the JWT payload without signature verification.
///
/// The JWT format is: header.payload.signature
/// We only decode the middle segment (payload), which is base64url-encoded JSON.
fn parse_jwt_payload(token: &str) -> Result<JwtClaims> {
    let parts: Vec<&str> = token.split('.').collect();
    if parts.len() != 3 {
        bail!("invalid JWT format: expected 3 dot-separated segments, got {}", parts.len());
    }

    let payload_bytes = URL_SAFE_NO_PAD
        .decode(parts[1])
        .context("failed to base64url-decode JWT payload")?;

    let claims: JwtClaims =
        serde_json::from_slice(&payload_bytes).context("failed to deserialize JWT claims")?;

    Ok(claims)
}

/// Step 3: Fetch the CA certificate from the controller's HTTP endpoint.
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let client = Client::builder()
        .build()
        .context("failed to build HTTP client")?;

    let url = format!("http://{}/ca.crt", http_addr);
    let resp = client
        .get(&url)
        .send()
        .await
        .with_context(|| format!("failed to GET {}", url))?;

    if !resp.status().is_success() {
        bail!(
            "failed to fetch CA cert: HTTP {} from {}",
            resp.status(),
            url
        );
    }

    let pem = resp
        .text()
        .await
        .context("failed to read CA cert response body")?;

    if !pem.contains("-----BEGIN CERTIFICATE-----") {
        bail!("response from {} does not contain a valid PEM certificate", url);
    }

    Ok(pem)
}

/// Step 4: Verify the CA certificate fingerprint matches the JWT claim.
///
/// Parses the PEM → DER bytes, computes SHA-256, hex-encodes, and compares.
/// Mismatch → bail with MITM warning.
fn verify_ca_fingerprint(ca_pem: &str, expected_fingerprint: &str) -> Result<()> {
    // Parse PEM to DER using rustls_pemfile
    let certs = rustls_pemfile::certs(&mut ca_pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse PEM certificates")?;

    if certs.is_empty() {
        bail!("no certificates found in PEM data");
    }

    // Compute SHA-256 of the first cert's DER bytes
    let der_bytes = &certs[0];
    let hash = Sha256::digest(der_bytes);
    let fingerprint = hex::encode(hash);

    if fingerprint != expected_fingerprint {
        warn!(
            expected = expected_fingerprint,
            actual = %fingerprint,
            "CA FINGERPRINT MISMATCH — possible MITM attack, aborting!"
        );
        bail!(
            "CA fingerprint mismatch! Expected {}, got {}. Aborting — possible MITM.",
            expected_fingerprint,
            fingerprint
        );
    }

    Ok(())
}

/// Derive the HTTP address from the gRPC address.
///
/// If `controller_http_addr` is not set in config, we assume the HTTP server
/// runs on port 8080 of the same host as the gRPC server (port 9090).
///
/// Example: "controller.example.com:9090" → "controller.example.com:8080"
fn derive_http_addr(grpc_addr: &str) -> String {
    // Split host:port, replace port with 8080
    if let Some(colon_pos) = grpc_addr.rfind(':') {
        let host = &grpc_addr[..colon_pos];
        return format!("{}:8080", host);
    }
    // Fallback: append :8080
    format!("{}:8080", grpc_addr)
}

/// Read the system hostname from /etc/hostname.
/// Falls back to "unknown" if the file is missing or unreadable.
fn read_hostname() -> String {
    fs::read_to_string("/etc/hostname")
        .map(|h| h.trim().to_string())
        .unwrap_or_else(|_| "unknown".to_string())
}
