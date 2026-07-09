// Sprint 11 Phase 3 — Scenario 4 (TEAM-E5): probe security.
//
// Exercises `relay_probe::probe_relays` against three in-process test
// relays running on real QUIC mTLS:
//
//   4a — wrong `request_id` echoed → probe result dropped.
//   4b — relay cert SPIFFE ≠ `LabelledRelayInfo.spiffe_id` → mTLS verifier
//        rejects the connection → result dropped.
//   4c — relay accepts but never responds → result dropped (timeout path).
//
// All three call into `probe_relays` directly (no selector / no register).

mod common;

use std::time::Duration;

use common::{make_test_certs, ProbeBehaviour, ProbeRelay};
use zecurity_connector::proto::{LabelledRelayInfo, RelayCapacityLabel};
use zecurity_connector::relay_probe::probe_relays;

#[tokio::test]
async fn wrong_request_id_is_discarded() {
    let certs = make_test_certs(1);
    let relay = ProbeRelay::spawn(
        &certs.relays[0],
        certs.workspace_ca_der.clone(),
        ProbeBehaviour::WrongRequestId,
    )
    .await
    .expect("spawn probe relay");

    let candidates = vec![relay.info(RelayCapacityLabel::RelayCapacityHigh)];
    let results = probe_relays(
        &candidates,
        &certs.connector_id,
        &certs.connector_cert_pem,
        &certs.connector_key_pem,
        &certs.workspace_ca_pem,
        &certs.workspace_ca_pem,
        4,
        Duration::from_secs(3),
    )
    .await;

    assert!(
        results.is_empty(),
        "probe must reject mismatched request_id, got: {:?}",
        results
    );
}

#[tokio::test]
async fn wrong_spiffe_is_unreachable() {
    let certs = make_test_certs(1);
    let relay = ProbeRelay::spawn(
        &certs.relays[0],
        certs.workspace_ca_der.clone(),
        ProbeBehaviour::EchoCorrectly {
            connection_count: 0,
            capacity: 100,
        },
    )
    .await
    .expect("spawn probe relay");

    // Build the LabelledRelayInfo with a wrong SPIFFE URI — pointing to a
    // different relay UUID than the cert actually carries. The connector's
    // ExactRelaySpiffeVerifier must reject the TLS handshake.
    let real_info = relay.info(RelayCapacityLabel::RelayCapacityHigh);
    let mismatched_spiffe = format!("spiffe://zecurity.in/relay/{}", uuid::Uuid::new_v4());
    let candidates = vec![LabelledRelayInfo {
        spiffe_id: mismatched_spiffe,
        ..real_info
    }];

    let results = probe_relays(
        &candidates,
        &certs.connector_id,
        &certs.connector_cert_pem,
        &certs.connector_key_pem,
        &certs.workspace_ca_pem,
        &certs.workspace_ca_pem,
        4,
        Duration::from_secs(3),
    )
    .await;

    assert!(
        results.is_empty(),
        "probe must reject relay with mismatched SPIFFE peer cert, got: {:?}",
        results
    );
}

#[tokio::test]
async fn no_response_is_dropped() {
    let certs = make_test_certs(1);
    let relay = ProbeRelay::spawn(
        &certs.relays[0],
        certs.workspace_ca_der.clone(),
        ProbeBehaviour::NoResponse,
    )
    .await
    .expect("spawn probe relay");

    let candidates = vec![relay.info(RelayCapacityLabel::RelayCapacityHigh)];
    let results = probe_relays(
        &candidates,
        &certs.connector_id,
        &certs.connector_cert_pem,
        &certs.connector_key_pem,
        &certs.workspace_ca_pem,
        &certs.workspace_ca_pem,
        4,
        Duration::from_secs(2),
    )
    .await;

    assert!(
        results.is_empty(),
        "probe must drop relay that never responds, got: {:?}",
        results
    );
}

#[tokio::test]
async fn correct_probe_succeeds_baseline() {
    // Sanity check: with everything correct, probe_relays returns a result.
    // Guards against the three negative tests above passing because the
    // probe is always broken.
    let certs = make_test_certs(1);
    let relay = ProbeRelay::spawn(
        &certs.relays[0],
        certs.workspace_ca_der.clone(),
        ProbeBehaviour::EchoCorrectly {
            connection_count: 7,
            capacity: 100,
        },
    )
    .await
    .expect("spawn probe relay");

    let candidates = vec![relay.info(RelayCapacityLabel::RelayCapacityHigh)];
    let results = probe_relays(
        &candidates,
        &certs.connector_id,
        &certs.connector_cert_pem,
        &certs.connector_key_pem,
        &certs.workspace_ca_pem,
        &certs.workspace_ca_pem,
        4,
        Duration::from_secs(5),
    )
    .await;

    assert_eq!(results.len(), 1, "baseline probe must succeed");
    assert_eq!(results[0].relay_id, relay.relay_id);
    // fill_ratio removed: relay load is controller-owned; probes are RTT-only (ADR-016)
}
