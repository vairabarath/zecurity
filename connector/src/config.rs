// config.rs — Configuration loading for the ZECURITY connector
//
// Reads config from two sources (higher priority wins):
//   1. /etc/zecurity/connector.conf  (TOML file, optional — missing file is silently skipped)
//   2. Environment variables          (CONTROLLER_ADDR, ENROLLMENT_TOKEN, LOG_LEVEL, etc.)
//
// Environment variables are automatically lowercased to match struct field names:
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
//   heartbeat_interval_secs     — 30 seconds (Phase 6 heartbeat loop)
//   update_check_interval_secs  — 86400 seconds = 24 hours (Phase 7 updater)
//   state_dir                   — /var/lib/zecurity-connector (where certs, keys, state.json are saved)
//
// Called by: main.rs at startup via ConnectorConfig::load()

use figment::{
    providers::{Env, Format, Toml},
    Figment,
};
use serde::Deserialize;

/// Default config file path. The install script (Phase 9) writes this file
/// with CONTROLLER_ADDR and ENROLLMENT_TOKEN during installation.
pub const CONFIG_FILE_PATH: &str = "/etc/zecurity/connector.conf";

const DEFAULT_STATE_DIR: &str = "/var/lib/zecurity-connector";
const DEFAULT_LOG_LEVEL: &str = "info";
const DEFAULT_HEARTBEAT_INTERVAL_SECS: u64 = 30;
const DEFAULT_UPDATE_CHECK_INTERVAL_SECS: u64 = 86400; // 24 hours

/// Connector configuration.
///
/// All fields map 1:1 to TOML keys or environment variables.
/// Serde deserializes from figment's merged config.
#[derive(Debug, Clone, Deserialize)]
pub struct ConnectorConfig {
    /// gRPC address of the controller (e.g., "controller.example.com:9090").
    /// Used by enrollment.rs (Phase 5) and heartbeat.rs (Phase 6) to connect.
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

    /// Whether automatic binary updates are enabled (Phase 7 updater).
    #[serde(default)]
    pub auto_update_enabled: bool,

    /// Log level filter (e.g., "info", "debug", "trace").
    /// Passed to tracing_subscriber::EnvFilter in main.rs.
    #[serde(default = "default_log_level")]
    pub log_level: String,

    /// Heartbeat interval in seconds. The connector sends a heartbeat to the
    /// controller every N seconds to prove it's alive (Phase 6).
    #[serde(default = "default_heartbeat_interval_secs")]
    pub heartbeat_interval_secs: u64,

    /// How often to check GitHub releases for updates, in seconds (Phase 7).
    #[serde(default = "default_update_check_interval_secs")]
    pub update_check_interval_secs: u64,

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

fn default_heartbeat_interval_secs() -> u64 {
    DEFAULT_HEARTBEAT_INTERVAL_SECS
}

fn default_update_check_interval_secs() -> u64 {
    DEFAULT_UPDATE_CHECK_INTERVAL_SECS
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
        Figment::new()
            .merge(Toml::file(CONFIG_FILE_PATH))
            .merge(Env::raw().map(|key| key.as_str().to_ascii_lowercase().into()))
            .extract()
    }
}
