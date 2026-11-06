//! FU-9 (PENDING-21): the tunnel hub with a fake Shield that binds a stream
//! for each tunnel (`attach`), as the `Tunnel` handler does. Real tonic
//! streams over mTLS are exercised in agent_server.rs.

use super::*;
use crate::abort_stream::{SESSION_REVOKED_CODE, TUNNEL_FAILED_CODE};
use std::sync::atomic::{AtomicBool, AtomicU32};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio_stream::wrappers::ReceiverStream;

pub(crate) const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
const OTHER: &str = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d";
const WAIT: Duration = Duration::from_secs(5);

fn far() -> tokio::time::Instant {
    tokio::time::Instant::now() + Duration::from_secs(3600)
}

fn data(d: &[u8]) -> Result<TunnelFrame, Status> {
    Ok(TunnelFrame {
        frame: Some(Frame::Data(d.to_vec())),
    })
}

fn end(error: &str) -> Result<TunnelFrame, Status> {
    Ok(TunnelFrame {
        frame: Some(Frame::End(TunnelEnd {
            error: error.to_string(),
        })),
    })
}

/// The Shield's end of one tunnel stream.
pub(crate) struct FakeTunnel {
    pub cid: String,
    pub to_connector: mpsc::Sender<Result<TunnelFrame, Status>>,
    pub from_connector: ToShield,
}

impl FakeTunnel {
    /// Bind a stream for `cid` as `shield`.
    pub fn attach(hub: &AgentTunnelHub, shield: &str, cid: &str) -> Result<Self, Status> {
        Self::attach_until(hub, shield, cid, far())
    }

    fn attach_until(
        hub: &AgentTunnelHub,
        shield: &str,
        cid: &str,
        expires_at: tokio::time::Instant,
    ) -> Result<Self, Status> {
        let (to_connector, rx) = mpsc::channel(64);
        let from_connector =
            hub.attach(shield, cid, Box::pin(ReceiverStream::new(rx)), expires_at)?;
        Ok(Self {
            cid: cid.to_string(),
            to_connector,
            from_connector,
        })
    }

    pub async fn send(&self, frame: Result<TunnelFrame, Status>) {
        self.to_connector
            .send(frame)
            .await
            .expect("connector reads the tunnel");
    }

    /// The next frame from the Connector (None: the stream ended).
    pub async fn next(&mut self) -> Option<Result<TunnelFrame, Status>> {
        tokio::time::timeout(WAIT, self.from_connector.next())
            .await
            .expect("a frame or the end within the timeout")
    }

    pub async fn next_data(&mut self) -> Vec<u8> {
        match self.next().await {
            Some(Ok(TunnelFrame {
                frame: Some(Frame::Data(d)),
            })) => d,
            other => panic!("expected data, got {other:?}"),
        }
    }
}

/// A Shield on `rx` (as `shield`) that binds a stream for every TunnelOpen
/// and hands it out; it also reports the Control aborts (TunnelClose) it gets.
pub(crate) fn accept_all(
    hub: AgentTunnelHub,
    shield: &'static str,
    mut rx: mpsc::Receiver<ShieldControlMessage>,
) -> (mpsc::Receiver<FakeTunnel>, mpsc::Receiver<TunnelClose>) {
    let (tunnels_tx, tunnels) = mpsc::channel(1024);
    let (closes_tx, closes) = mpsc::channel(1024);
    // Aborts come from the Shield's mailbox, as the Control loop delivers them.
    let mailbox = hub.abort_mailbox(shield);
    let mailbox_closes = closes_tx.clone();
    tokio::spawn(async move {
        loop {
            mailbox.notified().await;
            for c in mailbox.take().0 {
                if mailbox_closes.send(c).await.is_err() {
                    return;
                }
            }
        }
    });
    tokio::spawn(async move {
        while let Some(m) = rx.recv().await {
            match m.body {
                Some(Body::TunnelOpen(o)) => {
                    let t = FakeTunnel::attach(&hub, shield, &o.connection_id).expect("attach");
                    let _ = tunnels_tx.send(t).await;
                }
                Some(Body::TunnelClose(c)) => {
                    let _ = closes_tx.send(c).await;
                }
                _ => {}
            }
        }
    });
    (tunnels, closes)
}

struct Shield {
    hub: AgentTunnelHub,
    tunnels: mpsc::Receiver<FakeTunnel>,
    closes: mpsc::Receiver<TunnelClose>,
}

/// A hub with SHIELD registered on Control stream 1 and accepting tunnels.
fn shield() -> Shield {
    let hub = AgentTunnelHub::new();
    let (tx, rx) = mpsc::channel(64);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let (tunnels, closes) = accept_all(hub.clone(), SHIELD, rx);
    Shield {
        hub,
        tunnels,
        closes,
    }
}

impl Shield {
    async fn open(&mut self, protocol: &str) -> (RelaySession, FakeTunnel) {
        let session = self
            .hub
            .open_relay_session(SHIELD, "10.0.0.9", 80, protocol)
            .await
            .expect("opened");
        let tunnel = self.tunnels.recv().await.expect("the shield's stream");
        assert_eq!(tunnel.cid, session.connection_id);
        (session, tunnel)
    }
}

/// A device stream that records the code it was aborted with (FU-15 / FU-17;
/// 0 = not aborted) and whether it was shut down.
struct Probe {
    inner: tokio::io::DuplexStream,
    aborted: Arc<AtomicU32>,
    shut: Arc<AtomicBool>,
}

fn probe(inner: tokio::io::DuplexStream) -> (Probe, Arc<AtomicU32>, Arc<AtomicBool>) {
    let aborted = Arc::new(AtomicU32::new(0));
    let shut = Arc::new(AtomicBool::new(false));
    (
        Probe {
            inner,
            aborted: aborted.clone(),
            shut: shut.clone(),
        },
        aborted,
        shut,
    )
}

impl crate::abort_stream::AbortStream for Probe {
    fn abort(&mut self, code: u32) {
        self.aborted.store(code, Ordering::SeqCst);
    }
}

impl tokio::io::AsyncRead for Probe {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut tokio::io::ReadBuf<'_>,
    ) -> Poll<std::io::Result<()>> {
        Pin::new(&mut self.inner).poll_read(cx, buf)
    }
}

