//! Cross-language authorization-contract test (connector-revocation design
//! T8/T20/T21, Phase A): the REAL Go controller Control handler (served by
//! grpc-go through `controller/internal/connector/xlangharness`) against the
//! REAL Rust tonic client, the production `stamp_sends`,
//! `observe_controller_msg` and `classify_status`.
//!
//! Proves on the wire, producer and consumer together:
//!   - the `zecurity-connector-state` trailer marker survives a trailers-only
//!     refusal at open (revoked / unknown) and a mid-stream termination after
//!     messages were sent (live revocation, deletion);
//!   - REVOKED vs UNKNOWN_CONNECTOR are distinguished;
//!   - a DB failure and a contract violation (seq = 0) arrive UNMARKED;
//!   - AgentPolicy{authorization_acks} arrives first and an AuthorizationAck
//!     for the sent seq is accepted by the production tracker;
//!   - ConnectorRevoked is delivered on the live stream.
//!
//! Needs Go, Postgres and the harness binary, so it is #[ignore]d in a plain
//! `cargo test` (shown as ignored, never as passed). Run it with
//! `scripts/xlang-authz-markers.sh`, which builds the harness and sets
//! ZECURITY_XLANG_HARNESS, XLANG_DB_URL and XLANG_MIGRATIONS_DIR.

use std::process::Stdio;
use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader, Lines};
use tokio::process::{Child, ChildStdin, ChildStdout, Command};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::Channel;
use tonic::{Code, Request, Status, Streaming};

use zecurity_connector::authorization::{
    classify_status, observe_controller_msg, stamp_sends, AckTracker, SharedAckTracker,
    StatusClass, StreamAuthz, TerminalSignal,
};
use zecurity_connector::proto::connector_control_message::Body as CBody;
use zecurity_connector::proto::connector_service_client::ConnectorServiceClient;
use zecurity_connector::proto::{ConnectorControlMessage, ConnectorHealthReport};

const WAIT: Duration = Duration::from_secs(10);
const H: Duration = Duration::from_secs(15);

struct Harness {
    child: Child,
    stdin: ChildStdin,
    lines: Lines<BufReader<ChildStdout>>,
    seeded: serde_json::Value,
}

impl Harness {
    async fn start() -> Self {
        let bin = std::env::var("ZECURITY_XLANG_HARNESS")
            .expect("ZECURITY_XLANG_HARNESS not set — run scripts/xlang-authz-markers.sh");
        let mut child = Command::new(bin)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .kill_on_drop(true)
            .spawn()
            .expect("spawn xlangharness");
        let stdin = child.stdin.take().unwrap();
        let mut lines = BufReader::new(child.stdout.take().unwrap()).lines();
        let first = tokio::time::timeout(Duration::from_secs(120), lines.next_line())
            .await
            .expect("harness start timeout")
            .unwrap()
            .expect("harness printed nothing");
        let seeded: serde_json::Value = serde_json::from_str(&first).expect("seed json");
        Harness {
            child,
            stdin,
            lines,
            seeded,
        }
    }

    fn id(&self, key: &str) -> String {
        self.seeded[key].as_str().unwrap().to_string()
    }

    async fn cmd(&mut self, c: &str) -> String {
        self.stdin
            .write_all(format!("{c}\n").as_bytes())
            .await
            .unwrap();
        let reply = tokio::time::timeout(WAIT, self.lines.next_line())
            .await
            .expect("harness reply timeout")
            .unwrap()
            .unwrap();
        assert!(reply.starts_with("ok"), "harness `{c}` failed: {reply}");
        reply
    }

    async fn client(&self) -> ConnectorServiceClient<Channel> {
        let addr = self.seeded["addr"].as_str().unwrap();
        let ch = Channel::from_shared(format!("http://{addr}"))
            .unwrap()
            .connect()
            .await
            .expect("connect to harness");
        ConnectorServiceClient::new(ch)
    }
}

struct Open {
    tx: mpsc::Sender<ConnectorControlMessage>,
    inbound: Streaming<ConnectorControlMessage>,
}

