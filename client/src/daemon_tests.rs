use std::net::Ipv4Addr;
use std::collections::HashMap;
use std::sync::Once;

use rcgen::{CertificateParams, KeyPair, SanType};

use crate::daemon::{
    build_transports_by_resource, effective_config, ordered_connectors_for_entry,
    resolve_entry_coords,
};
use crate::grpc::client_v1::{
    AclConnector, AclEntry, AclRemoteNetwork, AclSnapshot, TransportConnector,
    TransportRemoteNetwork, TransportSnapshot,
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
        tpm_key_material: None,
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

// ── Fix 01 Phase 1: effective_config ───────────────────────────────────────

fn fake_device_info() -> DeviceInfo {
    DeviceInfo {
        id: "device1".to_string(),
        spiffe_id: "spiffe://test.example/client/device1".to_string(),
        certificate_pem: "-----BEGIN CERTIFICATE-----\nFAKE_CERT\n-----END CERTIFICATE-----\n".to_string(),
        private_key_pem: "-----BEGIN PRIVATE KEY-----\nSUPER_SECRET_KEY\n-----END PRIVATE KEY-----\n".to_string(),
        tpm_key_material: None,
        ca_cert_pem: "-----BEGIN CERTIFICATE-----\nFAKE_CA\n-----END CERTIFICATE-----\n".to_string(),
        cert_expires_at: i64::MAX,
        hostname: "test-host".to_string(),
        os: "linux".to_string(),
    }
}

#[test]
fn effective_config_metadata_differences_equal() {
    let device = fake_device_info();
    let acl1 = AclSnapshot {
        version: 1,
        workspace_id: "ws-1".to_string(),
        generated_at: 1000,
        relay_addr: "global-relay-1:9093".to_string(),
        relay_spiffe_id: "spiffe://global/relay/1".to_string(),
        entries: vec![AclEntry {
            resource_id: "res-1".to_string(),
            name: "service-alpha".to_string(),
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            preferred_connector_id: String::new(),
            ..Default::default()
        }],
        remote_networks: vec![AclRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            name: "rn-name-1".to_string(),
            connectors: vec![tp_acl_conn("c1", "relay-c1:9093")],
        }],
    };

    let acl2 = AclSnapshot {
        version: 2,
        workspace_id: "ws-2".to_string(),
        generated_at: 2000,
        relay_addr: "global-relay-2:9093".to_string(),
        relay_spiffe_id: "spiffe://global/relay/2".to_string(),
        entries: vec![AclEntry {
            resource_id: "res-1".to_string(),
            name: "service-alpha-renamed".to_string(),
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            preferred_connector_id: String::new(),
            ..Default::default()
        }],
        remote_networks: vec![AclRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            name: "rn-name-2".to_string(),
            connectors: vec![tp_acl_conn("c1", "relay-c1:9093")],
        }],
    };

    let cfg1 = effective_config(&acl1, None, &device);
    let cfg2 = effective_config(&acl2, None, &device);
    assert_eq!(cfg1, cfg2);
}

#[test]
fn effective_config_shuffled_non_preferred_connectors_equal() {
    let device = fake_device_info();
    let acl = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            preferred_connector_id: String::new(),
            ..Default::default()
        }],
        ..Default::default()
    };
    let tp1 = TransportSnapshot {
        version: 1,
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![
                tp_transport_conn("c1", "relay-1:9093"),
                tp_transport_conn("c2", "relay-2:9093"),
                tp_transport_conn("c3", "relay-3:9093"),
            ],
        }],
    };
    let tp2 = TransportSnapshot {
        version: 2,
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![
                tp_transport_conn("c3", "relay-3:9093"),
                tp_transport_conn("c1", "relay-1:9093"),
                tp_transport_conn("c2", "relay-2:9093"),
            ],
        }],
    };

    let cfg1 = effective_config(&acl, Some(&tp1), &device);
    let cfg2 = effective_config(&acl, Some(&tp2), &device);
    assert_eq!(cfg1, cfg2);
}

#[test]
fn effective_config_preferred_connector_order_and_value_change() {
    let device = fake_device_info();
    let acl_pref_c2 = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            preferred_connector_id: "c2".to_string(),
            ..Default::default()
        }],
        ..Default::default()
    };

    // c2 is at position 1 in tp1, and position 0 in tp2
    let tp1 = TransportSnapshot {
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![
                tp_transport_conn("c1", "relay-1:9093"),
                tp_transport_conn("c2", "relay-2:9093"),
                tp_transport_conn("c3", "relay-3:9093"),
            ],
        }],
        ..Default::default()
    };
    let tp2 = TransportSnapshot {
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![
                tp_transport_conn("c2", "relay-2:9093"),
                tp_transport_conn("c3", "relay-3:9093"),
                tp_transport_conn("c1", "relay-1:9093"),
            ],
        }],
        ..Default::default()
    };

    let cfg1 = effective_config(&acl_pref_c2, Some(&tp1), &device);
    let cfg2 = effective_config(&acl_pref_c2, Some(&tp2), &device);
    assert_eq!(
        cfg1, cfg2,
        "preferred connector order position in input should not change effective config"
    );

    let acl_pref_c1 = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            preferred_connector_id: "c1".to_string(),
            ..Default::default()
        }],
        ..Default::default()
    };
    let cfg3 = effective_config(&acl_pref_c1, Some(&tp1), &device);
    assert_ne!(
        cfg1, cfg3,
        "changing preferred_connector_id value must change effective config"
    );
}