impl tokio::io::AsyncWrite for Probe {
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
        self.shut.store(true, Ordering::SeqCst);
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
}

async fn expect_aborted_stream(t: &mut FakeTunnel) {
    loop {
        match t.next().await {
            Some(Ok(_)) => continue, // frames queued before the abort
            Some(Err(status)) => {
                assert_eq!(status.code(), tonic::Code::Aborted);
                return;
            }
            None => panic!("an aborted tunnel must end with an error, not cleanly"),
        }
    }
}

// ── data ─────────────────────────────────────────────────────────────────

/// Bytes flow both ways in order; the resource's clean end shuts the device
/// stream down after all its data (F-7).
#[tokio::test]
async fn tcp_data_flows_both_ways_and_a_clean_end_shuts_the_device_down() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let (p, aborted, shut) = probe(relay_end);
    let relay = tokio::spawn(session.relay_stream(p));

    device.write_all(b"GET / HTTP/1.0\r\n\r\n").await.unwrap();
    assert_eq!(tunnel.next_data().await, b"GET / HTTP/1.0\r\n\r\n");
    tunnel.send(data(b"HTTP/1.0 200 OK\r\n\r\n")).await;
    tunnel.send(data(b"body")).await;
    tunnel.send(end("")).await;
    tokio::time::timeout(WAIT, relay)
        .await
        .expect("session ends")
        .unwrap()
        .unwrap();

    let mut got = Vec::new();
    tokio::time::timeout(WAIT, device.read_to_end(&mut got))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(got, b"HTTP/1.0 200 OK\r\n\r\nbody");
    assert!(
        shut.load(Ordering::SeqCst),
        "a clean end must shut the device stream down"
    );
    assert_eq!(
        aborted.load(Ordering::SeqCst),
        0,
        "a clean end is not an abort"
    );
    assert_eq!(s.hub.tunnel_count(), 0, "an ended session frees its place");
}

/// The device's end reaches the Shield as TunnelEnd{}; the session keeps
/// delivering the resource's answer until the resource ends (half-close).
#[tokio::test]
async fn the_device_end_is_a_half_close() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));

    device.write_all(b"request").await.unwrap();
    device.shutdown().await.unwrap();
    assert_eq!(tunnel.next_data().await, b"request");
    match tunnel.next().await {
        Some(Ok(TunnelFrame {
            frame: Some(Frame::End(e)),
        })) => assert!(e.error.is_empty()),
        other => panic!("expected TunnelEnd, got {other:?}"),
    }
    tunnel.send(data(b"answer")).await;
    tunnel.send(end("")).await;
    tokio::time::timeout(WAIT, relay)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    let mut got = Vec::new();
    device.read_to_end(&mut got).await.unwrap();
    assert_eq!(got, b"answer");
}

/// An error end from the Shield drops the device stream without a clean
/// shutdown, and aborts the Shield's side.
#[tokio::test]
async fn an_error_end_is_not_a_clean_end() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (_device, relay_end) = tokio::io::duplex(64 * 1024);
    let (p, aborted, shut) = probe(relay_end);
    let relay = tokio::spawn(session.relay_stream(p));
    tunnel.send(end("resource reset")).await;
    tokio::time::timeout(WAIT, relay)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(
        !shut.load(Ordering::SeqCst),
        "an error end must not look like a clean end"
    );
    // FU-17: not merely "not shut down" — a dropped QUIC stream is finished,
    // which the app reads as a clean EOF. The device stream must be reset.
    assert_eq!(aborted.load(Ordering::SeqCst), TUNNEL_FAILED_CODE);
    expect_aborted_stream(&mut tunnel).await;
}

/// A stream that ends without TunnelEnd (or with an error status) is a
/// failure, not a clean end.
#[tokio::test]
async fn a_stream_lost_without_tunnel_end_is_not_a_clean_end() {
    for lost in [None, Some(Status::unavailable("connection reset"))] {
        let mut s = shield();
        let (session, tunnel) = s.open("tcp").await;
        let (_device, relay_end) = tokio::io::duplex(1024);
        let (p, aborted, shut) = probe(relay_end);
        let relay = tokio::spawn(session.relay_stream(p));
        let FakeTunnel { to_connector, .. } = tunnel;
        let case = format!("{lost:?}");
        if let Some(status) = lost {
            to_connector.send(Err(status)).await.unwrap();
        }
        drop(to_connector);
        tokio::time::timeout(WAIT, relay)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        assert!(!shut.load(Ordering::SeqCst));
        assert_eq!(
            aborted.load(Ordering::SeqCst),
            TUNNEL_FAILED_CODE,
            "FU-17: a lost shield stream ({case}) must reset the device stream"
        );
    }
}

/// FU-17: a frame the Shield must never send mid-tunnel is a failure too:
/// the device stream is reset, not finished.
#[tokio::test]
async fn an_unexpected_shield_frame_resets_the_device() {
    let mut s = shield();
    let (session, tunnel) = s.open("tcp").await;
    let (_device, relay_end) = tokio::io::duplex(1024);
    let (p, aborted, shut) = probe(relay_end);
    let relay = tokio::spawn(session.relay_stream(p));
    tunnel
        .send(Ok(TunnelFrame {
            frame: Some(Frame::Start(crate::shield_proto::TunnelStart {
                connection_id: "not-expected".into(),
            })),
        }))
        .await;
    tokio::time::timeout(WAIT, relay)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(!shut.load(Ordering::SeqCst));
    assert_eq!(aborted.load(Ordering::SeqCst), TUNNEL_FAILED_CODE);
}

/// FU-17 on a UDP tunnel: a failed Shield stream resets the device stream
/// as for TCP; a clean UDP end does not.
#[tokio::test]
async fn a_failed_udp_tunnel_resets_the_device_and_a_clean_one_does_not() {
    for failed in [true, false] {
        let mut s = shield();
        let (session, tunnel) = s.open("udp").await;
        let (_device, relay_end) = tokio::io::duplex(1024);
        let (p, aborted, _) = probe(relay_end);
        let relay = tokio::spawn(session.relay_stream(p));
        tunnel.send(data(b"datagram")).await;
        tunnel
            .send(if failed {
                end("resource unreachable")
            } else {
                end("")
            })
            .await;
        tokio::time::timeout(WAIT, relay)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        let expected = if failed { TUNNEL_FAILED_CODE } else { 0 };
        assert_eq!(aborted.load(Ordering::SeqCst), expected, "failed={failed}");
    }
}

