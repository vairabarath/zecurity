// enrollment.rs — Full Shield enrollment flow
//
// WHAT IS ENROLLMENT?
//   Enrollment is the one-time process where the shield:
//     1. Proves it has a valid invitation (the JWT enrollment token)
//     2. Gets a signed SPIFFE certificate from the controller
//     3. Learns which connector to open a control stream through
//     4. Gets assigned a /32 IP address for the zecurity0 interface
//     5. Saves all of this to disk so future restarts skip enrollment
//
// WHY NO JWT SIGNATURE VERIFICATION?
//   The shield has no copy of the JWT_SECRET (that lives only on the controller).
//   Instead, trust is established by verifying the CA certificate fingerprint
//   embedded in the JWT. If a MITM intercepts the token and substitutes their
//   own CA, the fingerprint check catches it and we abort.
//   The controller verifies the JWT signature server-side when we call Enroll().
//
// ENROLLMENT FLOW (12 steps):
//   1.  Parse JWT payload → extract shield_id, trust_domain, ca_fingerprint, etc.
//   2.  Fetch CA cert from controller HTTP endpoint (plain HTTP, no TLS yet)
//   3.  Verify CA fingerprint (SHA-256) — MITM detection
//   4.  Generate EC P-384 keypair, save shield.key (mode 0600)
//   5.  Build PKCS#10 CSR with SPIFFE SAN URI
//   6.  Connect to controller gRPC over TLS (rooted in the verified CA)
//   7.  Call ShieldService.Enroll RPC
//   8.  Save shield.crt (leaf cert)
//   9.  Save workspace_ca.crt (CA chain for future mTLS)
//   10. Write state.json (shield_id, connector_addr, interface_addr, etc.)
//   11. Remove ENROLLMENT_TOKEN from config file (best-effort)
//   12. Return ShieldState (main.rs uses this to start the control stream)
//
// CALLED BY: main.rs when state.json does not exist (first run)

use std::fs;
use std::path::Path;

use anyhow::{bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use reqwest::Client;
use rustls_pemfile::certs;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint};
use tracing::{info, warn};
use x509_parser::prelude::*;

use crate::appmeta;
use crate::config::ShieldConfig;
use crate::crypto;
use crate::proto;
use crate::types::ShieldState;
use crate::util;

// ── JWT claims ────────────────────────────────────────────────────────────────

/// Claims extracted from the enrollment JWT payload.
///
/// These match the Go `EnrollmentClaims` struct in controller/internal/shield/token.go.
/// The controller embeds all the information the shield needs to enroll:
///   - shield_id       — the UUID pre-created in the DB by GenerateShieldToken
///   - trust_domain    — e.g. "ws-acme.zecurity.in" (used in SPIFFE URI)
///   - ca_fingerprint  — SHA-256 hex of the workspace CA DER (MITM detection)
///   - connector_id    — which connector was selected (least-loaded)
///   - connector_addr  — where to open the control stream after enrollment
///   - interface_addr  — the /32 IP assigned to zecurity0
#[derive(Debug, Deserialize)]
struct JwtClaims {
    shield_id: String,
    workspace_id: String,
    trust_domain: String,
    ca_fingerprint: String,
    connector_id: String,
    connector_addr: String,
    interface_addr: String,
}

// ── Public entry point ────────────────────────────────────────────────────────

