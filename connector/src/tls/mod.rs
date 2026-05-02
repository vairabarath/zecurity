pub mod cert_store;
pub mod server_cfg;

// tls/mod.rs — SPIFFE-based controller certificate verification
//
// Used by controller_client.rs after the mTLS handshake to verify that the
// controller's presented certificate contains the expected SPIFFE URI.
//
// This prevents a rogue server signed by the same CA from impersonating
// the controller.

use anyhow::{bail, Context, Result};
use x509_parser::prelude::*;

use crate::appmeta;

/// Verify that a certificate's SAN URI matches the expected controller SPIFFE ID.
///
/// Called post-handshake in controller_client.rs after establishing an mTLS connection.
/// The `cert_der` parameter is the DER-encoded peer certificate from the TLS handshake.
///
/// Returns `Ok(())` if the SPIFFE ID matches, `Err` otherwise.
pub fn verify_controller_spiffe(cert_der: &[u8]) -> Result<()> {
    let (_, cert) =
        X509Certificate::from_der(cert_der).context("failed to parse peer certificate as X.509")?;

    // Look for the Subject Alternative Name extension (OID 2.5.29.17)
    let san = cert
        .subject_alternative_name()
        .context("failed to parse SAN extension")?
        .ok_or_else(|| anyhow::anyhow!("peer certificate has no SAN extension"))?;

    // Search for a URI SAN matching the expected controller SPIFFE ID
    for name in &san.value.general_names {
        if let GeneralName::URI(uri) = name {
            if *uri == appmeta::SPIFFE_CONTROLLER_ID {
                tracing::info!(spiffe_id = %uri, "controller SPIFFE identity verified");
                return Ok(());
            }
        }
    }

    bail!(
        "controller certificate does not contain expected SPIFFE URI '{}'. \
         This may be an impostor server signed by the same CA.",
        appmeta::SPIFFE_CONTROLLER_ID
    );
}
