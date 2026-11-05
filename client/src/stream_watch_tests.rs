//! FU-16: `WatchedStream` / `ResetAwareRecv` against a real quinn connection
//! on loopback.

use super::*;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;

const ALPN: &[u8] = b"fu16-test";
const WAIT: Duration = Duration::from_secs(5);

/// What the test server does with the first stream a client opens.
#[derive(Clone, Copy)]
enum Server {
    /// Write `n` bytes, then reset the stream.
    WriteThenReset(usize),
    /// Write `n` bytes, then close the whole connection.
    WriteThenClose(usize),
    /// Write `n` bytes and finish the stream.
    WriteThenFinish(usize),
    /// Keep the stream open, send nothing.
    Idle,
}

fn provider() -> Arc<rustls::crypto::CryptoProvider> {
    Arc::new(rustls::crypto::ring::default_provider())
}

/// A server running `behaviour`; returns its address and a client endpoint
/// that trusts it.
fn lab(behaviour: Server) -> (SocketAddr, quinn::Endpoint, mpsc::Receiver<()>) {
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
    let crypto = quinn::crypto::rustls::QuicServerConfig::try_from(tls).unwrap();
    let server = quinn::Endpoint::server(
        quinn::ServerConfig::with_crypto(Arc::new(crypto)),
        "127.0.0.1:0".parse().unwrap(),
    )
    .unwrap();
    let addr = server.local_addr().unwrap();
    let (done_tx, done_rx) = mpsc::channel(4);
    tokio::spawn(async move {
        let conn = server.accept().await.unwrap().await.unwrap();
        let (mut send, mut recv) = conn.accept_bi().await.unwrap();
        let mut first = [0u8; 1];
        recv.read_exact(&mut first).await.unwrap();
        match behaviour {
            Server::WriteThenReset(n) => {
                send.write_all(&vec![7u8; n]).await.unwrap();
                tokio::time::sleep(Duration::from_millis(200)).await;
                send.reset(0x5256_4b44u32.into()).unwrap();
            }
            Server::WriteThenClose(n) => {
                send.write_all(&vec![7u8; n]).await.unwrap();
                tokio::time::sleep(Duration::from_millis(200)).await;
                conn.close(0u32.into(), b"connector stopped");
            }
            Server::WriteThenFinish(n) => {
                send.write_all(&vec![7u8; n]).await.unwrap();
                send.finish().unwrap();
            }
            Server::Idle => {}
        }
        let _ = done_tx.send(()).await;
        // Keep the connection (and endpoint) alive for the test.
        tokio::time::sleep(Duration::from_secs(30)).await;
        drop((send, recv, conn, server));
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
    (addr, client, done_rx)
}

/// Connect, open a stream and announce it (the server sees a stream only
/// once data arrives). Returns the connection and the raw stream halves.
async fn open(
    addr: SocketAddr,
    client: &quinn::Endpoint,
) -> (quinn::Connection, quinn::SendStream, quinn::RecvStream) {
    let conn = client.connect(addr, "localhost").unwrap().await.unwrap();
    let (mut send, recv) = conn.open_bi().await.unwrap();
    send.write_all(b"!").await.unwrap();
    (conn, send, recv)
}

fn watched(
    conn: &quinn::Connection,
    send: quinn::SendStream,
    recv: quinn::RecvStream,
) -> WatchedStream<tokio::io::Join<ResetAwareRecv, quinn::SendStream>> {
    WatchedStream::new(
        tokio::io::join(ResetAwareRecv(recv), send),
        quic_connection_gone(conn),
        true,
    )
}

/// A watch read reports the peer's reset while 100 KB sits unread: it
/// consumes nothing (it is not a data read), and nothing of the data is
/// handed out afterwards.
#[tokio::test]
async fn a_watch_sees_a_reset_without_consuming_data() {
    let (addr, client, mut done) = lab(Server::WriteThenReset(100 * 1024));
    let (conn, send, recv) = open(addr, &client).await;
    let mut stream = watched(&conn, send, recv);
    done.recv().await.unwrap();
    let err = tokio::time::timeout(WAIT, tunnel_ended(&mut stream))
        .await
        .expect("the reset is seen without reading");
    assert_eq!(err.kind(), io::ErrorKind::ConnectionReset, "{err}");
    let mut buf = [0u8; 1024];
    assert!(
        stream.read(&mut buf).await.is_err(),
        "data of a reset stream was handed out"
    );
}

/// Once the connection is closed, a normal read fails instead of handing out
/// the data quinn still holds for the stream. Control: the same situation on
/// a raw quinn stream does hand the data out.
#[tokio::test]
async fn a_closed_connection_hands_out_no_buffered_data() {
    // Control: raw quinn.
    let (addr, client, mut done) = lab(Server::WriteThenClose(200 * 1024));
    let (_conn, _send, mut raw) = open(addr, &client).await;
    done.recv().await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let mut buf = vec![0u8; 64 * 1024];
    let n = raw
        .read(&mut buf)
        .await
        .expect("quinn hands out buffered data");
    assert!(
        matches!(n, Some(n) if n > 0),
        "control: quinn returned {n:?}"
    );

    // Watched.
    let (addr, client, mut done) = lab(Server::WriteThenClose(200 * 1024));
    let (conn, send, recv) = open(addr, &client).await;
    let mut stream = watched(&conn, send, recv);
    done.recv().await.unwrap();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let err = stream
        .read(&mut buf)
        .await
        .expect_err("buffered data of a closed connection was handed out");
    assert_eq!(err.kind(), io::ErrorKind::ConnectionAborted);
}

/// A watch read resolves when the connection closes.
#[tokio::test]
async fn a_watch_ends_when_the_connection_closes() {
    let (addr, client, _done) = lab(Server::WriteThenClose(1024));
    let (conn, send, recv) = open(addr, &client).await;
    let mut stream = watched(&conn, send, recv);
    let err = tokio::time::timeout(WAIT, tunnel_ended(&mut stream))
        .await
        .expect("the close is seen");
    assert_eq!(err.kind(), io::ErrorKind::ConnectionAborted);
}

/// A stream the peer finished is read to its end, and is not an error for
/// the watch afterwards.
#[tokio::test]
async fn a_finished_stream_is_delivered_and_is_not_an_error() {
    let (addr, client, mut done) = lab(Server::WriteThenFinish(150 * 1024));
    let (conn, send, recv) = open(addr, &client).await;
    let mut stream = watched(&conn, send, recv);
    done.recv().await.unwrap();
    let mut all = Vec::new();
    tokio::time::timeout(WAIT, stream.read_to_end(&mut all))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(all.len(), 150 * 1024);
    assert!(
        tokio::time::timeout(Duration::from_millis(300), tunnel_ended(&mut stream))
            .await
            .is_err(),
        "a finished stream on a live connection reported an error"
    );
}

/// While the tunnel is up the watch stays pending and costs nothing.
#[tokio::test]
async fn a_watch_stays_pending_while_the_tunnel_is_up() {
    let (addr, client, _done) = lab(Server::Idle);
    let (conn, send, recv) = open(addr, &client).await;
    let mut stream = watched(&conn, send, recv);
    assert!(
        tokio::time::timeout(Duration::from_millis(300), tunnel_ended(&mut stream))
            .await
            .is_err()
    );
    stream.write_all(b"still usable").await.unwrap();
}