/// D-5: a device that reads far slower than the Shield sends is never cut,
/// and gets every byte in order (the old 2 s stall limit ended it).
#[tokio::test(start_paused = true)]
async fn a_steadily_slow_device_completes_a_long_transfer() {
    const CHUNKS: u32 = 3000;
    let mut s = shield();
    let (session, tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));
    let sender = tokio::spawn(async move {
        for i in 0..CHUNKS {
            tunnel.send(data(&i.to_be_bytes())).await;
        }
        tunnel.send(end("")).await;
        tunnel
    });
    // 4 bytes every 10 ms: 30 s of simulated time, always behind the sender.
    for i in 0..CHUNKS {
        let mut b = [0u8; 4];
        device.read_exact(&mut b).await.unwrap();
        assert_eq!(u32::from_be_bytes(b), i, "chunk {i} out of order");
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    relay.await.unwrap().unwrap();
    let _tunnel = sender.await.unwrap();
}

/// D-5: a device that stops reading for a long time is not cut; when it
/// resumes it gets every byte.
#[tokio::test(start_paused = true)]
async fn a_stalled_device_is_not_cut() {
    let mut s = shield();
    let (session, tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));
    let sender = tokio::spawn(async move {
        for i in 0..100u8 {
            tunnel.send(data(&[i; 512])).await;
        }
        tunnel.send(end("")).await;
        tunnel
    });
    tokio::time::sleep(Duration::from_secs(300)).await;
    assert!(
        !relay.is_finished(),
        "a stalled device must not end the session"
    );
    let mut got = Vec::new();
    device.read_to_end(&mut got).await.unwrap();
    assert_eq!(got.len(), 100 * 512);
    for (i, chunk) in got.chunks(512).enumerate() {
        assert!(chunk.iter().all(|b| *b == i as u8), "chunk {i} corrupted");
    }
    relay.await.unwrap().unwrap();
    let _tunnel = sender.await.unwrap();
}

/// No head-of-line blocking in the hub: a tunnel whose device has stopped
/// reading does not hold up another tunnel of the same Shield.
#[tokio::test]
async fn a_stalled_tunnel_does_not_hold_up_another() {
    let mut s = shield();
    let (stalled, stalled_tunnel) = s.open("tcp").await;
    let (_stalled_device, stalled_end) = tokio::io::duplex(1024);
    let _r1 = tokio::spawn(stalled.relay_stream(stalled_end));
    let filler = tokio::spawn(async move {
        loop {
            stalled_tunnel.send(data(&[0u8; 16 * 1024])).await;
        }
    });
    tokio::time::sleep(Duration::from_millis(100)).await;

    let (other, other_tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let _r2 = tokio::spawn(other.relay_stream(relay_end));
    other_tunnel.send(data(b"still flowing")).await;
    let mut got = [0u8; 13];
    tokio::time::timeout(Duration::from_secs(1), device.read_exact(&mut got))
        .await
        .expect("the other tunnel is not held up")
        .unwrap();
    assert_eq!(&got, b"still flowing");
    filler.abort();
}

/// F-7: the close_notify queues behind data still in flight, so a reader
/// slower than any short grace still gets the clean end.
#[tokio::test(start_paused = true)]
async fn a_slow_reader_still_gets_the_clean_end() {
    struct DrainGated {
        inner: tokio::io::DuplexStream,
        drained: Arc<AtomicBool>,
        waker: Arc<parking_lot::Mutex<Option<std::task::Waker>>>,
        completed: Arc<AtomicBool>,
    }
    impl crate::abort_stream::AbortStream for DrainGated {
        fn abort(&mut self, _code: u32) {}
    }
    impl tokio::io::AsyncRead for DrainGated {
        fn poll_read(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
            buf: &mut tokio::io::ReadBuf<'_>,
        ) -> Poll<std::io::Result<()>> {
            Pin::new(&mut self.inner).poll_read(cx, buf)
        }
    }
    impl tokio::io::AsyncWrite for DrainGated {
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
        fn poll_shutdown(
            mut self: Pin<&mut Self>,
            cx: &mut Context<'_>,
        ) -> Poll<std::io::Result<()>> {
            if !self.drained.load(Ordering::SeqCst) {
                *self.waker.lock() = Some(cx.waker().clone());
                return Poll::Pending;
            }
            self.completed.store(true, Ordering::SeqCst);
            Pin::new(&mut self.inner).poll_shutdown(cx)
        }
    }

    let mut s = shield();
    let (session, tunnel) = s.open("tcp").await;
    let (_device, relay_end) = tokio::io::duplex(1024);
    let drained = Arc::new(AtomicBool::new(false));
    let waker = Arc::new(parking_lot::Mutex::new(None::<std::task::Waker>));
    let completed = Arc::new(AtomicBool::new(false));
    let relay = tokio::spawn(session.relay_stream(DrainGated {
        inner: relay_end,
        drained: drained.clone(),
        waker: waker.clone(),
        completed: completed.clone(),
    }));
    tunnel.send(data(b"body")).await;
    tunnel.send(end("")).await;
    tokio::time::sleep(Duration::from_secs(10)).await;
    drained.store(true, Ordering::SeqCst);
    if let Some(w) = waker.lock().take() {
        w.wake();
    }
    tokio::time::timeout(WAIT, relay)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(
        completed.load(Ordering::SeqCst),
        "the shutdown must complete after a slow drain"
    );
}

// ── UDP ──────────────────────────────────────────────────────────────────

fn framed(d: &[u8]) -> Vec<u8> {
    let mut v = (d.len() as u32).to_be_bytes().to_vec();
    v.extend_from_slice(d);
    v
}

/// D-4: one device frame is one datagram frame to the Shield, and back;
/// boundaries survive datagrams that arrive together.
#[tokio::test]
async fn udp_keeps_datagram_boundaries_both_ways() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("udp").await;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let _relay = tokio::spawn(session.relay_stream(relay_end));

    let mut both = framed(b"first");
    both.extend(framed(b""));
    both.extend(framed(&[7u8; 1400]));
    device.write_all(&both).await.unwrap();
    assert_eq!(tunnel.next_data().await, b"first");
    assert_eq!(tunnel.next_data().await, b"");
    assert_eq!(tunnel.next_data().await, vec![7u8; 1400]);

    tunnel.send(data(b"reply-1")).await;
    tunnel.send(data(b"reply-2")).await;
    let mut got = vec![0u8; framed(b"reply-1").len() * 2];
    device.read_exact(&mut got).await.unwrap();
    let mut want = framed(b"reply-1");
    want.extend(framed(b"reply-2"));
    assert_eq!(got, want);
}

