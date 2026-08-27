// dns.rs — Sprint 16 Phase 11 (PENDING-14 Stage 3)
//
// Answers DNS queries for **managed** names with their synthetic IPs, so an app can
// connect by name instead of needing a `hosts` entry. Phase 9.5's manual `hosts`
// entry works, but it is per-machine configuration the daemon does not own, cannot
// update when resources change, and cannot clean up. This makes the mapping live and
// tied to the daemon's lifetime.
//
// SOURCE OF TRUTH: `RuntimeState.synthetic_bindings`, populated by `handle_up` and
// cleared by `handle_down`. This module adds a *protocol*, not a second mapping — it
// never allocates, never caches, and holds no state of its own. A binding that is not
// live is not answered, which is why the responder can run for the daemon's whole
// lifetime rather than being started and stopped with the tunnel.
//
// WHAT THIS MODULE DELIBERATELY DOES NOT DO: touch the OS resolver configuration.
// That is Phase 12 entirely, and keeping it out means this phase is fully testable
// with `dig` pointed explicitly at 127.0.0.1.

use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::Arc;

use anyhow::{Context, Result};
use hickory_proto::op::{Message, MessageType, OpCode, ResponseCode};
use hickory_proto::rr::rdata::A;
use hickory_proto::rr::{RData, Record, RecordType};
use hickory_proto::serialize::binary::BinDecodable;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream, UdpSocket};
use tracing::{debug, info, warn};

use crate::runtime::SharedState;

/// Loopback only. Binding elsewhere would make this an open resolver.
pub const BIND_ADDR: &str = "127.0.0.1:53";

/// Answer TTL. Short enough that a binding change propagates without a stale answer
/// pinning traffic to a released address, long enough to avoid a query per
/// connection.
///
/// It deliberately does **not** track the backend's DNS TTL: the synthetic IP is
/// stable for the life of the binding, and the backend's own TTL is the connector's
/// concern (see connector `resolver.rs`). Conflating the two would make a client
/// re-query every time a backend record changed, for no benefit.
pub const TTL_SECS: u32 = 30;

/// Largest datagram we will read.
///
/// 512 is the classic *non-EDNS* limit, but a resolver advertising EDNS0 may send a
/// larger query, and `recv_from` into a short buffer **silently truncates** — which
/// would present as an unparseable message and a dropped query. 1232 is the
/// widely-adopted EDNS payload size that avoids IP fragmentation on both v4 and v6.
const MAX_UDP: usize = 1232;
/// Cap on a TCP-framed message, so a hostile length prefix cannot make us allocate.
const MAX_TCP: usize = 4096;

// ── pure core ────────────────────────────────────────────────────────────────
//
// Separated from the sockets so every rule below is testable without binding a
// privileged port — the same reason `nft_rule_plan` and `display_address` are pure.

/// Build the response for one query.
///
/// `lookup` receives a lowercased, dot-stripped name and returns the synthetic IP if
/// that name is managed.
pub(crate) fn respond<F>(req: &Message, lookup: F) -> Message
where
    F: Fn(&str) -> Option<Ipv4Addr>,
{
    let mut resp = Message::response(req.metadata.id, req.metadata.op_code);
    // We are authoritative for the synthetic zone and we never recurse. RA=0 is not
    // cosmetic: a resolver that believes we recurse may stop trying other servers.
    resp.metadata.authoritative = true;
    resp.metadata.recursion_available = false;
    resp.metadata.recursion_desired = req.metadata.recursion_desired;

    // Anything other than a standard query is not ours to interpret.
    if req.metadata.op_code != OpCode::Query || req.metadata.message_type != MessageType::Query {
        resp.metadata.response_code = ResponseCode::NotImp;
        return resp;
    }

    let Some(query) = req.queries.first() else {
        resp.metadata.response_code = ResponseCode::FormErr;
        return resp;
    };
    // Echo the question verbatim, including the querier's capitalisation.
    resp.add_query(query.clone());

    // Exact-name match only. No wildcards this sprint: `pattern` is `reserved 14` on
    // ACLEntry and invariant #4 requires wire-level pattern validation before any
    // wildcard is honoured. Do not add prefix/suffix matching here "while we're at
    // it" — there is no field to validate against yet.
    let queried = query.name().to_utf8();
    let key = queried.trim_end_matches('.').to_ascii_lowercase();

    let Some(ip) = lookup(&key) else {
        // NOT a forged NXDOMAIN. A negative answer for a name we do not manage would
        // break the user's unrelated DNS and be very hard to attribute back to us.
        // REFUSED says truthfully "not mine to answer".
        resp.metadata.response_code = ResponseCode::Refused;
        return resp;
    };

    resp.metadata.response_code = ResponseCode::NoError;
    match query.query_type() {
        RecordType::A => {
            resp.add_answer(Record::from_rdata(
                query.name().clone(),
                TTL_SECS,
                RData::A(A(ip)),
            ));
        }
        // NODATA — empty answer with NOERROR — never NXDOMAIN. NXDOMAIN would say the
        // *name* does not exist, which on some stacks suppresses the A lookup
        // entirely. This is the client-side half of the sprint's IPv4-only stance and
        // only makes sense because the connector's resolver is deliberately v4.
        RecordType::AAAA => {}
        // A managed name we do serve, asked for a type we do not. NOERROR/empty is the
        // honest answer: the name exists, that record does not.
        _ => {}
    }
    resp
}