/// Open Control as `id` exactly as the connector does (production stamping).
async fn open(
    client: &mut ConnectorServiceClient<Channel>,
    id: &str,
    tracker: &SharedAckTracker,
) -> Result<Open, Status> {
    let (tx, rx) = mpsc::channel(16);
    let mut req = Request::new(stamp_sends(ReceiverStream::new(rx), tracker.clone()));
    req.metadata_mut()
        .insert("x-xlang-connector-id", id.parse().unwrap());
    let resp = tokio::time::timeout(WAIT, client.control(req))
        .await
        .expect("Control() timeout")?;
    Ok(Open {
        tx,
        inbound: resp.into_inner(),
    })
}

fn health(seq: u64) -> ConnectorControlMessage {
    ConnectorControlMessage {
        body: Some(CBody::ConnectorHealth(ConnectorHealthReport {
            version: "xlang".into(),
            hostname: "xlang".into(),
            heartbeat_seq: seq,
            ..Default::default()
        })),
    }
}

/// Read messages (observing each with the production observer) until `stop`
/// matches; returns everything read.
async fn read_until(
    o: &mut Open,
    s: &mut StreamAuthz,
    t: &SharedAckTracker,
    stop: impl Fn(&ConnectorControlMessage) -> bool,
) -> Vec<ConnectorControlMessage> {
    let mut seen = Vec::new();
    loop {
        let m = tokio::time::timeout(WAIT, o.inbound.message())
            .await
            .expect("message timeout")
            .expect("unexpected stream error")
            .expect("unexpected end of stream");
        observe_controller_msg(&m, s, t);
        let done = stop(&m);
        seen.push(m);
        if done {
            return seen;
        }
    }
}

/// Drain until the stream ends with an error status; returns it.
async fn read_error(
    o: &mut Open,
    s: &mut StreamAuthz,
    t: &SharedAckTracker,
) -> (Status, Vec<ConnectorControlMessage>) {
    let mut seen = Vec::new();
    loop {
        match tokio::time::timeout(WAIT, o.inbound.message())
            .await
            .expect("status timeout")
        {
            Ok(Some(m)) => {
                observe_controller_msg(&m, s, t);
                seen.push(m);
            }
            Ok(None) => panic!("stream ended cleanly, expected an error status; read {seen:?}"),
            Err(st) => return (st, seen),
        }
    }
}

fn is_ack(m: &ConnectorControlMessage, seq: u64) -> bool {
    matches!(&m.body, Some(CBody::AuthorizationAck(a)) if a.heartbeat_seq == seq)
}

