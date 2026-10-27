use std::collections::HashMap;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::task::{Context as TaskContext, Poll};
use std::time::{Duration, Instant};

use anyhow::{anyhow, bail, Context, Result};
use quinn::Connection;
use rustls::version::TLS13;
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, ReadBuf};
use tokio::sync::Mutex;
use tokio_rustls::TlsConnector;
use x509_parser::prelude::{FromDer, X509Certificate};

use rustls::sign::{CertifiedKey, SingleCertAndKey};

use crate::tunnel_pool::{
    build_client_certified_key, classify_quinn, parse_ca_bundle, parse_cert_chain,
    root_store_from_cert, AuthenticatedStream, ExactSpiffeVerifier, TunnelOpenError,
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
    connections: Arc<Mutex<HashMap<String, CachedRelay>>>,
    endpoint: quinn::Endpoint,
    /// Cert chain + signing key, software or TPM-backed. Held rather than
    /// rebuilt because the inner (relay-wrapped) tunnel handshake in
    /// `open_tunnel` authenticates with the same identity.
    client_key: Arc<CertifiedKey>,
    workspace_ca: rustls::pki_types::CertificateDer<'static>,
    relay_crl: crate::crl::CrlManager,
}

struct CachedRelay {
    connection: Connection,
    established_at: Instant,
    flows: Arc<RelayFlows>,
}

/// Sprint 20 F43: after this age a cached relay connection is *retired*, not
/// closed: new flows dial a fresh connection, and the old one is closed when
/// its last flow ends. Closing it at once (as before) killed every flow still
/// multiplexed on it. Revocation does not depend on this: each connection's
/// revocation monitor closes it within 1 s of a revoked serial or an
/// unavailable CRL.
const MAX_RELAY_CONNECTION_AGE: Duration = Duration::from_secs(60);

/// Open flows on one relay connection and whether it has been retired. The
/// relay keeps connections alive (keep-alive pings) and the revocation monitor
/// holds a handle, so a retired connection never idles out on its own: the
/// last flow guard closes it.
struct RelayFlows {
    connection: Connection,
    active: AtomicUsize,
    retired: AtomicBool,
}

impl RelayFlows {
    fn new(connection: Connection) -> Arc<Self> {
        Arc::new(Self {
            connection,
            active: AtomicUsize::new(0),
            retired: AtomicBool::new(false),
        })
    }

    /// Counts a flow. Called under the pool lock, so a connection can't be
    /// retired between being handed out and its flow being counted.
    fn guard(self: &Arc<Self>) -> RelayFlowGuard {
        self.active.fetch_add(1, Ordering::SeqCst);
        RelayFlowGuard(self.clone())
    }

    /// Stops new flows (caller removes it from the cache) and closes it now if
    /// no flow is open. Called under the pool lock.
    fn retire(&self) {
        self.retired.store(true, Ordering::SeqCst);
        if self.active.load(Ordering::SeqCst) == 0 {
            self.connection
                .close(0u32.into(), b"relay connection retired");
        }
    }
}

/// One open flow on a relay connection; dropping it ends the flow.
struct RelayFlowGuard(Arc<RelayFlows>);

impl Drop for RelayFlowGuard {
    fn drop(&mut self) {
        let flows = &self.0;
        if flows.active.fetch_sub(1, Ordering::SeqCst) == 1 && flows.retired.load(Ordering::SeqCst)
        {
            flows
                .connection
                .close(0u32.into(), b"relay connection retired");
        }
    }
}

/// A relay stream that keeps its connection's flow count while it lives.
struct RelayFlowStream<S> {
    inner: S,
    _flow: RelayFlowGuard,
}

impl<S: AsyncRead + Unpin> AsyncRead for RelayFlowStream<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_read(cx, buf)
    }
}

impl<S: AsyncWrite + Unpin> AsyncWrite for RelayFlowStream<S> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buf)
    }

    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut TaskContext<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }

    fn poll_shutdown(
        mut self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }

    fn poll_write_vectored(
        mut self: Pin<&mut Self>,
        cx: &mut TaskContext<'_>,
        bufs: &[std::io::IoSlice<'_>],
    ) -> Poll<std::io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write_vectored(cx, bufs)
    }

    fn is_write_vectored(&self) -> bool {
        self.inner.is_write_vectored()
    }
}

