// Sprint 11 ADR-016 Phase 1 — relay probe.
//
// Dials each labelled relay over QUIC mTLS, exchanges a single Probe
// handshake message, measures RTT, scores the relay. No registration
// happens; the connection is dropped immediately after the response.
//
// Wire format mirrors the existing register path in relay_client.rs:
// JSON-encoded `HandshakeMsg::Probe` with a 4-byte big-endian length
// prefix. The relay parses with the same `serde_json` codec at
// relay/src/protocol.rs.
//
// All failure modes (dial timeout, mTLS rejection, stream-closed,
// request_id mismatch) collapse to "result absent" — the caller sees a
// shorter Vec, never an error.

use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use tokio::sync::Semaphore;
use tokio::task::JoinSet;
use tokio::time::timeout;
use tracing::debug;

use crate::proto::LabelledRelayInfo;
use crate::relay_client::{read_message, write_message, RelayClient};

const PROBE_BIDI_STREAM_LIMIT: u32 = 1;
const PROBE_IDLE_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum ProbeMsg<'a> {
    Probe {
        connector_id: &'a str,
        request_id: u64,
    },
}

#[derive(Debug, Deserialize)]
struct ProbeResponse {
    connection_count: u32,
    capacity: u32,
    request_id: u64,
}

#[derive(Debug, Clone)]
pub struct RelayProbeResult {
    pub relay_id: String,
    pub relay_addr: String,
    pub spiffe_id: String,
    pub rtt_ms: u64,
    pub fill_ratio: f64,
    pub score: u64,
}

/// Probe every labelled relay concurrently (capped at `max_concurrent`) and
/// return scored results for every relay that answered with a matching
/// request_id. Unreachable / timed-out / mismatched relays are silently
/// dropped from the output.
pub async fn probe_relays(
    candidates: &[LabelledRelayInfo],
    connector_id: &str,
    cert_pem: &[u8],
    key_pem: &[u8],
    workspace_ca_pem: &[u8],
    intermediate_ca_pem: &[u8],
    max_concurrent: usize,
    probe_timeout: Duration,
) -> Vec<RelayProbeResult> {
    if candidates.is_empty() {
        return Vec::new();
    }
    let permits = std::sync::Arc::new(Semaphore::new(max_concurrent.max(1)));

    let mut set: JoinSet<Option<RelayProbeResult>> = JoinSet::new();
    for candidate in candidates {
        let permits = permits.clone();
        let connector_id = connector_id.to_owned();
        let cert_pem = cert_pem.to_owned();
        let key_pem = key_pem.to_owned();
        let workspace_ca_pem = workspace_ca_pem.to_owned();
        let intermediate_ca_pem = intermediate_ca_pem.to_owned();
        let candidate = candidate.clone();
        set.spawn(async move {
            let _permit = permits.acquire_owned().await.ok()?;
            probe_one(
                &candidate,
                &connector_id,
                &cert_pem,
                &key_pem,
                &workspace_ca_pem,
                &intermediate_ca_pem,
                probe_timeout,
            )
            .await
        });
    }

    let mut results = Vec::with_capacity(candidates.len());
    while let Some(joined) = set.join_next().await {
        if let Ok(Some(result)) = joined {
            results.push(result);
        }
    }
    results
}

async fn probe_one(
    candidate: &LabelledRelayInfo,
    connector_id: &str,
    cert_pem: &[u8],
    key_pem: &[u8],
    workspace_ca_pem: &[u8],
    intermediate_ca_pem: &[u8],
    probe_timeout: Duration,
) -> Option<RelayProbeResult> {
    let socket_addr = tokio::net::lookup_host(&candidate.relay_addr)
        .await
        .ok()?
        .next()?;

    let t0 = Instant::now();

    // RelayClient::connect performs the full mTLS handshake including the
    // exact-SPIFFE peer verification — a mismatched relay cert fails here.
    let client = match timeout(
        probe_timeout,
        RelayClient::connect(
            socket_addr,
            &candidate.spiffe_id,
            cert_pem,
            key_pem,
            workspace_ca_pem,
            intermediate_ca_pem,
            PROBE_BIDI_STREAM_LIMIT,
            PROBE_IDLE_TIMEOUT,
        ),
    )
    .await
    {
        Ok(Ok(c)) => c,
        Ok(Err(e)) => {
            debug!(relay_id = %candidate.relay_id, error = %e, "Relay probe connect failed");
            return None;
        }
        Err(_) => {
            debug!(relay_id = %candidate.relay_id, "Relay probe connect timed out");
            return None;
        }
    };

    let request_id = new_request_id();
    let response: ProbeResponse = match timeout(
        probe_timeout,
        send_and_recv(&client, connector_id, request_id),
    )
    .await
    {
        Ok(Ok(r)) => r,
        Ok(Err(e)) => {
            debug!(relay_id = %candidate.relay_id, error = %e, "Relay probe exchange failed");
            return None;
        }
        Err(_) => {
            debug!(relay_id = %candidate.relay_id, "Relay probe exchange timed out");
            return None;
        }
    };

    if response.request_id != request_id {
        debug!(
            relay_id = %candidate.relay_id,
            expected = request_id,
            got = response.request_id,
            "Relay probe response request_id mismatch"
        );
        return None;
    }

    let rtt_ms = t0.elapsed().as_millis().min(u128::from(u64::MAX)) as u64;
    let fill_ratio = compute_fill_ratio(response.connection_count, response.capacity);
    let score = compute_score(rtt_ms, fill_ratio);

    Some(RelayProbeResult {
        relay_id: candidate.relay_id.clone(),
        relay_addr: candidate.relay_addr.clone(),
        spiffe_id: candidate.spiffe_id.clone(),
        rtt_ms,
        fill_ratio,
        score,
    })
}

