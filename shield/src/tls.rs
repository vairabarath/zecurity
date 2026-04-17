// tls.rs — SPIFFE-based connector certificate verification for mTLS
//
// WHY THIS EXISTS:
//   mTLS (mutual TLS) means both sides present certificates.
//   After the TLS handshake, we know the peer has a cert signed by our CA.
//   But that's not enough — any cert signed by the same CA would pass.
//
//   We need to verify the peer is specifically the CONNECTOR we enrolled with,
//   not just "any valid cert from our CA". We do this by checking the SPIFFE URI
//   in the peer's certificate SAN (Subject Alternative Name) extension.
//
// HOW IT WORKS:
//   1. Shield connects to connector :9091 with mTLS
//   2. TLS handshake completes — both sides verify each other's CA chain
//   3. Shield calls verify_connector_spiffe() with the connector's cert DER
//   4. We parse the cert, find the URI SAN, compare to expected SPIFFE ID
//   5. Expected ID is built from state.json: "spiffe://<trust_domain>/connector/<connector_id>"
//   6. If mismatch → reject connection (could be a rogue server)
//
// CALLED BY:
//   heartbeat.rs (Phase J) — after establishing mTLS channel to connector :9091

use anyhow::{bail, Context, Result};
use x509_parser::prelude::*;

use crate::appmeta;

/// Verify that a connector's certificate contains the expected SPIFFE URI.
///
/// Parameters:
///   - `cert_der`    — DER-encoded peer certificate from the TLS handshake
///   - `connector_id` — the connector UUID from state.json
///   - `trust_domain` — the workspace trust domain from state.json
///                      (e.g. "ws-acme.zecurity.in")
///
/// Returns `Ok(())` if the SPIFFE ID matches, `Err` with a descriptive
/// message if it doesn't.
///
/// Example expected SPIFFE ID:
///   "spiffe://ws-acme.zecurity.in/connector/abc-123"
pub fn verify_connector_spiffe(
    cert_der: &[u8],
    connector_id: &str,
    trust_domain: &str,
) -> Result<()> {
    // Build the expected SPIFFE URI from our enrolled state.
    // This is what the connector's cert MUST contain.
    let expected = appmeta::connector_spiffe_id(trust_domain, connector_id);

    // Parse the DER certificate
    let (_, cert) = X509Certificate::from_der(cert_der)
        .context("failed to parse connector certificate as X.509")?;

    // Find the Subject Alternative Name extension (OID 2.5.29.17).
    // All SPIFFE certificates must have a URI SAN — it's the identity carrier.
    let san = cert
        .subject_alternative_name()
        .context("failed to parse SAN extension from connector certificate")?
        .ok_or_else(|| anyhow::anyhow!("connector certificate has no SAN extension"))?;

    // Search for a URI SAN matching our expected connector SPIFFE ID
    for name in &san.value.general_names {
        if let GeneralName::URI(uri) = name {
            if *uri == expected {
                tracing::debug!(
                    spiffe_id = %uri,
                    "connector SPIFFE identity verified"
                );
                return Ok(());
            }
        }
    }

    // No matching URI SAN found — reject the connection
    bail!(
        "connector certificate does not contain expected SPIFFE URI '{}'. \
         Found SANs: {:?}. This may be a rogue server signed by the same CA.",
        expected,
        san.value
            .general_names
            .iter()
            .filter_map(|n| if let GeneralName::URI(u) = n {
                Some(*u)
            } else {
                None
            })
            .collect::<Vec<_>>()
    );
}
