//! FU-9 (PENDING-21): the Shield's tunnels against a fake Connector — a real
//! tonic server whose `Tunnel` handler hands each bound stream to the test,
//! and that counts its live connections (renewal: an old connection must
//! close once its last tunnel ends). The client uses the production HTTP/2
//! settings (`tls::tunnel_endpoint`).

use super::*;
use std::pin::Pin;
use std::sync::atomic::AtomicUsize;
use std::task::{Context, Poll};
use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};
use tokio::net::TcpListener;
use tonic::{Response, Status};

#[allow(clippy::all, dead_code)]
mod fake {
    include!(concat!(env!("OUT_DIR"), "/test_server/shield.v1.rs"));
}
use fake::shield_service_server::{ShieldService, ShieldServiceServer};
use fake::tunnel_frame::Frame as FakeFrame;

const WAIT: Duration = Duration::from_secs(5);

// ── fake Connector ───────────────────────────────────────────────────────

/// A tunnel stream the fake Connector bound.
struct Bound {
    cid: String,
    /// The client port of the connection the stream runs on.
    port: u16,
    inbound: tonic::Streaming<fake::TunnelFrame>,
    outbound: mpsc::Sender<Result<fake::TunnelFrame, Status>>,
}

impl Bound {
    async fn send(&self, d: &[u8]) {
        self.outbound
            .send(Ok(fake::TunnelFrame {
                frame: Some(FakeFrame::Data(d.to_vec())),
            }))
            .await
            .expect("the shield reads the tunnel");
    }

    async fn end(&self) {
        let _ = self
            .outbound
            .send(Ok(fake::TunnelFrame {
                frame: Some(FakeFrame::End(fake::TunnelEnd::default())),
            }))
            .await;
    }

    async fn next(&mut self) -> Option<FakeFrame> {
        tokio::time::timeout(WAIT, self.inbound.message())
            .await
            .expect("a frame within the timeout")
            .ok()
            .flatten()
            .and_then(|f| f.frame)
    }

    async fn next_data(&mut self) -> Vec<u8> {
        match self.next().await {
            Some(FakeFrame::Data(d)) => d,
            other => panic!("expected data, got {other:?}"),
        }
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Mode {
    Accept,
    /// Hold every Tunnel call until the gate opens.
    Hold,
    /// A pre-FU-9 Connector.
    Unimplemented,
}

#[derive(Clone)]
struct FakeConnector {
    bound: mpsc::Sender<Bound>,
    mode: Arc<Mutex<Mode>>,
    gate: Arc<watch::Sender<bool>>,
}

type BoxStream<T> = Pin<Box<dyn tokio_stream::Stream<Item = Result<T, Status>> + Send>>;

#[tonic::async_trait]
impl ShieldService for FakeConnector {
    async fn enroll(
        &self,
        _: tonic::Request<fake::EnrollRequest>,
    ) -> Result<Response<fake::EnrollResponse>, Status> {
        Err(Status::unimplemented(""))
    }
    type ControlStream = BoxStream<fake::ShieldControlMessage>;
    async fn control(
        &self,
        _: tonic::Request<tonic::Streaming<fake::ShieldControlMessage>>,
    ) -> Result<Response<Self::ControlStream>, Status> {
        Err(Status::unimplemented(""))
    }
    async fn renew_cert(
        &self,
        _: tonic::Request<fake::RenewCertRequest>,
    ) -> Result<Response<fake::RenewCertResponse>, Status> {
        Err(Status::unimplemented(""))
    }
    async fn goodbye(
        &self,
        _: tonic::Request<fake::GoodbyeRequest>,
    ) -> Result<Response<fake::GoodbyeResponse>, Status> {
        Err(Status::unimplemented(""))
    }
    type TunnelStream = BoxStream<fake::TunnelFrame>;
    async fn tunnel(
        &self,
        request: tonic::Request<tonic::Streaming<fake::TunnelFrame>>,
    ) -> Result<Response<Self::TunnelStream>, Status> {
        let mode = *self.mode.lock().unwrap();
        match mode {
            Mode::Unimplemented => return Err(Status::unimplemented("no Tunnel")),
            Mode::Hold => {
                let _ = self.gate.subscribe().wait_for(|open| *open).await;
            }
            Mode::Accept => {}
        }
        let port = request.remote_addr().map(|a| a.port()).unwrap_or(0);
        let mut inbound = request.into_inner();
        let Some(fake::TunnelFrame {
            frame: Some(FakeFrame::Start(start)),
        }) = inbound.message().await?
        else {
            return Err(Status::invalid_argument("expected TunnelStart"));
        };
        let (outbound, rx) = mpsc::channel(4);
        let _ = self
            .bound
            .send(Bound {
                cid: start.connection_id,
                port,
                inbound,
                outbound,
            })
            .await;
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }
}

/// A TCP connection that counts itself while open.
struct Counted {
    inner: TcpStream,
    live: Arc<AtomicUsize>,
}

impl Drop for Counted {
    fn drop(&mut self) {
        self.live.fetch_sub(1, Ordering::SeqCst);
    }
}

impl tonic::transport::server::Connected for Counted {
    type ConnectInfo = tonic::transport::server::TcpConnectInfo;
    fn connect_info(&self) -> Self::ConnectInfo {
        self.inner.connect_info()
    }
}

impl AsyncRead for Counted {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_read(cx, buf)
    }
}

impl AsyncWrite for Counted {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buf)
    }
    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }
    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

