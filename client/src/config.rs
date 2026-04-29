use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct ClientConf {
    pub workspace: String,
    /// Only set in dev. Empty = use compiled-in constant from appmeta.
    #[serde(default)]
    pub controller_address: String,
    /// Only set in dev. Empty = use compiled-in constant from appmeta.
    #[serde(default)]
    pub connector_address: String,
    /// Only set in dev. Empty = use compiled-in constant from appmeta.
    #[serde(default)]
    pub http_base_url: String,
}

impl ClientConf {
    pub fn controller(&self) -> &str {
        if self.controller_address.is_empty() {
            crate::appmeta::DEFAULT_CONTROLLER_ADDRESS
        } else {
            &self.controller_address
        }
    }

    pub fn connector(&self) -> &str {
        if self.connector_address.is_empty() {
            crate::appmeta::DEFAULT_CONNECTOR_ADDRESS
        } else {
            &self.connector_address
        }
    }

    pub fn http_base(&self) -> String {
        if !self.http_base_url.is_empty() {
            return trim_trailing_slash(&self.http_base_url);
        }
        if !crate::appmeta::DEFAULT_HTTP_BASE_URL.is_empty() {
            return trim_trailing_slash(crate::appmeta::DEFAULT_HTTP_BASE_URL);
        }
        format!("http://{}:8080", controller_host_for_url(self.controller()))
    }
}

fn trim_trailing_slash(value: &str) -> String {
    value.trim_end_matches('/').to_string()
}

fn controller_host_for_url(controller_address: &str) -> String {
    if let Some(rest) = controller_address.strip_prefix('[') {
        if let Some((host, _)) = rest.split_once(']') {
            return format!("[{}]", host);
        }
    }

    let host = controller_address
        .rsplit_once(':')
        .map(|(host, _)| host)
        .unwrap_or(controller_address);

    if host.contains(':') {
        format!("[{}]", host)
    } else {
        host.to_string()
    }
}

pub fn conf_paths() -> Vec<PathBuf> {
    let mut paths = vec![PathBuf::from("/etc/zecurity/client.conf")];
    if let Some(d) = dirs::config_dir() {
        paths.push(d.join("zecurity-client").join("client.conf"));
    }
    paths
}

pub fn load() -> Result<ClientConf> {
    for path in conf_paths() {
        if path.exists() {
            let raw = std::fs::read_to_string(&path)?;
            return Ok(toml::from_str(&raw)?);
        }
    }
    Err(crate::error::ClientError::NotConfigured.into())
}

pub fn save(conf: &ClientConf) -> Result<PathBuf> {
    let system_path = PathBuf::from("/etc/zecurity/client.conf");
    let user = dirs::config_dir()
        .unwrap_or_else(|| PathBuf::from("."))
        .join("zecurity-client")
        .join("client.conf");

    // Try system path first - check if parent dir exists AND is writable
    let system_parent = system_path.parent().unwrap();
    if system_parent.exists() {
        if let Ok(_) = std::fs::write(&system_path, "") {
            // Can write - use system path
            std::fs::remove_file(&system_path)?;
            std::fs::write(&system_path, toml::to_string_pretty(conf)?)?;
            return Ok(system_path);
        }
    }

    // Fall back to user config
    std::fs::create_dir_all(user.parent().unwrap())?;
    std::fs::write(&user, toml::to_string_pretty(conf)?)?;
    Ok(user)
}
