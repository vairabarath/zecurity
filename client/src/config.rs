use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
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
    /// Windows only: the SID of the enrolling user, the one non-admin account
    /// the daemon's IPC pipe admits (ipc::pipe_sddl). Written by
    /// client-install.ps1; unused on Linux, where the daemon runs as that user.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub ipc_user_sid: String,
}

impl ClientConf {
    pub fn controller(&self) -> &str {
        if self.controller_address.is_empty() {
            crate::appmeta::DEFAULT_CONTROLLER_ADDRESS
        } else {
            &self.controller_address
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

/// The system-wide config file.
#[cfg(not(windows))]
fn system_conf_path() -> PathBuf {
    PathBuf::from("/etc/zecurity/client.conf")
}

/// `%ProgramData%\Zecurity`: the Windows client's system-wide directory
/// (config, state, logs). The installer grants the enrolling user write
/// access, matching Linux, where the daemon runs as that user.
#[cfg(windows)]
pub fn program_data_dir() -> PathBuf {
    std::env::var_os("ProgramData")
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from(r"C:\ProgramData"))
        .join("Zecurity")
}

#[cfg(windows)]
fn system_conf_path() -> PathBuf {
    program_data_dir().join("client.conf")
}

#[cfg(not(windows))]
pub fn conf_paths() -> Vec<PathBuf> {
    let mut paths = vec![system_conf_path()];
    if let Some(d) = dirs::config_dir() {
        paths.push(d.join("zecurity-client").join("client.conf"));
    }
    paths
}

/// Windows: the system file only. The daemon runs as LocalSystem and can't
/// see the user's profile, so a per-user file would be ignored by it.
#[cfg(windows)]
pub fn conf_paths() -> Vec<PathBuf> {
    vec![system_conf_path()]
}

pub fn load() -> Result<ClientConf> {
    // Merge all config files that exist. System config (/etc/zecurity/client.conf)
    // is loaded first and provides default addresses; user config
    // (~/.config/zecurity-client/client.conf) is loaded second and its non-empty
    // fields take precedence. This lets the install script write addresses system-wide
    // while `setup` writes the workspace to the per-user config.
    let mut merged = ClientConf::default();
    let mut found = false;

    for path in conf_paths() {
        if path.exists() {
            let raw = std::fs::read_to_string(&path)?;
            let c: ClientConf = toml::from_str(&raw)?;
            if !c.workspace.is_empty() {
                merged.workspace = c.workspace;
            }
            if !c.controller_address.is_empty() {
                merged.controller_address = c.controller_address;
            }
            if !c.connector_address.is_empty() {
                merged.connector_address = c.connector_address;
            }
            if !c.http_base_url.is_empty() {
                merged.http_base_url = c.http_base_url;
            }
            if !c.ipc_user_sid.is_empty() {
                merged.ipc_user_sid = c.ipc_user_sid;
            }
            found = true;
        }
    }

    if found {
        Ok(merged)
    } else {
        Err(crate::error::ClientError::NotConfigured.into())
    }
}

/// Windows: `setup` updates the one system file instead of replacing it, so
/// the installer's values (controller addresses, `ipc_user_sid`) survive.
/// Fields left empty in `update` keep their current values.
#[cfg(windows)]
pub fn merged_with_existing(update: ClientConf) -> ClientConf {
    let mut merged: ClientConf = std::fs::read_to_string(system_conf_path())
        .ok()
        .and_then(|raw| toml::from_str(&raw).ok())
        .unwrap_or_default();
    merged.workspace = update.workspace;
    if !update.controller_address.is_empty() {
        merged.controller_address = update.controller_address;
    }
    if !update.connector_address.is_empty() {
        merged.connector_address = update.connector_address;
    }
    if !update.http_base_url.is_empty() {
        merged.http_base_url = update.http_base_url;
    }
    merged
}

pub fn save(conf: &ClientConf) -> Result<PathBuf> {
    let system_path = system_conf_path();
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
