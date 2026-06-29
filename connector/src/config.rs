// config.rs — Configuration loading for the ZECURITY connector
//
// Reads config from environment variables only. The systemd unit uses
// `EnvironmentFile=-/etc/zecurity/connector.conf` to inject KEY=VALUE
// pairs from that file into the process environment before the service
// starts, so the file path itself is never opened by this code.
//
// We deliberately do NOT parse /etc/zecurity/connector.conf as TOML here:
// systemd's EnvironmentFile= syntax (KEY=VALUE, unquoted) is incompatible
// with TOML syntax (key = "value"). Dual-loading caused startup failures.
//
// Environment variable names are automatically lowercased to match struct
// field names:
//   CONTROLLER_ADDR → controller_addr
//   LOG_LEVEL       → log_level
//
// Required fields:
//   controller_addr    — always required (gRPC target, e.g. "controller.example.com:9090")
//   enrollment_token   — required on FIRST RUN only (JWT from admin UI "install command")
//
// Optional fields (have defaults):
//   connector_id                — None until enrollment completes (Phase 5 writes it)
//   auto_update_enabled         — false (Phase 7 updater checks this)
//   log_level                   — "info"
//   update_check_interval_secs  — 86400 seconds = 24 hours (Phase 7 updater)
//   state_dir                   — /var/lib/zecurity-connector (where certs, keys, state.json are saved)
//
// Called by: main.rs at startup via ConnectorConfig::load()

use figment::{providers::Env, Figment};
use serde::Deserialize;

/// Default config file path. The install script (Phase 9) writes this file
/// with CONTROLLER_ADDR and ENROLLMENT_TOKEN during installation.
pub const CONFIG_FILE_PATH: &str = "/etc/zecurity/connector.conf";

const DEFAULT_STATE_DIR: &str = "/var/lib/zecurity-connector";
const DEFAULT_LOG_LEVEL: &str = "info";
const DEFAULT_UPDATE_CHECK_INTERVAL_SECS: u64 = 86400; // 24 hours
const DEFAULT_RELAY_INNER_HANDSHAKE_TIMEOUT_SECS: u64 = 10;
const DEFAULT_RELAY_MAX_TUNNEL_STREAMS: u32 = 256;
const DEFAULT_RELAY_IDLE_TIMEOUT_SECS: u64 = 60;
const DEFAULT_RELAY_REPROBE_INTERVAL_SECS: u64 = 300;
const DEFAULT_RELAY_MAX_CONCURRENT_PROBES: usize = 5;
const DEFAULT_RELAY_RECONNECT_BASE_SECS: u64 = 5;
const DEFAULT_RELAY_RECONNECT_MAX_SECS: u64 = 120;
const DEFAULT_RELAY_RECONNECT_BACKOFF_FACTOR: f64 = 2.0;
const DEFAULT_RELAY_DRAIN_TIMEOUT_SECS: u64 = 30;

/// Connector configuration.
///
/// All fields map 1:1 to TOML keys or environment variables.
/// Serde deserializes from figment's merged config.
#[derive(Debug, Clone, Deserialize)]
pub struct ConnectorConfig {
    /// gRPC address of the controller (e.g., "controller.example.com:9090").
    /// Used by enrollment.rs and control_stream.rs to connect.
    pub controller_addr: String,

    /// HTTP address of the controller for the /ca.crt endpoint.
    /// The gRPC port (controller_addr) and HTTP port are different.
    /// If unset, derived from controller_addr host + port 8080.
    /// Example: "controller.example.com:8080"
    #[serde(default)]
    pub controller_http_addr: Option<String>,

    /// Single-use enrollment token (JWT). The admin UI generates this via
    /// the generateConnectorToken GraphQL mutation. After enrollment succeeds,
    /// this token is consumed and no longer needed.
    #[serde(default)]
    pub enrollment_token: Option<String>,

    /// Connector UUID — set after successful enrollment.
    /// Stored in state.json and read on subsequent startups.
    #[serde(default)]
    pub connector_id: Option<String>,

    /// Deadline for Client-to-Connector inner mTLS through Relay.
    #[serde(default = "default_relay_inner_handshake_timeout_secs")]
    pub relay_inner_handshake_timeout_secs: u64,

    /// Maximum concurrent Relay tunnel streams, including inner mTLS handshakes.
    #[serde(default = "default_relay_max_tunnel_streams")]
    pub relay_max_tunnel_streams: u32,

    /// Outer Connector-to-Relay QUIC idle timeout.
    #[serde(default = "default_relay_idle_timeout_secs")]
    pub relay_idle_timeout_secs: u64,

