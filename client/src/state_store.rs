use aes_gcm::{
    aead::{Aead, AeadCore, KeyInit},
    Aes256Gcm, Key, Nonce,
};
use anyhow::{anyhow, Context, Result};
use base64::{
    engine::general_purpose::{STANDARD as B64, URL_SAFE_NO_PAD},
    Engine,
};
use rand::RngCore;
use serde::{Deserialize, Serialize};
use std::{
    fs,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use crate::{
    appmeta,
    login::LoginResult,
    runtime::{DeviceInfo, Resource, SessionInfo, UserInfo, WorkspaceInfo},
};

const ENC_PREFIX: &str = "enc1:";

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredWorkspaceState {
    #[serde(default)]
    pub schema_version: u32,
    pub workspace: StoredWorkspace,
    pub user: StoredUser,
    pub device: StoredDevice,
    pub session: StoredSession,
    #[serde(default)]
    pub resources: Vec<StoredResource>,
    #[serde(default)]
    pub last_sync_at: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredWorkspace {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub slug: String,
    #[serde(default)]
    pub trust_domain: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredUser {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub email: String,
    #[serde(default)]
    pub role: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredDevice {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub spiffe_id: String,
    #[serde(default)]
    pub certificate_pem: String,
    #[serde(default)]
    pub ca_cert_pem: String,
    #[serde(default)]
    pub cert_expires_at: i64,
    /// Plaintext in memory after load; encrypted before it is written to disk.
    #[serde(default)]
    pub private_key_pem: String,
    #[serde(default)]
    pub hostname: String,
    #[serde(default)]
    pub os: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredSession {
    #[serde(default)]
    pub access_token: String,
    #[serde(default)]
    pub refresh_token: String,
    #[serde(default)]
    pub expires_at: i64,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct StoredResource {
    #[serde(default)]
    pub id: String,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub host: String,
    #[serde(default)]
    pub port: u16,
    #[serde(default)]
    pub protocol: String,
}

#[derive(Debug, Deserialize, Default)]
struct JwtClaims {
    #[serde(default)]
    sub: String,
    #[serde(default)]
    tenant_id: String,
    #[serde(default)]
    role: String,
}

impl StoredWorkspaceState {
    pub fn from_login(result: LoginResult) -> Self {
        let claims = decode_claims(&result.session.access_token).unwrap_or_default();
        Self {
            schema_version: appmeta::SCHEMA_VERSION,
            workspace: StoredWorkspace {
                id: claims.tenant_id,
                name: result.workspace.name,
                slug: result.workspace.slug,
                trust_domain: result.workspace.trust_domain,
            },
            user: StoredUser {
                id: claims.sub,
                email: result.user.email,
                role: claims.role,
            },
            device: StoredDevice {
                id: result.device.id,
                spiffe_id: result.device.spiffe_id,
                certificate_pem: result.device.certificate_pem,
                ca_cert_pem: result.device.ca_cert_pem,
                cert_expires_at: result.device.cert_expires_at,
                private_key_pem: result.device.private_key_pem,
                hostname: result.device.hostname,
                os: result.device.os,
            },
            session: StoredSession {
                access_token: result.session.access_token,
                refresh_token: result.session.refresh_token,
                expires_at: result.session.expires_at,
            },
            resources: Vec::new(),
            last_sync_at: now_unix(),
        }
    }
}

impl From<&StoredWorkspaceState> for WorkspaceInfo {
    fn from(state: &StoredWorkspaceState) -> Self {
        Self {
            id: state.workspace.id.clone(),
            name: state.workspace.name.clone(),
            slug: state.workspace.slug.clone(),
            trust_domain: state.workspace.trust_domain.clone(),
        }
    }
}

impl From<&StoredWorkspaceState> for UserInfo {
    fn from(state: &StoredWorkspaceState) -> Self {
        Self {
            id: state.user.id.clone(),
            email: state.user.email.clone(),
            role: state.user.role.clone(),
        }
    }
}

impl From<&StoredWorkspaceState> for DeviceInfo {
    fn from(state: &StoredWorkspaceState) -> Self {
        Self {
            id: state.device.id.clone(),
            spiffe_id: state.device.spiffe_id.clone(),
            certificate_pem: state.device.certificate_pem.clone(),
            private_key_pem: state.device.private_key_pem.clone(),
            ca_cert_pem: state.device.ca_cert_pem.clone(),
            cert_expires_at: state.device.cert_expires_at,
            hostname: state.device.hostname.clone(),
            os: state.device.os.clone(),
        }
    }
}

impl From<&StoredWorkspaceState> for SessionInfo {
    fn from(state: &StoredWorkspaceState) -> Self {
        Self {
            access_token: state.session.access_token.clone(),
            refresh_token: state.session.refresh_token.clone(),
            expires_at: state.session.expires_at,
        }
    }
}

impl From<&StoredResource> for Resource {
    fn from(resource: &StoredResource) -> Self {
        Self {
            id: resource.id.clone(),
            name: resource.name.clone(),
            host: resource.host.clone(),
            port: resource.port,
            protocol: resource.protocol.clone(),
        }
    }
}

pub fn state_dir() -> PathBuf {
    dirs::data_local_dir()
        .or_else(dirs::data_dir)
        .unwrap_or_else(|| PathBuf::from("."))
        .join("zecurity-client")
}

pub fn state_path(workspace_slug: &str) -> PathBuf {
    state_dir().join(format!("{}.json", sanitize_workspace_slug(workspace_slug)))
}

fn key_path(workspace_slug: &str) -> PathBuf {
    state_dir().join(format!(".{}.key", sanitize_workspace_slug(workspace_slug)))
}

pub fn save_workspace_state(workspace_slug: &str, state: &StoredWorkspaceState) -> Result<PathBuf> {
    let path = state_path(workspace_slug);
    let mut persisted = state.clone();
    persisted.schema_version = appmeta::SCHEMA_VERSION;

    if !persisted.device.private_key_pem.is_empty()
        && !persisted.device.private_key_pem.starts_with(ENC_PREFIX)
    {
        let key = get_or_create_workspace_key(workspace_slug)?;
        persisted.device.private_key_pem =
            encrypt_private_key(&key, &persisted.device.private_key_pem)?;
    }

    let json = serde_json::to_string_pretty(&persisted)?;
    write_secure(&path, json.as_bytes())?;
    Ok(path)
}

pub fn load_workspace_state(workspace_slug: &str) -> Result<StoredWorkspaceState> {
    let path = state_path(workspace_slug);
    let data = fs::read_to_string(&path)
        .with_context(|| format!("read client state from {}", path.display()))?;
    let mut state: StoredWorkspaceState = serde_json::from_str(&data)
        .with_context(|| format!("parse client state from {}", path.display()))?;

    if state.device.private_key_pem.starts_with(ENC_PREFIX) {
        let key = get_or_create_workspace_key(workspace_slug)?;
        state.device.private_key_pem = decrypt_private_key(&key, &state.device.private_key_pem)?;
    }

    Ok(state)
}

pub fn clear_workspace_state(workspace_slug: &str) -> Result<bool> {
    let state = state_path(workspace_slug);
    let key = key_path(workspace_slug);
    let mut removed = false;

    if remove_if_exists(&state)? {
        removed = true;
    }
    if remove_if_exists(&key)? {
        removed = true;
    }

    Ok(removed)
}

pub fn format_duration_until(timestamp: i64) -> String {
    let seconds = timestamp.saturating_sub(now_unix());
    let days = seconds / 86_400;
    let hours = (seconds % 86_400) / 3_600;
    let minutes = (seconds % 3_600) / 60;

    if days > 0 {
        format!("{}d {}h", days, hours)
    } else if hours > 0 {
        format!("{}h {}m", hours, minutes)
    } else {
        format!("{}m", minutes)
    }
}

fn get_or_create_workspace_key(workspace_slug: &str) -> Result<[u8; 32]> {
    let path = key_path(workspace_slug);
    if path.exists() {
        let encoded = fs::read_to_string(&path)
            .with_context(|| format!("read client key from {}", path.display()))?;
        let bytes = B64.decode(encoded.trim())?;
        if bytes.len() == 32 {
            let mut key = [0u8; 32];
            key.copy_from_slice(&bytes);
            return Ok(key);
        }
    }

    let mut key = [0u8; 32];
    rand::rngs::OsRng.fill_bytes(&mut key);
    write_secure(&path, B64.encode(key).as_bytes())?;
    Ok(key)
}

fn encrypt_private_key(key_bytes: &[u8; 32], pem: &str) -> Result<String> {
    let key = Key::<Aes256Gcm>::from_slice(key_bytes);
    let cipher = Aes256Gcm::new(key);
    let nonce = Aes256Gcm::generate_nonce(&mut rand::rngs::OsRng);
    let ciphertext = cipher
        .encrypt(&nonce, pem.as_bytes())
        .map_err(|_| anyhow!("encrypt private key"))?;
    let mut blob = nonce.to_vec();
    blob.extend_from_slice(&ciphertext);
    Ok(format!("{}{}", ENC_PREFIX, B64.encode(blob)))
}

fn decrypt_private_key(key_bytes: &[u8; 32], encrypted: &str) -> Result<String> {
    let encoded = encrypted
        .strip_prefix(ENC_PREFIX)
        .ok_or_else(|| anyhow!("private key is not encrypted with enc1"))?;
    let blob = B64.decode(encoded.trim())?;
    if blob.len() < 12 {
        return Err(anyhow!("encrypted private key is too short"));
    }
    let (nonce_bytes, ciphertext) = blob.split_at(12);
    let key = Key::<Aes256Gcm>::from_slice(key_bytes);
    let cipher = Aes256Gcm::new(key);
    let nonce = Nonce::from_slice(nonce_bytes);
    let plaintext = cipher
        .decrypt(nonce, ciphertext)
        .map_err(|_| anyhow!("decrypt private key"))?;
    Ok(String::from_utf8(plaintext)?)
}

fn write_secure(path: &Path, data: &[u8]) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            fs::set_permissions(parent, fs::Permissions::from_mode(0o700))?;
        }
    }

    fs::write(path, data)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

fn remove_if_exists(path: &Path) -> Result<bool> {
    match fs::remove_file(path) {
        Ok(()) => Ok(true),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(err) => Err(err).with_context(|| format!("remove {}", path.display())),
    }
}

fn decode_claims(access_token: &str) -> Result<JwtClaims> {
    let payload = access_token
        .split('.')
        .nth(1)
        .ok_or_else(|| anyhow!("access token is not a JWT"))?;
    let decoded = URL_SAFE_NO_PAD.decode(payload)?;
    Ok(serde_json::from_slice(&decoded)?)
}

fn sanitize_workspace_slug(slug: &str) -> String {
    slug.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
                c
            } else {
                '_'
            }
        })
        .collect()
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_secs() as i64)
        .unwrap_or_default()
}