struct Harness {
    addr: std::net::SocketAddr,
    bound: mpsc::Receiver<Bound>,
    mode: Arc<Mutex<Mode>>,
    gate: Arc<watch::Sender<bool>>,
    live: Arc<AtomicUsize>,
    hub: TunnelHub,
    refusals: mpsc::Receiver<ShieldControlMessage>,
}

async fn harness() -> Harness {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let live = Arc::new(AtomicUsize::new(0));
    let (conn_tx, conn_rx) = mpsc::channel::<Counted>(64);
    {
        let live = live.clone();
        tokio::spawn(async move {
            while let Ok((tcp, _)) = listener.accept().await {
                live.fetch_add(1, Ordering::SeqCst);
                let _ = conn_tx
                    .send(Counted {
                        inner: tcp,
                        live: live.clone(),
                    })
                    .await;
            }
        });
    }
    let (bound_tx, bound) = mpsc::channel(1024);
    let mode = Arc::new(Mutex::new(Mode::Accept));
    let gate = Arc::new(watch::channel(false).0);
    let svc = FakeConnector {
        bound: bound_tx,
        mode: mode.clone(),
        gate: gate.clone(),
    };
    tokio::spawn(
        tonic::transport::Server::builder()
            .initial_stream_window_size(crate::tls::H2_STREAM_WINDOW)
            .initial_connection_window_size(crate::tls::H2_CONNECTION_WINDOW)
            .add_service(ShieldServiceServer::new(svc))
            .serve_with_incoming(ReceiverStream::new(conn_rx).map(Ok::<_, std::io::Error>)),
    );
    let hub = new_hub();
    let (up_tx, refusals) = mpsc::channel(1024);
    hub.set_upstream(Some(up_tx));
    let h = Harness {
        addr,
        bound,
        mode,
        gate,
        live,
        hub,
        refusals,
    };
    h.hub.set_client(Some(h.client().await));
    h
}

use tokio_stream::StreamExt as _;

impl Harness {
    /// A client to the fake Connector with the production HTTP/2 settings
    /// (a new connection each time).
    async fn client(&self) -> Client {
        let channel = crate::tls::tunnel_endpoint(
            tonic::transport::Endpoint::from_shared(format!("http://{}", self.addr)).unwrap(),
        )
        .connect()
        .await
        .unwrap();
        ShieldServiceClient::new(channel)
    }

    async fn open(&self, cid: &str, port: u16, protocol: &str) {
        handle_tunnel_open(
            self.hub.clone(),
            TunnelOpen {
                connection_id: cid.into(),
                destination: "127.0.0.1".into(),
                port: port.into(),
                protocol: protocol.into(),
            },
        )
        .await;
    }

    async fn next_bound(&mut self) -> Bound {
        tokio::time::timeout(WAIT, self.bound.recv())
            .await
            .expect("the shield opens a tunnel stream")
            .unwrap()
    }

