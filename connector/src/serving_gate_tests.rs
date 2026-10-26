//! Connector-revocation Phase B1 (AB1-1, design §7 step 2): every
//! data-plane listener refuses at the transport level while the connector
//! isn't authorized to serve, and admits once it is. Real sockets, real
//! TLS/QUIC, the production listeners; the serving state is driven through
//! the production `AckTracker`.

use std::sync::Arc;
use std::time::Duration;

use tokio::sync::mpsc;
use tokio::time::{timeout, Instant};

use crate::agent_tunnel::AgentTunnelHub;
use crate::authorization::{AckTracker, SharedAckTracker, TerminalSignal};
use crate::crl::CrlManager;
use crate::policy::PolicyCache;
use crate::relay_handler::{RelayDrainTracker, RelayHandler};
use crate::session_registry::SessionRegistry;
use crate::test_support::{install_crypto_provider, TestPki, CONNECTOR_ID};

const H: Duration = Duration::from_secs(3600);
const WAIT: Duration = Duration::from_secs(5);
const ATTEMPT: Duration = Duration::from_millis(500);

/// Accept a valid acknowledgement now: Starting → Serving.
fn serve(authz: &SharedAckTracker) {
    let mut t = authz.lock();
    let s = t.next_seq();
    let now = Instant::now();
    t.record_sent(s, now);
    t.on_ack(s, true, now).expect("ack accepted");
}