/// D-4: with the Shield not reading, the queue fills and the newest
/// datagrams are dropped; the device side never blocks, and the tunnel
/// carries on once the Shield reads again.
#[tokio::test]
async fn udp_drops_the_newest_datagrams_when_the_queue_is_full() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("udp").await;
    let (mut device, relay_end) = tokio::io::duplex(1024 * 1024);
    let _relay = tokio::spawn(session.relay_stream(relay_end));

    let total = UDP_TO_SHIELD_QUEUE as u32 + 20;
    for i in 0..total {
        tokio::time::timeout(
            Duration::from_secs(1),
            device.write_all(&framed(&i.to_be_bytes())),
        )
        .await
        .expect("a full UDP queue must not block the device side")
        .unwrap();
    }
    // Let the relay read everything the device sent.
    tokio::time::sleep(Duration::from_millis(200)).await;
    for i in 0..UDP_TO_SHIELD_QUEUE as u32 {
        assert_eq!(
            tunnel.next_data().await,
            i.to_be_bytes(),
            "the oldest datagrams are kept"
        );
    }
    // The ones that found the queue full were dropped; new ones flow again.
    device.write_all(&framed(b"later")).await.unwrap();
    assert_eq!(tunnel.next_data().await, b"later");
}

/// A device frame over the datagram limit ends the tunnel.
#[tokio::test]
async fn udp_an_oversized_frame_ends_the_tunnel() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("udp").await;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let _relay = tokio::spawn(session.relay_stream(relay_end));
    device.write_all(&(70_000u32).to_be_bytes()).await.unwrap();
    match tunnel.next().await {
        Some(Ok(TunnelFrame {
            frame: Some(Frame::End(e)),
        })) => assert!(!e.error.is_empty()),
        other => panic!("expected an error TunnelEnd, got {other:?}"),
    }
}

// ── admission ────────────────────────────────────────────────────────────

/// A Shield that never answers: every open stays pending.
fn silent_shield(hub: &AgentTunnelHub, shield: &str, stream_id: u64) {
    let (tx, mut rx) = mpsc::channel::<ShieldControlMessage>(4096);
    assert!(hub.register_shield(shield.into(), stream_id, tx));
    tokio::spawn(async move { while rx.recv().await.is_some() {} });
}

async fn wait_count(hub: &AgentTunnelHub, n: usize) {
    tokio::time::timeout(WAIT, async {
        while hub.tunnel_count() != n {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap_or_else(|_| panic!("expected {n} tunnels, have {}", hub.tunnel_count()));
}

fn open_err(r: Result<RelaySession>) -> OpenError {
    match r {
        Ok(_) => panic!("expected the open to fail"),
        Err(e) => e.downcast::<OpenError>().expect("an OpenError"),
    }
}

/// D-3: a Shield has at most 256 tunnels, pending opens included; the 257th
/// is refused at once (no queueing), other Shields are unaffected, and a
/// place frees when a tunnel ends.
#[tokio::test]
async fn the_257th_tunnel_of_a_shield_is_refused_at_once() {
    let hub = AgentTunnelHub::new();
    silent_shield(&hub, SHIELD, 1);
    silent_shield(&hub, OTHER, 2);
    let mut pending = Vec::new();
    for _ in 0..MAX_TUNNELS_PER_SHIELD {
        let hub = hub.clone();
        pending.push(tokio::spawn(async move {
            hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await
        }));
    }
    wait_count(&hub, MAX_TUNNELS_PER_SHIELD).await;

    let refused = tokio::time::timeout(
        Duration::from_millis(200),
        hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp"),
    )
    .await
    .expect("refused at once, not queued");
    let e = open_err(refused);
    assert_eq!(e, OpenError::ShieldLimit);
    assert!(e.is_limit());
    assert!(e.to_string().contains("256"), "actionable message: {e}");

    // Another Shield is not limited by this one.
    let other = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(OTHER, "10.0.0.9", 80, "tcp").await })
    };
    wait_count(&hub, MAX_TUNNELS_PER_SHIELD + 1).await;

    // A place frees when one ends (here: its open is given up).
    pending.pop().unwrap().abort();
    wait_count(&hub, MAX_TUNNELS_PER_SHIELD).await;
    let again = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    wait_count(&hub, MAX_TUNNELS_PER_SHIELD + 1).await;
    again.abort();
    other.abort();
    for p in pending {
        p.abort();
    }
    wait_count(&hub, 0).await;
}

/// The Connector-wide budget (shield_tunnel_limit) counts tunnels of every
/// Shield, pending ones included, and refuses at once.
#[tokio::test]
async fn the_connector_wide_tunnel_limit_refuses_at_once() {
    let hub = AgentTunnelHub::new();
    hub.set_tunnel_limit(3);
    silent_shield(&hub, SHIELD, 1);
    silent_shield(&hub, OTHER, 2);
    let mut pending = Vec::new();
    for shield in [SHIELD, SHIELD, OTHER] {
        let hub = hub.clone();
        pending.push(tokio::spawn(async move {
            hub.open_relay_session(shield, "10.0.0.9", 80, "tcp").await
        }));
    }
    wait_count(&hub, 3).await;
    for shield in [SHIELD, OTHER] {
        let refused = tokio::time::timeout(
            Duration::from_millis(200),
            hub.open_relay_session(shield, "10.0.0.9", 80, "tcp"),
        )
        .await
        .expect("refused at once");
        assert_eq!(open_err(refused), OpenError::ConnectorLimit(3));
    }
    for p in pending {
        p.abort();
    }
}