    async fn next_refusal(&mut self) -> TunnelOpened {
        match tokio::time::timeout(WAIT, self.refusals.recv()).await {
            Ok(Some(ShieldControlMessage {
                body: Some(Body::TunnelOpened(o)),
            })) => o,
            other => panic!("expected TunnelOpened, got {other:?}"),
        }
    }

    async fn wait_live(&self, n: usize) -> Option<Duration> {
        let t0 = Instant::now();
        while t0.elapsed() < Duration::from_secs(10) {
            if self.live.load(Ordering::SeqCst) == n {
                return Some(t0.elapsed());
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        None
    }
}

async fn wait_len(hub: &TunnelHub, n: usize) {
    tokio::time::timeout(WAIT, async {
        while hub.len() != n {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap_or_else(|_| panic!("expected {n} tunnels, have {}", hub.len()));
}

/// A TCP resource: accepts once and hands the socket out.
async fn tcp_resource() -> (u16, tokio::task::JoinHandle<TcpStream>) {
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    (
        port,
        tokio::spawn(async move { l.accept().await.unwrap().0 }),
    )
}

// ── TCP ──────────────────────────────────────────────────────────────────

/// Bytes flow both ways; the Connector's end half-closes the resource, whose
/// own end reaches the Connector as TunnelEnd{} and the end of the stream.
#[tokio::test]
async fn a_tcp_tunnel_relays_both_ways_and_ends_cleanly() {
    let mut h = harness().await;
    let (port, accepted) = tcp_resource().await;
    h.open("t1", port, "tcp").await;
    let mut bound = h.next_bound().await;
    assert_eq!(bound.cid, "t1");
    let mut resource = accepted.await.unwrap();

    bound.send(b"GET / HTTP/1.0\r\n\r\n").await;
    let mut req = [0u8; 18];
    resource.read_exact(&mut req).await.unwrap();
    assert_eq!(&req, b"GET / HTTP/1.0\r\n\r\n");
    bound.end().await;
    let mut rest = Vec::new();
    tokio::time::timeout(WAIT, resource.read_to_end(&mut rest))
        .await
        .unwrap()
        .unwrap();
    assert!(
        rest.is_empty(),
        "the connector's end half-closes the resource"
    );

    resource
        .write_all(b"HTTP/1.0 200 OK\r\n\r\nbody")
        .await
        .unwrap();
    resource.shutdown().await.unwrap();
    let mut got = Vec::new();
    loop {
        match bound.next().await {
            Some(FakeFrame::Data(d)) => got.extend(d),
            Some(FakeFrame::End(e)) => {
                assert!(e.error.is_empty());
                break;
            }
            other => panic!("unexpected {other:?}"),
        }
    }
    assert_eq!(got, b"HTTP/1.0 200 OK\r\n\r\nbody");
    drop(bound);
    wait_len(&h.hub, 0).await;
}

/// D-5: a resource that reads slower than the Connector sends, and one that
/// stops for longer than the old 2 s limit, gets every byte in order.
#[tokio::test]
async fn a_slow_or_paused_resource_is_not_cut() {
    let mut h = harness().await;
    let (port, accepted) = tcp_resource().await;
    h.open("slow", port, "tcp").await;
    let bound = h.next_bound().await;
    let mut resource = accepted.await.unwrap();
    const TOTAL: usize = 256 * 1024;
    let sender = tokio::spawn(async move {
        for i in 0..(TOTAL / 1024) {
            bound.send(&[(i % 251) as u8; 1024]).await;
        }
        bound.end().await;
        bound
    });
    // Pause 3 s, then read 8 KB every 50 ms (~160 KB/s).
    tokio::time::sleep(Duration::from_secs(3)).await;
    let mut got = Vec::with_capacity(TOTAL);
    let mut buf = [0u8; 8192];
    loop {
        let n = resource.read(&mut buf).await.unwrap();
        if n == 0 {
            break;
        }
        got.extend_from_slice(&buf[..n]);
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(got.len(), TOTAL);
    for (i, chunk) in got.chunks(1024).enumerate() {
        assert!(
            chunk.iter().all(|b| *b == (i % 251) as u8),
            "chunk {i} corrupted"
        );
    }
    let _ = sender.await.unwrap();
}

/// No head-of-line blocking at the Shield: with one tunnel's resource not
/// reading and its stream full, another tunnel moves data and a new tunnel
/// opens and moves data.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_stalled_tunnel_does_not_hold_up_others() {
    let mut h = harness().await;
    let (port, accepted) = tcp_resource().await;
    h.open("stalled", port, "tcp").await;
    let stalled = h.next_bound().await;
    let _stalled_resource = accepted.await.unwrap(); // never read
    let flooding = tokio::spawn(async move {
        loop {
            stalled.send(&[1u8; 16 * 1024]).await;
        }
    });
    tokio::time::sleep(Duration::from_millis(500)).await;

    for cid in ["other", "new"] {
        let (port, accepted) = tcp_resource().await;
        let t0 = Instant::now();
        h.open(cid, port, "tcp").await;
        let bound = h.next_bound().await;
        let mut resource = accepted.await.unwrap();
        assert!(
            t0.elapsed() < Duration::from_secs(1),
            "{cid} opened in {:?}",
            t0.elapsed()
        );
        let sender = tokio::spawn(async move {
            for _ in 0..256 {
                bound.send(&[2u8; 16 * 1024]).await;
            }
            bound
        });
        let mut got = 0;
        let mut buf = vec![0u8; 64 * 1024];
        tokio::time::timeout(Duration::from_secs(2), async {
            while got < 4 * 1024 * 1024 {
                got += resource.read(&mut buf).await.unwrap();
            }
        })
        .await
        .unwrap_or_else(|_| panic!("{cid}: only {got} bytes while another tunnel is stalled"));
        let _ = sender.await;
    }
    flooding.abort();
}

/// The Shield's HTTP/2 windows (tls::tunnel_endpoint): a response stream the
/// client does not read holds at most about its 256 KB stream window, and
/// with 300 such streams stalled (75 MB of windows, far over hyper's 5 MB
/// default connection window) a fresh stream still flows.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_client_windows_isolate_many_stalled_streams() {
    let mut h = harness().await;
    let client = h.client().await;
    let mut held = Vec::new();
    let mut counters = Vec::new();
    let open_one = |cid: String| {
        let mut client = client.clone();
        async move {
            let (tx, rx) = mpsc::channel(4);
            tx.send(TunnelFrame {
                frame: Some(Frame::Start(TunnelStart { connection_id: cid })),
            })
            .await
            .unwrap();
            let response = client
                .tunnel(ReceiverStream::new(rx))
                .await
                .unwrap()
                .into_inner();
            (tx, response)
        }
    };
    for i in 0..300 {
        let (tx, response) = open_one(format!("s{i}")).await;
        let bound = h.next_bound().await;
        let sent = Arc::new(AtomicUsize::new(0));
        let counter = sent.clone();
        let flood = tokio::spawn(async move {
            loop {
                bound.send(&[0u8; 16 * 1024]).await;
                counter.fetch_add(16 * 1024, Ordering::SeqCst);
            }
        });
        counters.push(sent);
        held.push((tx, response, flood));
    }
    tokio::time::sleep(Duration::from_secs(1)).await;
    let max = counters
        .iter()
        .map(|c| c.load(Ordering::SeqCst))
        .max()
        .unwrap();
    assert!(
        max < 600 * 1024,
        "a stalled stream took {max} bytes: the 256 KB window is not in effect"
    );

    let (_tx, mut response) = open_one("fresh".into()).await;
    let bound = h.next_bound().await;
    let sender = tokio::spawn(async move {
        for _ in 0..64 {
            bound.send(&[3u8; 16 * 1024]).await;
        }
        bound
    });
    let mut got = 0;
    tokio::time::timeout(Duration::from_secs(2), async {
        while got < 1024 * 1024 {
            match response.message().await.unwrap().unwrap().frame {
                Some(Frame::Data(d)) => got += d.len(),
                other => panic!("{other:?}"),
            }
        }
    })
    .await
    .expect("a fresh stream flows with 300 stalled");
    let _ = sender.await;
    for (_, _, flood) in held {
        flood.abort();
    }
}

// ── admission ────────────────────────────────────────────────────────────

/// D-3: at most 256 tunnels, pending opens included (here: their streams are
/// held by the Connector); the 257th is refused at once on Control; places
/// free as tunnels end.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn the_257th_tunnel_is_refused_at_once() {
    let mut h = harness().await;
    *h.mode.lock().unwrap() = Mode::Hold;
    let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let port = l.local_addr().unwrap().port();
    let accepting = tokio::spawn(async move {
        let mut socks = Vec::new();
        while let Ok((s, _)) = l.accept().await {
            socks.push(s);
        }
    });
    for i in 0..MAX_TUNNELS {
        h.open(&format!("p{i}"), port, "tcp").await;
    }
    assert_eq!(h.hub.len(), MAX_TUNNELS, "pending opens count");
    let t0 = Instant::now();
    h.open("one-too-many", port, "tcp").await;
    let refused = h.next_refusal().await;
    assert!(t0.elapsed() < Duration::from_millis(500), "refused at once");
    assert_eq!(refused.connection_id, "one-too-many");
    assert!(!refused.ok);
    assert!(
        refused.error.contains("256"),
        "actionable: {}",
        refused.error
    );

    h.gate.send_replace(true);
    let mut bound = Vec::new();
    for _ in 0..MAX_TUNNELS {
        bound.push(h.next_bound().await);
    }
    assert_eq!(h.hub.len(), MAX_TUNNELS);
    // Ending tunnels frees their places.
    for b in bound.drain(..10) {
        abort_tunnel(&h.hub, &b.cid);
    }
    wait_len(&h.hub, MAX_TUNNELS - 10).await;
    h.open("fits-again", port, "tcp").await;
    assert_eq!(h.next_bound().await.cid, "fits-again");
    accepting.abort();
}

/// Opening the stream is bounded: a Connector that never answers gets a
/// refusal on Control after 5 s, and the place is freed.
#[tokio::test]
async fn a_stream_open_that_hangs_is_given_up() {
    let mut h = harness().await;
    *h.mode.lock().unwrap() = Mode::Hold;
    let (port, _accepted) = tcp_resource().await;
    h.open("hung", port, "tcp").await;
    let refused = tokio::time::timeout(Duration::from_secs(8), h.refusals.recv())
        .await
        .expect("given up within the stream-open timeout")
        .unwrap();
    match refused.body {
        Some(Body::TunnelOpened(o)) => {
            assert!(!o.ok);
            assert!(o.error.contains("in time"), "{}", o.error);
        }
        other => panic!("{other:?}"),
    }
    wait_len(&h.hub, 0).await;
}

/// A pre-FU-9 Connector (no Tunnel RPC) gets a clear refusal, not a hang.
#[tokio::test]
async fn an_old_connector_gets_a_clear_refusal() {
    let mut h = harness().await;
    *h.mode.lock().unwrap() = Mode::Unimplemented;
    let (port, _accepted) = tcp_resource().await;
    h.open("old", port, "tcp").await;
    let refused = h.next_refusal().await;
    assert!(
        refused.error.contains("upgrade the connector"),
        "{}",
        refused.error
    );
    wait_len(&h.hub, 0).await;
}

/// A resource that refuses the connection is reported on Control.
#[tokio::test]
async fn a_resource_connect_failure_is_reported() {
    let mut h = harness().await;
    let port = {
        let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
        l.local_addr().unwrap().port()
    };
    h.open("nobody-home", port, "tcp").await;
    let refused = h.next_refusal().await;
    assert_eq!(refused.connection_id, "nobody-home");
    assert!(!refused.ok);
    wait_len(&h.hub, 0).await;
}

/// The same tunnel id twice is refused.
#[tokio::test]
async fn a_duplicate_tunnel_id_is_refused() {
    let mut h = harness().await;
    let (port, _accepted) = tcp_resource().await;
    h.open("dup", port, "tcp").await;
    let _bound = h.next_bound().await;
    h.open("dup", port, "tcp").await;
    let refused = h.next_refusal().await;
    assert!(refused.error.contains("already in use"));
    assert_eq!(h.hub.len(), 1);
}

// ── ending ───────────────────────────────────────────────────────────────

async fn expect_reset_or_closed(resource: &mut TcpStream) {
    let mut buf = [0u8; 64];
    loop {
        match tokio::time::timeout(WAIT, resource.read(&mut buf)).await {
            Ok(Ok(0)) | Ok(Err(_)) => return,
            Ok(Ok(_)) => continue,
            Err(_) => panic!("the resource connection must be closed"),
        }
    }
}

/// The Connector aborts a tunnel over Control (FU-15): the resource
/// connection is reset at once and the tunnel's stream ends.
#[tokio::test]
async fn a_control_abort_resets_the_resource_and_ends_the_stream() {
    let mut h = harness().await;
    let (port, accepted) = tcp_resource().await;
    h.open("t", port, "tcp").await;
    let bound = h.next_bound().await;
    let mut resource = accepted.await.unwrap();
    abort_tunnel(&h.hub, "t");
    let mut buf = [0u8; 8];
    match tokio::time::timeout(WAIT, resource.read(&mut buf)).await {
        Ok(Err(e)) => assert_eq!(e.kind(), std::io::ErrorKind::ConnectionReset),
        other => panic!("expected a reset, got {other:?}"),
    }
    drop(bound);
    wait_len(&h.hub, 0).await;
}

/// The Connector ends the stream with an error (its session was aborted):
/// the resource connection is reset, not ended normally.
#[tokio::test]
async fn an_error_from_the_connector_resets_the_resource() {
    let mut h = harness().await;
    let (port, accepted) = tcp_resource().await;
    h.open("t", port, "tcp").await;
    let bound = h.next_bound().await;
    let mut resource = accepted.await.unwrap();
    bound
        .outbound
        .send(Err(Status::aborted("tunnel ended by the connector")))
        .await
        .unwrap();
    let mut buf = [0u8; 8];
    match tokio::time::timeout(WAIT, resource.read(&mut buf)).await {
        Ok(Err(e)) => assert_eq!(e.kind(), std::io::ErrorKind::ConnectionReset),
        other => panic!("expected a reset, got {other:?}"),
    }
    wait_len(&h.hub, 0).await;
}

/// F44: the Control session ending closes every tunnel.
#[tokio::test]
async fn close_all_closes_every_tunnel() {
    let mut h = harness().await;
    let mut resources = Vec::new();
    for cid in ["a", "b", "c"] {
        let (port, accepted) = tcp_resource().await;
        h.open(cid, port, "tcp").await;
        let _ = h.next_bound().await;
        resources.push(accepted.await.unwrap());
    }
    assert_eq!(h.hub.close_all(), 3);
    for r in &mut resources {
        expect_reset_or_closed(r).await;
    }
    wait_len(&h.hub, 0).await;
}

// ── renewal ──────────────────────────────────────────────────────────────

/// PENDING-21 §5 Experiment 2, through the production code: after the hub
/// moves to a new client (renewal), the tunnel on the old connection keeps
/// flowing both ways, new tunnels use the new connection, and the old
/// connection closes once its last tunnel ends — no tunnel keeps a client
/// handle (the spike's retention trap), and nothing is leaked.
#[tokio::test]
async fn an_old_connection_lives_only_as_long_as_its_tunnels() {
    let mut h = harness().await; // the hub's client: connection A
    assert!(h.wait_live(1).await.is_some());
    let (port, accepted) = tcp_resource().await;
    h.open("old", port, "tcp").await;
    let mut old = h.next_bound().await;
    let mut old_resource = accepted.await.unwrap();

    // Renewal: connection B becomes current; every handle to A is dropped.
    let new_client = h.client().await;
    h.hub.set_client(Some(new_client));
    assert!(h.wait_live(2).await.is_some());

    old.send(b"down").await;
    let mut got = [0u8; 4];
    old_resource.read_exact(&mut got).await.unwrap();
    assert_eq!(&got, b"down");
    old_resource.write_all(b"up").await.unwrap();
    assert_eq!(old.next_data().await, b"up");
    assert_eq!(
        h.live.load(Ordering::SeqCst),
        2,
        "A stays while its tunnel runs"
    );

    let (port2, accepted2) = tcp_resource().await;
    h.open("new", port2, "tcp").await;
    let new = h.next_bound().await;
    let _new_resource = accepted2.await.unwrap();
    assert_ne!(
        new.port, old.port,
        "a new tunnel must use the new connection"
    );

    // The old tunnel ends; connection A must close.
    old.end().await;
    old_resource.shutdown().await.unwrap();
    while let Some(f) = old.next().await {
        if matches!(f, FakeFrame::End(_)) {
            break;
        }
    }
    drop(old);
    assert!(
        h.wait_live(1).await.is_some(),
        "the old connection was not released after its last tunnel ended"
    );
    assert_eq!(h.hub.len(), 1, "only the new tunnel remains");
    assert_eq!(
        h.hub.tunnels.lock().unwrap().len(),
        1,
        "no leaked registration"
    );
}

// ── UDP ──────────────────────────────────────────────────────────────────

async fn udp_resource() -> (u16, UdpSocket) {
    let s = UdpSocket::bind("127.0.0.1:0").await.unwrap();
    (s.local_addr().unwrap().port(), s)
}

/// D-4: one frame is one datagram, both ways.
#[tokio::test]
async fn udp_keeps_datagram_boundaries() {
    let mut h = harness().await;
    let (port, resource) = udp_resource().await;
    h.open("u", port, "udp").await;
    let mut bound = h.next_bound().await;
    bound.send(b"one").await;
    bound.send(&[9u8; 1400]).await;
    let mut buf = [0u8; 2048];
    let (n, peer) = resource.recv_from(&mut buf).await.unwrap();
    assert_eq!(&buf[..n], b"one");
    let (n, _) = resource.recv_from(&mut buf).await.unwrap();
    assert_eq!(&buf[..n], &[9u8; 1400]);
    resource.send_to(b"reply-a", peer).await.unwrap();
    resource.send_to(b"reply-b", peer).await.unwrap();
    assert_eq!(bound.next_data().await, b"reply-a");
    assert_eq!(bound.next_data().await, b"reply-b");
}

/// D-4: with the Connector not reading, the queue toward it saturates and
/// the newest datagrams are dropped — the oldest are kept in order, the
/// resource side never blocks, and datagrams flow again afterwards.
#[tokio::test]
async fn udp_drops_the_newest_datagrams_when_saturated() {
    let mut h = harness().await;
    let (port, resource) = udp_resource().await;
    h.open("u", port, "udp").await;
    let mut bound = h.next_bound().await;
    bound.send(b"hello").await;
    let mut buf = [0u8; 64];
    let (_, peer) = resource.recv_from(&mut buf).await.unwrap();

    // 4000 × 1 KB = 4 MB, far over the 256 KB stream window plus the queue.
    const SENT: u32 = 4000;
    for i in 0..SENT {
        let mut d = vec![0u8; 1024];
        d[..4].copy_from_slice(&i.to_be_bytes());
        resource.send_to(&d, peer).await.unwrap();
        if i % 100 == 0 {
            tokio::task::yield_now().await;
        }
    }
    tokio::time::sleep(Duration::from_millis(300)).await;
    // Saturated: a datagram now must be dropped by the Shield at once — not
    // left in the kernel socket buffer for later, as a Shield that blocked
    // on the full queue would.
    resource.send_to(b"marker", peer).await.unwrap();
    tokio::time::sleep(Duration::from_millis(300)).await;
    let mut ids = Vec::new();
    while let Ok(Ok(Some(f))) =
        tokio::time::timeout(Duration::from_millis(300), bound.inbound.message()).await
    {
        if let Some(FakeFrame::Data(d)) = f.frame {
            assert_ne!(d, b"marker", "the newest datagram was not dropped");
            ids.push(u32::from_be_bytes(d[..4].try_into().unwrap()));
        }
    }
    assert!(!ids.is_empty());
    assert!((ids.len() as u32) < SENT, "nothing was dropped ({} kept)", ids.len());
    assert_eq!(ids[0], 0, "the oldest datagram is kept");
    assert!(ids.windows(2).all(|w| w[0] < w[1]), "datagrams out of order");

    resource.send_to(b"after", peer).await.unwrap();
    let mut saw_after = false;
    for _ in 0..10 {
        match bound.next().await {
            Some(FakeFrame::Data(d)) if d == b"after" => {
                saw_after = true;
                break;
            }
            Some(_) => continue,
            None => break,
        }
    }
    assert!(saw_after, "datagrams flow again once the connector reads");
}
