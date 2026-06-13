use std::env;
use std::net::{IpAddr, SocketAddr};

use anyhow::{bail, Context, Result};
use uuid::Uuid;

const DEFAULT_STATE_DIR: &str = "pki";
const DEFAULT_LOG_LEVEL: &str = "info";
const DEFAULT_RELAY_BIND: &str = "0.0.0.0:9093";

#[derive(Debug, Clone)]
pub struct RelayConfig {
    pub relay_id: String,
    pub controller_addr: String,
    pub controller_http_addr: String,
    pub bind_addr: SocketAddr,
    pub ca_fingerprint: String,
    pub state_dir: String,
    pub dns_sans: Vec<String>,
    pub ip_sans: Vec<IpAddr>,
    pub log_level: String,
}

impl RelayConfig {
    pub fn load() -> Result<Self> {
        let relay_id = required_env("RELAY_ID")?;
        let parsed_id = Uuid::parse_str(&relay_id).context("RELAY_ID must be a UUID")?;
        if parsed_id.hyphenated().to_string() != relay_id {
            bail!("RELAY_ID must be a canonical lowercase UUID");
        }

        let controller_addr = required_env("CONTROLLER_ADDR")?;
        let controller_http_addr =
            env::var("CONTROLLER_HTTP_ADDR").unwrap_or_else(|_| derive_http_addr(&controller_addr));
        let ca_fingerprint = required_env("RELAY_CA_FINGERPRINT")?.to_ascii_lowercase();
        if ca_fingerprint.len() != 64
            || !ca_fingerprint.bytes().all(|byte| byte.is_ascii_hexdigit())
        {
            bail!("RELAY_CA_FINGERPRINT must be a 64-character SHA-256 hex digest");
        }

        Ok(Self {
            relay_id,
            controller_addr,
            controller_http_addr,
            bind_addr: env::var("RELAY_BIND")
                .unwrap_or_else(|_| DEFAULT_RELAY_BIND.to_owned())
                .parse()
                .context("RELAY_BIND must be a socket address")?,
            ca_fingerprint,
            state_dir: env::var("RELAY_STATE_DIR").unwrap_or_else(|_| DEFAULT_STATE_DIR.to_owned()),
            dns_sans: comma_separated("RELAY_DNS_SANS"),
            ip_sans: parse_ip_sans(&comma_separated("RELAY_IP_SANS"))?,
            log_level: env::var("LOG_LEVEL").unwrap_or_else(|_| DEFAULT_LOG_LEVEL.to_owned()),
        })
    }
}

fn required_env(name: &str) -> Result<String> {
    env::var(name).with_context(|| format!("{name} is required"))
}

fn comma_separated(name: &str) -> Vec<String> {
    env::var(name)
        .unwrap_or_default()
        .split(',')
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .collect()
}

fn parse_ip_sans(values: &[String]) -> Result<Vec<IpAddr>> {
    values
        .iter()
        .map(|value| {
            value
                .parse()
                .with_context(|| format!("invalid IP address in RELAY_IP_SANS: {value}"))
        })
        .collect()
}

fn derive_http_addr(grpc_addr: &str) -> String {
    match grpc_addr.rsplit_once(':') {
        Some((host, _)) => format!("{host}:8080"),
        None => format!("{grpc_addr}:8080"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derives_controller_http_address() {
        assert_eq!(
            derive_http_addr("controller.example.com:9090"),
            "controller.example.com:8080"
        );
    }

    #[test]
    fn parses_ip_sans() {
        let values = vec!["10.0.0.5".to_owned(), "2001:db8::1".to_owned()];
        assert_eq!(parse_ip_sans(&values).unwrap().len(), 2);
    }

    #[test]
    fn default_relay_bind_is_valid() {
        assert_eq!(
            DEFAULT_RELAY_BIND.parse::<SocketAddr>().unwrap().port(),
            9093
        );
    }
}
