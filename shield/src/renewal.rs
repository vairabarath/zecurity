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

use anyhow::{anyhow, Context, Result};
use rustls_pemfile::certs;
use tracing::{info, warn};
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

    // Step 3: Build mTLS channel to connector :9091, walking the peer list
    // on failure. First Connector that both connects AND accepts the RenewCert
    // RPC wins; it becomes the new head so future heartbeats/renewals prefer it.
    let ca_pem = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_bytes = tokio::fs::read(state_dir.join("shield.key")).await?;

    let mut selected_idx: Option<usize> = None;
    let mut resp: Option<proto::RenewCertResponse> = None;
    let mut last_err: Option<anyhow::Error> = None;

    for (idx, conn) in state.connectors.iter().enumerate() {
        let channel = match tls::build_connector_channel(
            &ca_pem,
            &cert_pem,
            &key_bytes,
            &conn.connector_id,
            &state.trust_domain,
            &conn.connector_addr,
        )
        .await
        {
            Ok(ch) => ch,
            Err(e) => {
                warn!(
                    connector_id = %conn.connector_id,
                    connector_addr = %conn.connector_addr,
                    error = %e,
                    "renewal: connector unreachable, trying next peer",
                );
                last_err = Some(e.context(format!(
                    "connector {} at {}",
                    conn.connector_id, conn.connector_addr
                )));
                continue;
            }
        };

        // Step 4: Call RenewCert (connector proxies to controller)
        let mut client = proto::shield_service_client::ShieldServiceClient::new(channel);
        match client
            .renew_cert(tonic::Request::new(proto::RenewCertRequest {
                shield_id: state.shield_id.clone(),
                public_key_der: public_key_der.clone(),
            }))
            .await
        {
            Ok(r) => {
                resp = Some(r.into_inner());
                selected_idx = Some(idx);
                break;
            }
            Err(e) => {
                warn!(
                    connector_id = %conn.connector_id,
                    error = %e,
                    "renewal: RenewCert RPC failed, trying next peer",
                );
                last_err = Some(anyhow!(e).context(format!(
                    "connector {} RenewCert",
                    conn.connector_id
                )));
                continue;
            }
        }
    }

    let resp = resp.ok_or_else(|| {
        last_err.unwrap_or_else(|| anyhow!("no peer connectors available for renewal"))
    })?;
    let selected_idx =
        selected_idx.expect("selected_idx is Some when resp is Some (paired above)");

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

    // Step 7: Update state.json — bump cert_not_after and (if we failed over
    // to a non-head Connector) rotate that Connector to the head so the next
    // heartbeat / renewal picks it first.
    let mut new_connectors = state.connectors.clone();
    if selected_idx > 0 {
        new_connectors.rotate_left(selected_idx);
    }
    let new_state = ShieldState {
        shield_id: state.shield_id.clone(),
        trust_domain: state.trust_domain.clone(),
        connectors: new_connectors,
        interface_addr: state.interface_addr.clone(),
        enrolled_at: state.enrolled_at.clone(),
        cert_not_after,
    };
    new_state
        .save(&cfg.state_dir)
        .context("failed to save state.json after renewal")?;

    info!(
        shield_id = %state.shield_id,
        new_expiry = cert_not_after,
        renewed_via = %new_state.connectors[0].connector_id,
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
