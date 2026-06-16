use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{anyhow, bail, Context, Result};
use quinn::Connection;
use rustls::version::TLS13;
use serde::{Deserialize, Serialize};
use tokio::io::AsyncReadExt;
use tokio::sync::Mutex;
use tokio_rustls::TlsConnector;

use crate::tunnel_pool::{
    classify_quinn, parse_ca_bundle, parse_cert_chain, parse_private_key_der, root_store_from_cert,
    AuthenticatedStream, ExactSpiffeVerifier, TunnelOpenError,
};

const RELAY_ALPN: &[u8] = b"ztna-relay-v1";
const INNER_TUNNEL_ALPN: &[u8] = b"ztna-tunnel-v1";
const MAX_MSG_SIZE: usize = 16 * 1024;

#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum LookupMessage<'a> {
    Lookup { connector_id: &'a str },
}

#[derive(Debug, Deserialize)]
struct RelayAck {
    ok: bool,
    error: Option<String>,
}

pub struct RelayPool {
    connections: Arc<Mutex<HashMap<String, Connection>>>,
    endpoint: quinn::Endpoint,
    client_cert_chain: Vec<rustls::pki_types::CertificateDer<'static>>,
    client_private_key: rustls::pki_types::PrivateKeyDer<'static>,
    workspace_ca: rustls::pki_types::CertificateDer<'static>,
}

impl RelayPool {
    pub fn new(
        cert_pem: &str,
        key_pem: &str,
        ca_bundle_pem: &str,
        relay_spiffe_id: &str,
    ) -> Result<Self> {
        let client_cert_chain = parse_cert_chain(cert_pem, "device certificate")?;
        let client_private_key = parse_private_key_der(key_pem, "device")?;
        let ca_bundle = parse_ca_bundle(ca_bundle_pem)?;

        let relay_roots =
            root_store_from_cert(&ca_bundle.intermediate_ca, "platform intermediate CA")?;
        let verifier = ExactSpiffeVerifier::new(relay_roots, relay_spiffe_id)?;

        let mut tls_config = rustls::ClientConfig::builder()
            .dangerous()
            .with_custom_certificate_verifier(verifier)
            .with_client_auth_cert(client_cert_chain.clone(), client_private_key.clone_key())
            .context("build Relay rustls client config")?;
        tls_config.alpn_protocols = vec![RELAY_ALPN.to_vec()];

        let quic_config = quinn::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .map_err(|e| anyhow!("build Relay QUIC TLS config: {}", e))?;
        let client_config = quinn::ClientConfig::new(Arc::new(quic_config));

        let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse().unwrap())
            .context("bind Relay QUIC client endpoint")?;
        endpoint.set_default_client_config(client_config);

        Ok(Self {
            connections: Arc::new(Mutex::new(HashMap::new())),
            endpoint,
            client_cert_chain,
            client_private_key,
            workspace_ca: ca_bundle.workspace_ca,
        })
    }

    /// Open a byte-zero authenticated stream to the connector via the relay.
    ///
    /// See `TunnelPool::open_authenticated_stream` for the fallback-boundary
    /// contract. On the relay path no further fallback exists, but we still
    /// classify failures consistently for diagnostics.
    pub async fn open_authenticated_stream(
        &self,
        relay_addr: &str,
        connector_id: &str,
        connector_spiffe: &str,
    ) -> Result<AuthenticatedStream, TunnelOpenError> {
        let conn = self.get_or_connect(relay_addr).await?;
        let (mut send, mut recv) = match conn.open_bi().await {
            Ok(streams) => streams,
            Err(error) => {
                self.connections.lock().await.remove(relay_addr);
                return Err(TunnelOpenError::Connect(anyhow::anyhow!(
                    "open Relay Lookup stream: {error}"
                )));
            }
        };

        write_lookup(&mut send, connector_id)
            .await
            .map_err(TunnelOpenError::Connect)?;
        read_ack(&mut recv)
            .await
            .map_err(TunnelOpenError::Connect)?;

        let roots = root_store_from_cert(&self.workspace_ca, "workspace CA")
            .map_err(TunnelOpenError::Authenticate)?;
        let verifier = ExactSpiffeVerifier::new(roots, connector_spiffe)
            .map_err(TunnelOpenError::Authenticate)?;
        let mut tls_config = rustls::ClientConfig::builder_with_protocol_versions(&[&TLS13])
            .dangerous()
            .with_custom_certificate_verifier(verifier)
            .with_client_auth_cert(
                self.client_cert_chain.clone(),
                self.client_private_key.clone_key(),
            )
            .map_err(|e| {
                TunnelOpenError::Authenticate(
                    anyhow::Error::from(e).context("build inner rustls client config"),
                )
            })?;
        tls_config.alpn_protocols = vec![INNER_TUNNEL_ALPN.to_vec()];

        let connector = TlsConnector::from(Arc::new(tls_config));
        let relay_stream = tokio::io::join(recv, send);
        let server_name = rustls::pki_types::ServerName::try_from("connector").map_err(|e| {
            TunnelOpenError::Authenticate(anyhow::Error::from(e).context("inner ServerName"))
        })?;
        let tls_stream = connector
            .connect(server_name, relay_stream)
            .await
            .map_err(|e| {
                // tokio-rustls handshake failures are inherently TLS-layer:
                // wrong server identity, cert verification rejection, etc.
                TunnelOpenError::Authenticate(anyhow::anyhow!(
                    "inner Client-to-Connector TLS handshake: {e}"
                ))
            })?;
        Ok(Box::new(tls_stream))
    }

    async fn get_or_connect(&self, relay_addr: &str) -> Result<Connection, TunnelOpenError> {
        {
            let mut conns = self.connections.lock().await;
            if let Some(conn) = conns.get(relay_addr) {
                if conn.close_reason().is_none() {
                    return Ok(conn.clone());
                }
                conns.remove(relay_addr);
            }
        }

        let addr = resolve_relay_addr(relay_addr).await.map_err(|e| {
            TunnelOpenError::Connect(e.context(format!("resolve Relay address {relay_addr}")))
        })?;
        let new_conn = self
            .endpoint
            .connect(addr, "relay")
            .map_err(|e| {
                TunnelOpenError::Connect(
                    anyhow::Error::from(e).context("start Relay QUIC connection"),
                )
            })?
            .await
            .map_err(|e| classify_quinn(&e))?;

        let mut conns = self.connections.lock().await;
        if let Some(existing) = conns.get(relay_addr) {
            if existing.close_reason().is_none() {
                new_conn.close(0u32.into(), b"raced");
                return Ok(existing.clone());
            }
        }
        conns.insert(relay_addr.to_string(), new_conn.clone());
        Ok(new_conn)
    }
}