/// Simultaneous opens cannot get past the per-Shield limit together.
#[tokio::test(flavor = "multi_thread", worker_threads = 8)]
async fn simultaneous_opens_cannot_pass_the_limit() {
    let hub = AgentTunnelHub::new();
    silent_shield(&hub, SHIELD, 1);
    let barrier = Arc::new(tokio::sync::Barrier::new(400));
    let mut tasks = Vec::new();
    for _ in 0..400 {
        let hub = hub.clone();
        let barrier = barrier.clone();
        tasks.push(tokio::spawn(async move {
            barrier.wait().await;
            tokio::time::timeout(
                Duration::from_millis(500),
                hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp"),
            )
            .await
        }));
    }
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert_eq!(hub.tunnel_count(), MAX_TUNNELS_PER_SHIELD);
    let mut refused = 0;
    for t in tasks {
        if let Ok(r) = t.await.unwrap() {
            assert_eq!(open_err(r), OpenError::ShieldLimit);
            refused += 1;
        }
    }
    assert_eq!(refused, 400 - MAX_TUNNELS_PER_SHIELD);
}

/// An open that gets no stream gives up after TUNNEL_OPEN_TIMEOUT, frees its
/// place, and a stream arriving later is refused (expired).
#[tokio::test(start_paused = true)]
async fn an_unanswered_open_times_out_and_a_late_stream_is_refused() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let opening = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
        panic!("expected TunnelOpen");
    };
    let t0 = tokio::time::Instant::now();
    tokio::time::sleep(TUNNEL_OPEN_TIMEOUT + Duration::from_millis(1)).await;
    assert_eq!(open_err(opening.await.unwrap()), OpenError::Timeout);
    // Under the paused clock any timeout fires eventually: check it is this one.
    assert!(
        t0.elapsed() <= TUNNEL_OPEN_TIMEOUT + Duration::from_secs(1),
        "the open gave up after {:?}, not TUNNEL_OPEN_TIMEOUT",
        t0.elapsed()
    );
    assert_eq!(hub.tunnel_count(), 0);
    let late = FakeTunnel::attach(&hub, SHIELD, &open.connection_id);
    assert_eq!(late.err().unwrap().code(), tonic::Code::NotFound);
}

// ── binding ──────────────────────────────────────────────────────────────

/// A stream is bound only to a pending tunnel of its own Shield, once.
#[tokio::test]
async fn a_tunnel_stream_binds_only_to_its_own_pending_tunnel_once() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let opening = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
        panic!("expected TunnelOpen");
    };
    let cid = open.connection_id;

    let unknown = FakeTunnel::attach(&hub, SHIELD, "00000000-0000-4000-8000-000000000000");
    assert_eq!(unknown.err().unwrap().code(), tonic::Code::NotFound);
    let foreign = FakeTunnel::attach(&hub, OTHER, &cid);
    assert_eq!(foreign.err().unwrap().code(), tonic::Code::PermissionDenied);
    assert!(
        !opening.is_finished(),
        "another shield's stream must not disturb the open"
    );

    let _bound = FakeTunnel::attach(&hub, SHIELD, &cid).expect("its own shield binds it");
    let _session = opening.await.unwrap().expect("opened");
    let again = FakeTunnel::attach(&hub, SHIELD, &cid);
    assert_eq!(again.err().unwrap().code(), tonic::Code::AlreadyExists);
}

/// The Shield reports a tunnel it could not open; a legacy `ok = true` (a
/// pre-FU-9 Shield) fails the open too, with an upgrade hint; an answer from
/// another Shield is ignored.
#[tokio::test]
async fn tunnel_opened_on_control_fails_the_open() {
    for (from, ok, want) in [
        (SHIELD, false, Some("connection refused")),
        (SHIELD, true, Some("upgrade the shield")),
        (OTHER, false, None),
    ] {
        let hub = AgentTunnelHub::new();
        let (tx, mut rx) = mpsc::channel(8);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let opening = {
            let hub = hub.clone();
            tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
        };
        let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
            panic!("expected TunnelOpen");
        };
        hub.dispatch_opened(from, &open.connection_id, ok, "connection refused".into());
        match want {
            Some(want) => match open_err(opening.await.unwrap()) {
                OpenError::Refused(e) => assert!(e.contains(want), "{e}"),
                other => panic!("expected Refused, got {other:?}"),
            },
            None => {
                tokio::time::sleep(Duration::from_millis(50)).await;
                assert!(
                    !opening.is_finished(),
                    "another shield's answer must be ignored"
                );
                opening.abort();
            }
        }
    }
}

// ── ending ───────────────────────────────────────────────────────────────