/// Run the full enrollment flow.
///
/// Called from main.rs when state.json does not exist (first run).
/// On success, returns a ShieldState that main.rs uses to start the control stream.
/// On fatal error (MITM, bad token, controller rejection), returns Err.
pub async fn enroll(cfg: &ShieldConfig) -> Result<ShieldState> {
    let token = cfg
        .enrollment_token
        .as_deref()
        .context("ENROLLMENT_TOKEN is required for first-run enrollment — set it in /etc/zecurity/shield.conf")?;

    // ── Step 1: Parse JWT payload ─────────────────────────────────────────────
    //
    // JWT format: base64url(header).base64url(payload).base64url(signature)
    // We decode only the middle segment. The controller verifies the signature
    // server-side when we call Enroll(). We trust the payload only after
    // verifying the CA fingerprint in Step 3.
    let claims = parse_jwt_payload(token)?;
    info!(
        shield_id    = %claims.shield_id,
        workspace_id = %claims.workspace_id,
        trust_domain = %claims.trust_domain,
        connector_id = %claims.connector_id,
        "parsed enrollment token"
    );

    // ── Step 2: Fetch CA certificate ──────────────────────────────────────────
    //
    // The shield has no TLS material yet, so it fetches the CA cert over plain
    // HTTP. This is safe because we immediately verify the fingerprint in Step 3.
    // A MITM can substitute a rogue CA cert, but the fingerprint check will catch it.
    let ca_pem = fetch_ca_cert(&cfg.controller_http_addr).await?;
    info!("fetched CA certificate from controller HTTP endpoint");

    // ── Step 3: Verify CA fingerprint ─────────────────────────────────────────
    //
    // The JWT embeds a SHA-256 fingerprint of the real CA cert DER bytes.
    // We compute the fingerprint of what we downloaded and compare.
    // If they don't match → MITM attack → abort immediately.
    // This is the core security guarantee of the enrollment flow.
    verify_ca_fingerprint(&ca_pem, &claims.ca_fingerprint)?;
    info!("CA fingerprint verified — no MITM detected");

    // ── Step 4: Generate EC P-384 keypair ─────────────────────────────────────
    //
    // The private key never leaves this machine. We save it with mode 0600
    // (owner read+write only) to prevent other users from reading it.
    let key_pair = crypto::generate_keypair()?;
    let key_path = Path::new(&cfg.state_dir).join("shield.key");
    crypto::save_private_key(&key_pair, &key_path)?;
    info!(path = %key_path.display(), "saved EC P-384 private key");

    // ── Step 5: Build PKCS#10 CSR ─────────────────────────────────────────────
    //
    // The CSR contains:
    //   - CN: "shield-<shield_id>" (matches PKI_SHIELD_CN_PREFIX + shield_id)
    //   - SAN URI: "spiffe://ws-<slug>.zecurity.in/shield/<shield_id>"
    //   - Self-signature (proves we hold the private key)
    //
    // The controller verifies the self-signature and checks the SPIFFE URI
    // matches what it expects for this shield_id.
    let cn = format!("{}{}", appmeta::PKI_SHIELD_CN_PREFIX, claims.shield_id);
    let spiffe_uri = appmeta::shield_spiffe_id(&claims.trust_domain, &claims.shield_id);
    let csr_der = crypto::build_csr(&key_pair, &cn, &spiffe_uri)?;
    info!(cn = %cn, san = %spiffe_uri, "built PKCS#10 CSR");

    // ── Step 6: Connect to controller gRPC over TLS ───────────────────────────
    //
    // Now that we have the verified CA cert, we can establish a TLS connection
    // rooted in it. This is plain TLS (no client cert yet — we don't have one).
    // The controller's cert must chain up to this CA.
    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let grpc_host = cfg
        .controller_addr
        .rsplit_once(':')
        .map(|(h, _)| h.to_string())
        .unwrap_or_else(|| cfg.controller_addr.clone());

    let channel = Endpoint::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid controller gRPC address: {}", grpc_addr))?
        .tls_config(
            ClientTlsConfig::new()
                .ca_certificate(Certificate::from_pem(ca_pem.as_bytes()))
                .domain_name(grpc_host),
        )
        .context("failed to configure TLS for controller gRPC")?
        .connect()
        .await
        .with_context(|| format!("failed to connect to controller at {}", grpc_addr))?;

    let mut client = proto::shield_service_client::ShieldServiceClient::new(channel);
    info!(addr = %grpc_addr, "connected to controller gRPC");

    // ── Step 7: Call ShieldService.Enroll RPC ────────────────────────────────
    //
    // The controller will:
    //   1. Verify the JWT signature (it has the JWT_SECRET)
    //   2. Burn the JTI in Redis (single-use token)
    //   3. Verify the shield is in 'pending' state in the DB
    //   4. Verify the CSR self-signature
    //   5. Check the SPIFFE URI in the CSR matches the expected shield SPIFFE ID
    //   6. Sign a 7-day SPIFFE certificate
    //   7. Update the shield DB record to 'active'
    //   8. Return the signed cert + CA chain
    let hostname = util::read_hostname();
    let response = client
        .enroll(tonic::Request::new(proto::EnrollRequest {
            enrollment_token: token.to_string(),
            csr_der,
            version: env!("CARGO_PKG_VERSION").to_string(),
            hostname,
        }))
        .await
        .context("ShieldService.Enroll RPC failed")?
        .into_inner();

    info!(
        shield_id    = %response.shield_id,
        interface_addr = %response.interface_addr,
        connector_addr = %response.connector_addr,
        "enrollment RPC succeeded"
    );

    // ── Step 8: Save shield.crt ───────────────────────────────────────────────
    //
    // We save the leaf cert + workspace CA as a chain. The mTLS client needs
    // to present the full chain so the connector can verify it.
    let state_dir = Path::new(&cfg.state_dir);
    fs::create_dir_all(state_dir)
        .with_context(|| format!("failed to create state dir {}", state_dir.display()))?;

    let leaf_pem = String::from_utf8(response.certificate_pem.clone())
        .context("certificate_pem is not valid UTF-8")?;
    let ws_ca_pem = String::from_utf8(response.workspace_ca_pem.clone())
        .context("workspace_ca_pem is not valid UTF-8")?;

    // Full chain = leaf cert + workspace CA (connector needs both to verify)
    let cert_path = state_dir.join("shield.crt");
    fs::write(&cert_path, format!("{}\n{}", leaf_pem, ws_ca_pem))
        .with_context(|| format!("failed to write {}", cert_path.display()))?;
    info!(path = %cert_path.display(), "saved shield certificate chain");

    // Parse cert expiry for state.json
    let cert_not_after = parse_cert_not_after(&response.certificate_pem)?;

    // ── Step 9: Save workspace_ca.crt ────────────────────────────────────────
    //
    // The CA chain (workspace CA + intermediate CA) is used by the mTLS client
    // to verify the connector's certificate during the control stream.
    let int_ca_pem = String::from_utf8(response.intermediate_ca_pem)
        .context("intermediate_ca_pem is not valid UTF-8")?;
    let ca_chain_path = state_dir.join("workspace_ca.crt");
    fs::write(&ca_chain_path, format!("{}\n{}", ws_ca_pem, int_ca_pem))
        .with_context(|| format!("failed to write {}", ca_chain_path.display()))?;
    info!(path = %ca_chain_path.display(), "saved CA chain");

    // ── Step 10: Write state.json ─────────────────────────────────────────────
    //
    // state.json is the "enrolled" marker. On next startup, main.rs checks for
    // this file. If it exists → skip enrollment, load state, start control stream.
    let enrolled_at = ::time::OffsetDateTime::now_utc()
        .format(&::time::format_description::well_known::Rfc3339)
        .context("failed to format enrollment timestamp")?;

    let state = ShieldState {
        shield_id: response.shield_id,
        trust_domain: claims.trust_domain,
        connector_id: response.connector_id,
        connector_addr: response.connector_addr,
        interface_addr: response.interface_addr,
        enrolled_at,
        cert_not_after,
    };

    state.save(&cfg.state_dir)?;
    info!(path = %state_dir.join("state.json").display(), "saved state.json");

    // ── Step 11: Clean up config file ─────────────────────────────────────────
    //
    // Remove ENROLLMENT_TOKEN from /etc/zecurity/shield.conf.
    // The token is already burned in Redis, but keeping the raw JWT on disk
    // leaks claims (shield_id, trust_domain, connector_addr) if the file is
    // ever exposed. Best-effort — log a warning if we can't write.
    cleanup_config_after_enrollment(&state.shield_id, &cfg.state_dir);

    // ── Step 12: Network setup ────────────────────────────────────────────────
    //
    // Create the local `zecurity0` TUN interface and install the base nftables
    // table. This is intentionally best-effort for Sprint 4:
    //   - enrollment already succeeded at this point
    //   - certs/state are safely on disk
    //   - if Linux networking setup fails, the operator can still inspect logs,
    //     fix host permissions, and restart the service without re-enrolling
    if let Err(e) = crate::network::setup(&state.interface_addr, &state.connector_addr).await {
        warn!(error = %e, "network setup failed (non-fatal for now)");
    }

    info!(shield_id = %state.shield_id, "enrollment complete");
    Ok(state)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

/// Parse the JWT payload segment without verifying the signature.
///
/// JWT = base64url(header) . base64url(payload) . base64url(signature)
/// We decode only the payload (middle segment).
fn parse_jwt_payload(token: &str) -> Result<JwtClaims> {
    let parts: Vec<&str> = token.splitn(3, '.').collect();
    if parts.len() != 3 {
        bail!(
            "invalid JWT: expected 3 dot-separated segments, got {}",
            parts.len()
        );
    }

    let payload_bytes = URL_SAFE_NO_PAD
        .decode(parts[1])
        .context("failed to base64url-decode JWT payload segment")?;

    serde_json::from_slice::<JwtClaims>(&payload_bytes)
        .context("failed to deserialize JWT claims from payload")
}

/// Fetch the CA certificate from the controller's HTTP endpoint.
///
/// Plain HTTP is intentional here — we have no TLS material yet.
/// Security comes from the fingerprint check in verify_ca_fingerprint().
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let url = format!("http://{}/ca.crt", http_addr);
    let resp = Client::new()
        .get(&url)
        .send()
        .await
        .with_context(|| format!("failed to GET {}", url))?;

    if !resp.status().is_success() {
        bail!("CA cert fetch failed: HTTP {} from {}", resp.status(), url);
    }

    let pem = resp
        .text()
        .await
        .context("failed to read CA cert response body")?;

    if !pem.contains("-----BEGIN CERTIFICATE-----") {
        bail!("response from {} is not a valid PEM certificate", url);
    }

    Ok(pem)
}

