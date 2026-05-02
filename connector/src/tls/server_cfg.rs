use std::sync::Arc;

use anyhow::{Context, Result};
use rustls::server::WebPkiClientVerifier;
use rustls::RootCertStore;
use rustls_pemfile::{certs, private_key};

use super::cert_store::CertStore;

/// Build a `rustls::ServerConfig` for the device tunnel TLS listener on :9092.
///
/// - Requires client certificate (mTLS) — devices must present their SPIFFE cert.
/// - Trusts only the workspace CA (devices are signed by that CA).
/// - Sets ALPN to `ztna-tunnel-v1`.
pub fn build_device_tunnel_tls(store: &CertStore) -> Result<rustls::ServerConfig> {
    // Parse workspace CA into a root store — only trust devices signed by this CA.
    let mut roots = RootCertStore::empty();
    let ca_certs: Vec<_> = certs(&mut store.workspace_ca_pem.as_slice())
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("parse workspace CA PEM")?;
    for cert in ca_certs {
        roots.add(cert).context("add workspace CA to root store")?;
    }

    let client_verifier = WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .context("build client verifier")?;

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

    cfg.alpn_protocols = vec![b"ztna-tunnel-v1".to_vec()];
    Ok(cfg)
}
