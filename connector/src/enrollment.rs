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
//  10. Return EnrollmentState

use std::fs;
use std::path::Path;

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use reqwest::Client;
use rustls::pki_types::CertificateDer;
use rustls_pemfile::certs;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use time::OffsetDateTime;
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint};
use tracing::{info, warn};
use x509_parser::parse_x509_certificate;
use x509_parser::prelude::{FromDer, X509Certificate};

use crate::appmeta;
use crate::config::ConnectorConfig;
use crate::crypto;
use crate::proto;
use crate::util;

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
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct EnrollmentState {
    pub connector_id: String,
    pub trust_domain: String,
    pub workspace_id: String,
    pub enrolled_at: String,
    pub cert_not_after: String,
}
impl EnrollmentState {
    /// Load EnrollmentState from state.json in the given state directory.
    pub fn load(state_dir: &str) -> anyhow::Result<Self> {
        let path = Path::new(state_dir).join("state.json");
        let json = fs::read_to_string(&path)
            .with_context(|| format!("failed to read {}", path.display()))?;
        serde_json::from_str(&json).with_context(|| format!("failed to parse {}", path.display()))
    }

    pub fn save(&self, state_dir: &str) -> anyhow::Result<()> {
        let dir = Path::new(state_dir);
        fs::create_dir_all(dir)
            .with_context(|| format!("failed to create state dir {}", dir.display()))?;
        let path = dir.join("state.json");
        let json =
            serde_json::to_string_pretty(self).context("failed to serialize EnrollmentState")?;
        fs::write(&path, json).with_context(|| format!("failed to write {}", path.display()))
    }
}
// ── Public entry point ──────────────────────────────────────────────────────

/// Run the full enrollment flow.
///
/// Called from main.rs when state.json does not exist.
/// Exits the process on fatal errors (e.g., MITM detection).
pub async fn enroll(cfg: &ConnectorConfig) -> Result<EnrollmentState> {
    let state = bootstrap_enroll(cfg).await?;
    // Step 9b: Clean up config file — remove ENROLLMENT_TOKEN, add CONNECTOR_ID.
    // Best-effort: the config file is typically owned by root:zecurity with mode 0640,
    // so the connector process (running as zecurity) may not have write access.
    cleanup_config_after_enrollment(&state.connector_id);
    Ok(state)
}

