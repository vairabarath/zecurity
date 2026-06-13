use std::sync::Arc;

use anyhow::{bail, Context, Result};
use quinn::{Connection, ServerConfig};
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::server::WebPkiClientVerifier;
use rustls::RootCertStore;
use rustls_pemfile::{certs, private_key};

use crate::spiffe::{extract_spiffe_uri, parse_spiffe, validate_relay_spiffe, ParsedSpiffe};

pub const RELAY_ALPN: &[u8] = b"ztna-relay-v1";

/// Build the Relay QUIC server configuration.
///
/// The Relay trusts only the Platform Intermediate CA. Connector and Client
/// peers must present their leaf plus Workspace CA so rustls can build:
/// leaf -> Workspace CA -> Platform Intermediate CA.
pub fn build_server_config(
    cert_pem: &[u8],
    key_pem: &[u8],
    intermediate_ca_pem: &[u8],
    expected_relay_id: &str,
) -> Result<ServerConfig> {
    let cert_chain = parse_certificates(cert_pem, "Relay certificate")?;
    if cert_chain.len() != 1 {
        bail!(
            "Relay certificate PEM must contain exactly one leaf certificate, got {}",
            cert_chain.len()
        );
    }
    validate_relay_certificate(&cert_chain[0], expected_relay_id)?;

    let private_key = parse_private_key(key_pem)?;

    let intermediate_cas = parse_certificates(intermediate_ca_pem, "Platform Intermediate CA")?;
    if intermediate_cas.len() != 1 {
        bail!(
            "Intermediate CA PEM must contain exactly one trust anchor, got {}",
            intermediate_cas.len()
        );
    }
    let mut roots = RootCertStore::empty();
    roots
        .add(intermediate_cas[0].clone())
        .context("add Platform Intermediate CA trust anchor")?;

    // build() requires client authentication unless allow_unauthenticated() is
    // explicitly selected. Do not enable that option for the Relay listener.
    let client_verifier = WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .context("build Relay client certificate verifier")?;

    let mut tls_config = rustls::ServerConfig::builder()
        .with_client_cert_verifier(client_verifier)
        .with_single_cert(cert_chain, private_key)
        .context("build Relay TLS config; certificate and private key may not match")?;
    tls_config.alpn_protocols = vec![RELAY_ALPN.to_vec()];

    let quic_config = quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
        .context("build Relay QUIC TLS config")?;
    Ok(ServerConfig::with_crypto(Arc::new(quic_config)))
}

/// Extract the authenticated Connector or Client identity after a QUIC
/// handshake. The TLS verifier has already validated the certificate chain.
pub fn authenticated_peer_identity(connection: &Connection) -> Result<ParsedSpiffe> {
    let chain = connection
        .peer_identity()
        .context("peer did not present a certificate")?
        .downcast::<Vec<CertificateDer<'static>>>()
        .map_err(|_| anyhow::anyhow!("unexpected QUIC peer identity type"))?;

    authenticated_identity_from_chain(&chain)
}

fn authenticated_identity_from_chain(chain: &[CertificateDer<'_>]) -> Result<ParsedSpiffe> {
    if chain.len() < 2 {
        bail!("peer must present leaf certificate plus Workspace CA");
    }

    let spiffe_uri = extract_spiffe_uri(chain[0].as_ref())
        .context("peer certificate has no exact SPIFFE URI")?;
    let identity =
        parse_spiffe(&spiffe_uri).context("peer certificate has malformed SPIFFE URI")?;
    if identity.role != "connector" && identity.role != "client_device" {
        bail!(
            "peer SPIFFE role {:?} is not allowed by Relay",
            identity.role
        );
    }
    Ok(identity)
}

fn validate_relay_certificate(cert: &CertificateDer<'_>, expected_relay_id: &str) -> Result<()> {
    let spiffe_uri =
        extract_spiffe_uri(cert.as_ref()).context("Relay certificate has no exact SPIFFE URI")?;
    let identity =
        validate_relay_spiffe(&spiffe_uri).context("Relay certificate has invalid SPIFFE ID")?;
    if identity.relay_id != expected_relay_id {
        bail!(
            "Relay certificate ID {} does not match configured Relay ID {}",
            identity.relay_id,
            expected_relay_id
        );
    }
    Ok(())
}

fn parse_certificates(pem: &[u8], label: &str) -> Result<Vec<CertificateDer<'static>>> {
    let certificates = certs(&mut pem.as_ref())
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label} PEM"))?;
    if certificates.is_empty() {
        bail!("{label} PEM contains no certificates");
    }
    Ok(certificates)
}

