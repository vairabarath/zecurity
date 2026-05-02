use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use quinn::Connection;
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls_pemfile::{certs, private_key};
use tokio::sync::Mutex;

pub struct TunnelPool {
    connections: Arc<Mutex<HashMap<SocketAddr, Connection>>>,
    endpoint: quinn::Endpoint,
}

impl TunnelPool {
    /// Build a QUIC client endpoint with device mTLS cert and workspace CA verification.
    pub fn new(cert_pem: &str, key_pem: &str, ca_pem: &str) -> Result<Self> {
        let cert_chain: Vec<CertificateDer<'static>> = {
            let mut reader = std::io::BufReader::new(cert_pem.as_bytes());
            certs(&mut reader)
                .collect::<Result<Vec<_>, _>>()
                .context("parse device cert PEM")?
        };

        let private_key: PrivateKeyDer<'static> = {
            let mut reader = std::io::BufReader::new(key_pem.as_bytes());
            private_key(&mut reader)
                .context("parse private key PEM")?
                .context("no private key found")?
        };

        let ca_certs: Vec<CertificateDer<'static>> = {
            let mut reader = std::io::BufReader::new(ca_pem.as_bytes());
            certs(&mut reader)
                .collect::<Result<Vec<_>, _>>()
                .context("parse CA cert PEM")?
        };

        let mut root_store = rustls::RootCertStore::empty();
        for ca in ca_certs {
            root_store.add(ca).context("add CA cert to root store")?;
        }

        let tls_config = rustls::ClientConfig::builder()
            .with_root_certificates(root_store)
            .with_client_auth_cert(cert_chain, private_key)
            .context("build rustls client config")?;

        let quic_client_cfg = quinn_proto::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .map_err(|e| anyhow::anyhow!("build QUIC client config: {}", e))?;

        let mut client_cfg = quinn::ClientConfig::new(Arc::new(quic_client_cfg));

        let mut transport = quinn::TransportConfig::default();
        transport.keep_alive_interval(Some(std::time::Duration::from_secs(10)));
        client_cfg.transport_config(Arc::new(transport));

        let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse().unwrap())
            .context("bind QUIC client endpoint")?;
        endpoint.set_default_client_config(client_cfg);

        Ok(Self {
            connections: Arc::new(Mutex::new(HashMap::new())),
            endpoint,
        })
    }

    /// Return an existing connection to `addr` or establish a new one.
    /// Never opens a second parallel connection to the same address.
    pub async fn get_or_connect(&self, addr: SocketAddr) -> Result<Connection> {
        let mut conns = self.connections.lock().await;
        if let Some(conn) = conns.get(&addr) {
            if conn.close_reason().is_none() {
                return Ok(conn.clone());
            }
            conns.remove(&addr);
        }
        let conn = self
            .endpoint
            .connect(addr, "connector")
            .context("initiate QUIC connection")?
            .await
            .context("QUIC handshake")?;
        conns.insert(addr, conn.clone());
        Ok(conn)
    }

    /// Open one bidirectional QUIC stream on the pooled connection to `addr`.
    pub async fn open_stream(
        &self,
        addr: SocketAddr,
    ) -> Result<(quinn::SendStream, quinn::RecvStream)> {
        let conn = self.get_or_connect(addr).await?;
        conn.open_bi().await.context("open QUIC stream")
    }
}
