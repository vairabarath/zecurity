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
//   authz_request_period_ms     — 4500 (P; B2)
//   control_reconnect_base_ms   — 1000 (O5; B2)
//   control_reconnect_max_ms    — 5000 (O5; B2)
//   control_reconnect_jitter_pct — 20  (O5; B2)
//   disabled_shield_close_secs  — 30   (O8; B2)
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
const DEFAULT_RELAY_DRAIN_TIMEOUT_SECS: u64 = 120;
// Connector-revocation timing (decided 2026-10-21 from the Phase M
// measurements; consumed by Phase B2). H itself — 15 s,
// control_stream::AUTHORIZATION_FRESHNESS — is fixed (design D1) and not
// configurable.
const DEFAULT_AUTHZ_REQUEST_PERIOD_MS: u64 = 4_500; // P
const DEFAULT_CONTROL_RECONNECT_BASE_MS: u64 = 1_000; // O5
const DEFAULT_CONTROL_RECONNECT_MAX_MS: u64 = 5_000; // O5
const DEFAULT_CONTROL_RECONNECT_JITTER_PCT: u8 = 20; // O5
const DEFAULT_DISABLED_SHIELD_CLOSE_SECS: u64 = 30; // O8
/// Budget kept for B (enqueue → ack processed) when bounding P: P + B < H.
const MIN_RESPONSE_BUDGET_MS: u64 = 1_000;

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
    /// Example: "controller.example.com:8080" (assumes http://) or
    /// "https://controller.example.com" (explicit scheme — required when the
    /// controller is only reachable over HTTPS, e.g. a remote/WAN deployment).
    /// Do not include a trailing slash.
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

    /// P: period of authorization requests to the controller, in ms. Must
    /// leave a response budget inside H (P ≤ H − 1 s). 4.5 s tolerates two
    /// consecutive lost requests before the deadline. Phase B2 consumes.
    #[serde(default = "default_authz_request_period_ms")]
    pub authz_request_period_ms: u64,

    /// Control-stream reconnect backoff while not serving: initial delay
    /// (ms). The backoff resets after a session receives an accepted
    /// authorization ack (O5). Phase B2 consumes.
    #[serde(default = "default_control_reconnect_base_ms")]
    pub control_reconnect_base_ms: u64,

    /// Control-stream reconnect backoff ceiling while not serving (ms).
    /// Phase B2 consumes.
    #[serde(default = "default_control_reconnect_max_ms")]
    pub control_reconnect_max_ms: u64,

    /// ± jitter applied to each reconnect delay, in percent (0–50).
    /// Phase B2 consumes.
    #[serde(default = "default_control_reconnect_jitter_pct")]
    pub control_reconnect_jitter_pct: u8,

    /// O8: how long a recoverable `Disabled` state keeps shield streams open
    /// (carrying nothing) before closing them so shields fail over (s).
    /// Phase B2 consumes.
    #[serde(default = "default_disabled_shield_close_secs")]
    pub disabled_shield_close_secs: u64,

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

fn default_authz_request_period_ms() -> u64 {
    DEFAULT_AUTHZ_REQUEST_PERIOD_MS
}
fn default_control_reconnect_base_ms() -> u64 {
    DEFAULT_CONTROL_RECONNECT_BASE_MS
}
fn default_control_reconnect_max_ms() -> u64 {
    DEFAULT_CONTROL_RECONNECT_MAX_MS
}
fn default_control_reconnect_jitter_pct() -> u8 {
    DEFAULT_CONTROL_RECONNECT_JITTER_PCT
}
fn default_disabled_shield_close_secs() -> u64 {
    DEFAULT_DISABLED_SHIELD_CLOSE_SECS
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
        Self::from_figment(
            Figment::new().merge(Env::raw().map(|key| key.as_str().to_ascii_lowercase().into())),
        )
    }

    fn from_figment(figment: Figment) -> Result<Self, figment::Error> {
        let cfg: Self = figment.extract()?;
        cfg.validate_authz_timing().map_err(figment::Error::from)?;
        Ok(cfg)
    }

    /// Range checks for the connector-revocation timing values. A value
    /// that would weaken the authorization deadline is refused at startup,
    /// never silently clamped.
    fn validate_authz_timing(&self) -> Result<(), String> {
        let h_ms = crate::control_stream::AUTHORIZATION_FRESHNESS.as_millis() as u64;
        let p = self.authz_request_period_ms;
        if p < 500 || p > h_ms - MIN_RESPONSE_BUDGET_MS {
            return Err(format!(
                "AUTHZ_REQUEST_PERIOD_MS={p} must be between 500 and {} (H = {h_ms} ms minus a {MIN_RESPONSE_BUDGET_MS} ms response budget)",
                h_ms - MIN_RESPONSE_BUDGET_MS
            ));
        }
        let (base, max) = (self.control_reconnect_base_ms, self.control_reconnect_max_ms);
        if base < 100 || max < base || max > 60_000 {
            return Err(format!(
                "CONTROL_RECONNECT_BASE_MS={base} / CONTROL_RECONNECT_MAX_MS={max}: need 100 ≤ base ≤ max ≤ 60000"
            ));
        }
        if self.control_reconnect_jitter_pct > 50 {
            return Err(format!(
                "CONTROL_RECONNECT_JITTER_PCT={} must be 0–50",
                self.control_reconnect_jitter_pct
            ));
        }
        if self.disabled_shield_close_secs == 0 || self.disabled_shield_close_secs > 3_600 {
            return Err(format!(
                "DISABLED_SHIELD_CLOSE_SECS={} must be 1–3600",
                self.disabled_shield_close_secs
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod authz_timing_tests {
    use super::*;
    use figment::providers::{Format, Toml};

    fn load(extra: &str) -> Result<ConnectorConfig, figment::Error> {
        ConnectorConfig::from_figment(Figment::new().merge(Toml::string(&format!(
            "controller_addr = \"c:9090\"\n{extra}"
        ))))
    }

    #[test]
    fn defaults_are_the_accepted_values() {
        let c = load("").unwrap();
        assert_eq!(c.authz_request_period_ms, 4_500);
        assert_eq!(c.control_reconnect_base_ms, 1_000);
        assert_eq!(c.control_reconnect_max_ms, 5_000);
        assert_eq!(c.control_reconnect_jitter_pct, 20);
        assert_eq!(c.disabled_shield_close_secs, 30);
    }

    #[test]
    fn overrides_within_range_are_accepted() {
        let c = load("authz_request_period_ms = 6500\ncontrol_reconnect_max_ms = 8000\ndisabled_shield_close_secs = 60").unwrap();
        assert_eq!(c.authz_request_period_ms, 6_500);
        assert_eq!(c.control_reconnect_max_ms, 8_000);
        assert_eq!(c.disabled_shield_close_secs, 60);
    }

    #[test]
    fn values_that_break_the_deadline_or_ranges_are_refused() {
        for bad in [
            "authz_request_period_ms = 14001", // P + 1 s budget > H
            "authz_request_period_ms = 15000",
            "authz_request_period_ms = 499",
            "control_reconnect_base_ms = 50",
            "control_reconnect_base_ms = 6000", // base > max
            "control_reconnect_max_ms = 60001",
            "control_reconnect_jitter_pct = 51",
            "disabled_shield_close_secs = 0",
        ] {
            assert!(load(bad).is_err(), "{bad} must be refused");
        }
        assert!(load("authz_request_period_ms = 14000").is_ok(), "boundary P = H − 1 s");
    }
}