#[tokio::test]
#[ignore = "needs Go + Postgres + xlangharness: run scripts/xlang-authz-markers.sh"]
async fn go_controller_and_tonic_connector_agree_on_authorization_contract() {
    let mut hx = Harness::start().await;
    let mut client = hx.client().await;

    // 1. Revoked at open: trailers-only refusal carries the revoked marker.
    let t = AckTracker::shared(H);
    let st = open(&mut client, &hx.id("revoked"), &t)
        .await
        .err()
        .expect("revoked connector must be refused at Control()");
    assert_eq!(st.code(), Code::PermissionDenied);
    assert_eq!(classify_status(&st), StatusClass::Revoked, "status: {st:?}");

    // 2. Unknown identity at open: NotFound + unknown marker, distinct.
    let st = open(&mut client, &hx.id("unknown"), &t)
        .await
        .err()
        .expect("unknown connector must be refused at Control()");
    assert_eq!(st.code(), Code::NotFound);
    assert_eq!(
        classify_status(&st),
        StatusClass::UnknownConnector,
        "status: {st:?}"
    );

    // 2b. Revoked, then deleted (Fix 13, F40): the tombstone keeps it revoked.
    let st = open(&mut client, &hx.id("revoked_del"), &t)
        .await
        .err()
        .expect("a deleted revoked connector must be refused at Control()");
    assert_eq!(st.code(), Code::PermissionDenied);
    assert_eq!(classify_status(&st), StatusClass::Revoked, "status: {st:?}");

    // 3. Authorized: capability first, then an ack the production tracker
    //    accepts for the request it actually sent.
    let t = AckTracker::shared(H);
    let mut s = StreamAuthz::default();
    let mut o = open(&mut client, &hx.id("active"), &t)
        .await
        .expect("open active");
    let seq = t.lock().next_seq();
    o.tx.send(health(seq)).await.unwrap();
    let seen = read_until(&mut o, &mut s, &t, |m| is_ack(m, seq)).await;
    match &seen[0].body {
        Some(CBody::AgentPolicy(p)) => assert!(p.authorization_acks, "capability must be asserted"),
        other => panic!("first message must be AgentPolicy, got {other:?}"),
    }
    assert_eq!(t.lock().highest_accepted(), seq, "ack accepted end to end");
    assert!(t.lock().latest_accepted_sent_at().is_some());

    // 4. Live revocation: ConnectorRevoked on the live stream, then the stream
    //    ends with the revoked marker in the trailers (after messages).
    let reply = hx.cmd(&format!("revoke {}", hx.id("active"))).await;
    assert_eq!(
        reply, "ok revoke 1",
        "delivered on exactly the one live stream"
    );
    let (st, seen) = read_error(&mut o, &mut s, &t).await;
    assert!(
        seen.iter()
            .any(|m| matches!(m.body, Some(CBody::ConnectorRevoked(_)))),
        "ConnectorRevoked not received: {seen:?}"
    );
    assert_eq!(t.lock().terminal(), Some(TerminalSignal::Revoked));
    assert_eq!(st.code(), Code::PermissionDenied);
    assert_eq!(
        classify_status(&st),
        StatusClass::Revoked,
        "mid-stream trailer: {st:?}"
    );

    // 5. Deleted mid-stream: the next report ends the stream with the
    //    unknown marker (not revoked).
    let t = AckTracker::shared(H);
    let mut s = StreamAuthz::default();
    let mut o = open(&mut client, &hx.id("active_del"), &t)
        .await
        .expect("open active_del");
    let s1 = t.lock().next_seq();
    o.tx.send(health(s1)).await.unwrap();
    read_until(&mut o, &mut s, &t, |m| is_ack(m, s1)).await;
    hx.cmd(&format!("delete {}", hx.id("active_del"))).await;
    let s2 = t.lock().next_seq();
    o.tx.send(health(s2)).await.unwrap();
    let (st, seen) = read_error(&mut o, &mut s, &t).await;
    assert!(
        !seen.iter().any(|m| is_ack(m, s2)),
        "deleted connector acknowledged"
    );
    assert_eq!(st.code(), Code::NotFound);
    assert_eq!(
        classify_status(&st),
        StatusClass::UnknownConnector,
        "status: {st:?}"
    );

    // 6. seq = 0 (no contract): FailedPrecondition, UNMARKED, no ack.
    let t = AckTracker::shared(H);
    let mut s = StreamAuthz::default();
    let mut o = open(&mut client, &hx.id("active_zero"), &t)
        .await
        .expect("open active_zero");
    o.tx.send(health(0)).await.unwrap();
    let (st, seen) = read_error(&mut o, &mut s, &t).await;
    assert!(!seen
        .iter()
        .any(|m| matches!(m.body, Some(CBody::AuthorizationAck(_)))));
    assert_eq!(st.code(), Code::FailedPrecondition);
    assert_eq!(
        classify_status(&st),
        StatusClass::Unmarked(Code::FailedPrecondition)
    );

    // 7. DB failure: Unavailable, UNMARKED (never NotFound, never terminal).
    hx.cmd("dbdown").await;
    let t = AckTracker::shared(H);
    let st = open(&mut client, &hx.id("active_down"), &t)
        .await
        .err()
        .expect("lookup failure must refuse Control()");
    assert_eq!(st.code(), Code::Unavailable, "status: {st:?}");
    assert_eq!(
        classify_status(&st),
        StatusClass::Unmarked(Code::Unavailable)
    );

    hx.cmd("quit").await;
    // Let the harness drop its throwaway database before the process handle
    // is dropped (kill_on_drop would otherwise race the cleanup).
    let status = tokio::time::timeout(WAIT, hx.child.wait())
        .await
        .expect("harness exit timeout")
        .unwrap();
    assert!(status.success(), "harness exit: {status:?}");
}