/// Enrollment up to and including persisting the enrolled state (steps 1–9);
/// `enroll` adds the best-effort config-file cleanup. Split out so tests can
/// run a full enrollment against a fake controller without touching /etc.
async fn bootstrap_enroll(cfg: &ConnectorConfig) -> Result<EnrollmentState> {
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

    // Step 4: Pin the CA: /ca.crt must be exactly the pinned Platform
    // Intermediate, and only that certificate is trusted from here on.
    let pinned_ca = pinned_bootstrap_ca(&ca_pem, &claims.ca_fingerprint)?;
    info!("CA fingerprint verified");

    // Step 5: Generate the keypair. It stays in memory until the controller's
    // reply has been validated (step 9) — nothing is written before that.
    let key_pair = crypto::generate_keypair()?;

    // Step 6: Build CSR
    let cn = format!(
        "{}{}",
        appmeta::PKI_CONNECTOR_CN_PREFIX,
        claims.connector_id
    );
    let spiffe_uri = appmeta::connector_spiffe_id(&claims.trust_domain, &claims.connector_id);
    let csr_der = crypto::build_csr(&key_pair, &cn, &spiffe_uri)?;
    info!(cn = %cn, san = %spiffe_uri, "built CSR");

    // Step 7: Connect to controller gRPC over TLS rooted in the verified CA.
    let grpc_host = controller_host(&cfg.controller_addr);
    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let channel = Endpoint::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid gRPC address: {}", grpc_addr))?
        .tls_config(
            ClientTlsConfig::new()
                .ca_certificate(Certificate::from_pem(pem_from_der(&pinned_ca)))
                .domain_name(grpc_host.clone()),
        )
        .with_context(|| format!("failed to configure TLS for {}", grpc_addr))?
        .connect()
        .await
        .with_context(|| format!("failed to connect to {}", grpc_addr))?;
    let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);
    info!(addr = %grpc_addr, "connected to controller gRPC");

    // Step 8: Call Enroll RPC
    let hostname = util::read_hostname();
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

    // Step 9: Validate the reply against the pinned Platform Intermediate,
    // then install everything atomically (state.json last). Fail closed: on
    // any mismatch nothing is written.
    let key_spki = crate::tls::cert_holder::key_spki_der(key_pair.serialize_pem().as_bytes())?;
    validate_enrollment_reply(
        &pinned_ca,
        &response.workspace_ca_pem,
        &response.intermediate_ca_pem,
        &response.certificate_pem,
        &spiffe_uri,
        &key_spki,
    )
    .context("enrollment reply rejected")?;
    info!("enrollment reply validated against the pinned Platform Intermediate");

    let leaf_cert = String::from_utf8(response.certificate_pem.clone())
        .context("certificate_pem is not valid UTF-8")?;
    let workspace_ca = String::from_utf8(response.workspace_ca_pem.clone())
        .context("workspace_ca_pem is not valid UTF-8")?;
    let intermediate_ca = String::from_utf8(response.intermediate_ca_pem.clone())
        .context("intermediate_ca_pem is not valid UTF-8")?;
    let cert_not_after = parse_cert_not_after(&response.certificate_pem)?;
    let enrolled_at = OffsetDateTime::now_utc()
        .format(&time::format_description::well_known::Rfc3339)
        .context("failed to format RFC 3339 timestamp")?;
    let state = EnrollmentState {
        connector_id: claims.connector_id.clone(),
        trust_domain: claims.trust_domain.clone(),
        workspace_id: claims.workspace_id.clone(),
        enrolled_at,
        cert_not_after,
    };
    let state_json =
        serde_json::to_vec_pretty(&state).context("failed to serialize EnrollmentState")?;

    // Full chain (leaf + Workspace CA) for mTLS client auth: the controller's
    // x509.Verify() needs the Workspace CA as an intermediate. The CA chain
    // (Workspace CA + Platform Intermediate) is the trust store every later
    // connection and every renewal pins against.
    install_enrollment(
        Path::new(&cfg.state_dir),
        &[
            (
                "connector.key",
                key_pair.serialize_pem().into_bytes(),
                0o600,
            ),
            (
                "connector.crt",
                format!("{}\n{}", leaf_cert, workspace_ca).into_bytes(),
                0o644,
            ),
            (
                "workspace_ca.crt",
                format!("{}\n{}", workspace_ca, intermediate_ca).into_bytes(),
                0o644,
            ),
            ("state.json", state_json, 0o644),
        ],
    )?;
    info!(path = %Path::new(&cfg.state_dir).join("state.json").display(), "saved enrollment state");

    // Step 10: Return result
    Ok(state)
}

// ── Config cleanup ─────────────────────────────────────────────────────────

/// Best-effort cleanup of /etc/zecurity/connector.conf after enrollment.
///
/// Removes the ENROLLMENT_TOKEN line (the token is already burned in Redis,
/// but keeping the raw JWT on disk leaks claims like connector_id and
/// trust_domain if the config file is ever exposed).
///
/// Adds CONNECTOR_ID=<uuid> so the config file stays a complete record
/// of the connector's identity.
///
/// This is best-effort because the config file is typically owned by
/// root:zecurity with mode 0640 — the connector (running as zecurity)
/// may not have write permission. If the write fails, a warning is logged
/// and enrollment proceeds normally.
const CONFIG_PATH: &str = "/etc/zecurity/connector.conf";

fn cleanup_config_after_enrollment(connector_id: &str) {
    let path = Path::new(CONFIG_PATH);

    let content = match fs::read_to_string(path) {
        Ok(c) => c,
        Err(e) => {
            warn!(
                path = CONFIG_PATH,
                error = %e,
                "could not read config file to remove enrollment token — \
                 consider manually removing ENROLLMENT_TOKEN from {}", CONFIG_PATH
            );
            return;
        }
    };

    // Remove ENROLLMENT_TOKEN line, add CONNECTOR_ID if not already present.
    let mut lines: Vec<String> = content
        .lines()
        .filter(|line| {
            let trimmed = line.trim();
            !trimmed.starts_with("ENROLLMENT_TOKEN=")
        })
        .map(|l| l.to_string())
        .collect();

    let has_connector_id = lines.iter().any(|l| l.trim().starts_with("CONNECTOR_ID="));
    if !has_connector_id {
        lines.push(format!("CONNECTOR_ID={}", connector_id));
    }

    let new_content = lines.join("\n") + "\n";

    match fs::write(path, new_content) {
        Ok(()) => {
            info!(
                path = CONFIG_PATH,
                "cleaned config: removed ENROLLMENT_TOKEN, added CONNECTOR_ID"
            );
        }
        Err(e) => {
            warn!(
                path = CONFIG_PATH,
                error = %e,
                "could not write config file — \
                 consider manually removing ENROLLMENT_TOKEN from {}", CONFIG_PATH
            );
        }
    }
}

