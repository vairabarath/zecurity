// updater.rs — Auto-update mechanism for the ZECURITY connector
//
// Periodically checks GitHub releases for newer connector versions:
//   1. If auto_update_enabled is false → return immediately
//   2. Random startup delay 0–3600s (prevent thundering herd)
//   3. Every update_check_interval_secs (default 86400 = 24h):
//      a. GET GitHub releases API → parse tag_name
//      b. semver compare: latest > current? No → sleep and retry
//      c. Download binary + checksums.txt
//      d. Verify SHA-256 — mismatch → abort, binary unchanged
//      e. Backup old binary → replace → restart service
//      f. Health check after 10s → remove backup or rollback
//
// Called by: main.rs via tokio::spawn(updater::run_update_loop(&cfg))
// Dependencies: reqwest (HTTP), sha2 (checksum), semver (version compare)
//
// CRITICAL: SHA-256 verification must pass BEFORE the installed binary
// is touched. Any mismatch aborts the update with the old binary intact.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use tokio::time::sleep;
use tracing::{error, info};

use crate::config::ConnectorConfig;

// ── Constants ───────────────────────────────────────────────────────────────

/// GitHub repository owner (from git remote).
const GITHUB_OWNER: &str = "vairabarath";

/// GitHub repository name.
const GITHUB_REPO: &str = "zecurity";

/// Tag prefix used by Phase 10 CI (e.g., "connector-v0.2.0").
const TAG_PREFIX: &str = "connector-v";

/// systemd service name for restart/health-check.
const SYSTEMD_SERVICE: &str = "zecurity-connector";

/// Seconds to wait after restart before health-checking.
const HEALTH_CHECK_DELAY_SECS: u64 = 10;

/// Maximum random startup delay to prevent thundering herd.
const MAX_STARTUP_DELAY_SECS: u64 = 3600;

/// Fallback binary install path if current_exe() fails.
const DEFAULT_INSTALL_PATH: &str = "/usr/local/bin/zecurity-connector";

// ── GitHub API types ────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
struct GitHubRelease {
    tag_name: String,
    assets: Vec<GitHubAsset>,
}

#[derive(Debug, Deserialize)]
struct GitHubAsset {
    name: String,
    browser_download_url: String,
}

// ── Public entry point ──────────────────────────────────────────────────────

/// Run the auto-update loop. Called from main.rs as a spawned tokio task.
///
/// Returns Ok(()) immediately if auto_update_enabled is false.
/// Otherwise loops forever, checking for updates at the configured interval.
/// Errors from individual update checks are logged but never propagate —
/// the loop always continues to the next interval.
pub async fn run_update_loop(cfg: &ConnectorConfig) -> Result<()> {
    // Guard: if auto-update is disabled, return immediately.
    if !cfg.auto_update_enabled {
        info!("auto-update is disabled");
        return Ok(());
    }

    // Random startup delay 0–3600s to prevent thundering herd.
    // Uses PID + timestamp as a simple jitter source (no rand crate needed).
    let pid = std::process::id() as u64;
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .subsec_nanos() as u64;
    let delay_secs = pid.wrapping_mul(nanos) % MAX_STARTUP_DELAY_SECS;

    info!(
        delay_secs = delay_secs,
        "auto-updater starting after random delay"
    );
    sleep(Duration::from_secs(delay_secs)).await;

    // Create a reusable HTTP client for all requests.
    let client = reqwest::Client::builder()
        .user_agent(format!("zecurity-connector/{}", env!("CARGO_PKG_VERSION")))
        .timeout(Duration::from_secs(120))
        .build()
        .context("build HTTP client")?;

    // Main update loop — runs forever until the task is cancelled.
    loop {
        sleep(Duration::from_secs(cfg.update_check_interval_secs)).await;

        info!("checking for updates");
        if let Err(e) = check_and_update(&client).await {
            error!(error = %e, "update check failed");
        }
    }
}

/// Run a single update check and exit. Called when --check-update is passed.
///
/// This is the entry point for the systemd oneshot update service.
/// Returns Ok(()) after the check (whether or not an update was applied).
pub async fn run_single_check() -> Result<()> {
    let client = reqwest::Client::builder()
        .user_agent(format!("zecurity-connector/{}", env!("CARGO_PKG_VERSION")))
        .timeout(Duration::from_secs(120))
        .build()
        .context("build HTTP client")?;

    check_and_update(&client).await
}

// ── Core update logic ───────────────────────────────────────────────────────

