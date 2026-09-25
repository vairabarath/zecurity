use std::sync::Arc;

use anyhow::{Context, Result};
use rustls::server::{danger::ClientCertVerifier, WebPkiClientVerifier};
use rustls::RootCertStore;
use rustls_pemfile::{certs, private_key};

use super::cert_holder::{CertHolder, HolderCertResolver};
use super::cert_store::CertStore;

const DEVICE_TUNNEL_ALPN: &[u8] = b"ztna-tunnel-v1";

/// Client verifier for device mTLS: trusts only the CA bundle on disk
/// (devices are signed by the workspace CA).
fn device_client_verifier(store: &CertStore) -> Result<Arc<dyn ClientCertVerifier>> {
    let mut roots = RootCertStore::empty();
    let ca_certs: Vec<_> = certs(&mut store.workspace_ca_pem.as_slice())
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("parse workspace CA PEM")?;
    for cert in ca_certs {
        roots.add(cert).context("add workspace CA to root store")?;
    }
    WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .context("build client verifier")
}

/// Build a `rustls::ServerConfig` for the device tunnel listener on :9092
/// with a FIXED server certificate taken from `store`.
///
/// - Requires client certificate (mTLS) — devices must present their SPIFFE cert.
/// - Trusts only the workspace CA (devices are signed by that CA).
/// - Sets ALPN to `ztna-tunnel-v1`.
///
/// Used by the QUIC listener, which swaps the whole config on renewal via
/// `quinn::Endpoint::set_server_config`.
pub fn build_device_tunnel_tls(store: &CertStore) -> Result<rustls::ServerConfig> {
    let client_verifier = device_client_verifier(store)?;

    // Parse connector's own cert and key for the server side.
    let server_certs: Vec<_> = certs(&mut store.cert_pem.as_slice())
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("parse connector cert PEM")?;
    let server_key = private_key(&mut store.key_pem.as_slice())
        .context("read connector private key")?
        .context("no private key found in connector.key")?;

    let mut cfg = rustls::ServerConfig::builder()
        .with_client_cert_verifier(client_verifier)
        .with_single_cert(server_certs, server_key)
        .context("build rustls ServerConfig")?;

    cfg.alpn_protocols = vec![DEVICE_TUNNEL_ALPN.to_vec()];
    Ok(cfg)
}

/// Build the device tunnel TLS config whose server certificate is resolved
/// from the CertHolder on every handshake (Sprint 20 G-2a): new handshakes
/// present a renewed certificate, established sessions keep theirs.
///
/// The client-verifier CA bundle is pinned across renewals (the holder
/// refuses a renewal that changes it), so it is built once.
pub fn build_device_tunnel_tls_dynamic(holder: Arc<CertHolder>) -> Result<rustls::ServerConfig> {
    let client_verifier = device_client_verifier(&holder.current().store)?;
    let mut cfg = rustls::ServerConfig::builder()
        .with_client_cert_verifier(client_verifier)
        .with_cert_resolver(HolderCertResolver::new(holder));
    cfg.alpn_protocols = vec![DEVICE_TUNNEL_ALPN.to_vec()];
    Ok(cfg)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::{tls_handshake_serial, TestPki};
    use tokio_rustls::TlsAcceptor;

    /// Device TLS (:9092) serves the holder's certificate per handshake: a
    /// handshake after a renewal presents the NEW serial, with no rebuild.
    #[tokio::test]
    async fn device_tls_new_handshakes_use_renewed_serial() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let acceptor = TlsAcceptor::from(Arc::new(
            build_device_tunnel_tls_dynamic(holder.clone()).unwrap(),
        ));
        let client = pki.client_tls_config(false);

        let before = holder.current().serial_hex.clone();
        assert_eq!(
            tls_handshake_serial(&acceptor, client.clone()).await,
            before
        );

        let renewed = pki.renew(&holder, 7200);
        assert_ne!(renewed.serial_hex, before);
        assert_eq!(
            tls_handshake_serial(&acceptor, client).await,
            renewed.serial_hex
        );
    }
}