#[test]
fn effective_config_entry_added_or_removed() {
    let device = fake_device_info();
    let e1 = AclEntry {
        address: "10.0.0.1".to_string(),
        port: 80,
        protocol: "tcp".to_string(),
        allowed_spiffe_ids: vec![device.spiffe_id.clone()],
        remote_network_id: "rn-1".to_string(),
        ..Default::default()
    };
    let e2 = AclEntry {
        address: "10.0.0.2".to_string(),
        port: 443,
        protocol: "tcp".to_string(),
        allowed_spiffe_ids: vec![device.spiffe_id.clone()],
        remote_network_id: "rn-1".to_string(),
        ..Default::default()
    };
    let rn = AclRemoteNetwork {
        remote_network_id: "rn-1".to_string(),
        connectors: vec![tp_acl_conn("c1", "r:9093")],
        ..Default::default()
    };

    let acl_base = AclSnapshot {
        entries: vec![e1.clone()],
        remote_networks: vec![rn.clone()],
        ..Default::default()
    };
    let acl_added = AclSnapshot {
        entries: vec![e1.clone(), e2],
        remote_networks: vec![rn.clone()],
        ..Default::default()
    };
    let acl_removed = AclSnapshot {
        entries: vec![],
        remote_networks: vec![rn],
        ..Default::default()
    };

    let cfg_base = effective_config(&acl_base, None, &device);
    let cfg_added = effective_config(&acl_added, None, &device);
    let cfg_removed = effective_config(&acl_removed, None, &device);

    assert_ne!(cfg_base, cfg_added);
    assert_ne!(cfg_base, cfg_removed);
}

#[test]
fn effective_config_connector_relay_or_tunnel_addr_changed() {
    let device = fake_device_info();
    let acl = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            ..Default::default()
        }],
        ..Default::default()
    };
    let tp_base = TransportSnapshot {
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![TransportConnector {
                connector_id: "c1".to_string(),
                connector_tunnel_addr: "10.0.0.1:9092".to_string(),
                connector_spiffe: "spiffe://td/connector/c1".to_string(),
                relay_addr: "relay-a:9093".to_string(),
                relay_spiffe_id: "spiffe://r".to_string(),
            }],
        }],
        ..Default::default()
    };
    let tp_relay_changed = TransportSnapshot {
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![TransportConnector {
                connector_id: "c1".to_string(),
                connector_tunnel_addr: "10.0.0.1:9092".to_string(),
                connector_spiffe: "spiffe://td/connector/c1".to_string(),
                relay_addr: "relay-b:9093".to_string(), // changed
                relay_spiffe_id: "spiffe://r".to_string(),
            }],
        }],
        ..Default::default()
    };
    let tp_tunnel_changed = TransportSnapshot {
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![TransportConnector {
                connector_id: "c1".to_string(),
                connector_tunnel_addr: "10.0.0.2:9092".to_string(), // changed
                connector_spiffe: "spiffe://td/connector/c1".to_string(),
                relay_addr: "relay-a:9093".to_string(),
                relay_spiffe_id: "spiffe://r".to_string(),
            }],
        }],
        ..Default::default()
    };

    let cfg_base = effective_config(&acl, Some(&tp_base), &device);
    let cfg_relay = effective_config(&acl, Some(&tp_relay_changed), &device);
    let cfg_tunnel = effective_config(&acl, Some(&tp_tunnel_changed), &device);

    assert_ne!(cfg_base, cfg_relay);
    assert_ne!(cfg_base, cfg_tunnel);
}