fn free_tcp_port() -> u16 {
    std::net::TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn free_udp_port() -> u16 {
    std::net::UdpSocket::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

async fn tcp_connects(port: u16) -> bool {
    matches!(
        timeout(ATTEMPT, tokio::net::TcpStream::connect(("127.0.0.1", port))).await,
        Ok(Ok(_))
    )
}

async fn tls_handshake(pki: &TestPki, port: u16) -> bool {
    let Ok(Ok(tcp)) = timeout(ATTEMPT, tokio::net::TcpStream::connect(("127.0.0.1", port))).await
    else {
        return false;
    };
    let connector = tokio_rustls::TlsConnector::from(pki.client_tls_config(false));
    let name = rustls::pki_types::ServerName::try_from("localhost").unwrap();
    matches!(timeout(ATTEMPT, connector.connect(name, tcp)).await, Ok(Ok(_)))
}

async fn eventually<F, Fut>(what: &str, f: F)
where
    F: Fn() -> Fut,
    Fut: std::future::Future<Output = bool>,
{
    let deadline = Instant::now() + WAIT;
    while !f().await {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
}

/// TLS device tunnel: Starting refuses (socket dropped before the TLS
/// handshake), an accepted ack opens it, a terminal signal closes it again.
#[tokio::test]
async fn tls_listener_follows_the_serving_state() {
    install_crypto_provider();
    let pki = TestPki::new();
    let (_dir, holder) = pki.holder(-60, 3600);
    let authz = AckTracker::shared(H);
    let gate = authz.lock().gate();
    let port = free_tcp_port();
    let (tx, _rx) = mpsc::channel(8);
    let _listener = tokio::spawn(async move {
        crate::device_tunnel::listen(
            &format!("127.0.0.1:{port}"),
            holder,
            Arc::new(PolicyCache::new()),
            Arc::new(SessionRegistry::new()),
            AgentTunnelHub::new(),
            CrlManager::new(),
            CONNECTOR_ID.to_string(),
            tx,
            gate,
        )
        .await
    });
    eventually("listener up", || tcp_connects(port)).await;

    assert!(!tls_handshake(&pki, port).await, "Starting: TLS must be refused");
    serve(&authz);
    assert!(tls_handshake(&pki, port).await, "Serving: TLS accepted");
    authz.lock().on_terminal(TerminalSignal::Revoked);
    assert!(!tls_handshake(&pki, port).await, "Revoked: TLS must be refused");
}

async fn quic_connect(
    client: &quinn::Endpoint,
    addr: std::net::SocketAddr,
) -> Option<quinn::Connection> {
    match timeout(ATTEMPT, client.connect(addr, "localhost").unwrap()).await {
        Ok(Ok(conn)) => Some(conn),
        _ => None,
    }
}

/// One tunnel request on a new stream: Ok(bytes received before EOF) or the
/// read error.
async fn quic_request(conn: &quinn::Connection) -> Result<Vec<u8>, String> {
    let (mut send, mut recv) = conn.open_bi().await.map_err(|e| e.to_string())?;
    let body = br#"{"destination":"127.0.0.1","port":1,"protocol":"tcp"}"#;
    let _ = send.write_all(&(body.len() as u32).to_be_bytes()).await;
    let _ = send.write_all(body).await;
    let _ = send.finish();
    match timeout(WAIT, recv.read_to_end(64 * 1024)).await {
        Ok(Ok(bytes)) => Ok(bytes),
        Ok(Err(e)) => Err(e.to_string()),
        Err(_) => Err("timed out".into()),
    }
}

/// QUIC device tunnel: new connections refused while closed; on an
/// established connection, new streams are reset (no TunnelResponse) while
/// `Disabled`, and the connection itself is closed in a terminal state (B2,
/// D15).
#[tokio::test]
async fn quic_listener_follows_the_serving_state() {
    install_crypto_provider();
    let pki = TestPki::new();
    let (_dir, holder) = pki.holder(-60, 3600);
    let authz = AckTracker::shared(H);
    let gate = authz.lock().gate();
    serve(&authz);
    let port = free_udp_port();
    let addr: std::net::SocketAddr = format!("127.0.0.1:{port}").parse().unwrap();
    let (tx, _rx) = mpsc::channel(8);
    let _listener = tokio::spawn(async move {
        crate::quic_listener::listen(
            &format!("127.0.0.1:{port}"),
            &format!("127.0.0.1:{port}"),
            holder,
            Arc::new(PolicyCache::new()),
            Arc::new(SessionRegistry::new()),
            AgentTunnelHub::new(),
            CrlManager::new(),
            CONNECTOR_ID.to_string(),
            tx,
            gate,
            tokio_util::sync::CancellationToken::new(),
        )
        .await
    });
    let client = pki.quic_client();
    let deadline = Instant::now() + WAIT;
    let conn = loop {
        if let Some(c) = quic_connect(&client, addr).await {
            break c;
        }
        assert!(Instant::now() < deadline, "QUIC listener never accepted");
    };
    let served = quic_request(&conn).await.expect("serving: stream answered");
    assert!(!served.is_empty(), "serving: a TunnelResponse is sent");

    // Recoverable Disabled (D15): the connection is kept, but a new stream
    // on it is reset and a new connection is refused.
    authz.lock().expire(Instant::now() + H);
    let refused = quic_request(&conn).await;
    assert!(
        matches!(&refused, Err(e) if e.contains("reset")),
        "Disabled, existing connection: a new stream must be reset, got {refused:?}"
    );
    assert!(conn.close_reason().is_none(), "Disabled keeps the client connection (D15)");
    assert!(
        quic_connect(&client, addr).await.is_none(),
        "Disabled: a new QUIC connection must be refused"
    );

    // Terminal (B2): the client connection itself is closed.
    authz.lock().on_terminal(TerminalSignal::UnknownIdentity);
    let deadline = Instant::now() + WAIT;
    while conn.close_reason().is_none() {
        assert!(Instant::now() < deadline, "terminal: client QUIC connection not closed");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    assert!(
        quic_connect(&client, addr).await.is_none(),
        "terminal: a new QUIC connection must be refused"
    );
}

/// Sprint 20 Fix 16 (F51): on shutdown the QUIC listener closes every client
/// connection with a CONNECTION_CLOSE (clients move on at once instead of
/// waiting for their idle timeout), refuses new ones, and returns.
#[tokio::test]
async fn quic_listener_closes_client_connections_on_shutdown() {
    install_crypto_provider();
    let pki = TestPki::new();
    let (_dir, holder) = pki.holder(-60, 3600);
    let authz = AckTracker::shared(H);
    let gate = authz.lock().gate();
    serve(&authz);
    let port = free_udp_port();
    let addr: std::net::SocketAddr = format!("127.0.0.1:{port}").parse().unwrap();
    let (tx, _rx) = mpsc::channel(8);
    let shutdown = tokio_util::sync::CancellationToken::new();
    let listener = {
        let shutdown = shutdown.clone();
        tokio::spawn(async move {
            crate::quic_listener::listen(
                &format!("127.0.0.1:{port}"),
                &format!("127.0.0.1:{port}"),
                holder,
                Arc::new(PolicyCache::new()),
                Arc::new(SessionRegistry::new()),
                AgentTunnelHub::new(),
                CrlManager::new(),
                CONNECTOR_ID.to_string(),
                tx,
                gate,
                shutdown,
            )
            .await
        })
    };
    let client = pki.quic_client();
    let deadline = Instant::now() + WAIT;
    let conn = loop {
        if let Some(c) = quic_connect(&client, addr).await {
            break c;
        }
        assert!(Instant::now() < deadline, "QUIC listener never accepted");
    };
    assert!(conn.close_reason().is_none());

    shutdown.cancel();

    let deadline = Instant::now() + WAIT;
    while conn.close_reason().is_none() {
        assert!(Instant::now() < deadline, "shutdown: client QUIC connection not closed");
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let reason = format!("{:?}", conn.close_reason().unwrap());
    assert!(reason.contains("shutting down"), "close reason: {reason}");
    tokio::time::timeout(WAIT, listener)
        .await
        .expect("the listener returns after shutdown")
        .unwrap()
        .unwrap();
}

/// Relay-routed streams: reset before the inner handshake while closed.
#[tokio::test]
async fn relay_streams_are_reset_while_not_serving() {
    install_crypto_provider();
    let pki = TestPki::new();
    let (_dir, holder) = pki.holder(-60, 3600);
    let authz = AckTracker::shared(H);
    let (tx, _rx) = mpsc::channel(8);
    let handler = Arc::new(
        RelayHandler::new(
            holder.clone(),
            Arc::new(PolicyCache::new()),
            Arc::new(SessionRegistry::new()),
            AgentTunnelHub::new(),
            CrlManager::new(),
            CONNECTOR_ID.to_string(),
            tx,
            5,
            16,
            authz.lock().gate(),
        )
        .unwrap(),
    );
    // The Relay side (here: a plain QUIC peer) opens streams to the
    // connector's end of the connection.
    let server = quinn::Endpoint::server(
        crate::quic_listener::quic_server_config_for(&holder.current().store).unwrap(),
        "127.0.0.1:0".parse().unwrap(),
    )
    .unwrap();
    let addr = server.local_addr().unwrap();
    let client = pki.quic_client();
    let connecting = client.connect(addr, "localhost").unwrap();
    let connector_side = server.accept().await.unwrap().await.unwrap();
    let relay_side = connecting.await.unwrap();
    let _run = tokio::spawn(handler.run(connector_side, RelayDrainTracker::new()));

    let refused = quic_request(&relay_side).await;
    assert!(
        matches!(&refused, Err(e) if e.contains("reset")),
        "Starting: a Relay stream must be reset, got {refused:?}"
    );
}
