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
    io::Write,
    path::{Path, PathBuf},
    time::{SystemTime, UNIX_EPOCH},
};

use crate::{
    appmeta,
    login::LoginResult,
    runtime::{DeviceInfo, SessionInfo, UserInfo, WorkspaceInfo},
};

const ENC_PREFIX: &str = "enc1:";
const STATE_ENVELOPE_VERSION: u32 = 1;

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

#[derive(Debug, Clone, Serialize, Deserialize)]
struct EncryptedStateEnvelope {
    version: u32,
    ciphertext: String,
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
    /// Sprint 19 Track 2 (PENDING-13): "" (active) / "re_enroll_required" /
    /// "revoked" — mirrors runtime::DeviceState. Set only via mark_device_state,
    /// which also wipes certificate_pem/private_key_pem in the same write.
    #[serde(default)]
    pub device_state: String,
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
        let claims = decode_claims(&result.session.access_token).unwrap_or_default(); // comes from the login result
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
                // A fresh login always re-enrolls; any prior revoke/re-enroll
                // marker is stale.
                device_state: String::new(),
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
pub fn state_dir() -> PathBuf {
    // Override for tests — mirrors ipc::ipc_socket_path's ZECURITY_DAEMON_SOCKET
    // pattern, so tests never touch the real user's XDG state directory.
    if let Ok(p) = std::env::var("ZECURITY_STATE_DIR") {
        return PathBuf::from(p);
    }
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
    let key = get_or_create_workspace_key(workspace_slug)?;
    let envelope = encrypt_state(&key, workspace_slug, &persisted)?;
    let json = serde_json::to_string_pretty(&envelope)?;
    write_secure(&path, json.as_bytes())?;
    Ok(path)
}

pub fn load_workspace_state(workspace_slug: &str) -> Result<StoredWorkspaceState> {
    let path = state_path(workspace_slug);
    reject_symlink(&path)?;
    let data = fs::read_to_string(&path)
        .with_context(|| format!("read client state from {}", path.display()))?;

    if let Ok(envelope) = serde_json::from_str::<EncryptedStateEnvelope>(&data) {
        let key = load_existing_workspace_key(workspace_slug)?;
        return decrypt_state(&key, workspace_slug, &envelope)
            .with_context(|| format!("decrypt client state from {}", path.display()));
    }

    let mut state: StoredWorkspaceState = serde_json::from_str(&data)
        .with_context(|| format!("parse legacy client state from {}", path.display()))?;
    decrypt_legacy_private_key(workspace_slug, &mut state)?;
    Ok(state)
}

/// Replace the session tokens on disk after a successful /auth/refresh. The
/// controller rotates the refresh token on every call — the old value is
/// dead server-side the moment the response leaves the wire, so the caller
/// must invoke this before the next refresh attempt or the whole session
/// will be lost.
///
/// Load → mutate → save. Non-session fields (device cert, workspace, user,
/// resources) are preserved verbatim. The underlying write uses the same
/// atomic tmp-file + rename path as save_workspace_state, so a crash mid-
/// write leaves the old file intact.
///
/// Known failure window: if the process crashes AFTER the server rotated
/// the refresh token but BEFORE this save returns, the client keeps the
/// old (now dead) refresh token on disk. On next daemon start the old
/// token gets rejected and the user re-authenticates. Acceptable — the
/// alternative (two-phase commit) is disproportionate for a 15-min bug.
pub fn save_rotated_tokens(
    workspace_slug: &str,
    new_access: String,
    new_refresh: String,
    expires_at: i64,
) -> Result<()> {
    let mut state = load_workspace_state(workspace_slug)?;
    state.session.access_token = new_access;
    state.session.refresh_token = new_refresh;
    state.session.expires_at = expires_at;
    save_workspace_state(workspace_slug, &state)?;
    Ok(())
}

/// Persists a renewed device cert after a successful RenewCert (Sprint 19
/// Track 3 / PENDING-13, ADR-028 D1). Load -> mutate -> save, same idiom as
/// save_rotated_tokens. Touches only certificate_pem/ca_cert_pem/
/// cert_expires_at — private_key_pem is untouched because renewal reuses the
/// existing key (that's the whole point of the fingerprint-pinning check on
/// the controller side).
pub fn save_renewed_cert(
    workspace_slug: &str,
    certificate_pem: String,
    ca_cert_pem: String,
    cert_expires_at: i64,
) -> Result<()> {
    let mut state = load_workspace_state(workspace_slug)?;
    state.device.certificate_pem = certificate_pem;
    state.device.ca_cert_pem = ca_cert_pem;
    state.device.cert_expires_at = cert_expires_at;
    save_workspace_state(workspace_slug, &state)?;
    Ok(())
}

/// Sets the device_state marker (Sprint 19 Track 2 / PENDING-13) and wipes the
/// on-disk cert + key material — both REVOKED and RE_ENROLL_REQUIRED mean the
/// cert is dead server-side, so there is nothing to gain by keeping it on disk
/// and real security value in not doing so. id/spiffe_id are preserved:
/// RE_ENROLL_REQUIRED reuses the same device row on recovery (D-C).
///
/// Load -> mutate -> save, mirroring save_rotated_tokens. Same known failure
/// window: a crash between the server-side change and this save leaves a
/// stale marker on disk until the next successful poll.
pub fn mark_device_state(workspace_slug: &str, device_state: &str) -> Result<()> {
    let mut state = load_workspace_state(workspace_slug)?;
    state.device.device_state = device_state.to_string();
    state.device.certificate_pem.clear();
    state.device.private_key_pem.clear();
    state.device.cert_expires_at = 0;
    save_workspace_state(workspace_slug, &state)?;
    Ok(())
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
    reject_symlink(&path)?;
    if path.exists() {
        return read_workspace_key(&path);
    }

    let mut key = [0u8; 32];
    rand::rngs::OsRng.fill_bytes(&mut key);
    write_secure(&path, B64.encode(key).as_bytes())?;
    Ok(key)
}

fn load_existing_workspace_key(workspace_slug: &str) -> Result<[u8; 32]> {
    let path = key_path(workspace_slug);
    reject_symlink(&path)?;
    if !path.exists() {
        return Err(anyhow!(
            "client state exists but encryption key is missing; run `zecurity-client login` again"
        ));
    }
    read_workspace_key(&path)
}

fn read_workspace_key(path: &Path) -> Result<[u8; 32]> {
    let encoded = fs::read_to_string(path)
        .with_context(|| format!("read client key from {}", path.display()))?;
    let bytes = B64.decode(encoded.trim())?;
    if bytes.len() != 32 {
        return Err(anyhow!(
            "client encryption key at {} is invalid; run `zecurity-client login` again",
            path.display()
        ));
    }
    let mut key = [0u8; 32];
    key.copy_from_slice(&bytes);
    Ok(key)
}

fn encrypt_state(
    key_bytes: &[u8; 32],
    workspace_slug: &str,
    state: &StoredWorkspaceState,
) -> Result<EncryptedStateEnvelope> {
    let key = Key::<Aes256Gcm>::from_slice(key_bytes);
    let cipher = Aes256Gcm::new(key);
    let nonce = Aes256Gcm::generate_nonce(&mut rand::rngs::OsRng);
    let plaintext = serde_json::to_vec(state)?;
    let ciphertext = cipher
        .encrypt(
            &nonce,
            aes_gcm::aead::Payload {
                msg: &plaintext,
                aad: state_aad(workspace_slug).as_bytes(),
            },
        )
        .map_err(|_| anyhow!("encrypt client state"))?;
    let mut blob = nonce.to_vec();
    blob.extend_from_slice(&ciphertext);
    Ok(EncryptedStateEnvelope {
        version: STATE_ENVELOPE_VERSION,
        ciphertext: format!("{}{}", ENC_PREFIX, B64.encode(blob)),
    })
}

fn decrypt_state(
    key_bytes: &[u8; 32],
    workspace_slug: &str,
    envelope: &EncryptedStateEnvelope,
) -> Result<StoredWorkspaceState> {
    if envelope.version != STATE_ENVELOPE_VERSION {
        return Err(anyhow!(
            "unsupported client state version {}; run `zecurity-client login` again",
            envelope.version
        ));
    }
    let blob = decode_encrypted_blob(&envelope.ciphertext, "client state")?;
    let (nonce_bytes, ciphertext) = blob.split_at(12);
    let key = Key::<Aes256Gcm>::from_slice(key_bytes);
    let cipher = Aes256Gcm::new(key);
    let nonce = Nonce::from_slice(nonce_bytes);
    let plaintext = cipher
        .decrypt(
            nonce,
            aes_gcm::aead::Payload {
                msg: ciphertext,
                aad: state_aad(workspace_slug).as_bytes(),
            },
        )
        .map_err(|_| anyhow!("decrypt client state"))?;
    Ok(serde_json::from_slice(&plaintext)?)
}

fn decrypt_legacy_private_key(
    workspace_slug: &str,
    state: &mut StoredWorkspaceState,
) -> Result<()> {
    if state.device.private_key_pem.starts_with(ENC_PREFIX) {
        let key = load_existing_workspace_key(workspace_slug)?;
        state.device.private_key_pem = decrypt_private_key(&key, &state.device.private_key_pem)?;
    }
    Ok(())
}

fn decrypt_private_key(key_bytes: &[u8; 32], encrypted: &str) -> Result<String> {
    let blob = decode_encrypted_blob(encrypted, "private key")?;
    let (nonce_bytes, ciphertext) = blob.split_at(12);
    let key = Key::<Aes256Gcm>::from_slice(key_bytes);
    let cipher = Aes256Gcm::new(key);
    let nonce = Nonce::from_slice(nonce_bytes);
    let plaintext = cipher
        .decrypt(nonce, ciphertext)
        .map_err(|_| anyhow!("decrypt private key"))?;
    Ok(String::from_utf8(plaintext)?)
}

fn decode_encrypted_blob(encrypted: &str, label: &str) -> Result<Vec<u8>> {
    let encoded = encrypted
        .strip_prefix(ENC_PREFIX)
        .ok_or_else(|| anyhow!("{} is not encrypted with enc1", label))?;
    let blob = B64.decode(encoded.trim())?;
    if blob.len() < 12 {
        return Err(anyhow!("encrypted {} is too short", label));
    }
    Ok(blob)
}

fn state_aad(workspace_slug: &str) -> String {
    format!(
        "zecurity-client-state:v{}:{}:{}",
        STATE_ENVELOPE_VERSION,
        appmeta::SCHEMA_VERSION,
        sanitize_workspace_slug(workspace_slug)
    )
}

fn write_secure(path: &Path, data: &[u8]) -> Result<()> {
    reject_symlink(path)?;
    if let Some(parent) = path.parent() {
        ensure_secure_dir(parent)?;
    }

    let temp_path = temp_path_for(path);
    let write_result = (|| -> Result<()> {
        let mut file = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temp_path)
            .with_context(|| format!("create temp file {}", temp_path.display()))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            file.set_permissions(fs::Permissions::from_mode(0o600))?;
        }
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&temp_path, path)
            .with_context(|| format!("rename {} to {}", temp_path.display(), path.display()))?;
        sync_parent_dir(path)?;
        Ok(())
    })();

    if write_result.is_err() {
        let _ = fs::remove_file(&temp_path);
    }
    write_result?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    }
    Ok(())
}

