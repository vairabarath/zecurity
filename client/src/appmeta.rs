const fn get_controller() -> &'static str {
    match option_env!("ZECURITY_CONTROLLER_ADDRESS") {
        Some(s) => s,
        None => "localhost:9090",
    }
}

const fn get_connector() -> &'static str {
    match option_env!("ZECURITY_CONNECTOR_ADDRESS") {
        Some(s) => s,
        None => "localhost:9092",
    }
}

pub const DEFAULT_CONTROLLER_ADDRESS: &str = get_controller();
pub const DEFAULT_CONNECTOR_ADDRESS: &str = get_connector();
pub const SCHEMA_VERSION: u32 = 1;

const fn get_http_base_url() -> &'static str {
    match option_env!("ZECURITY_HTTP_BASE_URL") {
        Some(s) => s,
        None => "",
    }
}

pub const DEFAULT_HTTP_BASE_URL: &str = get_http_base_url();

// ── Same-host client/connector interop ──────────────────────────────────────

/// fwmark the CONNECTOR sets on its own egress sockets when dialing a protected
/// resource, and which the CLIENT's nft interception chain skips.
///
/// Why: the client marks packets by (destination IP, destination port) in a
/// `type route hook output` chain, which applies to EVERY process on the host. If
/// the connector runs on the SAME host as a client (common in dev/test), the
/// connector's own connection to a resource matches that rule and is routed into
/// the client's TUN — a routing loop (connector -> resource -> back into the
/// client's own tunnel). The resource never receives anything and the flow stalls.
///
/// The connector stamps its egress with this mark; the client's chain returns
/// early on it. Both sides MUST agree on this value.
pub const CONNECTOR_EGRESS_MARK: u32 = 0x5b;