#[test]
fn effective_config_renewal_lifecycle() {
    let device = fake_device_info();
    let acl_vn = AclSnapshot {
        version: 10,
        generated_at: 1000,
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            ..Default::default()
        }],
        remote_networks: vec![AclRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![tp_acl_conn("c1", "relay:9093")],
            ..Default::default()
        }],
        ..Default::default()
    };
    let tp_vn = TransportSnapshot {
        version: 10,
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![tp_transport_conn("c1", "relay:9093")],
        }],
    };

    // vN+1: connector c1 is absent from the transport RN during renewal transition
    let acl_vn_plus_1 = AclSnapshot {
        version: 11,
        generated_at: 1060,
        entries: acl_vn.entries.clone(),
        remote_networks: acl_vn.remote_networks.clone(),
        ..Default::default()
    };
    let tp_vn_plus_1 = TransportSnapshot {
        version: 11,
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![], // c1 absent
        }],
    };

    // vN+2: connector c1 restored, version is higher, generated_at is different
    let acl_vn_plus_2 = AclSnapshot {
        version: 12,
        generated_at: 1120,
        entries: acl_vn.entries.clone(),
        remote_networks: acl_vn.remote_networks.clone(),
        ..Default::default()
    };
    let tp_vn_plus_2 = TransportSnapshot {
        version: 12,
        remote_networks: vec![TransportRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![tp_transport_conn("c1", "relay:9093")],
        }],
    };

    let cfg_vn = effective_config(&acl_vn, Some(&tp_vn), &device);
    let cfg_vn_plus_1 = effective_config(&acl_vn_plus_1, Some(&tp_vn_plus_1), &device);
    let cfg_vn_plus_2 = effective_config(&acl_vn_plus_2, Some(&tp_vn_plus_2), &device);

    assert_ne!(cfg_vn, cfg_vn_plus_1);
    assert_eq!(cfg_vn, cfg_vn_plus_2);
}

#[test]
fn effective_config_excludes_unallowed_spiffe_entries() {
    let device = fake_device_info();
    let rn = AclRemoteNetwork {
        remote_network_id: "rn-1".to_string(),
        connectors: vec![tp_acl_conn("c1", "r:9093")],
        ..Default::default()
    };
    let allowed_entry = AclEntry {
        address: "10.0.0.1".to_string(),
        port: 80,
        protocol: "tcp".to_string(),
        allowed_spiffe_ids: vec![device.spiffe_id.clone()],
        remote_network_id: "rn-1".to_string(),
        ..Default::default()
    };
    let unallowed_entry = AclEntry {
        address: "10.0.0.2".to_string(),
        port: 8080,
        protocol: "tcp".to_string(),
        allowed_spiffe_ids: vec!["spiffe://other.domain/client/other-device".to_string()],
        remote_network_id: "rn-1".to_string(),
        ..Default::default()
    };

    let acl_base = AclSnapshot {
        entries: vec![allowed_entry.clone()],
        remote_networks: vec![rn.clone()],
        ..Default::default()
    };
    let acl_with_unallowed = AclSnapshot {
        entries: vec![allowed_entry, unallowed_entry],
        remote_networks: vec![rn],
        ..Default::default()
    };

    let cfg_base = effective_config(&acl_base, None, &device);
    let cfg_with_unallowed = effective_config(&acl_with_unallowed, None, &device);

    assert_eq!(cfg_base, cfg_with_unallowed);
}

#[test]
fn effective_config_identity_differences() {
    let dev1 = fake_device_info();
    let mut dev2 = fake_device_info();
    dev2.certificate_pem =
        "-----BEGIN CERTIFICATE-----\nDIFFERENT_CERT\n-----END CERTIFICATE-----\n".to_string();

    let mut dev3 = fake_device_info();
    dev3.spiffe_id = "spiffe://test.example/client/device3".to_string();

    let acl = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![dev1.spiffe_id.clone(), dev3.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            ..Default::default()
        }],
        remote_networks: vec![AclRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![tp_acl_conn("c1", "r:9093")],
            ..Default::default()
        }],
        ..Default::default()
    };

    let cfg1 = effective_config(&acl, None, &dev1);
    let cfg2 = effective_config(&acl, None, &dev2);
    let cfg3 = effective_config(&acl, None, &dev3);

    assert_ne!(cfg1, cfg2);
    assert_ne!(cfg1, cfg3);
}

#[test]
fn effective_config_debug_omits_private_key() {
    let device = fake_device_info();
    let acl = AclSnapshot {
        entries: vec![AclEntry {
            address: "10.0.0.1".to_string(),
            port: 80,
            protocol: "tcp".to_string(),
            allowed_spiffe_ids: vec![device.spiffe_id.clone()],
            remote_network_id: "rn-1".to_string(),
            ..Default::default()
        }],
        remote_networks: vec![AclRemoteNetwork {
            remote_network_id: "rn-1".to_string(),
            connectors: vec![tp_acl_conn("c1", "r:9093")],
            ..Default::default()
        }],
        ..Default::default()
    };

    let cfg = effective_config(&acl, None, &device);
    let debug_repr = format!("{:?}", cfg);

    assert!(!debug_repr.contains("SUPER_SECRET_KEY"));
    assert!(!debug_repr.contains("private_key_pem"));
    assert!(debug_repr.contains(&device.spiffe_id));
}