async fn send_and_recv(
    client: &RelayClient,
    connector_id: &str,
    request_id: u64,
) -> anyhow::Result<ProbeResponse> {
    let (mut send, mut recv) = client.connection().open_bi().await?;
    write_message(
        &mut send,
        &ProbeMsg::Probe {
            connector_id,
            request_id,
        },
    )
    .await?;
    read_message::<ProbeResponse>(&mut recv).await
}

fn new_request_id() -> u64 {
    // Per-probe nonce. Uniqueness within a sweep is the only requirement;
    // the relay echoes it and we check exact equality. Nanosecond clock is
    // enough resolution given the relay also rate-limits per-connector.
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos() as u64)
        .unwrap_or(1)
        .max(1)
}

fn compute_fill_ratio(connection_count: u32, capacity: u32) -> f64 {
    if capacity == 0 {
        0.0
    } else {
        f64::from(connection_count) / f64::from(capacity)
    }
}

fn compute_score(rtt_ms: u64, fill_ratio: f64) -> u64 {
    rtt_ms.saturating_add((fill_ratio * 50.0).ceil() as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn score_zero_capacity_is_rtt_only() {
        assert_eq!(compute_fill_ratio(0, 0), 0.0);
        assert_eq!(compute_fill_ratio(5, 0), 0.0);
        assert_eq!(compute_score(42, 0.0), 42);
    }

    #[test]
    fn score_half_full_adds_twenty_five() {
        assert_eq!(compute_fill_ratio(50, 100), 0.5);
        assert_eq!(compute_score(100, 0.5), 125);
    }

    #[test]
    fn score_full_adds_fifty() {
        assert_eq!(compute_fill_ratio(100, 100), 1.0);
        assert_eq!(compute_score(10, 1.0), 60);
    }

    #[test]
    fn score_partial_ratio_rounds_up() {
        // 1/3 capacity = 0.333..., * 50 = 16.66..., ceil = 17
        assert_eq!(compute_score(0, 1.0 / 3.0), 17);
    }

    #[test]
    fn score_orders_ascending_by_combined_metric() {
        let mut entries = vec![
            ("a", compute_score(50, 0.0)),  // 50
            ("b", compute_score(20, 0.8)),  // 20 + 40 = 60
            ("c", compute_score(30, 0.2)),  // 30 + 10 = 40
        ];
        entries.sort_by_key(|(_, s)| *s);
        assert_eq!(entries[0].0, "c");
        assert_eq!(entries[1].0, "a");
        assert_eq!(entries[2].0, "b");
    }

    #[test]
    fn request_id_nonce_is_nonzero() {
        for _ in 0..1000 {
            assert!(new_request_id() > 0);
        }
    }

    #[test]
    fn probe_response_deserializes_relay_json() {
        // Exact byte layout the relay produces (from relay/src/protocol.rs).
        let json = br#"{"connection_count":7,"capacity":100,"request_id":42}"#;
        let resp: ProbeResponse = serde_json::from_slice(json).unwrap();
        assert_eq!(resp.connection_count, 7);
        assert_eq!(resp.capacity, 100);
        assert_eq!(resp.request_id, 42);
    }

    #[test]
    fn probe_request_serializes_to_relay_wire_shape() {
        // Must match HandshakeMsg::Probe variant tag = "probe" with snake_case
        // field names, the form relay/src/protocol.rs decodes.
        let msg = ProbeMsg::Probe {
            connector_id: "abc",
            request_id: 99,
        };
        let v = serde_json::to_value(&msg).unwrap();
        assert_eq!(v["type"], "probe");
        assert_eq!(v["connector_id"], "abc");
        assert_eq!(v["request_id"], 99);
    }
}