/// FU-15: a revoked session aborts the device stream (nothing more is
/// delivered), and the Shield is told over Control and on the stream — also
/// while a write to a slow device is waiting.
#[tokio::test]
async fn a_revoked_session_aborts_the_device_and_the_shield() {
    for slow_device in [false, true] {
        let mut s = shield();
        let (session, mut tunnel) = s.open("tcp").await;
        let token = CancellationToken::new();
        let (_device, relay_end) = tokio::io::duplex(1024);
        let (p, aborted, shut) = probe(relay_end);
        let relay = tokio::spawn(session.with_cancel_token(token.clone()).relay_stream(p));
        if slow_device {
            tunnel.send(data(&[7u8; 64 * 1024])).await;
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        token.cancel();
        tokio::time::timeout(WAIT, relay)
            .await
            .expect("ends at once")
            .unwrap()
            .unwrap();
        assert_eq!(
            aborted.load(Ordering::SeqCst),
            SESSION_REVOKED_CODE,
            "not aborted as revoked (slow device: {slow_device})"
        );
        assert!(
            !shut.load(Ordering::SeqCst),
            "a revoked session must not end like a normal close"
        );
        let close = tokio::time::timeout(WAIT, s.closes.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(close.connection_id, tunnel.cid);
        assert_eq!(close.error, "session revoked");
        expect_aborted_stream(&mut tunnel).await;
    }
}

/// FU-15 while the device → shield direction is blocked (the Shield's stream
/// is full): the abort still happens at once.
#[tokio::test]
async fn a_revoked_session_aborts_while_the_shield_is_not_reading() {
    let mut s = shield();
    let (session, _tunnel) = s.open("tcp").await;
    let token = CancellationToken::new();
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let (p, aborted, _) = probe(relay_end);
    let relay = tokio::spawn(session.with_cancel_token(token.clone()).relay_stream(p));
    for _ in 0..8 {
        device.write_all(&[1u8; 16 * 1024]).await.unwrap();
    }
    tokio::time::sleep(Duration::from_millis(100)).await;
    token.cancel();
    tokio::time::timeout(WAIT, relay)
        .await
        .expect("ends at once")
        .unwrap()
        .unwrap();
    assert_eq!(aborted.load(Ordering::SeqCst), SESSION_REVOKED_CODE);
}

/// Revoking the Shield ends its tunnels (device aborted, stream aborted),
/// fails its pending opens, and refuses new ones.
#[tokio::test]
async fn revoking_the_shield_ends_its_tunnels_and_pending_opens() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (_device, relay_end) = tokio::io::duplex(1024);
    let (p, aborted, _) = probe(relay_end);
    let relay = tokio::spawn(session.relay_stream(p));

    // A pending open: a newer Control stream of this Shield that never answers.
    let (tx, _rx_kept) = mpsc::channel(8);
    assert!(s.hub.register_shield(SHIELD.into(), 2, tx));
    let pending = {
        let hub = s.hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    wait_count(&s.hub, 2).await;

    s.hub.revoke_shield(SHIELD);
    tokio::time::timeout(WAIT, relay)
        .await
        .expect("the tunnel ends")
        .unwrap()
        .unwrap();
    assert_eq!(aborted.load(Ordering::SeqCst), SESSION_REVOKED_CODE);
    expect_aborted_stream(&mut tunnel).await;
    assert_eq!(open_err(pending.await.unwrap()), OpenError::Closed);
    assert_eq!(
        open_err(
            s.hub
                .open_relay_session(SHIELD, "10.0.0.9", 80, "tcp")
                .await
        ),
        OpenError::Revoked
    );
    assert_eq!(s.hub.tunnel_count(), 0);
}

/// F44: an unclean end of the Shield's Control stream ends its tunnels.
#[tokio::test]
async fn a_control_stream_end_ends_its_tunnels() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (_device, relay_end) = tokio::io::duplex(1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));
    s.hub.end_stream(SHIELD, 1, false);
    tokio::time::timeout(WAIT, relay)
        .await
        .expect("the tunnel ends")
        .unwrap()
        .unwrap();
    expect_aborted_stream(&mut tunnel).await;
}

/// Renewal handover: a clean end of the old Control stream, with a newer one
/// registered, keeps the tunnels (their data is on their own streams); they
/// now belong to the new stream, whose unclean end ends them.
#[tokio::test]
async fn a_clean_handover_keeps_the_tunnels_on_the_new_control_stream() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    let (mut device, relay_end) = tokio::io::duplex(1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));

    let (tx2, _rx2) = mpsc::channel(8);
    assert!(s.hub.register_shield(SHIELD.into(), 2, tx2));
    assert_eq!(s.hub.end_stream(SHIELD, 1, true), 1);
    tunnel.send(data(b"still here")).await;
    let mut got = [0u8; 10];
    tokio::time::timeout(WAIT, device.read_exact(&mut got))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(&got, b"still here");
    device.write_all(b"up").await.unwrap();
    assert_eq!(tunnel.next_data().await, b"up");

    s.hub.end_stream(SHIELD, 2, false);
    tokio::time::timeout(WAIT, relay)
        .await
        .expect("ends with its new stream")
        .unwrap()
        .unwrap();
}

/// F45: an older stream's cleanup leaves a newer stream's registration.
#[tokio::test]
async fn an_older_streams_cleanup_keeps_the_newer_stream() {
    let hub = AgentTunnelHub::new();
    let (tx1, _rx1) = mpsc::channel(8);
    let (tx2, rx2) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx1));
    assert!(hub.register_shield(SHIELD.into(), 2, tx2));
    hub.end_stream(SHIELD, 1, false);
    let (mut tunnels, _) = accept_all(hub.clone(), SHIELD, rx2);
    hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp")
        .await
        .expect("opens on stream 2");
    assert!(tunnels.recv().await.is_some());
}

/// A tunnel stream ends when the certificate of its connection expires.
#[tokio::test(start_paused = true)]
async fn a_tunnel_ends_when_its_connection_certificate_expires() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let opening = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
        panic!("expected TunnelOpen");
    };
    let mut tunnel = FakeTunnel::attach_until(
        &hub,
        SHIELD,
        &open.connection_id,
        tokio::time::Instant::now() + Duration::from_secs(60),
    )
    .unwrap();
    let session = opening.await.unwrap().unwrap();
    let (_device, relay_end) = tokio::io::duplex(1024);
    let (p, aborted, _) = probe(relay_end);
    let relay = tokio::spawn(session.relay_stream(p));
    tokio::time::sleep(Duration::from_secs(59)).await;
    assert!(!relay.is_finished());
    tokio::time::sleep(Duration::from_secs(2)).await;
    relay.await.unwrap().unwrap();
    assert_eq!(aborted.load(Ordering::SeqCst), SESSION_REVOKED_CODE);
    expect_aborted_stream(&mut tunnel).await;
}

/// A session dropped without relaying (e.g. the device went away before the
/// answer) frees its place and aborts the Shield's side.
#[tokio::test]
async fn a_dropped_session_frees_its_place_and_aborts_the_shield_side() {
    let mut s = shield();
    let (session, mut tunnel) = s.open("tcp").await;
    assert_eq!(s.hub.tunnel_count(), 1);
    drop(session);
    assert_eq!(s.hub.tunnel_count(), 0);
    expect_aborted_stream(&mut tunnel).await;
}

// ── reliable abort (FU-9 review) ─────────────────────────────────────────

