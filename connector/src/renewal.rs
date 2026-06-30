// renewal.rs — Certificate renewal for the ZECURITY connector
//
// Called when Control stream receives re_enroll.
// The connector keeps its existing EC P-384 keypair.
// We just get a fresh cert for the same key + same SPIFFE identity.
//
// Steps:
//   1. Load cert/key material from disk (CertStore)
//   2. Extract public key in DER format
//   3. Build mTLS channel (uses existing cert — still valid for ~48h)
//   4. Call RenewCert RPC
//   5. Save new connector.crt to disk
//   6. Save updated CA chain (workspace CA + intermediate CA)
//   7. Parse new cert_not_after from the returned certificate
//   8. Build updated EnrollmentState with new expiry
//   9. Save updated state.json

use std::path::Path;

use crate::controller_client;
use crate::tls::cert_store::CertStore;
use anyhow::{Context, Result};
use tracing::info;

use crate::config::ConnectorConfig;
use crate::crypto;
use crate::enrollment::EnrollmentState;
use crate::proto;

/// Renew the connector's certificate.
///
/// Called from control_stream.rs when ReEnrollSignal arrives.
/// Returns the updated enrollment state (with new cert_not_after).
pub async fn renew_cert(state: &EnrollmentState, cfg: &ConnectorConfig) -> Result<EnrollmentState> {
    info!("starting certificate renewal");

    // 1. Read existing private key from disk
    let cert_store = CertStore::load_async(&cfg.state_dir)
        .await
        .context("failed to load cert store for renewal")?;

    // 2. Extract public key in DER format
    let key_pem_str =
        std::str::from_utf8(&cert_store.key_pem).context("connector.key is not valid UTF-8")?;
    let public_key_der =
        crypto::extract_public_key_der(&key_pem_str).context("failed to extract public key")?;

    // 3. Build mTLS channel (uses existing cert — still valid)
    let channel = controller_client::build_channel(cfg, &cert_store)
        .await
        .context("failed to build mTLS channel")?;

    let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);

    // 4. Call RenewCert RPC
    let req = proto::RenewCertRequest {
        connector_id: state.connector_id.clone(),
        public_key_der,
    };

    let resp = client
        .renew_cert(req)
        .await
        .with_context(|| "renew_cert RPC failed")?
        .into_inner();

    // 5. Save new connector.crt
    let cert_path = Path::new(&cfg.state_dir).join("connector.crt");
    tokio::fs::write(&cert_path, &resp.certificate_pem)
        .await
        .with_context(|| format!("failed to write {}", cert_path.display()))?;

    // 6. Save updated CA chain
    let ca_path = Path::new(&cfg.state_dir).join("workspace_ca.crt");
    let ca_chain = format!(
        "{}\n{}",
        String::from_utf8_lossy(&resp.workspace_ca_pem),
        String::from_utf8_lossy(&resp.intermediate_ca_pem),
    );
    tokio::fs::write(&ca_path, ca_chain.as_bytes())
        .await
        .with_context(|| format!("failed to write {}", ca_path.display()))?;

    // 7. Parse new cert_not_after from the cert
    let new_not_after = crypto::parse_cert_not_after(&resp.certificate_pem)
        .context("failed to parse new cert expiry")?;

    // 8. Update state.json
    let new_not_after_str = format!("{}", new_not_after);
    let new_state = EnrollmentState {
        connector_id: state.connector_id.clone(),
        trust_domain: state.trust_domain.clone(),
        workspace_id: state.workspace_id.clone(),
        enrolled_at: state.enrolled_at.clone(),
        cert_not_after: new_not_after_str.clone(),
    };

    // 9. Save updated state.json
    new_state
        .save(&cfg.state_dir)
        .context("failed to save renewed state")?;

    info!(
        "certificate renewed successfully, new expiry: {}",
        new_not_after_str
    );

    Ok(new_state)
}
