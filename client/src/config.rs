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

/// Android and iOS: the app's files directory holds the one config file
/// (Android: the app's private directory; iOS: the App Group container the
/// app shares with its tunnel extension, where the daemon runs).
#[cfg(any(target_os = "android", target_os = "ios"))]
fn system_conf_path() -> PathBuf {
    crate::mobile::files_dir().join("client.conf")
}

/// The system-wide config file.
#[cfg(not(any(windows, target_os = "android", target_os = "ios")))]
fn system_conf_path() -> PathBuf {
    PathBuf::from("/etc/zecurity/client.conf")
}

/// `%ProgramData%\Zecurity`: the Windows client's system-wide directory
/// (config, state, logs). Administrators and SYSTEM own it; the enrolled user
/// may only read it (win_security::secure_data_dir).
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
/// the installer's values (controller addresses) survive.
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

/// Windows: the one system file, which only administrators may change (the
/// SYSTEM daemon reads it; see win_security). No per-user fallback: the
/// daemon would never see it.
#[cfg(windows)]
pub fn save(conf: &ClientConf) -> Result<PathBuf> {
    use anyhow::Context;

    let path = system_conf_path();
    let dir = path.parent().context("config path has no directory")?;
    // A fresh file, renamed over the old one: it carries only what the
    // directory grants (SYSTEM and Administrators full control, the enrolled
    // user read), never an existing file's access list.
    let tmp = dir.join(format!("client.conf.tmp.{}", std::process::id()));
    if let Err(e) = std::fs::write(&tmp, toml::to_string_pretty(conf)?) {
        return Err(if e.kind() == std::io::ErrorKind::PermissionDenied {
            anyhow::anyhow!(
                "writing {} needs administrator rights: run `zecurity-client setup` from an \
                 elevated terminal",
                path.display()
            )
        } else {
            e.into()
        });
    }
    let result = (|| -> Result<()> {
        // The daemon accepts only SYSTEM/Administrators-owned files
        // (win_security); a file an elevated admin creates may be owned by
        // that admin's own account. icacls by full path, never a PATH lookup.
        let system_root = std::env::var_os("SystemRoot")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from(r"C:\Windows"));
        let status = std::process::Command::new(system_root.join("System32").join("icacls.exe"))
            .arg(&tmp)
            .args(["/setowner", "*S-1-5-32-544", "/L"])
            .stdout(std::process::Stdio::null())
            .status()
            .context("run icacls")?;
        anyhow::ensure!(status.success(), "icacls /setowner failed with {status}");
        std::fs::rename(&tmp, &path).with_context(|| format!("replace {}", path.display()))?;
        Ok(())
    })();
    if result.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    result.map(|()| path)
}

#[cfg(not(windows))]
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
