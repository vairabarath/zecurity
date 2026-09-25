//! Sprint 20 G-2a: after ONE renewal is published, every connector TLS
//! consumer observes the same certificate serial.

use std::sync::Arc;
use std::time::Duration;

use parking_lot::Mutex;
use tokio_rustls::TlsAcceptor;
use tonic::transport::Channel;

use crate::agent_server::ShieldRegistry;
use crate::quic_listener::{quic_server_config_for, spawn_quic_config_updates};
use crate::relay_handler::build_inner_tls_server_config;
use crate::test_support::{
    leaf_serial, quic_server_serial, selector_config, tls_handshake_serial, TestPki, CONNECTOR_ID,
};
use crate::tls::server_cfg::build_device_tunnel_tls_dynamic;

fn first_serial(pem: &[u8]) -> String {
    let chain: Vec<_> = rustls_pemfile::certs(&mut &pem[..])
        .collect::<Result<_, _>>()
        .unwrap();
    leaf_serial(&chain[0])
}

async fn wait_until(mut cond: impl FnMut() -> bool, what: &str) {
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    while !cond() {
        assert!(
            tokio::time::Instant::now() < deadline,
            "timed out waiting for {what}"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

#[tokio::test]
async fn every_consumer_observes_the_same_serial_after_publication() {
    let pki = TestPki::new();
    let (dir, holder) = pki.holder(-60, 3600);
    let original = holder.current().serial_hex.clone();

    // Device TLS (:9092 TCP) and Relay inner TLS — holder-backed resolvers.
    let device_tls = TlsAcceptor::from(Arc::new(
        build_device_tunnel_tls_dynamic(holder.clone()).unwrap(),
    ));
    let (inner, _) = build_inner_tls_server_config(holder.clone()).unwrap();
    let relay_inner_tls = TlsAcceptor::from(Arc::new(inner));

    // Device QUIC (:9092 UDP) — set_server_config watcher.
    let quic = quinn::Endpoint::server(
        quic_server_config_for(&holder.current().store).unwrap(),
        "127.0.0.1:0".parse().unwrap(),
    )
    .unwrap();
    let quic_addr = quic.local_addr().unwrap();
    let _quic_updates = spawn_quic_config_updates(quic.clone(), holder.subscribe());
    {
        let quic = quic.clone();
        tokio::spawn(async move {
            while let Some(incoming) = quic.accept().await {
                tokio::spawn(async move {
                    if let Ok(conn) = incoming.await {
                        let _ = conn.closed().await;
                    }
                });
            }
        });
    }
    let quic_client = pki.quic_client();

    // Relay dials/probes — identity read at dial time.
    let selector = selector_config(holder.clone(), dir.path());

    // Shield-proxy controller channel — rebuilt on publish.
    let (ack_tx, _ack_rx) = tokio::sync::mpsc::channel(8);
    let registry = ShieldRegistry::new(
        Channel::from_static("http://127.0.0.1:1").connect_lazy(),
        "ws-test.zecurity.in".to_string(),
        CONNECTOR_ID.to_string(),
        ack_tx,
        Arc::new(crate::policy::PolicyCache::new()),
    );
    let proxy_built_with = Arc::new(Mutex::new(Vec::<String>::new()));
    let recorder = proxy_built_with.clone();
    let _refresh = registry.spawn_controller_channel_refresh(holder.clone(), move |material| {
        recorder.lock().push(material.serial_hex.clone());
        async move { Ok(Channel::from_static("http://127.0.0.1:1").connect_lazy()) }
    });

    // --- publish ONE renewal ---
    let renewed = pki.renew(&holder, 7200);
    let want = renewed.serial_hex.clone();
    assert_ne!(want, original);

    // Control stream: run_once connects with holder.current().store.
    let control_stream = first_serial(&holder.current().store.cert_pem);
    // Device TLS + Relay inner TLS: next handshake.
    let device = tls_handshake_serial(&device_tls, pki.client_tls_config(false)).await;
    let relay_inner = tls_handshake_serial(&relay_inner_tls, pki.client_tls_config(true)).await;
    // Relay dials.
    let relay_dial = first_serial(&selector.current_identity().cert_pem);
    // QUIC: the watcher swaps asynchronously; wait for a new handshake to show it.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
    let quic_serial = loop {
        let conn = quic_client
            .connect(quic_addr, "localhost")
            .unwrap()
            .await
            .unwrap();
        let serial = quic_server_serial(&conn);
        if serial == want || tokio::time::Instant::now() >= deadline {
            break serial;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    };
    // Shield-proxy: the refresh task rebuilds asynchronously.
    wait_until(
        || registry.controller_channel_swaps() == 1,
        "Shield-proxy channel rebuild",
    )
    .await;
    let shield_proxy = proxy_built_with.lock()[0].clone();

    for (consumer, seen) in [
        ("control stream", &control_stream),
        ("device TLS", &device),
        ("device QUIC", &quic_serial),
        ("relay inner TLS", &relay_inner),
        ("relay dial", &relay_dial),
        ("shield-proxy channel", &shield_proxy),
    ] {
        assert_eq!(
            seen, &want,
            "{consumer} did not switch to the renewed certificate"
        );
    }
}