fn temp_path_for(path: &Path) -> PathBuf {
    let mut nonce = [0u8; 8];
    rand::rngs::OsRng.fill_bytes(&mut nonce);
    let suffix = format!(".tmp.{}.{}", std::process::id(), u64::from_le_bytes(nonce));
    let file_name = path
        .file_name()
        .map(|name| format!("{}{}", name.to_string_lossy(), suffix))
        .unwrap_or_else(|| format!("zecurity-client{}", suffix));
    path.with_file_name(file_name)
}

fn sync_parent_dir(path: &Path) -> Result<()> {
    if let Some(parent) = path.parent() {
        let dir = fs::File::open(parent)
            .with_context(|| format!("open parent directory {}", parent.display()))?;
        dir.sync_all()
            .with_context(|| format!("sync parent directory {}", parent.display()))?;
    }
    Ok(())
}

fn remove_if_exists(path: &Path) -> Result<bool> {
    reject_symlink(path)?;
    match fs::remove_file(path) {
        Ok(()) => Ok(true),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(false),
        Err(err) => Err(err).with_context(|| format!("remove {}", path.display())),
    }
}

fn ensure_secure_dir(path: &Path) -> Result<()> {
    match fs::symlink_metadata(path) {
        Ok(metadata) => {
            if metadata.file_type().is_symlink() {
                return Err(anyhow!(
                    "refusing to use symlinked directory {}",
                    path.display()
                ));
            }
            if !metadata.is_dir() {
                return Err(anyhow!("{} is not a directory", path.display()));
            }
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir_all(path)
                .with_context(|| format!("create directory {}", path.display()))?;
        }
        Err(err) => return Err(err).with_context(|| format!("inspect {}", path.display())),
    }

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

fn reject_symlink(path: &Path) -> Result<()> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() => {
            Err(anyhow!("refusing to use symlinked path {}", path.display()))
        }
        Ok(_) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err).with_context(|| format!("inspect {}", path.display())),
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