// ── Internal helpers ────────────────────────────────────────────────────────

/// Step 1: Parse the JWT payload without signature verification.
///
/// The JWT format is: header.payload.signature
/// We only decode the middle segment (payload), which is base64url-encoded JSON.
fn parse_jwt_payload(token: &str) -> Result<JwtClaims> {
    let parts: Vec<&str> = token.split('.').collect();
    if parts.len() != 3 {
        bail!(
            "invalid JWT format: expected 3 dot-separated segments, got {}",
            parts.len()
        );
    }

    let payload_bytes = URL_SAFE_NO_PAD
        .decode(parts[1])
        .context("failed to base64url-decode JWT payload")?;

    let claims: JwtClaims =
        serde_json::from_slice(&payload_bytes).context("failed to deserialize JWT claims")?;

    Ok(claims)
}

/// Normalize a configured controller HTTP address to an absolute base URL
/// (scheme included, no trailing slash).
///
/// `http_addr` may be a bare "host:port" (assumed http://, for co-located/dev use —
/// e.g. the relay-style "127.0.0.1:8080" pattern) or a full URL with an explicit
/// "http://" / "https://" scheme (required when the controller is on a different
/// network and only reachable via its public HTTPS endpoint).
pub fn http_base_url(http_addr: &str) -> String {
    if http_addr.starts_with("http://") || http_addr.starts_with("https://") {
        http_addr.trim_end_matches('/').to_string()
    } else {
        format!("http://{}", http_addr.trim_end_matches('/'))
    }
}

/// Builds the /ca.crt URL from a configured HTTP address.
fn ca_cert_url(http_addr: &str) -> String {
    format!("{}/ca.crt", http_base_url(http_addr))
}

/// Step 3: Fetch the CA certificate from the controller's HTTP endpoint.
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let client = Client::builder()
        .build()
        .context("failed to build HTTP client")?;

    let url = ca_cert_url(http_addr);
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
        bail!(
            "response from {} does not contain a valid PEM certificate",
            url
        );
    }

    Ok(pem)
}

/// Step 4 (certificate audit bug 1): `/ca.crt` must contain exactly ONE
/// certificate and it must match the pinned Platform Intermediate fingerprint
/// from the token. Returns that certificate — the only one trusted for the
/// enrollment TLS connection. Previously only the first certificate was
/// checked while every certificate in the file was trusted, so an on-path
/// attacker could append their own CA.
fn pinned_bootstrap_ca(
    ca_pem: &str,
    expected_fingerprint: &str,
) -> Result<CertificateDer<'static>> {
    let certificates = certs(&mut ca_pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse PEM certificates")?;
    let [ca] = certificates.as_slice() else {
        bail!(
            "/ca.crt must contain exactly one certificate (the Platform Intermediate), got {} — possible MITM, aborting",
            certificates.len()
        );
    };
    let fingerprint = hex::encode(Sha256::digest(ca.as_ref()));
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
    Ok(ca.clone().into_owned())
}

/// PEM for a single DER certificate.
fn pem_from_der(der: &CertificateDer<'_>) -> String {
    let body = base64::engine::general_purpose::STANDARD.encode(der.as_ref());
    let mut pem = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in body.as_bytes().chunks(64) {
        pem.push_str(std::str::from_utf8(line).expect("base64 is ASCII"));
        pem.push('\n');
    }
    pem.push_str("-----END CERTIFICATE-----\n");
    pem
}

/// Exactly one certificate in `pem`.
fn single_certificate(pem: &[u8], label: &str) -> Result<CertificateDer<'static>> {
    let certificates = certs(&mut &pem[..])
        .collect::<Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label}"))?;
    match <[_; 1]>::try_from(certificates) {
        Ok([certificate]) => Ok(certificate),
        Err(certificates) => bail!(
            "{label} must be exactly one certificate, got {}",
            certificates.len()
        ),
    }
}

