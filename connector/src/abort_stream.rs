//! FU-15 / FU-17: ending a device stream abruptly.
//!
//! - FU-15: when its session's authorization is revoked (`SESSION_REVOKED_CODE`).
//! - FU-17: when its tunnel fails, i.e. the upstream (shield tunnel or resource
//!   connection) ended abnormally (`TUNNEL_FAILED_CODE`). A dropped stream would
//!   reach the app as a clean EOF, so a truncated download looked complete
//!   (live run 2026-11-05, LT-1).
//!
//! A normal end of the upstream stays a graceful shutdown (F-7).
//!
//! Dropping a device stream ends it *normally*: quinn finishes a dropped
//! `SendStream`, so everything the connector had queued still reaches the
//! client, which then hands its own backlog to the app. After a member
//! removal a rate-limited download kept receiving 5.6 MB for 18.6 s (live E2E
//! run, O-6). `abort` discards instead: QUIC reset (the peer drops what it
//! hasn't delivered and sees an error), or a TCP RST on the direct TLS path.

use std::time::Duration;

/// QUIC application error code for a session whose authorization was revoked
/// ("RVKD").
pub const SESSION_REVOKED_CODE: u32 = 0x5256_4b44;

/// QUIC application error code for a tunnel whose upstream failed: the
/// shield tunnel or the resource connection ended abnormally ("TFLD").
pub const TUNNEL_FAILED_CODE: u32 = 0x5446_4c44;

/// A device stream that can be ended abruptly. Best effort: a stream that is
/// already closed has nothing left to discard.
pub trait AbortStream {
    /// End the stream abruptly; `code` is the QUIC application error code
    /// (ignored where the transport has none).
    fn abort(&mut self, code: u32);
}

/// Direct QUIC device stream (`quic_listener`).
impl AbortStream for tokio::io::Join<quinn::RecvStream, quinn::SendStream> {
    fn abort(&mut self, code: u32) {
        let code = quinn::VarInt::from_u32(code);
        let _ = self.writer_mut().reset(code);
        let _ = self.reader_mut().stop(code);
    }
}

/// Direct TLS-over-TCP device stream: an RST when the socket is dropped.
impl AbortStream for tokio::net::TcpStream {
    fn abort(&mut self, _code: u32) {
        let _ = self.set_zero_linger();
    }
}

/// Inner mTLS stream (relay path, TLS over TCP): abort the transport under it.
impl<IO: AbortStream> AbortStream for tokio_rustls::server::TlsStream<IO> {
    fn abort(&mut self, code: u32) {
        self.get_mut().0.abort(code);
    }
}

/// In-memory streams (tests): nothing to discard.
impl AbortStream for tokio::io::DuplexStream {
    fn abort(&mut self, _code: u32) {}
}

/// How long `relay_stream` waits for its device→shield task to hand the read
/// half back after a cancel before giving up on the abort.
pub const ABORT_REJOIN_GRACE: Duration = Duration::from_secs(1);

/// FU-17: what the peer of a device stream sees, over a real quinn
/// connection on loopback.
#[cfg(test)]
mod tests {
    use super::*;
    use std::net::SocketAddr;
    use std::sync::Arc;
    use tokio::io::AsyncWriteExt;

    const ALPN: &[u8] = b"fu17-test";
    const BODY: usize = 64 * 1024;

    #[derive(Clone, Copy)]
    enum End {
        Abort(u32),
        Drop,
        Finish,
    }

    fn provider() -> Arc<rustls::crypto::CryptoProvider> {
        Arc::new(rustls::crypto::ring::default_provider())
    }

    /// A server that answers the first stream with BODY bytes, then ends it
    /// as `end` says, through the device-stream type `quic_listener` uses.
    fn lab(end: End) -> (SocketAddr, quinn::Endpoint) {
        let key = rcgen::KeyPair::generate().unwrap();
        let cert = rcgen::CertificateParams::new(vec!["localhost".into()])
            .unwrap()
            .self_signed(&key)
            .unwrap();
        let der = cert.der().clone();
        let key_der = rustls::pki_types::PrivateKeyDer::try_from(key.serialize_der()).unwrap();
        let mut tls = rustls::ServerConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_no_client_auth()
            .with_single_cert(vec![der.clone()], key_der)
            .unwrap();
        tls.alpn_protocols = vec![ALPN.to_vec()];
        let server = quinn::Endpoint::server(
            quinn::ServerConfig::with_crypto(Arc::new(
                quinn::crypto::rustls::QuicServerConfig::try_from(tls).unwrap(),
            )),
            "127.0.0.1:0".parse().unwrap(),
        )
        .unwrap();
        let addr = server.local_addr().unwrap();
        tokio::spawn(async move {
            let conn = server.accept().await.unwrap().await.unwrap();
            let (send, mut recv) = conn.accept_bi().await.unwrap();
            let mut first = [0u8; 1];
            recv.read_exact(&mut first).await.unwrap();
            let mut stream = tokio::io::join(recv, send);
            stream.write_all(&vec![7u8; BODY]).await.unwrap();
            match end {
                End::Abort(code) => stream.abort(code),
                End::Drop => drop(stream),
                End::Finish => stream.shutdown().await.unwrap(),
            }
            tokio::time::sleep(Duration::from_secs(5)).await;
            drop((conn, server));
        });
        let mut roots = rustls::RootCertStore::empty();
        roots.add(der).unwrap();
        let mut client_tls = rustls::ClientConfig::builder_with_provider(provider())
            .with_protocol_versions(&[&rustls::version::TLS13])
            .unwrap()
            .with_root_certificates(roots)
            .with_no_client_auth();
        client_tls.alpn_protocols = vec![ALPN.to_vec()];
        let mut client = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
        client.set_default_client_config(quinn::ClientConfig::new(Arc::new(
            quinn::crypto::rustls::QuicClientConfig::try_from(client_tls).unwrap(),
        )));
        (addr, client)
    }

    /// Read the whole stream as the client does.
    async fn client_reads(end: End) -> Result<usize, quinn::ReadToEndError> {
        let (addr, client) = lab(end);
        let conn = client.connect(addr, "localhost").unwrap().await.unwrap();
        let (mut send, mut recv) = conn.open_bi().await.unwrap();
        send.write_all(b"!").await.unwrap();
        tokio::time::timeout(Duration::from_secs(5), recv.read_to_end(16 * BODY))
            .await
            .expect("the stream ends")
            .map(|body| body.len())
    }

    /// An aborted device stream reaches the peer as an error carrying the
    /// code, never as a clean end of a (truncated) body.
    #[tokio::test]
    async fn an_aborted_stream_reaches_the_peer_as_an_error_not_eof() {
        for code in [TUNNEL_FAILED_CODE, SESSION_REVOKED_CODE] {
            match client_reads(End::Abort(code)).await {
                Err(quinn::ReadToEndError::Read(quinn::ReadError::Reset(got))) => {
                    assert_eq!(got, quinn::VarInt::from_u32(code))
                }
                other => panic!("code {code:#x}: expected a reset, got {other:?}"),
            }
        }
    }

    /// The FU-17 mechanism (control): a dropped stream is finished by quinn,
    /// so the peer reads every byte and a clean end — indistinguishable from
    /// a normal completion. Hence `abort`, not drop, on a failed tunnel.
    #[tokio::test]
    async fn a_dropped_stream_reaches_the_peer_as_a_clean_end() {
        assert_eq!(client_reads(End::Drop).await.unwrap(), BODY);
        assert_eq!(client_reads(End::Finish).await.unwrap(), BODY);
    }
}