/// True when a query source may be served. Loopback only — we bind loopback anyway,
/// but checking makes the invariant explicit rather than a property of the bind.
fn is_local(addr: &SocketAddr) -> bool {
    match addr.ip() {
        IpAddr::V4(v4) => v4.is_loopback(),
        IpAddr::V6(v6) => v6.is_loopback(),
    }
}

/// Snapshot the live bindings. Taken per query rather than cached so a released
/// binding stops being answered immediately.
async fn bindings(state: &SharedState) -> HashMap<String, Ipv4Addr> {
    state
        .read()
        .await
        .synthetic_bindings
        .iter()
        .map(|(h, ip)| (h.trim().to_ascii_lowercase(), *ip))
        .collect()
}

fn handle_bytes(buf: &[u8], map: &HashMap<String, Ipv4Addr>) -> Option<Vec<u8>> {
    let req = match Message::from_bytes(buf) {
        Ok(m) => m,
        Err(e) => {
            debug!(error = %e, "dropping unparseable DNS message");
            return None;
        }
    };
    let resp = respond(&req, |name| map.get(name).copied());
    match resp.to_vec() {
        Ok(v) => Some(v),
        Err(e) => {
            warn!(error = %e, "could not encode DNS response");
            None
        }
    }
}

// ── sockets ──────────────────────────────────────────────────────────────────

/// Serve UDP **and** TCP on loopback:53 until cancelled.
///
/// ⚠️ TCP/53 is not optional: a resolver that receives a truncated UDP answer retries
/// over TCP, and a responder that only speaks UDP produces intermittent,
/// hard-to-diagnose failures rather than a clean one.
pub async fn serve(state: SharedState) -> Result<()> {
    let udp = Arc::new(
        UdpSocket::bind(BIND_ADDR)
            .await
            .with_context(|| format!("bind UDP {BIND_ADDR}"))?,
    );
    let tcp = TcpListener::bind(BIND_ADDR)
        .await
        .with_context(|| format!("bind TCP {BIND_ADDR}"))?;
    info!(
        addr = BIND_ADDR,
        ttl_secs = TTL_SECS,
        "DNS responder listening (UDP+TCP)"
    );

    let udp_state = state.clone();
    let udp_sock = udp.clone();
    let udp_task = tokio::spawn(async move {
        let mut buf = vec![0u8; MAX_UDP];
        loop {
            let (n, from) = match udp_sock.recv_from(&mut buf).await {
                Ok(v) => v,
                Err(e) => {
                    warn!(error = %e, "DNS UDP recv failed");
                    continue;
                }
            };
            if !is_local(&from) {
                debug!(%from, "dropping off-host DNS query");
                continue;
            }
            let map = bindings(&udp_state).await;
            if let Some(out) = handle_bytes(&buf[..n], &map) {
                if let Err(e) = udp_sock.send_to(&out, from).await {
                    warn!(error = %e, %from, "DNS UDP send failed");
                }
            }
        }
    });

    let tcp_task = tokio::spawn(async move {
        loop {
            let (stream, from) = match tcp.accept().await {
                Ok(v) => v,
                Err(e) => {
                    warn!(error = %e, "DNS TCP accept failed");
                    continue;
                }
            };
            if !is_local(&from) {
                debug!(%from, "dropping off-host DNS connection");
                continue;
            }
            let st = state.clone();
            tokio::spawn(async move {
                if let Err(e) = serve_tcp_conn(stream, st).await {
                    debug!(error = %e, %from, "DNS TCP connection ended");
                }
            });
        }
    });

    let _ = tokio::try_join!(udp_task, tcp_task);
    Ok(())
}