/// Perform a single update check and apply if a newer version is available.
///
/// Steps:
///   a. Fetch latest GitHub release
///   b. Compare versions (semver)
///   c. Download binary + checksums.txt
///   d. Verify SHA-256
///   e. Backup → replace binary
///   f. Restart with health check + rollback
async fn check_and_update(client: &reqwest::Client) -> Result<()> {
    // Step a: Fetch latest release from GitHub.
    let release = fetch_latest_release(client).await?;

    // Step b: Compare versions.
    let latest_version = parse_version_from_tag(&release.tag_name)?;
    let current_version: semver::Version = env!("CARGO_PKG_VERSION")
        .parse()
        .context("parse current CARGO_PKG_VERSION as semver")?;

    if latest_version <= current_version {
        info!(
            current = %current_version,
            latest = %latest_version,
            "connector is up to date"
        );
        return Ok(());
    }

    info!(
        current = %current_version,
        latest = %latest_version,
        "newer version available — starting update"
    );

    // Step c: Determine platform and find assets.
    let binary_name = platform_binary_name()?;

    let binary_asset = release
        .assets
        .iter()
        .find(|a| a.name == binary_name)
        .context(format!("no asset named '{}' in release", binary_name))?;

    let checksums_asset = release
        .assets
        .iter()
        .find(|a| a.name == "checksums.txt")
        .context("no checksums.txt asset in release")?;

    // Download binary to temp file (same filesystem as install path for atomic rename).
    let install_path = binary_install_path();
    let tmp_path = install_path.with_extension("tmp");
    let backup_path = install_path.with_extension("bak");

    download_to_file(client, &binary_asset.browser_download_url, &tmp_path).await?;

    // Download checksums.txt to memory (small text file).
    let checksums_text = client
        .get(&checksums_asset.browser_download_url)
        .send()
        .await
        .context("download checksums.txt")?
        .text()
        .await
        .context("read checksums.txt body")?;

    // Step d: Verify SHA-256.
    if let Err(e) = verify_checksum(&tmp_path, &checksums_text, &binary_name) {
        // CRITICAL: delete temp file, never touch the installed binary.
        let _ = fs::remove_file(&tmp_path);
        return Err(e);
    }

    info!("SHA-256 checksum verified");

    // Step e: Backup current binary and replace.
    replace_binary(&tmp_path, &install_path, &backup_path)?;

    info!(
        install_path = %install_path.display(),
        "binary replaced — triggering restart"
    );

    // Step f: Spawn detached restart + health check + rollback process.
    spawn_restart_with_rollback(&install_path, &backup_path)?;

    Ok(())
}

// ── Helper functions ────────────────────────────────────────────────────────

/// Fetch the latest release metadata from GitHub.
async fn fetch_latest_release(client: &reqwest::Client) -> Result<GitHubRelease> {
    let url = format!(
        "https://api.github.com/repos/{}/{}/releases/latest",
        GITHUB_OWNER, GITHUB_REPO
    );

    let resp = client
        .get(&url)
        .header("Accept", "application/vnd.github+json")
        .send()
        .await
        .context("GET GitHub releases API")?;

    if !resp.status().is_success() {
        bail!("GitHub API returned HTTP {}", resp.status());
    }

    let release: GitHubRelease = resp.json().await.context("parse GitHub release JSON")?;

    Ok(release)
}

/// Extract a semver::Version from a tag like "connector-v0.2.0".
fn parse_version_from_tag(tag: &str) -> Result<semver::Version> {
    let version_str = tag.strip_prefix(TAG_PREFIX).context(format!(
        "tag '{}' does not start with '{}'",
        tag, TAG_PREFIX
    ))?;

    version_str
        .parse()
        .context(format!("parse '{}' as semver", version_str))
}

/// Return the platform-specific binary asset name.
/// Maps Rust arch constants to the naming convention from Phase 10 CI.
fn platform_binary_name() -> Result<String> {
    let arch = std::env::consts::ARCH;
    let suffix = match arch {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        _ => bail!("unsupported architecture: {}", arch),
    };
    Ok(format!("connector-linux-{}", suffix))
}

/// Resolve the path of the currently running binary.
fn binary_install_path() -> PathBuf {
    std::env::current_exe().unwrap_or_else(|_| PathBuf::from(DEFAULT_INSTALL_PATH))
}

/// Download a URL to a file, streaming to avoid holding the entire binary in memory.
async fn download_to_file(client: &reqwest::Client, url: &str, dest: &Path) -> Result<()> {
    info!(url = %url, dest = %dest.display(), "downloading");

    let resp = client
        .get(url)
        .send()
        .await
        .context("HTTP GET for binary download")?;

    if !resp.status().is_success() {
        bail!("download returned HTTP {}", resp.status());
    }

    let bytes = resp.bytes().await.context("read download body")?;

    let mut file =
        fs::File::create(dest).with_context(|| format!("create temp file {}", dest.display()))?;

    file.write_all(&bytes)
        .with_context(|| format!("write to {}", dest.display()))?;

    Ok(())
}

