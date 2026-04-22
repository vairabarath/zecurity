// renewal.rs — Certificate renewal for the ZECURITY Shield
//
// Called from control_stream.rs when ReEnrollSignal arrives.
// Keeps the same EC P-384 keypair — sends existing public key to controller
// (via connector proxy) and receives a fresh cert for the same identity.
//
// Steps:
//   1. Read existing shield.key from disk
//   2. Build CSR DER (proof of key possession; sent as public_key_der per proto)
//   3. Build mTLS channel to connector :9091
//   4. Call RenewCert RPC (connector proxies to controller)
//   5. Save new shield.crt + updated workspace_ca.crt
//   6. Update cert_not_after in state.json
//   7. Return updated ShieldState (control_stream.rs reconnects with new cert)

use std::path::Path;

use anyhow::{Context, Result};
use rustls_pemfile::certs;
use tracing::info;
use x509_parser::prelude::*;

use crate::config::ShieldConfig;
use crate::crypto;
use crate::proto;
use crate::tls;
use crate::types::ShieldState;

pub async fn renew_cert(state: &ShieldState, cfg: &ShieldConfig) -> Result<ShieldState> {
    info!(shield_id = %state.shield_id, "starting certificate renewal");

    let state_dir = Path::new(&cfg.state_dir);

    // Step 1: Read existing private key
    let key_pem = tokio::fs::read_to_string(state_dir.join("shield.key"))
        .await
        .context("failed to read shield.key for renewal")?;

    // Step 2: Build CSR DER (same keypair — proof of possession)
    let public_key_der = crypto::extract_public_key_der(&key_pem)
        .context("failed to extract public key DER for renewal")?;

    // Step 3: Build mTLS channel to connector :9091
    let ca_pem = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_bytes = tokio::fs::read(state_dir.join("shield.key")).await?;

    let channel = tls::build_connector_channel(
        &ca_pem,
        &cert_pem,
        &key_bytes,
        &state.connector_id,
        &state.trust_domain,
        &state.connector_addr,
    )
    .await
    .context("failed to build mTLS channel to connector for renewal")?;

    let mut client = proto::shield_service_client::ShieldServiceClient::new(channel);

    // Step 4: Call RenewCert (connector proxies to controller)
    let resp = client
        .renew_cert(tonic::Request::new(proto::RenewCertRequest {
            shield_id: state.shield_id.clone(),
            public_key_der,
        }))
        .await
        .context("RenewCert RPC failed")?
        .into_inner();

    // Step 5: Save new shield.crt (leaf + workspace CA chain)
    let leaf_pem = String::from_utf8(resp.certificate_pem.clone())
        .context("certificate_pem is not valid UTF-8")?;
    let ws_ca_pem = String::from_utf8(resp.workspace_ca_pem.clone())
        .context("workspace_ca_pem is not valid UTF-8")?;
    let int_ca_pem = String::from_utf8(resp.intermediate_ca_pem)
        .context("intermediate_ca_pem is not valid UTF-8")?;

    tokio::fs::write(
        state_dir.join("shield.crt"),
        format!("{}\n{}", leaf_pem, ws_ca_pem),
    )
    .await
    .context("failed to write renewed shield.crt")?;

    tokio::fs::write(
        state_dir.join("workspace_ca.crt"),
        format!("{}\n{}", ws_ca_pem, int_ca_pem),
    )
    .await
    .context("failed to write renewed workspace_ca.crt")?;

    // Step 6: Parse new cert expiry
    let cert_not_after = parse_cert_not_after(&resp.certificate_pem)?;

    // Step 7: Update state.json
    let new_state = ShieldState {
        cert_not_after,
        ..state.clone()
    };
    new_state
        .save(&cfg.state_dir)
        .context("failed to save state.json after renewal")?;

    info!(
        shield_id = %state.shield_id,
        new_expiry = cert_not_after,
        "cert renewed"
    );

    Ok(new_state)
}

fn parse_cert_not_after(cert_pem: &[u8]) -> Result<i64> {
    let der_certs = certs(&mut cert_pem.as_ref())
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("failed to parse cert PEM")?;
    let der = der_certs.first().context("no certificate in PEM")?;
    let (_, cert) = X509Certificate::from_der(der.as_ref()).context("failed to parse cert DER")?;
    Ok(cert.validity().not_after.timestamp())
}