/// An abort reaches the Shield's mailbox even when its Control channel is
/// full, at once: the tunnel ends without waiting for Control capacity, and
/// the TunnelClose is held for delivery instead of being dropped.
#[tokio::test]
async fn an_abort_is_not_lost_when_control_is_full() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(1);
    assert!(hub.register_shield(SHIELD.into(), 1, tx.clone()));
    let opening = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
        panic!("expected TunnelOpen");
    };
    let mut tunnel = FakeTunnel::attach(&hub, SHIELD, &open.connection_id).unwrap();
    let session = opening.await.unwrap().unwrap();
    // Control is now full and nobody reads it.
    tx.send(ShieldControlMessage::default()).await.unwrap();
    assert!(
        tx.try_send(ShieldControlMessage::default()).is_err(),
        "Control full"
    );

    let token = CancellationToken::new();
    let (_device, relay_end) = tokio::io::duplex(1024);
    let (p, aborted, _) = probe(relay_end);
    let relay = tokio::spawn(session.with_cancel_token(token.clone()).relay_stream(p));
    token.cancel();
    tokio::time::timeout(Duration::from_millis(500), relay)
        .await
        .expect("ends without waiting for Control capacity")
        .unwrap()
        .unwrap();
    assert_eq!(aborted.load(Ordering::SeqCst), SESSION_REVOKED_CODE);
    expect_aborted_stream(&mut tunnel).await;
    let (posted, overflowed) = hub.abort_mailbox(SHIELD).take();
    assert!(!overflowed);
    assert_eq!(posted.len(), 1, "the abort was dropped");
    assert_eq!(posted[0].connection_id, open.connection_id);
    assert_eq!(posted[0].error, "session revoked");
}

/// A tunnel that never got a stream posts no abort (its late stream is
/// refused); one that did posts exactly one, however it ends.
#[tokio::test]
async fn only_tunnels_with_a_stream_post_an_abort_once() {
    let hub = AgentTunnelHub::new();
    silent_shield(&hub, SHIELD, 1);
    let pending = {
        let hub = hub.clone();
        tokio::spawn(async move { hub.open_relay_session(SHIELD, "10.0.0.9", 80, "tcp").await })
    };
    wait_count(&hub, 1).await;
    pending.abort();
    wait_count(&hub, 0).await;
    assert_eq!(hub.abort_mailbox(SHIELD).pending_len(), 0);

    let mut s = shield();
    let (session, _tunnel) = s.open("tcp").await;
    drop(session); // not relayed: an unclean end
    let close = tokio::time::timeout(WAIT, s.closes.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(close.error, "session ended");
    assert!(
        tokio::time::timeout(Duration::from_millis(100), s.closes.recv())
            .await
            .is_err(),
        "posted twice"
    );
}

/// The mailbox never grows past its cap: past it, it is marked overflowed
/// (the draining Control stream then ends, closing every tunnel) and the
/// mark is cleared by the take that reports it.
#[test]
fn the_abort_mailbox_is_bounded_and_fails_closed() {
    let m = AbortMailbox::default();
    for i in 0..ABORT_MAILBOX_CAP + 10 {
        m.post(&format!("c{i}"), "x");
    }
    let (list, overflowed) = m.take();
    assert_eq!(list.len(), ABORT_MAILBOX_CAP);
    assert!(overflowed);
    assert_eq!(m.take(), (vec![], false), "the overflow is reported once");
}

/// Aborts a closing stream could not send are put back in order, ahead of
/// newer ones.
#[test]
fn unsent_aborts_are_restored_in_order() {
    let m = AbortMailbox::default();
    m.post("a", "");
    m.post("b", "");
    let (taken, _) = m.take();
    m.post("c", "");
    m.restore(taken);
    let ids: Vec<String> = m.take().0.into_iter().map(|c| c.connection_id).collect();
    assert_eq!(ids, ["a", "b", "c"]);
}

/// A tunnel's abort is posted once, however many times it is aborted.
#[test]
fn an_abort_is_posted_once_per_tunnel() {
    let m = AbortMailbox::default();
    m.post("a", "session revoked");
    m.post("a", "session ended");
    let (list, _) = m.take();
    assert_eq!(list.len(), 1);
    assert_eq!(list[0].error, "session revoked");
}

/// Aborts that a closing Control stream could not send stay in the mailbox,
/// in order, for the Shield's next stream; a live stream gets them all.
#[tokio::test]
async fn aborts_a_closed_stream_could_not_send_are_kept() {
    let m = AbortMailbox::default();
    m.post("a", "");
    m.post("b", "");
    let (closed_tx, closed_rx) = mpsc::channel(4);
    drop(closed_rx);
    assert_eq!(deliver_aborts(&m, &closed_tx).await, Drained::StreamClosed);
    let (live_tx, mut live_rx) = mpsc::channel(4);
    assert_eq!(deliver_aborts(&m, &live_tx).await, Drained::Delivered);
    let mut ids = Vec::new();
    while let Ok(Ok(ShieldControlMessage {
        body: Some(Body::TunnelClose(c)),
    })) = live_rx.try_recv()
    {
        ids.push(c.connection_id);
    }
    assert_eq!(ids, ["a", "b"]);
    assert_eq!(m.pending_len(), 0);
}

// ── gate 4: revocation while opening and racing the first frames ────────────

fn open_revocable(
    hub: &AgentTunnelHub,
    token: &CancellationToken,
) -> tokio::task::JoinHandle<Result<RelaySession>> {
    let hub = hub.clone();
    let token = token.clone();
    tokio::spawn(async move {
        hub.open_relay_session_revocable(SHIELD, "10.0.0.9", 80, "tcp", &token)
            .await
    })
}

/// A session revoked while its tunnel is opening (the Shield is still
/// connecting to the resource): the open fails at once, the pending tunnel
/// is gone (a late stream is refused), and no abort needs posting.
#[tokio::test]
async fn a_revocation_while_opening_fails_the_open_at_once() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let token = CancellationToken::new();
    let opening = open_revocable(&hub, &token);
    let Some(Body::TunnelOpen(open)) = rx.recv().await.unwrap().body else {
        panic!("expected TunnelOpen");
    };
    token.cancel();
    let r = tokio::time::timeout(Duration::from_millis(100), opening)
        .await
        .expect("the open must not wait for its timeout")
        .unwrap();
    assert_eq!(open_err(r), OpenError::SessionRevoked);
    assert_eq!(hub.tunnel_count(), 0, "the pending tunnel is cleaned up");
    let late = FakeTunnel::attach(&hub, SHIELD, &open.connection_id);
    assert_eq!(late.err().unwrap().code(), tonic::Code::NotFound);
    assert_eq!(hub.abort_mailbox(SHIELD).pending_len(), 0);
}