/// Step 9 (certificate audit bug 1): validate the enrollment reply before
/// anything is persisted. The chain must be pinned Platform Intermediate →
/// Workspace CA → leaf, and the leaf must be this connector's: its exact
/// SPIFFE ID, client and server usage, and the key generated in step 5.
fn validate_enrollment_reply(
    pinned_intermediate: &CertificateDer<'_>,
    workspace_ca_pem: &[u8],
    intermediate_ca_pem: &[u8],
    leaf_pem: &[u8],
    expected_spiffe_id: &str,
    key_spki_der: &[u8],
) -> Result<()> {
    let returned_intermediate =
        single_certificate(intermediate_ca_pem, "returned Intermediate CA")?;
    if returned_intermediate.as_ref() != pinned_intermediate.as_ref() {
        bail!("returned Intermediate CA is not the pinned Platform Intermediate");
    }
    let (_, intermediate) = X509Certificate::from_der(pinned_intermediate.as_ref())
        .map_err(|e| anyhow::anyhow!("parse pinned Intermediate CA: {e:?}"))?;

    let workspace_der = single_certificate(workspace_ca_pem, "returned Workspace CA")?;
    let (_, workspace) = X509Certificate::from_der(workspace_der.as_ref())
        .map_err(|e| anyhow::anyhow!("parse returned Workspace CA: {e:?}"))?;
    if !workspace.is_ca() {
        bail!("returned Workspace CA is not a CA certificate");
    }
    if workspace.issuer() != intermediate.subject() {
        bail!("returned Workspace CA was not issued by the pinned Platform Intermediate");
    }
    workspace
        .verify_signature(Some(intermediate.public_key()))
        .map_err(|e| anyhow::anyhow!("returned Workspace CA signature invalid: {e:?}"))?;
    if !workspace.validity().is_valid() {
        bail!("returned Workspace CA is outside its validity period");
    }

    let leaf_der = single_certificate(leaf_pem, "returned certificate")?;
    let (_, leaf) = X509Certificate::from_der(leaf_der.as_ref())
        .map_err(|e| anyhow::anyhow!("parse returned certificate: {e:?}"))?;
    if leaf.is_ca() {
        bail!("returned certificate must not be a CA");
    }
    if leaf.issuer() != workspace.subject() {
        bail!("returned certificate was not issued by the returned Workspace CA");
    }
    leaf.verify_signature(Some(workspace.public_key()))
        .map_err(|e| anyhow::anyhow!("returned certificate signature invalid: {e:?}"))?;
    if !leaf.validity().is_valid() {
        bail!("returned certificate is outside its validity period");
    }
    let uris: Vec<&str> = leaf
        .subject_alternative_name()
        .ok()
        .flatten()
        .map(|san| {
            san.value
                .general_names
                .iter()
                .filter_map(|name| match name {
                    x509_parser::extensions::GeneralName::URI(uri) => Some(*uri),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default();
    if uris != [expected_spiffe_id] {
        bail!("returned certificate identity {uris:?} is not {expected_spiffe_id}");
    }
    let eku = leaf
        .extended_key_usage()
        .ok()
        .flatten()
        .context("returned certificate has no extended key usage")?;
    if !eku.value.client_auth || !eku.value.server_auth {
        bail!("returned certificate must allow client and server authentication");
    }
    if leaf.public_key().raw != key_spki_der {
        bail!("returned certificate is not for the key this connector generated");
    }
    Ok(())
}

/// Step 9: write every enrollment file atomically (temp file, fsync,
/// rename), in order, `state.json` last — its presence is what marks the
/// connector enrolled. If any write fails, the files already written by this
/// attempt are removed, so a failed enrollment leaves nothing behind.
fn install_enrollment(state_dir: &Path, files: &[(&str, Vec<u8>, u32)]) -> Result<()> {
    fs::create_dir_all(state_dir)
        .with_context(|| format!("failed to create state directory: {}", state_dir.display()))?;
    let mut written = Vec::new();
    for (name, data, mode) in files {
        let path = state_dir.join(name);
        if let Err(e) = crate::tls::cert_holder::write_atomic(&path, data, *mode) {
            for done in &written {
                let _ = fs::remove_file(done);
            }
            return Err(e).with_context(|| format!("failed to write {}", path.display()));
        }
        info!(path = %path.display(), "installed");
        written.push(path);
    }
    Ok(())
}

fn parse_cert_not_after(cert_pem: &[u8]) -> Result<String> {
    let mut pem_slice = cert_pem;
    let certs = certs(&mut pem_slice)
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse connector certificate PEM")?;
    let leaf = certs
        .first()
        .context("connector certificate PEM did not contain a certificate")?;
    let (_, cert) = parse_x509_certificate(leaf.as_ref())
        .context("failed to parse connector certificate DER")?;

    Ok(cert
        .validity()
        .not_after
        .to_datetime()
        .format(&time::format_description::well_known::Rfc3339)
        .context("failed to format connector certificate expiry")?)
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

fn controller_host(grpc_addr: &str) -> String {
    grpc_addr
        .rsplit_once(':')
        .map(|(host, _)| host.to_string())
        .unwrap_or_else(|| grpc_addr.to_string())
}

#[cfg(test)]
#[path = "enrollment_tests.rs"]
mod tests;
