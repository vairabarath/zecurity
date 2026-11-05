//! FU-9 lab harness, Connector side (PENDING-21 §11). Not a unit test: run
//! by hand, with the Shield side (`shield/src/fu9_lab.rs`) in another
//! network namespace behind an emulated link (scripts in the PENDING-21
//! record). Production code on this side: the Shield-facing mTLS server
//! (`ShieldRegistry::serve_on`, HTTP/2 settings, `Control` and `Tunnel`
//! handlers) and the tunnel hub (`open_relay_session`, `relay_stream`). The
//! device side is simulated in memory.
//!
//! ```text
//! LAB_DIR=/tmp/lab LAB_LISTEN=10.9.0.1:9091 \
//!   cargo test --release --lib fu9_lab -- --ignored --nocapture
//! ```

use std::net::SocketAddr;
use std::sync::Arc;
use std::time::{Duration, Instant};

use sha2::{Digest, Sha256};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::mpsc;

use crate::agent_server::ShieldRegistry;
use crate::agent_tunnel::AgentTunnelHub;
use crate::test_support::{TestPki, CONNECTOR_ID, TRUST_DOMAIN};

const SHIELD_ID: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
/// Ports of the Shield side's test resources (on its 127.0.0.1).
const TCP_RESOURCE: u16 = 18080;
const UDP_RESOURCE: u16 = 18081;

/// The download resource's bytes: the same generator on both sides.
pub(crate) fn gen_byte(i: u64) -> u8 {
    ((i as u32).wrapping_mul(2_654_435_761).rotate_left(7) >> 3) as u8
}

fn expected_sha(size: u64) -> String {
    let mut h = Sha256::new();
    let mut buf = vec![0u8; 64 * 1024];
    let mut i = 0;
    while i < size {
        let n = (size - i).min(buf.len() as u64) as usize;
        for (k, b) in buf[..n].iter_mut().enumerate() {
            *b = gen_byte(i + k as u64);
        }
        h.update(&buf[..n]);
        i += n as u64;
    }
    hex::encode(h.finalize())
}

fn log(t0: Instant, msg: impl AsRef<str>) {
    println!("[{:7.2}s] {}", t0.elapsed().as_secs_f64(), msg.as_ref());
}

#[derive(Debug)]
struct Download {
    bytes: u64,
    sha: String,
    elapsed: Duration,
    ended_cleanly: bool,
}