/// A session revoked before the open starts sends nothing to the Shield.
#[tokio::test]
async fn a_session_revoked_before_the_open_sends_nothing() {
    let hub = AgentTunnelHub::new();
    let (tx, mut rx) = mpsc::channel(8);
    assert!(hub.register_shield(SHIELD.into(), 1, tx));
    let token = CancellationToken::new();
    token.cancel();
    let r = open_revocable(&hub, &token).await.unwrap();
    assert_eq!(open_err(r), OpenError::SessionRevoked);
    assert!(
        rx.try_recv().is_err(),
        "a TunnelOpen was sent for a revoked session"
    );
    assert_eq!(hub.tunnel_count(), 0);
}

/// Revoked right after the open, before the first frame, with the device's
/// request already waiting: none of it leaves for the Shield.
#[tokio::test]
async fn a_revocation_before_the_first_frame_forwards_nothing() {
    for _ in 0..50 {
        let mut s = shield();
        let token = CancellationToken::new();
        let session = s
            .hub
            .open_relay_session_revocable(SHIELD, "10.0.0.9", 80, "tcp", &token)
            .await
            .unwrap();
        let mut tunnel = s.tunnels.recv().await.unwrap();
        let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
        device.write_all(&[b'X'; 32 * 1024]).await.unwrap();
        token.cancel();
        let relay = tokio::spawn(session.relay_stream(relay_end));
        tokio::time::timeout(WAIT, relay)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        loop {
            match tunnel.next().await {
                Some(Ok(TunnelFrame {
                    frame: Some(Frame::Data(d)),
                })) => panic!("{} bytes forwarded after the revocation", d.len()),
                Some(Ok(_)) => continue,
                Some(Err(status)) => {
                    assert_eq!(status.code(), tonic::Code::Aborted);
                    break;
                }
                None => panic!("the stream must end with an error"),
            }
        }
        assert_eq!(s.hub.tunnel_count(), 0);
    }
}

/// Gate 4, deterministic: revoking the session sets the tunnel's abort
/// inside `cancel()` — the stream to the Shield ends at once even though no
/// relay loop runs to notice the revocation (the per-frame gate is closed
/// before any task is scheduled).
#[tokio::test]
async fn a_revocation_closes_the_gate_to_the_shield_synchronously() {
    let mut s = shield();
    let token = CancellationToken::new();
    let session = s
        .hub
        .open_relay_session_revocable(SHIELD, "10.0.0.9", 80, "tcp", &token)
        .await
        .unwrap();
    let mut tunnel = s.tunnels.recv().await.unwrap();
    token.cancel();
    // Polled right away, with nothing else run since `cancel()`.
    let first = futures_poll_once(&mut tunnel.from_connector);
    match first {
        std::task::Poll::Ready(Some(Err(status))) => {
            assert_eq!(status.code(), tonic::Code::Aborted)
        }
        other => panic!("the gate was not closed by cancel(): {other:?}"),
    }
    drop(session);
}

/// Gate 4 follow-up (deferred at the FU-9 review): after its initial
/// `Err(Aborted)` the stream to the Shield is over — every later poll is
/// `None`, so HTTP/2 ends it instead of waiting for more frames.
#[tokio::test]
async fn after_the_abort_the_stream_to_the_shield_ends() {
    let mut s = shield();
    let token = CancellationToken::new();
    let session = s
        .hub
        .open_relay_session_revocable(SHIELD, "10.0.0.9", 80, "tcp", &token)
        .await
        .unwrap();
    let mut tunnel = s.tunnels.recv().await.unwrap();
    token.cancel();
    match futures_poll_once(&mut tunnel.from_connector) {
        std::task::Poll::Ready(Some(Err(status))) => {
            assert_eq!(status.code(), tonic::Code::Aborted)
        }
        other => panic!("expected the abort first: {other:?}"),
    }
    for _ in 0..3 {
        assert!(
            matches!(
                futures_poll_once(&mut tunnel.from_connector),
                std::task::Poll::Ready(None)
            ),
            "the stream to the shield did not end after its abort"
        );
    }
    drop(session);
}

/// Gate 4 follow-up (deferred at the FU-9 review): a data frame already
/// queued for the Shield when the session is revoked is discarded, not sent.
/// Control: without the revocation the same queued frame is delivered.
#[tokio::test]
async fn a_frame_queued_before_the_revocation_is_discarded() {
    for revoke in [false, true] {
        let mut s = shield();
        let token = CancellationToken::new();
        let session = s
            .hub
            .open_relay_session_revocable(SHIELD, "10.0.0.9", 80, "tcp", &token)
            .await
            .unwrap();
        let mut tunnel = s.tunnels.recv().await.unwrap();
        // Queued as the device->shield task would, with no relay loop running.
        session
            .io
            .outbound
            .try_send(data(b"queued before the revocation"))
            .unwrap();
        if revoke {
            token.cancel();
        }
        let first = futures_poll_once(&mut tunnel.from_connector);
        match (revoke, first) {
            (
                false,
                std::task::Poll::Ready(Some(Ok(TunnelFrame {
                    frame: Some(Frame::Data(d)),
                }))),
            ) => assert_eq!(d, b"queued before the revocation"),
            (true, std::task::Poll::Ready(Some(Err(status)))) => {
                assert_eq!(status.code(), tonic::Code::Aborted);
                assert!(
                    matches!(
                        futures_poll_once(&mut tunnel.from_connector),
                        std::task::Poll::Ready(None)
                    ),
                    "the queued frame was sent after the abort"
                );
            }
            (revoke, other) => panic!("revoke={revoke}: unexpected {other:?}"),
        }
        drop(session);
    }
}

fn futures_poll_once(
    s: &mut ToShield,
) -> std::task::Poll<Option<Result<TunnelFrame, Status>>> {
    let waker = std::task::Waker::noop();
    let mut cx = std::task::Context::from_waker(waker);
    Pin::new(s).poll_next(&mut cx)
}
