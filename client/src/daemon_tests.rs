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
    let result = build_transports_by_resource(&[], &[], None, &device);
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

    let result = build_transports_by_resource(&[entry], &[rn], None, &device);
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

    let map =
        build_transports_by_resource(&[entry], &[rn], None, &device).expect("build transports");
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

    let result = build_transports_by_resource(&[entry], &[rn], None, &device);
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

    let result = build_transports_by_resource(&entries, &remote_networks, None, &device);
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
