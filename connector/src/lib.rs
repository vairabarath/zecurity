// zecurity-connector library surface.
//
// This file exists so integration tests in `connector/tests/` can link
// against the connector's modules. The actual entry point is in `main.rs`,
// which uses `zecurity_connector::*` to access these modules.

pub mod agent_server;
pub mod agent_tunnel;
pub mod appmeta;
pub mod config;
pub mod control_stream;
pub mod controller_client;
pub mod crl;
pub mod crypto;
pub mod device_tunnel;
pub mod discovery;
pub mod enrollment;
pub mod net_util;
pub mod policy;
pub mod quic_listener;
pub mod relay_attachment;
pub mod relay_client;
pub mod relay_handler;
pub mod relay_probe;
pub mod relay_ranking;
pub mod relay_selector;
pub mod renewal;
pub mod tls;
pub mod updater;
pub mod util;
pub mod watchdog;

/// Generated gRPC client stubs from connector.proto.
pub mod shield {
    pub mod v1 {
        tonic::include_proto!("shield.v1");
    }
}

/// Generated client.v1 message types — used for ACLSnapshot referenced in connector.proto.
pub mod client {
    pub mod v1 {
        tonic::include_proto!("client.v1");
    }
}

/// Alias so existing agent_server.rs code can use `crate::shield_proto::*`.
pub use shield::v1 as shield_proto;

/// Type alias used by quic_listener.rs and device_tunnel.rs.
/// Maps the spec name to the real ShieldRegistry type.
pub type AgentRegistry = agent_server::ShieldRegistry;

/// Type alias used by device_tunnel.rs for the control stream message type.
pub type ControlMessage = proto::ConnectorControlMessage;

/// Generated connector gRPC stubs.
pub mod connector {
    pub mod v1 {
        tonic::include_proto!("connector.v1");
    }
}

/// Alias so connector modules can use `proto::*`.
pub use connector::v1 as proto;