    /// Sprint 11 ADR-016 — interval between background re-probes of the
    /// labelled-relay list. Phase 2 consumes; Phase 1 lands the knob.
    #[serde(default = "default_relay_reprobe_interval_secs")]
    pub relay_reprobe_interval_secs: u64,

    /// Maximum parallel relay probes during a single probe sweep.
    #[serde(default = "default_relay_max_concurrent_probes")]
    pub relay_max_concurrent_probes: usize,

    /// Initial backoff after a relay disconnect before the connector
    /// re-attempts attachment. Phase 3 consumes.
    #[serde(default = "default_relay_reconnect_base_secs")]
    pub relay_reconnect_base_secs: u64,

    /// Backoff ceiling for relay reconnect attempts.
    #[serde(default = "default_relay_reconnect_max_secs")]
    pub relay_reconnect_max_secs: u64,

    /// Multiplicative factor applied to the relay reconnect backoff each retry.
    #[serde(default = "default_relay_reconnect_backoff_factor")]
    pub relay_reconnect_backoff_factor: f64,

    /// Grace period to drain active streams before tearing down an old relay
    /// session during a rotation.
    #[serde(default = "default_relay_drain_timeout_secs")]
    pub relay_drain_timeout_secs: u64,

    /// Whether automatic binary updates are enabled (Phase 7 updater).
    #[serde(default)]
    pub auto_update_enabled: bool,

    /// Log level filter (e.g., "info", "debug", "trace").
    /// Passed to tracing_subscriber::EnvFilter in main.rs.
    #[serde(default = "default_log_level")]
    pub log_level: String,

    /// How often to check GitHub releases for updates, in seconds (Phase 7).
    #[serde(default = "default_update_check_interval_secs")]
    pub update_check_interval_secs: u64,

    /// LAN address shields use to reach this connector's gRPC server (:9091).
    /// Set this to override auto-detection (e.g. "192.168.1.10:9091").
    /// If unset, the connector auto-detects its RFC-1918 LAN IP at startup.
    #[serde(default, alias = "connector_lan_addr")]
    pub lan_addr: Option<String>,

    /// Directory for persistent state files:
    ///   connector.key      — EC P-384 private key (mode 0600)
    ///   connector.crt      — signed SPIFFE certificate
    ///   workspace_ca.crt   — CA chain for mTLS
    ///   state.json         — connector_id, trust_domain, enrollment metadata
    #[serde(default = "default_state_dir")]
    pub state_dir: String,
}

fn default_log_level() -> String {
    DEFAULT_LOG_LEVEL.to_owned()
}

fn default_update_check_interval_secs() -> u64 {
    DEFAULT_UPDATE_CHECK_INTERVAL_SECS
}

fn default_relay_inner_handshake_timeout_secs() -> u64 {
    DEFAULT_RELAY_INNER_HANDSHAKE_TIMEOUT_SECS
}

fn default_relay_max_tunnel_streams() -> u32 {
    DEFAULT_RELAY_MAX_TUNNEL_STREAMS
}

fn default_relay_idle_timeout_secs() -> u64 {
    DEFAULT_RELAY_IDLE_TIMEOUT_SECS
}

fn default_relay_reprobe_interval_secs() -> u64 {
    DEFAULT_RELAY_REPROBE_INTERVAL_SECS
}

fn default_relay_max_concurrent_probes() -> usize {
    DEFAULT_RELAY_MAX_CONCURRENT_PROBES
}

fn default_relay_reconnect_base_secs() -> u64 {
    DEFAULT_RELAY_RECONNECT_BASE_SECS
}

fn default_relay_reconnect_max_secs() -> u64 {
    DEFAULT_RELAY_RECONNECT_MAX_SECS
}

fn default_relay_reconnect_backoff_factor() -> f64 {
    DEFAULT_RELAY_RECONNECT_BACKOFF_FACTOR
}

fn default_relay_drain_timeout_secs() -> u64 {
    DEFAULT_RELAY_DRAIN_TIMEOUT_SECS
}

fn default_state_dir() -> String {
    DEFAULT_STATE_DIR.to_owned()
}

impl ConnectorConfig {
    /// Load configuration from TOML config file and environment variables.
    ///
    /// Priority (highest wins):
    /// 1. Environment variables (lowercased to match struct fields)
    /// 2. TOML config file at `/etc/zecurity/connector.conf`
    ///
    /// The config file is optional — missing file is silently skipped.
    pub fn load() -> Result<Self, figment::Error> {
        // Env-only. systemd's EnvironmentFile= injects vars from
        // /etc/zecurity/connector.conf (KEY=VALUE format) into our process
        // env before we run, and figment picks them up here.
        Figment::new()
            .merge(Env::raw().map(|key| key.as_str().to_ascii_lowercase().into()))
            .extract()
    }
}
