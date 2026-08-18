use std::net::Ipv4Addr;
use std::collections::HashMap;
use std::sync::Once;

use rcgen::{CertificateParams, KeyPair, SanType};

use crate::daemon::{build_transports_by_resource, ordered_connectors_for_entry, resolve_entry_coords};
use crate::grpc::client_v1::{
    AclConnector, AclEntry, AclRemoteNetwork, TransportConnector, TransportRemoteNetwork,
};
use crate::runtime::DeviceInfo;

fn install_crypto_provider() {
    static INSTALL: Once = Once::new();
    INSTALL.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

fn issue_device_cert(spiffe_uri: &str) -> (String, String) {
    let key = KeyPair::generate().unwrap();
    let key_pem = key.serialize_pem();
    let mut params = CertificateParams::default();
    params
        .subject_alt_names
        .push(SanType::URI(spiffe_uri.try_into().unwrap()));
    let cert = params.self_signed(&key).unwrap();
    (cert.pem(), key_pem)
}

fn issue_ca_bundle() -> String {
    let k1 = KeyPair::generate().unwrap();
    let ca1 = CertificateParams::default().self_signed(&k1).unwrap();
    let k2 = KeyPair::generate().unwrap();
    let ca2 = CertificateParams::default().self_signed(&k2).unwrap();
    ca1.pem() + &ca2.pem()
}

fn test_device_info() -> DeviceInfo {
    install_crypto_provider();
    let (cert_pem, key_pem) = issue_device_cert("spiffe://test.example/client/device1");
    DeviceInfo {
        id: "device1".to_string(),
        spiffe_id: "spiffe://test.example/client/device1".to_string(),
        certificate_pem: cert_pem,
        private_key_pem: key_pem,
        ca_cert_pem: issue_ca_bundle(),
        cert_expires_at: i64::MAX,
        hostname: "test-host".to_string(),
        os: "linux".to_string(),
    }
}

// Regression: function signature no longer accepts global relay_addr / relay_spiffe_id.
// Any old call site that passed global relay coords would fail to compile.
#[test]
fn build_transports_empty_inputs_returns_empty_map() {
    install_crypto_provider();
    let device = test_device_info();
    let result = build_transports_by_resource(&[], &[], None, &device, None);
    assert!(result.is_ok());
    assert!(result.unwrap().is_empty());
}

// Gap 4 regression: connector with empty relay_addr must produce a transport
// (direct-only path). Old code used the removed global relay_addr param.
// New code: empty connector.relay_addr → relay = None, no RelayPool created.
#[tokio::test]
async fn connector_without_relay_addr_builds_direct_only_transport() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = AclEntry {
        resource_id: "res1".to_string(),
        address: "10.0.0.1".to_string(),
        port: 80,
        remote_network_id: "rn1".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn1".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(), // empty → direct-only, no RelayPool
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let result = build_transports_by_resource(&[entry], &[rn], None, &device, None);
    assert!(result.is_ok(), "expected Ok, got: {:?}", result.err());
    let map = result.unwrap();
    let key = ("10.0.0.1".parse::<Ipv4Addr>().unwrap(), 80u16);
    assert!(
        map.contains_key(&key),
        "resource 10.0.0.1:80 missing from transport map"
    );
    assert!(
        map[&key].is_some(),
        "transport slot is None — connector is active"
    );
}

// Phase 2 regression: the transport map must carry the resource identity that the
// client asserts on the tunnel handshake. Every other assertion in this file only
// checks that the slot *exists* — so an empty or wrong resource_id would be
// completely invisible without this test.
#[tokio::test]
async fn transport_map_carries_resource_id() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = AclEntry {
        resource_id: "res-identity".to_string(),
        address: "10.0.0.7".to_string(),
        port: 5432,
        remote_network_id: "rn1".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn1".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(),
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let map = build_transports_by_resource(&[entry], &[rn], None, &device, None)
        .expect("build transports");
    let key = ("10.0.0.7".parse::<Ipv4Addr>().unwrap(), 5432u16);
    let target = map[&key]
        .as_ref()
        .expect("transport slot is None — connector is active");
    assert_eq!(
        target.resource_id, "res-identity",
        "resource identity must reach the transport map — it is what the connector authorizes on"
    );
    assert!(
        !target.transports.is_empty(),
        "expected at least one transport for an online connector"
    );
}

// Gap 4 regression: connector with relay_addr+relay_spiffe_id set must build
// a transport without error. Old code read these from removed global params.
// New code reads connector.relay_addr and connector.relay_spiffe_id directly.
#[tokio::test]
async fn connector_with_relay_addr_builds_transport_with_relay() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = AclEntry {
        resource_id: "res2".to_string(),
        address: "10.0.0.2".to_string(),
        port: 443,
        remote_network_id: "rn2".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn2".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn2".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn2".to_string(),
            relay_addr: "127.0.0.1:9093".to_string(),
            relay_spiffe_id: "spiffe://global/relay/relay-a".to_string(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let result = build_transports_by_resource(&[entry], &[rn], None, &device, None);
    assert!(result.is_ok(), "expected Ok, got: {:?}", result.err());
    let map = result.unwrap();
    let key = ("10.0.0.2".parse::<Ipv4Addr>().unwrap(), 443u16);
    assert!(map.contains_key(&key), "resource 10.0.0.2:443 missing");
    assert!(map[&key].is_some(), "transport slot is None unexpectedly");
}

// Gap 4 regression: two connectors in different RNs with DIFFERENT relay_addr
// values must build independently. Old code: both used the same global relay_addr.
// New code: each reads its own connector.relay_addr field.
#[tokio::test]
async fn two_connectors_different_relay_addrs_build_independently() {
    install_crypto_provider();
    let device = test_device_info();

    let entries = vec![
        AclEntry {
            resource_id: "res-a".to_string(),
            address: "10.1.0.1".to_string(),
            port: 80,
            remote_network_id: "rn-a".to_string(),
            protocol: "tcp".to_string(),
            ..Default::default()
        },
        AclEntry {
            resource_id: "res-b".to_string(),
            address: "10.2.0.1".to_string(),
            port: 80,
            remote_network_id: "rn-b".to_string(),
            protocol: "tcp".to_string(),
            ..Default::default()
        },
    ];
    let remote_networks = vec![
        AclRemoteNetwork {
            remote_network_id: "rn-a".to_string(),
            connectors: vec![AclConnector {
                connector_id: "conn-a".to_string(),
                connector_tunnel_addr: "127.0.0.1:9092".to_string(),
                connector_spiffe: "spiffe://test.example/connector/conn-a".to_string(),
                relay_addr: "127.0.0.1:9093".to_string(), // conn-a has relay
                relay_spiffe_id: "spiffe://global/relay/relay-a".to_string(),
                ..Default::default()
            }],
            ..Default::default()
        },
        AclRemoteNetwork {
            remote_network_id: "rn-b".to_string(),
            connectors: vec![AclConnector {
                connector_id: "conn-b".to_string(),
                connector_tunnel_addr: "127.0.0.1:9092".to_string(),
                connector_spiffe: "spiffe://test.example/connector/conn-b".to_string(),
                relay_addr: String::new(), // conn-b is direct-only
                relay_spiffe_id: String::new(),
                ..Default::default()
            }],
            ..Default::default()
        },
    ];

    let result = build_transports_by_resource(&entries, &remote_networks, None, &device, None);
    assert!(result.is_ok(), "expected Ok, got: {:?}", result.err());
    let map = result.unwrap();

    let key_a = ("10.1.0.1".parse::<Ipv4Addr>().unwrap(), 80u16);
    let key_b = ("10.2.0.1".parse::<Ipv4Addr>().unwrap(), 80u16);
    assert!(map.contains_key(&key_a), "res-a missing from transport map");
    assert!(map.contains_key(&key_b), "res-b missing from transport map");
    assert!(map[&key_a].is_some(), "res-a: transport slot is None");
    assert!(map[&key_b].is_some(), "res-b: transport slot is None");
}

// Shield-routed resources should use the connector currently holding the Shield
// even when that connector is not the first active connector in the RN list.
#[test]
fn shield_resource_uses_preferred_connector_id() {
    let entry = AclEntry {
        resource_id: "res-shield".to_string(),
        address: "10.3.0.1".to_string(),
        port: 8443,
        remote_network_id: "rn-shield".to_string(),
        protocol: "tcp".to_string(),
        route_type: "shield".to_string(),
        shield_id: "shield-1".to_string(),
        preferred_connector_id: "conn-holder".to_string(),
        ..Default::default()
    };

    let rn = AclRemoteNetwork {
        remote_network_id: "rn-shield".to_string(),
        connectors: vec![
            AclConnector {
                connector_id: "conn-other".to_string(),
                connector_tunnel_addr: "not-a-valid-socket-address".to_string(),
                connector_spiffe: "spiffe://test.example/connector/conn-other".to_string(),
                ..Default::default()
            },
            AclConnector {
                connector_id: "conn-holder".to_string(),
                connector_tunnel_addr: "127.0.0.1:9092".to_string(),
                connector_spiffe: "spiffe://test.example/connector/conn-holder".to_string(),
                ..Default::default()
            },
        ],
        ..Default::default()
    };

    let connector = ordered_connectors_for_entry(&entry, &rn)
        .into_iter()
        .next()
        .expect("connector selected");
    assert_eq!(connector.connector_id, "conn-holder");
}

// ── Track B routing decision (resolve_entry_coords) ─────────────────────────
// Pure: no certs/network. Verifies transport-plane preference, ACL fallback,
// and preferred-connector ordering.

fn tp_entry(rn: &str, preferred: &str) -> AclEntry {
    AclEntry {
        remote_network_id: rn.to_string(),
        preferred_connector_id: preferred.to_string(),
        ..Default::default()
    }
}

fn tp_acl_conn(id: &str, relay_addr: &str) -> AclConnector {
    AclConnector {
        connector_id: id.to_string(),
        connector_tunnel_addr: "10.0.0.1:9092".to_string(),
        connector_spiffe: format!("spiffe://td/connector/{id}"),
        relay_addr: relay_addr.to_string(),
        relay_spiffe_id: "spiffe://zecurity.in/relay/r".to_string(),
    }
}

fn tp_transport_conn(id: &str, relay_addr: &str) -> TransportConnector {
    TransportConnector {
        connector_id: id.to_string(),
        connector_tunnel_addr: "10.0.0.1:9092".to_string(),
        connector_spiffe: format!("spiffe://td/connector/{id}"),
        relay_addr: relay_addr.to_string(),
        relay_spiffe_id: "spiffe://zecurity.in/relay/r".to_string(),
    }
}

#[test]
fn resolve_prefers_transport_plane_when_rn_present() {
    let e = tp_entry("rn1", "");
    let acl_rn = AclRemoteNetwork {
        remote_network_id: "rn1".into(),
        name: String::new(),
        connectors: vec![tp_acl_conn("c1", "relay-old:9093")],
    };
    let tp_rn = TransportRemoteNetwork {
        remote_network_id: "rn1".into(),
        connectors: vec![tp_transport_conn("c1", "relay-new:9093")],
    };
    let rn_by_id = HashMap::from([("rn1", &acl_rn)]);
    let trn_by_id = HashMap::from([("rn1", &tp_rn)]);

    let coords = resolve_entry_coords(&e, &rn_by_id, &trn_by_id);
    assert_eq!(coords.len(), 1);
    assert_eq!(coords[0].relay_addr, "relay-new:9093");
}

#[test]
fn resolve_falls_back_to_acl_when_transport_lacks_rn() {
    let e = tp_entry("rn1", "");
    let acl_rn = AclRemoteNetwork {
        remote_network_id: "rn1".into(),
        name: String::new(),
        connectors: vec![tp_acl_conn("c1", "relay-old:9093")],
    };
    let rn_by_id = HashMap::from([("rn1", &acl_rn)]);
    let trn_by_id: HashMap<&str, &TransportRemoteNetwork> = HashMap::new();

    let coords = resolve_entry_coords(&e, &rn_by_id, &trn_by_id);
    assert_eq!(coords.len(), 1);
    assert_eq!(coords[0].relay_addr, "relay-old:9093");
}

#[test]
fn resolve_empty_when_neither_plane_has_rn() {
    let e = tp_entry("rn-missing", "");
    let rn_by_id: HashMap<&str, &AclRemoteNetwork> = HashMap::new();
    let trn_by_id: HashMap<&str, &TransportRemoteNetwork> = HashMap::new();
    assert!(resolve_entry_coords(&e, &rn_by_id, &trn_by_id).is_empty());
}

#[test]
fn resolve_honors_preferred_connector_in_transport() {
    let e = tp_entry("rn1", "c2");
    let tp_rn = TransportRemoteNetwork {
        remote_network_id: "rn1".into(),
        connectors: vec![
            tp_transport_conn("c1", "r1:9093"),
            tp_transport_conn("c2", "r2:9093"),
        ],
    };
    let rn_by_id: HashMap<&str, &AclRemoteNetwork> = HashMap::new();
    let trn_by_id = HashMap::from([("rn1", &tp_rn)]);

    let coords = resolve_entry_coords(&e, &rn_by_id, &trn_by_id);
    assert_eq!(coords.len(), 2);
    assert_eq!(coords[0].connector_id, "c2");
}

/// Sprint 16 Phase 9.4b: a name-addressed resource must be keyed on the SYNTHETIC
/// IP the registry bound to its hostname, not dropped.
///
/// Before this phase the builder did `filter_map(|e| e.address.parse().ok()?)`,
/// which silently discarded every entry without a pinned IPv4 address — that is
/// why an FQDN resource could not be expressed at all, and it is the line this
/// test guards against coming back.
#[tokio::test]
async fn named_resource_is_keyed_on_its_synthetic_ip() {
    install_crypto_provider();
    let device = test_device_info();

    let entry = AclEntry {
        resource_id: "res-named".to_string(),
        address: String::new(), // no pinned IP — this is the whole point
        hostname: "db.internal".to_string(),
        port: 5432,
        remote_network_id: "rn1".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn1".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(),
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let mut reg = crate::registry::BindingRegistry::new(crate::registry::Net::new(
        "100.64.0.0".parse().unwrap(),
        22,
    ));
    let synth = reg.bind("db.internal", "res-named", 0).unwrap();

    let map = build_transports_by_resource(&[entry], &[rn], None, &device, Some(&reg))
        .expect("build transports");

    let target = map
        .get(&(synth, 5432))
        .expect("must be keyed on the synthetic IP, not dropped for lacking an address")
        .as_ref()
        .expect("connector is reachable, so the slot must be populated");
    assert_eq!(target.resource_id, "res-named");
    assert!(
        target.synthetic,
        "must be flagged synthetic so net_stack sends an empty destination — \
         sending the synthetic IP would be denied as destination_mismatch"
    );
}

/// Without a registry (or with a hostname it never bound) a name-addressed entry
/// has no routable address, so it must be omitted entirely. No key means no
/// listener and no transport — fail closed, never a passthrough.
#[tokio::test]
async fn unbound_named_resource_is_omitted_not_guessed() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = AclEntry {
        resource_id: "res-unbound".to_string(),
        address: String::new(),
        hostname: "nowhere.internal".to_string(),
        port: 443,
        remote_network_id: "rn1".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn1".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(),
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let map = build_transports_by_resource(&[entry], &[rn], None, &device, None)
        .expect("build transports");
    assert!(
        map.is_empty(),
        "an unbound name-addressed resource must not appear under any key"
    );
}

/// Regression: pinned-IP resources are untouched by the synthetic path and must
/// still be flagged non-synthetic, so they keep sending their destination.
#[tokio::test]
async fn pinned_resource_is_not_flagged_synthetic() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = AclEntry {
        resource_id: "res-pinned".to_string(),
        address: "10.0.0.7".to_string(),
        port: 5432,
        remote_network_id: "rn1".to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn1".to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(),
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };

    let map = build_transports_by_resource(&[entry], &[rn], None, &device, None)
        .expect("build transports");
    let key = ("10.0.0.7".parse::<Ipv4Addr>().unwrap(), 5432u16);
    let target = map[&key].as_ref().expect("pinned resource must be present");
    assert!(!target.synthetic);
}

// ── The three-state transport map (ADR-009), asserted at last ────────────────
//
// Phase 9's task list carries this as INHERITED DEBT from Phase 2: the three
// states were documented and relied upon but never asserted, and "losing them
// converts a fail-closed case into a passthrough, which is a security
// regression". Phase 9.4b rewrote the exact match arms these states feed, so the
// debt is discharged here.
//
// Per ADR-009:
//   Some(Some(target)) → managed, connector online  → tunnel via QUIC
//   Some(None)         → managed, connector offline → FAIL CLOSED (RST)
//   absent             → not a managed flow         → fail closed; bypass is a
//                                                     KERNEL property (never
//                                                     marked, never reaches TUN)

fn three_state_entry(rn: &str, addr: &str, port: u32) -> AclEntry {
    AclEntry {
        resource_id: format!("res-{addr}-{port}"),
        address: addr.to_string(),
        port,
        remote_network_id: rn.to_string(),
        protocol: "tcp".to_string(),
        ..Default::default()
    }
}

fn three_state_rn(rn: &str) -> AclRemoteNetwork {
    AclRemoteNetwork {
        remote_network_id: rn.to_string(),
        connectors: vec![AclConnector {
            connector_id: "conn1".to_string(),
            connector_tunnel_addr: "127.0.0.1:9092".to_string(),
            connector_spiffe: "spiffe://test.example/connector/conn1".to_string(),
            relay_addr: String::new(),
            relay_spiffe_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    }
}

/// State 1 — connector reachable: the slot is populated and carries transports.
#[tokio::test]
async fn state1_connector_online_yields_a_populated_target() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = three_state_entry("rn1", "10.0.0.7", 5432);
    let map = build_transports_by_resource(&[entry], &[three_state_rn("rn1")], None, &device, None)
        .expect("build transports");

    let key = ("10.0.0.7".parse::<Ipv4Addr>().unwrap(), 5432u16);
    let target = map
        .get(&key)
        .expect("managed flow must be present in the map")
        .as_ref()
        .expect("state 1: connector online → Some(Some(target))");
    assert!(
        !target.transports.is_empty(),
        "state 1 must carry at least one transport, or net_stack cannot tunnel"
    );
}

/// State 2 — the security-critical one. A managed resource whose remote network
/// has no reachable connector must be present-but-empty (`Some(None)`), which
/// net_stack treats as FAIL CLOSED.
///
/// If this ever became `absent` instead, the distinction between "managed but
/// unreachable" and "not managed" would collapse — and since bypass is a kernel
/// property keyed on the nft rules (which ARE installed for this flow), the
/// packet would still be captured while nothing knew it was managed.
#[tokio::test]
async fn state2_connector_offline_is_present_but_empty_not_absent() {
    install_crypto_provider();
    let device = test_device_info();
    // The entry's remote network is not in the snapshot → no connector coords.
    let entry = three_state_entry("rn-missing", "10.0.0.8", 443);
    let map = build_transports_by_resource(&[entry], &[three_state_rn("rn1")], None, &device, None)
        .expect("build transports");

    let key = ("10.0.0.8".parse::<Ipv4Addr>().unwrap(), 443u16);
    assert!(
        map.contains_key(&key),
        "state 2 must be PRESENT — dropping the key would make a managed \
         resource indistinguishable from unmanaged traffic"
    );
    assert!(
        map[&key].is_none(),
        "state 2 must be Some(None): managed, connector offline → fail closed"
    );
}

/// State 3 — a flow that is not a managed resource has no key at all. net_stack
/// fails closed on it; genuine bypass never reaches net_stack because the kernel
/// never marks it (ADR-009).
#[tokio::test]
async fn state3_unmanaged_flow_is_absent_from_the_map() {
    install_crypto_provider();
    let device = test_device_info();
    let entry = three_state_entry("rn1", "10.0.0.7", 5432);
    let map = build_transports_by_resource(&[entry], &[three_state_rn("rn1")], None, &device, None)
        .expect("build transports");

    // Same IP, a port nobody granted.
    let other_port = ("10.0.0.7".parse::<Ipv4Addr>().unwrap(), 9999u16);
    assert!(
        !map.contains_key(&other_port),
        "a non-ACL port must not appear in the map"
    );
    // A different IP entirely.
    let other_ip = ("10.0.0.99".parse::<Ipv4Addr>().unwrap(), 5432u16);
    assert!(
        !map.contains_key(&other_ip),
        "an unmanaged IP must be absent"
    );
}

/// The three states must be mutually exclusive for a single flow — the property
/// that makes net_stack's match arms a decision rather than a guess.
#[tokio::test]
async fn the_three_states_are_distinguishable_in_one_map() {
    install_crypto_provider();
    let device = test_device_info();
    let online = three_state_entry("rn1", "10.0.0.7", 5432); // state 1
    let offline = three_state_entry("rn-missing", "10.0.0.8", 443); // state 2
    let map = build_transports_by_resource(
        &[online, offline],
        &[three_state_rn("rn1")],
        None,
        &device,
        None,
    )
    .expect("build transports");

    let k1 = ("10.0.0.7".parse::<Ipv4Addr>().unwrap(), 5432u16);
    let k2 = ("10.0.0.8".parse::<Ipv4Addr>().unwrap(), 443u16);
    let k3 = ("10.0.0.9".parse::<Ipv4Addr>().unwrap(), 22u16); // state 3

    assert!(map.get(&k1).is_some_and(|s| s.is_some()), "state 1");
    assert!(map.get(&k2).is_some_and(|s| s.is_none()), "state 2");
    assert!(map.get(&k3).is_none(), "state 3");
}