impl RelayPool {
    pub fn new(
        cert_pem: &str,
        key_pem: &str,
        tpm_key_material: Option<&str>,
        ca_bundle_pem: &str,
        relay_spiffe_id: &str,
        relay_crl: crate::crl::CrlManager,
    ) -> Result<Self> {
        let mut client_cert_chain = parse_cert_chain(cert_pem, "device certificate")?;
        let ca_bundle = parse_ca_bundle(ca_bundle_pem)?;

        // Relay's mTLS verifier needs the workspace CA in the chain to reach
        // the platform root; the device leaf alone fails with UnknownIssuer.
        if client_cert_chain.len() == 1 {
            client_cert_chain.push(ca_bundle.workspace_ca.clone());
        }
        let client_key = build_client_certified_key(client_cert_chain, key_pem, tpm_key_material)?;

        let relay_roots =
            root_store_from_cert(&ca_bundle.intermediate_ca, "platform intermediate CA")?;
        let verifier =
            ExactSpiffeVerifier::new_relay(relay_roots, relay_spiffe_id, relay_crl.clone())?;

        let mut tls_config = rustls::ClientConfig::builder()
            .dangerous()
            .with_custom_certificate_verifier(verifier)
            .with_client_cert_resolver(Arc::new(SingleCertAndKey::from(client_key.clone())));
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
            client_key,
            workspace_ca: ca_bundle.workspace_ca,
            relay_crl,
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
        let (conn, flow) = self.get_or_connect(relay_addr).await?;
        let (mut send, mut recv) = match conn.open_bi().await {
            Ok(streams) => streams,
            Err(error) => {
                if let Some(stale) = self.connections.lock().await.remove(relay_addr) {
                    stale.flows.retire();
                }
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
            .with_client_cert_resolver(Arc::new(SingleCertAndKey::from(self.client_key.clone())));
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
        Ok(Box::new(RelayFlowStream {
            inner: tls_stream,
            _flow: flow,
        }))
    }

    async fn get_or_connect(
        &self,
        relay_addr: &str,
    ) -> Result<(Connection, RelayFlowGuard), TunnelOpenError> {
        if !self.relay_crl.has_valid_cache() {
            self.close_cached(relay_addr, b"relay revocation state unavailable")
                .await;
            return Err(TunnelOpenError::Authenticate(anyhow!(
                "relay revocation state unavailable"
            )));
        }
        {
            let mut conns = self.connections.lock().await;
            if let Some(conn) = conns.get(relay_addr) {
                if conn.connection.close_reason().is_none()
                    && conn.established_at.elapsed() < MAX_RELAY_CONNECTION_AGE
                {
                    return Ok((conn.connection.clone(), conn.flows.guard()));
                }
                if let Some(stale) = conns.remove(relay_addr) {
                    stale.flows.retire();
                }
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
        let peer_serial =
            peer_certificate_serial(&new_conn).map_err(TunnelOpenError::Authenticate)?;
        spawn_relay_revocation_monitor(new_conn.clone(), self.relay_crl.clone(), peer_serial);

        let mut conns = self.connections.lock().await;
        if let Some(existing) = conns.get(relay_addr) {
            if existing.connection.close_reason().is_none()
                && existing.established_at.elapsed() < MAX_RELAY_CONNECTION_AGE
            {
                new_conn.close(0u32.into(), b"raced");
                return Ok((existing.connection.clone(), existing.flows.guard()));
            }
        }
        let flows = RelayFlows::new(new_conn.clone());
        let flow = flows.guard();
        if let Some(stale) = conns.insert(
            relay_addr.to_string(),
            CachedRelay {
                connection: new_conn.clone(),
                established_at: Instant::now(),
                flows,
            },
        ) {
            stale.flows.retire();
        }
        Ok((new_conn, flow))
    }

    async fn close_cached(&self, relay_addr: &str, reason: &'static [u8]) {
        if let Some(connection) = self.connections.lock().await.remove(relay_addr) {
            connection.connection.close(0u32.into(), reason);
        }
    }
}

fn peer_certificate_serial(connection: &Connection) -> Result<Vec<u8>> {
    let identity = connection
        .peer_identity()
        .context("Relay supplied no peer certificate")?;
    let chain = identity
        .downcast::<Vec<rustls::pki_types::CertificateDer<'static>>>()
        .map_err(|_| anyhow!("unexpected Relay peer identity type"))?;
    let leaf = chain
        .first()
        .context("Relay supplied an empty certificate chain")?;
    let (_, cert) = X509Certificate::from_der(leaf.as_ref())
        .map_err(|_| anyhow!("parse Relay peer certificate"))?;
    Ok(cert.raw_serial().to_vec())
}

fn relay_rejection_reason(
    manager: &crate::crl::CrlManager,
    serial: &[u8],
) -> Option<&'static [u8]> {
    match manager.check(serial) {
        crate::crl::RevocationStatus::NotRevoked => None,
        crate::crl::RevocationStatus::Revoked => Some(b"relay certificate revoked"),
        crate::crl::RevocationStatus::Unavailable => Some(b"relay revocation state unavailable"),
    }
}

fn spawn_relay_revocation_monitor(
    connection: Connection,
    manager: crate::crl::CrlManager,
    serial: Vec<u8>,
) {
    tokio::spawn(async move {
        let mut ticker = tokio::time::interval(Duration::from_secs(1));
        loop {
            tokio::select! {
                _ = ticker.tick() => {
                    if let Some(reason) = relay_rejection_reason(&manager, &serial) {
                        connection.close(0u32.into(), reason);
                        break;
                    }
                }
                _ = connection.closed() => break,
            }
        }
    });
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

    /// A real QUIC connection to a local server that never closes it.
    async fn quic_connection() -> Connection {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let cert = rcgen::generate_simple_self_signed(vec!["relay".to_string()]).unwrap();
        let der = rustls::pki_types::CertificateDer::from(cert.cert.der().to_vec());
        let key = rustls::pki_types::PrivatePkcs8KeyDer::from(cert.key_pair.serialize_der());
        let mut server_tls = rustls::ServerConfig::builder()
            .with_no_client_auth()
            .with_single_cert(vec![der.clone()], key.into())
            .unwrap();
        server_tls.alpn_protocols = vec![b"t".to_vec()];
        let server_cfg = quinn::ServerConfig::with_crypto(Arc::new(
            quinn::crypto::rustls::QuicServerConfig::try_from(server_tls).unwrap(),
        ));
        let server = quinn::Endpoint::server(server_cfg, "127.0.0.1:0".parse().unwrap()).unwrap();
        let addr = server.local_addr().unwrap();
        tokio::spawn(async move {
            while let Some(incoming) = server.accept().await {
                if let Ok(conn) = incoming.await {
                    conn.closed().await;
                }
            }
        });
        let mut roots = rustls::RootCertStore::empty();
        roots.add(der).unwrap();
        let mut client_tls = rustls::ClientConfig::builder()
            .with_root_certificates(roots)
            .with_no_client_auth();
        client_tls.alpn_protocols = vec![b"t".to_vec()];
        let mut client = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
        client.set_default_client_config(quinn::ClientConfig::new(Arc::new(
            quinn::crypto::rustls::QuicClientConfig::try_from(client_tls).unwrap(),
        )));
        client.connect(addr, "relay").unwrap().await.unwrap()
    }

    /// Sprint 20 F43: an aged relay connection is retired, not closed — flows
    /// still on it keep running, and it closes when the last one ends.
    #[tokio::test]
    async fn retired_relay_connection_closes_when_its_last_flow_ends() {
        let conn = quic_connection().await;
        let flows = RelayFlows::new(conn.clone());
        let first = flows.guard();
        let second = flows.guard();
        flows.retire();
        assert!(
            conn.close_reason().is_none(),
            "open flows keep a retired connection"
        );
        drop(first);
        assert!(conn.close_reason().is_none(), "one flow is still open");
        drop(second);
        assert!(
            conn.close_reason().is_some(),
            "the last flow closes a retired connection"
        );
    }

    /// Retiring a connection with no open flow closes it at once.
    #[tokio::test]
    async fn retiring_an_idle_relay_connection_closes_it() {
        let conn = quic_connection().await;
        RelayFlows::new(conn.clone()).retire();
        assert!(conn.close_reason().is_some());
    }

    /// Flows ending on a connection that is still cached don't close it.
    #[tokio::test]
    async fn a_cached_relay_connection_outlives_its_flows() {
        let conn = quic_connection().await;
        let flows = RelayFlows::new(conn.clone());
        drop(flows.guard());
        drop(flows.guard());
        assert!(conn.close_reason().is_none());
    }

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

    #[test]
    fn relay_connection_fails_closed_without_crl() {
        let manager = crate::crl::CrlManager::new();
        assert_eq!(
            relay_rejection_reason(&manager, &[42]),
            Some(b"relay revocation state unavailable".as_slice())
        );
    }

    #[test]
    fn relay_connection_rejects_revoked_serial() {
        let manager = crate::crl::CrlManager::new();
        manager.install_test_cache(vec![vec![42]]);
        assert_eq!(
            relay_rejection_reason(&manager, &[42]),
            Some(b"relay certificate revoked".as_slice())
        );
    }

    #[test]
    fn relay_connection_accepts_non_revoked_serial_with_valid_crl() {
        let manager = crate::crl::CrlManager::new();
        manager.install_test_cache(Vec::new());
        assert_eq!(relay_rejection_reason(&manager, &[42]), None);
    }
}