fn parse_private_key(pem: &[u8]) -> Result<PrivateKeyDer<'static>> {
    private_key(&mut pem.as_ref())
        .context("parse Relay private key PEM")?
        .context("Relay private key PEM contains no private key")
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{CertificateParams, KeyPair, SanType};
    use std::sync::Once;

    const RELAY_ID: &str = "550e8400-e29b-41d4-a716-446655440000";
    const PEER_ID: &str = "9b2d5cae-5820-4702-adf4-231680852b11";

    fn install_crypto_provider() {
        static INSTALL: Once = Once::new();
        INSTALL.call_once(|| {
            rustls::crypto::ring::default_provider()
                .install_default()
                .unwrap();
        });
    }

    fn self_signed_relay(relay_id: &str) -> (Vec<u8>, Vec<u8>) {
        install_crypto_provider();
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::default();
        params.subject_alt_names.push(SanType::URI(
            crate::appmeta::relay_spiffe_id(relay_id)
                .try_into()
                .unwrap(),
        ));
        let cert = params.self_signed(&key).unwrap();
        (cert.pem().into_bytes(), key.serialize_pem().into_bytes())
    }

    fn peer_chain(role: &str, trust_domain: &str) -> Vec<CertificateDer<'static>> {
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::default();
        params.subject_alt_names.push(SanType::URI(
            format!("spiffe://{trust_domain}/{role}/{PEER_ID}")
                .try_into()
                .unwrap(),
        ));
        let leaf = params.self_signed(&key).unwrap();
        let (workspace_ca, _) = self_signed_relay(RELAY_ID);
        vec![
            leaf.der().clone(),
            parse_certificates(&workspace_ca, "test Workspace CA")
                .unwrap()
                .remove(0),
        ]
    }

    #[test]
    fn accepts_exact_relay_identity_and_matching_key() {
        let (cert, key) = self_signed_relay(RELAY_ID);
        build_server_config(&cert, &key, &cert, RELAY_ID).unwrap();
    }

    #[test]
    fn rejects_wrong_relay_identity() {
        let (cert, key) = self_signed_relay(RELAY_ID);
        assert!(
            build_server_config(&cert, &key, &cert, "9b2d5cae-5820-4702-adf4-231680852b11")
                .is_err()
        );
    }

    #[test]
    fn rejects_mismatched_private_key() {
        let (cert, _) = self_signed_relay(RELAY_ID);
        let (_, wrong_key) = self_signed_relay(RELAY_ID);
        assert!(build_server_config(&cert, &wrong_key, &cert, RELAY_ID).is_err());
    }

    #[test]
    fn rejects_leaf_only_peer_chain() {
        let (cert, _) = self_signed_relay(RELAY_ID);
        let chain = parse_certificates(&cert, "test certificate").unwrap();
        assert!(authenticated_identity_from_chain(&chain).is_err());
    }

    #[test]
    fn accepts_connectors_from_different_workspaces() {
        for trust_domain in ["workspace-a.zecurity.in", "workspace-b.zecurity.in"] {
            let identity =
                authenticated_identity_from_chain(&peer_chain("connector", trust_domain)).unwrap();
            assert_eq!(identity.trust_domain, trust_domain);
            assert_eq!(identity.role, "connector");
        }
    }

    #[test]
    fn rejects_unapproved_peer_role() {
        assert!(authenticated_identity_from_chain(&peer_chain(
            "shield",
            "workspace-a.zecurity.in"
        ))
        .is_err());
    }
}