/// Compute SHA-256 of a file and return the hex-encoded digest.
fn sha256_file(path: &Path) -> Result<String> {
    let data =
        fs::read(path).with_context(|| format!("read file for checksum: {}", path.display()))?;

    let mut hasher = Sha256::new();
    hasher.update(&data);
    let digest = hasher.finalize();

    Ok(hex::encode(digest))
}

/// Parse checksums.txt and verify the SHA-256 of the downloaded file.
///
/// checksums.txt format (from sha256sum): "<hex_hash>  <filename>"
/// Returns Ok(()) if match, Err on mismatch or missing entry.
fn verify_checksum(file_path: &Path, checksums_text: &str, asset_name: &str) -> Result<()> {
    // Find the line matching the asset name.
    let expected_hash = checksums_text
        .lines()
        .find_map(|line| {
            // Format: "<hash>  <filename>" (two spaces)
            let parts: Vec<&str> = line.splitn(2, "  ").collect();
            if parts.len() == 2 && parts[1].trim() == asset_name {
                Some(parts[0].to_lowercase())
            } else {
                None
            }
        })
        .context(format!(
            "no checksum entry for '{}' in checksums.txt",
            asset_name
        ))?;

    let actual_hash = sha256_file(file_path)?;

    if actual_hash.to_lowercase() != expected_hash {
        bail!(
            "SHA-256 checksum MISMATCH — possible tampered binary!\n\
             expected: {}\n\
             actual:   {}\n\
             Aborting update. The installed binary is unchanged.",
            expected_hash,
            actual_hash
        );
    }

    Ok(())
}

/// Backup the current binary and replace it with the new one.
///
/// Checks write permission before attempting. Uses fs::rename for atomic
/// operations (requires same filesystem — guaranteed by downloading to .tmp
/// in the same directory).
fn replace_binary(new_binary: &Path, install_path: &Path, backup_path: &Path) -> Result<()> {
    // Check we can write to the install directory.
    if let Some(parent) = install_path.parent() {
        let md = fs::metadata(parent).context("check install directory")?;
        if md.permissions().readonly() {
            bail!(
                "install directory {} is read-only — cannot replace binary.\n\
                 Hint: use the systemd update timer (runs as root) or run with elevated privileges.",
                parent.display()
            );
        }
    }

    // Copy permissions from old binary to new binary.
    if install_path.exists() {
        let perms = fs::metadata(install_path)
            .context("read current binary permissions")?
            .permissions();
        fs::set_permissions(new_binary, perms).context("set permissions on new binary")?;
    }

    // Backup: rename current → .bak
    if install_path.exists() {
        fs::rename(install_path, backup_path).context("backup current binary")?;
        info!(backup = %backup_path.display(), "backed up current binary");
    }

    // Replace: rename .tmp → install path
    fs::rename(new_binary, install_path).context("move new binary into place")?;

    Ok(())
}

/// Spawn a detached process that restarts the service, health-checks,
/// and rolls back if the new binary fails.
///
/// Uses `setsid` to create a new session so the child survives
/// the current process being killed by systemctl restart.
fn spawn_restart_with_rollback(install_path: &Path, backup_path: &Path) -> Result<()> {
    let script = format!(
        r#"
SERVICE="{service}"
INSTALL_PATH="{install}"
BACKUP_PATH="{backup}"
LOG_TAG="zecurity-updater"

logger -t "$LOG_TAG" "restarting $SERVICE after update"
systemctl restart "$SERVICE" 2>/dev/null || true

sleep {delay}

if systemctl is-active --quiet "$SERVICE"; then
    logger -t "$LOG_TAG" "health check passed — removing backup"
    rm -f "$BACKUP_PATH"
else
    logger -t "$LOG_TAG" "health check FAILED — rolling back to previous version"
    mv "$BACKUP_PATH" "$INSTALL_PATH"
    systemctl restart "$SERVICE" 2>/dev/null || true
    logger -t "$LOG_TAG" "rollback complete"
fi
"#,
        service = SYSTEMD_SERVICE,
        install = install_path.display(),
        backup = backup_path.display(),
        delay = HEALTH_CHECK_DELAY_SECS,
    );

    // Spawn detached via setsid so the child outlives the current process.
    std::process::Command::new("setsid")
        .arg("bash")
        .arg("-c")
        .arg(&script)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .context("spawn restart-and-rollback process")?;

    info!("restart + health-check process spawned (detached)");

    Ok(())
}