async fn resolve_relay_addr(relay_addr: &str) -> Result<SocketAddr> {
    tokio::net::lookup_host(relay_addr)
        .await
        .with_context(|| format!("resolve Relay address {relay_addr}"))?
        .next()
        .with_context(|| format!("Relay address {relay_addr} resolved to no socket addresses"))
}

async fn write_lookup(send: &mut quinn::SendStream, connector_id: &str) -> Result<()> {
    let frame = encode_lookup(connector_id)?;
    send.write_all(&frame)
        .await
        .context("write Relay Lookup request")?;
    Ok(())
}

async fn read_ack(recv: &mut quinn::RecvStream) -> Result<()> {
    let ack = read_ack_frame(recv).await?;
    if ack.ok {
        return Ok(());
    }
    bail!(
        "Relay rejected client lookup: {}",
        ack.error.as_deref().unwrap_or("unspecified error")
    )
}

fn encode_lookup(connector_id: &str) -> Result<Vec<u8>> {
    let body = serde_json::to_vec(&LookupMessage::Lookup { connector_id })?;
    if body.len() > MAX_MSG_SIZE {
        bail!("Relay lookup message too large");
    }
    let mut frame = Vec::with_capacity(4 + body.len());
    frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
    frame.extend_from_slice(&body);
    Ok(frame)
}

async fn read_ack_frame<R>(recv: &mut R) -> Result<RelayAck>
where
    R: AsyncReadExt + Unpin,
{
    let mut length = [0u8; 4];
    recv.read_exact(&mut length)
        .await
        .context("read Relay ACK length")?;
    let length = u32::from_be_bytes(length) as usize;
    if length > MAX_MSG_SIZE {
        bail!("Relay ACK too large: {length} bytes");
    }

    let mut body = vec![0u8; length];
    recv.read_exact(&mut body)
        .await
        .context("read Relay ACK body")?;
    let ack: RelayAck = serde_json::from_slice(&body).context("decode Relay ACK JSON")?;
    Ok(ack)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::BufReader;

    #[test]
    fn lookup_frame_matches_protocol() {
        let frame = encode_lookup("abc-123").unwrap();
        let len = u32::from_be_bytes(frame[..4].try_into().unwrap()) as usize;
        assert_eq!(len, frame.len() - 4);
        assert_eq!(
            std::str::from_utf8(&frame[4..]).unwrap(),
            r#"{"type":"lookup","connector_id":"abc-123"}"#
        );
    }

    #[tokio::test]
    async fn negative_ack_is_rejected() {
        let body = br#"{"ok":false,"error":"denied"}"#;
        let mut frame = Vec::new();
        frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
        frame.extend_from_slice(body);
        let mut reader = BufReader::new(frame.as_slice());
        let ack = read_ack_frame(&mut reader).await.unwrap();
        assert!(!ack.ok);
        assert_eq!(ack.error.as_deref(), Some("denied"));
    }

    #[tokio::test]
    async fn malformed_ack_is_rejected() {
        let body = br#"not-json"#;
        let mut frame = Vec::new();
        frame.extend_from_slice(&(body.len() as u32).to_be_bytes());
        frame.extend_from_slice(body);
        let mut reader = BufReader::new(frame.as_slice());
        assert!(read_ack_frame(&mut reader).await.is_err());
    }

    #[tokio::test]
    async fn oversized_ack_is_rejected() {
        let mut frame = Vec::new();
        frame.extend_from_slice(&((MAX_MSG_SIZE as u32) + 1).to_be_bytes());
        let mut reader = BufReader::new(frame.as_slice());
        assert!(read_ack_frame(&mut reader).await.is_err());
    }

    #[tokio::test]
    async fn truncated_ack_is_rejected() {
        let body = br#"{"ok":true}"#;
        let mut frame = Vec::new();
        frame.extend_from_slice(&((body.len() as u32) + 5).to_be_bytes());
        frame.extend_from_slice(body);
        let mut reader = BufReader::new(frame.as_slice());
        assert!(read_ack_frame(&mut reader).await.is_err());
    }
}