/// Download `size` bytes from the Shield's TCP resource through a tunnel,
/// reading at most `rate` bytes/s (None: as fast as possible). `stall`
/// stops reading entirely until it is set to false.
async fn download(
    hub: &AgentTunnelHub,
    size: u64,
    rate: Option<u64>,
    stall: Option<tokio::sync::watch::Receiver<bool>>,
    progress: Option<Arc<std::sync::atomic::AtomicU64>>,
) -> anyhow::Result<Download> {
    let t0 = Instant::now();
    let session = hub
        .open_relay_session(SHIELD_ID, "127.0.0.1", TCP_RESOURCE, "tcp")
        .await?;
    let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
    let relay = tokio::spawn(session.relay_stream(relay_end));
    device.write_all(format!("{size}\n").as_bytes()).await?;
    let mut h = Sha256::new();
    let mut got = 0u64;
    let chunk = match rate {
        Some(r) => (r / 10).max(1) as usize, // 10 reads per second
        None => 64 * 1024,
    };
    let mut buf = vec![0u8; chunk];
    let mut stall = stall;
    loop {
        if let Some(s) = stall.as_mut() {
            let _ = s.wait_for(|stalled| !*stalled).await;
        }
        let n = device.read(&mut buf).await.unwrap_or(0);
        if n == 0 {
            break;
        }
        h.update(&buf[..n]);
        got += n as u64;
        if let Some(p) = &progress {
            p.store(got, std::sync::atomic::Ordering::SeqCst);
        }
        if rate.is_some() {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    let ended_cleanly = matches!(
        tokio::time::timeout(Duration::from_secs(5), relay).await,
        Ok(Ok(Ok(())))
    ) && got == size;
    Ok(Download {
        bytes: got,
        sha: hex::encode(h.finalize()),
        elapsed: t0.elapsed(),
        ended_cleanly,
    })
}

/// A Control round trip: an instruction (a harness command) to the Shield
/// and its ResourceAck back, through the production Control handler.
async fn control_rtt(
    registry: &ShieldRegistry,
    acks: &mut mpsc::Receiver<(String, crate::shield_proto::ResourceAck)>,
    command: &str,
) -> Option<Duration> {
    let t0 = Instant::now();
    registry.push_instructions(
        SHIELD_ID,
        vec![crate::shield_proto::ResourceInstruction {
            resource_id: command.to_string(),
            action: "lab".into(),
            ..Default::default()
        }],
    );
    loop {
        match tokio::time::timeout(Duration::from_secs(5), acks.recv()).await {
            Ok(Some((_, ack))) if ack.resource_id == command => return Some(t0.elapsed()),
            Ok(Some(_)) => continue,
            _ => return None,
        }
    }
}

fn ms(d: Option<Duration>) -> String {
    d.map(|d| format!("{:.1} ms", d.as_secs_f64() * 1e3))
        .unwrap_or_else(|| "NO REPLY in 5 s".into())
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "FU-9 lab harness: run by hand in the netns lab (PENDING-21 §11)"]
async fn fu9_lab_connector() {
    crate::test_support::install_crypto_provider();
    let dir = std::env::var("LAB_DIR").expect("LAB_DIR");
    let listen: SocketAddr = std::env::var("LAB_LISTEN").expect("LAB_LISTEN").parse().unwrap();
    let t0 = Instant::now();

    // PKI for both sides; the Shield side reads its files from LAB_DIR.
    let pki = TestPki::new();
    let (_holder_dir, holder) = pki.holder(-60, 3600);
    let (a_chain, a_key) = pki.shield_identity_pem(SHIELD_ID);
    let (b_chain, b_key) = pki.shield_identity_pem(SHIELD_ID);
    let ca = format!("{}\n{}", pki.workspace_ca_pem.trim_end(), pki.intermediate_pem.trim_end());
    for (name, body) in [
        ("ca.pem", ca.as_str()),
        ("shield-a.crt", a_chain.as_str()),
        ("shield-a.key", a_key.as_str()),
        ("shield-b.crt", b_chain.as_str()),
        ("shield-b.key", b_key.as_str()),
        ("ids", &format!("{CONNECTOR_ID}\n{TRUST_DOMAIN}\n{SHIELD_ID}\n")),
    ] {
        std::fs::write(format!("{dir}/{name}.tmp"), body).unwrap();
        std::fs::rename(format!("{dir}/{name}.tmp"), format!("{dir}/{name}")).unwrap();
    }

    let (ack_tx, mut acks) = mpsc::channel(64);
    let registry = ShieldRegistry::new(
        tonic::transport::Channel::from_static("http://127.0.0.1:1").connect_lazy(),
        TRUST_DOMAIN.to_string(),
        CONNECTOR_ID.to_string(),
        ack_tx,
        Arc::new(crate::policy::PolicyCache::new()),
        crate::authorization::AuthzGate::open_for_tests(),
    );
    registry.apply_revocations(vec![]);
    let listener = TcpListener::bind(listen).await.unwrap();
    tokio::spawn(registry.clone().serve_on(listener, holder, Duration::from_secs(10)));
    std::fs::write(format!("{dir}/ready"), "").unwrap();
    log(t0, format!("connector listening on {listen}; waiting for the shield"));

    // Wait for the Shield's Control stream.
    let hub = registry.tunnel_hub.clone();
    while control_rtt(&registry, &mut acks, "hello").await.is_none() {}
    log(t0, format!("shield attached; control RTT {}", ms(control_rtt(&registry, &mut acks, "rtt").await)));

    // ── 1. Throughput of one tunnel ───────────────────────────────────────
    let d = download(&hub, 32 << 20, None, None, None).await.unwrap();
    let ok = d.sha == expected_sha(32 << 20);
    log(t0, format!(
        "THROUGHPUT 32 MB in {:.2} s = {:.2} MB/s, sha ok {ok}, clean end {}",
        d.elapsed.as_secs_f64(),
        (32.0) / d.elapsed.as_secs_f64(),
        d.ended_cleanly
    ));

    // ── 2. Slow reader, 25 KB/s for 60 s, running through 3–5 ─────────────
    const SLOW: u64 = 25 * 1024 * 60;
    let slow_progress = Arc::new(std::sync::atomic::AtomicU64::new(0));
    let slow = {
        let hub = hub.clone();
        let p = slow_progress.clone();
        tokio::spawn(async move { download(&hub, SLOW, Some(25 * 1024), None, Some(p)).await })
    };
    tokio::time::sleep(Duration::from_secs(5)).await;

    // ── 3. A stalled tunnel; another transfers; Control; a new tunnel ─────
    let (stall_tx, stall_rx) = tokio::sync::watch::channel(true);
    let stalled_progress = Arc::new(std::sync::atomic::AtomicU64::new(0));
    let stalled = {
        let hub = hub.clone();
        let p = stalled_progress.clone();
        tokio::spawn(async move { download(&hub, 64 << 20, None, Some(stall_rx), Some(p)).await })
    };
    tokio::time::sleep(Duration::from_secs(2)).await;
    let other = download(&hub, 8 << 20, None, None, None).await.unwrap();
    log(t0, format!(
        "STALL other tunnel: 8 MB in {:.2} s = {:.2} MB/s, sha ok {}",
        other.elapsed.as_secs_f64(),
        8.0 / other.elapsed.as_secs_f64(),
        other.sha == expected_sha(8 << 20)
    ));
    let mut worst = Duration::ZERO;
    for i in 0..5 {
        match control_rtt(&registry, &mut acks, &format!("rtt-{i}")).await {
            Some(d) => worst = worst.max(d),
            None => {
                worst = Duration::MAX;
                break;
            }
        }
    }
    log(t0, format!("STALL control RTT worst of 5: {}", if worst == Duration::MAX { "NO REPLY".into() } else { ms(Some(worst)) }));
    let t_open = Instant::now();
    let fresh = download(&hub, 64 * 1024, None, None, None).await.unwrap();
    log(t0, format!(
        "STALL new tunnel: open + 64 KB in {}, sha ok {}",
        ms(Some(t_open.elapsed())),
        fresh.sha == expected_sha(64 * 1024)
    ));
    log(t0, format!(
        "STALL stalled tunnel still open: {}, slow reader at {} bytes",
        !stalled.is_finished(),
        slow_progress.load(std::sync::atomic::Ordering::SeqCst)
    ));
    stall_tx.send_replace(false);
    let s = stalled.await.unwrap().unwrap();
    log(t0, format!(
        "STALL stalled tunnel resumed: 64 MB complete {}, sha ok {}, total {:.1} s",
        s.bytes == 64 << 20,
        s.sha == expected_sha(64 << 20),
        s.elapsed.as_secs_f64()
    ));

    // ── 4. Renewal during the slow download ────────────────────────────────
    log(t0, format!(
        "RENEW asking the shield to renew; slow reader at {} bytes",
        slow_progress.load(std::sync::atomic::Ordering::SeqCst)
    ));
    log(t0, format!("RENEW command acked in {}", ms(control_rtt(&registry, &mut acks, "renew").await)));
    tokio::time::sleep(Duration::from_secs(3)).await;
    log(t0, format!("RENEW control RTT on the new stream: {}", ms(control_rtt(&registry, &mut acks, "after-renew").await)));
    let after = download(&hub, 1 << 20, None, None, None).await.unwrap();
    log(t0, format!(
        "RENEW new tunnel after renewal: 1 MB sha ok {}, clean {}",
        after.sha == expected_sha(1 << 20),
        after.ended_cleanly
    ));

    // ── 5. UDP saturation ─────────────────────────────────────────────────
    {
        let session = hub
            .open_relay_session(SHIELD_ID, "127.0.0.1", UDP_RESOURCE, "udp")
            .await
            .unwrap();
        let (mut device, relay_end) = tokio::io::duplex(4 << 20);
        let _relay = tokio::spawn(session.relay_stream(relay_end));
        let frame = |d: &[u8]| {
            let mut v = (d.len() as u32).to_be_bytes().to_vec();
            v.extend_from_slice(d);
            v
        };
        device.write_all(&frame(b"go")).await.unwrap();
        // Not reading for 2 s while the resource floods 5000 × 1 KB.
        tokio::time::sleep(Duration::from_secs(2)).await;
        log(t0, format!("UDP control RTT during saturation: {}", ms(control_rtt(&registry, &mut acks, "udp-rtt").await)));
        let mut ids = Vec::new();
        let mut len = [0u8; 4];
        while let Ok(Ok(_)) =
            tokio::time::timeout(Duration::from_millis(500), device.read_exact(&mut len)).await
        {
            let mut d = vec![0u8; u32::from_be_bytes(len) as usize];
            device.read_exact(&mut d).await.unwrap();
            if d.len() >= 4 {
                ids.push(u32::from_be_bytes(d[..4].try_into().unwrap()));
            }
        }
        let ordered = ids.windows(2).all(|w| w[0] < w[1]);
        log(t0, format!(
            "UDP saturation: {} of 5000 datagrams delivered, in order {ordered}, first {:?}, last {:?}",
            ids.len(),
            ids.first(),
            ids.last()
        ));
        device.write_all(&frame(b"ping")).await.unwrap();
        let pong = tokio::time::timeout(Duration::from_secs(2), async {
            device.read_exact(&mut len).await.unwrap();
            let mut d = vec![0u8; u32::from_be_bytes(len) as usize];
            device.read_exact(&mut d).await.unwrap();
            d
        })
        .await;
        log(t0, format!("UDP after saturation: ping → {:?}", pong.map(|d| String::from_utf8_lossy(&d).to_string())));
    }

    // The slow download must finish, intact, not cut.
    let s = slow.await.unwrap().unwrap();
    log(t0, format!(
        "SLOW 25 KB/s: {} of {} bytes in {:.1} s, sha ok {}, clean end {}",
        s.bytes,
        SLOW,
        s.elapsed.as_secs_f64(),
        s.sha == expected_sha(SLOW),
        s.ended_cleanly
    ));

    // ── 6. Revocation during an active download ────────────────────────────
    let progress = Arc::new(std::sync::atomic::AtomicU64::new(0));
    let active = {
        let hub = hub.clone();
        let p = progress.clone();
        tokio::spawn(async move { download(&hub, 64 << 20, Some(200 * 1024), None, Some(p)).await })
    };
    tokio::time::sleep(Duration::from_secs(3)).await;
    let at = progress.load(std::sync::atomic::Ordering::SeqCst);
    let t_rev = Instant::now();
    registry.apply_revocations(vec![SHIELD_ID.to_string()]);
    let r = tokio::time::timeout(Duration::from_secs(10), active).await;
    let after = progress.load(std::sync::atomic::Ordering::SeqCst);
    match r {
        Ok(Ok(Ok(d))) => log(t0, format!(
            "REVOKE download ended {} after revocation; {} bytes at revocation, {} after (+{}); clean end {} (must be false)",
            ms(Some(t_rev.elapsed())), at, after, after - at, d.ended_cleanly
        )),
        other => log(t0, format!("REVOKE download did not end within 10 s: {other:?}")),
    }
    let refused = hub.open_relay_session(SHIELD_ID, "127.0.0.1", TCP_RESOURCE, "tcp").await;
    log(t0, format!("REVOKE new tunnel after revocation refused: {}", refused.is_err()));
    log(t0, "DONE");
    std::fs::write(format!("{dir}/done"), "").unwrap();
    tokio::time::sleep(Duration::from_secs(2)).await;
}
