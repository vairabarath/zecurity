// config.rs — Configuration loading for the ZECURITY Shield
//
// HOW CONFIG WORKS:
//   The shield reads its config from environment variables only.
//   The systemd unit uses `EnvironmentFile=-/etc/zecurity/shield.conf`
//   which injects KEY=VALUE pairs from that file into the process environment
//   before the service starts. Figment then reads those env vars here.
//
//   Why env vars and not TOML directly?
//   systemd's EnvironmentFile= syntax (KEY=VALUE, no quotes) is incompatible
//   with TOML syntax (key = "value"). Dual-loading caused startup failures
//   in the connector — we learned from that and use env-only here.
//
// REQUIRED FIELDS (shield won't start without these):
//   CONTROLLER_ADDR      — gRPC address of the controller (e.g. "controller.example.com:9090")
//   CONTROLLER_HTTP_ADDR — HTTP address for /ca.crt bootstrap (e.g. "controller.example.com:8080")
//   ENROLLMENT_TOKEN     — single-use JWT from admin UI (only needed on first run)
//
// OPTIONAL FIELDS (have sensible defaults):
//   AUTO_UPDATE_ENABLED           — false
//   LOG_LEVEL                     — "info"
//   SHIELD_HEARTBEAT_INTERVAL_SECS — 30
//
// STATE DIRECTORY (/var/lib/zecurity-shield/):
//   shield.key    — EC P-384 private key (mode 0600, written by crypto.rs)
//   shield.crt    — signed SPIFFE certificate (written by enrollment.rs)
//   ca_chain.crt  — workspace CA chain for mTLS (written by enrollment.rs)
//   state.json    — shield_id, connector_id, interface_addr, etc. (written by enrollment.rs)
//
// CALLED BY: main.rs at startup via ShieldConfig::load()

use figment::{providers::Env, Figment};
use serde::Deserialize;

const DEFAULT_STATE_DIR: &str = "/var/lib/zecurity-shield";
const DEFAULT_LOG_LEVEL: &str = "info";
const DEFAULT_HEARTBEAT_INTERVAL_SECS: u64 = 30;

/// Shield configuration.
///
/// All fields map 1:1 to environment variable names (lowercased).
/// Example: `CONTROLLER_ADDR` env var → `controller_addr` field.
#[derive(Debug, Clone, Deserialize)]
pub struct ShieldConfig {
    /// gRPC address of the controller.
    /// Used by enrollment.rs to call the Enroll RPC (plain TLS, no client cert yet).
    /// Example: "controller.example.com:9090"
    pub controller_addr: String,

    /// HTTP address of the controller for the /ca.crt bootstrap endpoint.
    /// The shield fetches the CA cert over HTTP before it has any TLS material,
    /// then verifies the CA fingerprint against the enrollment token's embedded hash.
    /// Example: "controller.example.com:8080"
    pub controller_http_addr: String,

    /// Single-use enrollment JWT from the admin UI "Add Shield" flow.
    /// The controller's GenerateShieldToken mutation creates this.
    /// After enrollment succeeds, this token is burned (Redis JTI) and useless.
    /// Only required on first run — subsequent starts use state.json instead.
    #[serde(default)]
    pub enrollment_token: Option<String>,

    /// Whether the auto-updater is enabled (Phase L — updater.rs).
    /// When true, the updater checks GitHub releases weekly and replaces
    /// /usr/local/bin/zecurity-shield if a newer version is available.
    #[serde(default)]
    pub auto_update_enabled: bool,

    /// Log level filter string passed to tracing_subscriber::EnvFilter.
    /// Valid values: "error", "warn", "info", "debug", "trace"
    /// Can also be module-scoped: "zecurity_shield=debug,tonic=warn"
    #[serde(default = "default_log_level")]
    pub log_level: String,

    /// How often the shield sends a heartbeat to the connector (in seconds).
    /// The connector uses heartbeat absence to detect disconnected shields.
    /// Must be less than the controller's SHIELD_DISCONNECT_THRESHOLD.
    #[serde(default = "default_heartbeat_interval_secs")]
    pub shield_heartbeat_interval_secs: u64,

    /// How often the health check loop probes each protected port (in seconds).
    #[serde(default = "default_resource_check_interval")]
    pub resource_check_interval_secs: u64,

    /// Directory for persistent state files (key, cert, CA chain, state.json).
    /// The systemd service unit grants write access to this directory.
    #[serde(default = "default_state_dir")]
    pub state_dir: String,
}

fn default_log_level() -> String {
    DEFAULT_LOG_LEVEL.to_owned()
}

fn default_heartbeat_interval_secs() -> u64 {
    DEFAULT_HEARTBEAT_INTERVAL_SECS
}

fn default_resource_check_interval() -> u64 {
    30
}

fn default_state_dir() -> String {
    DEFAULT_STATE_DIR.to_owned()
}

impl ShieldConfig {
    /// Load configuration from environment variables.
    ///
    /// Figment reads all env vars and lowercases the keys to match struct fields.
    /// Example: `CONTROLLER_ADDR=foo:9090` → `config.controller_addr = "foo:9090"`
    ///
    /// Returns an error if any required field (controller_addr, controller_http_addr)
    /// is missing from the environment.
    pub fn load() -> anyhow::Result<Self> {
        let config = Figment::new()
            .merge(Env::raw().map(|key| key.as_str().to_ascii_lowercase().into()))
            .extract::<Self>()
            .map_err(|e| anyhow::anyhow!("failed to load shield config: {}", e))?;
        Ok(config)
    }
}