/// One TCP query/response. RFC 1035 §4.2.2: each message is prefixed with a 2-byte
/// big-endian length.
async fn serve_tcp_conn(mut stream: TcpStream, state: SharedState) -> Result<()> {
    let mut len = [0u8; 2];
    stream.read_exact(&mut len).await?;
    let n = u16::from_be_bytes(len) as usize;
    if n == 0 || n > MAX_TCP {
        anyhow::bail!("implausible DNS/TCP length prefix: {n}");
    }
    let mut buf = vec![0u8; n];
    stream.read_exact(&mut buf).await?;

    let map = bindings(&state).await;
    if let Some(out) = handle_bytes(&buf, &map) {
        let olen = u16::try_from(out.len()).context("response too large for DNS/TCP framing")?;
        stream.write_all(&olen.to_be_bytes()).await?;
        stream.write_all(&out).await?;
        stream.flush().await?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use hickory_proto::op::Query;
    use hickory_proto::rr::Name;
    use std::str::FromStr;

    fn map(pairs: &[(&str, &str)]) -> HashMap<String, Ipv4Addr> {
        pairs
            .iter()
            .map(|(h, ip)| (h.to_string(), ip.parse().unwrap()))
            .collect()
    }

    /// ⚠️ `Name::from_ascii`, NOT `from_str`. `from_str` runs IDNA normalisation,
    /// which the crate documents as having "a side-effect of lowercasing the name" —
    /// so building a mixed-case query with it silently lowercases before the wire and
    /// makes a case-echo test vacuous.
    fn query(name: &str, rt: RecordType) -> Message {
        let mut m = Message::query();
        m.metadata.id = 0x1234;
        m.add_query(Query::query(Name::from_ascii(name).unwrap(), rt));
        m
    }

    fn ask(name: &str, rt: RecordType, m: &HashMap<String, Ipv4Addr>) -> Message {
        respond(&query(name, rt), |k| m.get(k).copied())
    }

    #[test]
    fn a_managed_name_answers_with_its_synthetic_ip() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("app.internal.", RecordType::A, &m);
        assert_eq!(r.metadata.response_code, ResponseCode::NoError);
        assert_eq!(r.answers.len(), 1);
        match &r.answers[0].data {
            RData::A(a) => assert_eq!(a.0, "100.64.0.2".parse::<Ipv4Addr>().unwrap()),
            other => panic!("expected an A record, got {other:?}"),
        }
        assert_eq!(r.answers[0].ttl, TTL_SECS);
        assert!((30..=60).contains(&TTL_SECS), "spec requires a 30-60s TTL");
    }

    /// The whole point of the phase: no `hosts` entry, the name resolves itself.
    #[test]
    fn the_answer_echoes_the_question() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("app.internal.", RecordType::A, &m);
        assert_eq!(r.queries.len(), 1, "the question section must be echoed");
        assert_eq!(r.queries[0].query_type(), RecordType::A);
    }

    /// NXDOMAIN for AAAA says the NAME does not exist and can suppress the A lookup
    /// entirely on some stacks. This is the client half of the IPv4-only stance.
    #[test]
    fn aaaa_for_a_managed_name_is_nodata_never_nxdomain() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("app.internal.", RecordType::AAAA, &m);
        assert_eq!(r.metadata.response_code, ResponseCode::NoError);
        assert!(r.answers.is_empty(), "AAAA must be an EMPTY NOERROR answer");
        assert_ne!(r.metadata.response_code, ResponseCode::NXDomain);
    }

    /// A forged negative answer for a name we do not manage breaks the user's
    /// unrelated DNS and is nearly impossible to attribute back to us.
    #[test]
    fn an_unmanaged_name_is_refused_not_nxdomain() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("example.com.", RecordType::A, &m);
        assert_eq!(r.metadata.response_code, ResponseCode::Refused);
        assert!(r.answers.is_empty());
        assert_ne!(r.metadata.response_code, ResponseCode::NXDomain);
    }

    /// The spec asks for the queried name's case to be echoed back. That depends on
    /// hickory's `Name` preserving case through **wire** parsing, not just through
    /// `from_str` — so this goes through serialise/parse rather than trusting the
    /// in-memory value.
    #[test]
    fn the_answer_echoes_the_queried_case_over_the_wire() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let bytes = query("ApP.InTeRnAl.", RecordType::A).to_vec().unwrap();
        let parsed_req = Message::from_bytes(&bytes).unwrap();
        let r = respond(&parsed_req, |k| m.get(k).copied());

        assert_eq!(r.metadata.response_code, ResponseCode::NoError);
        assert_eq!(r.answers.len(), 1);
        assert_eq!(
            r.answers[0].name.to_utf8().trim_end_matches('.'),
            "ApP.InTeRnAl",
            "the answer must carry the querier's capitalisation, not a normalised form"
        );
        assert_eq!(
            r.queries[0].name().to_utf8().trim_end_matches('.'),
            "ApP.InTeRnAl",
            "the echoed question must also keep the querier's case"
        );
    }

    #[test]
    fn matching_is_case_insensitive() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        for name in ["APP.INTERNAL.", "App.Internal.", "aPp.InTeRnAl."] {
            let r = ask(name, RecordType::A, &m);
            assert_eq!(
                r.metadata.response_code,
                ResponseCode::NoError,
                "{name} should match"
            );
            assert_eq!(r.answers.len(), 1, "{name} should answer");
        }
    }

    /// No wildcards this sprint — `pattern` is `reserved 14` and unvalidated.
    #[test]
    fn no_wildcard_or_suffix_matching() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        for name in [
            "sub.app.internal.",
            "app.internal.evil.com.",
            "pp.internal.",
        ] {
            assert_eq!(
                ask(name, RecordType::A, &m).metadata.response_code,
                ResponseCode::Refused,
                "{name} must NOT match app.internal"
            );
        }
    }

    #[test]
    fn we_never_claim_to_recurse() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("app.internal.", RecordType::A, &m);
        assert!(!r.metadata.recursion_available, "RA must be 0");
        assert!(
            r.metadata.authoritative,
            "we are authoritative for managed names"
        );
    }

    #[test]
    fn a_non_query_opcode_is_not_implemented() {
        let mut m = Message::new(7, MessageType::Query, OpCode::Update);
        m.add_query(Query::query(
            Name::from_str("app.internal.").unwrap(),
            RecordType::A,
        ));
        let r = respond(&m, |_| Some("100.64.0.2".parse().unwrap()));
        assert_eq!(r.metadata.response_code, ResponseCode::NotImp);
    }

    /// Tunnel down ⇒ no live bindings ⇒ nothing is ours to answer.
    #[test]
    fn with_no_bindings_everything_is_refused() {
        let empty = HashMap::new();
        assert_eq!(
            ask("app.internal.", RecordType::A, &empty)
                .metadata
                .response_code,
            ResponseCode::Refused
        );
    }

    #[test]
    fn the_response_id_matches_the_request() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let r = ask("app.internal.", RecordType::A, &m);
        assert_eq!(r.metadata.id, 0x1234);
    }

    #[test]
    fn a_response_round_trips_on_the_wire() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        let bytes = query("app.internal.", RecordType::A).to_vec().unwrap();
        let out = handle_bytes(&bytes, &m).expect("should produce a response");
        let parsed = Message::from_bytes(&out).expect("our own response must parse");
        assert_eq!(parsed.metadata.response_code, ResponseCode::NoError);
        assert_eq!(parsed.answers.len(), 1);
    }

    #[test]
    fn garbage_bytes_produce_no_response_rather_than_a_panic() {
        let m = map(&[("app.internal", "100.64.0.2")]);
        // No `|| true` escape hatch: an empty buffer is definitely unparseable, and
        // whatever a 12-byte 0xff header decodes to must not panic and must not
        // produce an answer for a name we do not manage.
        assert!(
            handle_bytes(&[], &m).is_none(),
            "empty input must not answer"
        );
        if let Some(out) = handle_bytes(&[0xff; 12], &m) {
            let parsed = Message::from_bytes(&out).expect("our own output must parse");
            assert!(
                parsed.answers.is_empty(),
                "garbage must never yield answer records"
            );
        }
    }

    #[test]
    fn only_loopback_sources_are_served() {
        assert!(is_local(&"127.0.0.1:5300".parse().unwrap()));
        assert!(is_local(&"[::1]:5300".parse().unwrap()));
        assert!(!is_local(&"192.168.1.38:5300".parse().unwrap()));
        assert!(!is_local(&"10.0.0.1:53".parse().unwrap()));
    }
}