/// Verify the downloaded CA cert's SHA-256 fingerprint matches the JWT claim.
///
/// This is the MITM detection step. The JWT was signed by the controller
/// and embeds the fingerprint of the real CA. If a MITM substitutes a rogue
/// CA cert, the fingerprint won't match and we abort.
fn verify_ca_fingerprint(ca_pem: &str, expected: &str) -> Result<()> {
    // Parse PEM → DER
    let der_certs = certs(&mut ca_pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse CA PEM")?;

    let der = der_certs
        .first()
        .context("no certificate found in CA PEM")?;

    // SHA-256 of DER bytes
    let fingerprint = hex::encode(Sha256::digest(der.as_ref()));

    if fingerprint != expected {
        bail!(
            "CA fingerprint mismatch — possible MITM attack!\n  expected: {}\n  got:      {}",
            expected,
            fingerprint
        );
    }

    Ok(())
}

/// Parse the NotAfter timestamp from a PEM certificate.
///
/// Returns a Unix timestamp (i64) stored in state.json.
/// control_stream.rs uses this to reconnect with renewed credentials.
fn parse_cert_not_after(cert_pem: &[u8]) -> Result<i64> {
    let der_certs = certs(&mut cert_pem.as_ref())
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse certificate PEM")?;

    let der = der_certs.first().context("no certificate in PEM")?;

    let (_, cert) =
        X509Certificate::from_der(der.as_ref()).context("failed to parse certificate DER")?;

    Ok(cert.validity().not_after.timestamp())
}

/// Best-effort cleanup of /etc/zecurity/shield.conf after enrollment.
///
/// Removes ENROLLMENT_TOKEN (already burned in Redis, but raw JWT on disk
/// leaks claims). Adds SHIELD_ID so the config stays a complete record.
fn cleanup_config_after_enrollment(shield_id: &str, _state_dir: &str) {
    let path = Path::new("/etc/zecurity/shield.conf");

    let content = match fs::read_to_string(path) {
        Ok(c) => c,
        Err(e) => {
            warn!(
                error = %e,
                "could not read /etc/zecurity/shield.conf to remove ENROLLMENT_TOKEN — \
                 consider removing it manually"
            );
            return;
        }
    };

    let mut lines: Vec<String> = content
        .lines()
        .filter(|l| !l.trim().starts_with("ENROLLMENT_TOKEN="))
        .map(|l| l.to_string())
        .collect();

    if !lines.iter().any(|l| l.trim().starts_with("SHIELD_ID=")) {
        lines.push(format!("SHIELD_ID={}", shield_id));
    }

    if let Err(e) = fs::write(path, lines.join("\n") + "\n") {
        warn!(
            error = %e,
            "could not update /etc/zecurity/shield.conf — consider removing ENROLLMENT_TOKEN manually"
        );
    } else {
        info!("removed ENROLLMENT_TOKEN from /etc/zecurity/shield.conf");
    }
}
