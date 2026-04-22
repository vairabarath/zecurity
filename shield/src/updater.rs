// updater.rs — Auto-update mechanism for the ZECURITY Shield
//
// Periodically checks GitHub releases for newer shield versions:
//   1. If auto_update_enabled is false → return immediately
//   2. Random startup delay 0–3600s (prevent thundering herd)
//   3. Every UPDATE_CHECK_INTERVAL_SECS (7 days):
//      a. GET GitHub releases API → parse tag_name
//      b. semver compare: latest > current? No → sleep and retry
//      c. Download binary + checksums.txt
//      d. Verify SHA-256 — mismatch → abort, binary unchanged
//      e. Backup old binary → replace → restart service
//      f. Health check after 10s → remove backup or rollback
//
// Called by: main.rs via tokio::spawn(updater::run_update_loop(&cfg))
// Also called by the systemd oneshot unit via --check-update.

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

use crate::config::ShieldConfig;

const GITHUB_OWNER: &str = "vairabarath";
const GITHUB_REPO: &str = "zecurity";
const TAG_PREFIX: &str = "shield-v";
const SYSTEMD_SERVICE: &str = "zecurity-shield";
const HEALTH_CHECK_DELAY_SECS: u64 = 10;
const MAX_STARTUP_DELAY_SECS: u64 = 3600;
const UPDATE_CHECK_INTERVAL_SECS: u64 = 7 * 24 * 60 * 60;
const DEFAULT_INSTALL_PATH: &str = "/usr/local/bin/zecurity-shield";

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

pub async fn run_update_loop(cfg: &ShieldConfig) -> Result<()> {
    if !cfg.auto_update_enabled {
        info!("auto-update is disabled");
        return Ok(());
    }

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

    let client = reqwest::Client::builder()
        .user_agent(format!("zecurity-shield/{}", env!("CARGO_PKG_VERSION")))
        .timeout(Duration::from_secs(120))
        .build()
        .context("build HTTP client")?;

    loop {
        sleep(Duration::from_secs(UPDATE_CHECK_INTERVAL_SECS)).await;

        info!("checking for updates");
        if let Err(e) = check_and_update(&client).await {
            error!(error = %e, "update check failed");
        }
    }
}

pub async fn run_single_check() -> Result<()> {
    let client = reqwest::Client::builder()
        .user_agent(format!("zecurity-shield/{}", env!("CARGO_PKG_VERSION")))
        .timeout(Duration::from_secs(120))
        .build()
        .context("build HTTP client")?;

    check_and_update(&client).await
}

async fn check_and_update(client: &reqwest::Client) -> Result<()> {
    let release = fetch_latest_release(client).await?;

    let latest_version = parse_version_from_tag(&release.tag_name)?;
    let current_version: semver::Version = env!("CARGO_PKG_VERSION")
        .parse()
        .context("parse current CARGO_PKG_VERSION as semver")?;

    if latest_version <= current_version {
        info!(
            current = %current_version,
            latest = %latest_version,
            "shield is up to date"
        );
        return Ok(());
    }

    info!(
        current = %current_version,
        latest = %latest_version,
        "newer version available — starting update"
    );

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

    let install_path = binary_install_path();
    let tmp_path = install_path.with_extension("tmp");
    let backup_path = install_path.with_extension("bak");

    download_to_file(client, &binary_asset.browser_download_url, &tmp_path).await?;

    let checksums_text = client
        .get(&checksums_asset.browser_download_url)
        .send()
        .await
        .context("download checksums.txt")?
        .text()
        .await
        .context("read checksums.txt body")?;

    if let Err(e) = verify_checksum(&tmp_path, &checksums_text, &binary_name) {
        let _ = fs::remove_file(&tmp_path);
        return Err(e);
    }

    info!("SHA-256 checksum verified");

    replace_binary(&tmp_path, &install_path, &backup_path)?;

    info!(
        install_path = %install_path.display(),
        "binary replaced — triggering restart"
    );

    spawn_restart_with_rollback(&install_path, &backup_path)?;

    Ok(())
}

/// Fetch the latest shield release metadata from GitHub.
///
/// Do not use `/releases/latest`: this repository publishes connector and
/// shield independently, and GitHub's global "latest" can point at either one.
async fn fetch_latest_release(client: &reqwest::Client) -> Result<GitHubRelease> {
    let url = format!(
        "https://api.github.com/repos/{}/{}/releases?per_page=100",
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

    let releases: Vec<GitHubRelease> = resp.json().await.context("parse GitHub releases JSON")?;
    releases
        .into_iter()
        .filter_map(|release| {
            let version = parse_version_from_tag(&release.tag_name).ok()?;
            Some((version, release))
        })
        .max_by(|(a, _), (b, _)| a.cmp(b))
        .map(|(_, release)| release)
        .context("no shield release found")
}

fn parse_version_from_tag(tag: &str) -> Result<semver::Version> {
    let version_str = tag.strip_prefix(TAG_PREFIX).context(format!(
        "tag '{}' does not start with '{}'",
        tag, TAG_PREFIX
    ))?;

    version_str
        .parse()
        .context(format!("parse '{}' as semver", version_str))
}

fn platform_binary_name() -> Result<String> {
    let arch = std::env::consts::ARCH;
    let suffix = match arch {
        "x86_64" => "amd64",
        "aarch64" => "arm64",
        _ => bail!("unsupported architecture: {}", arch),
    };
    Ok(format!("shield-linux-{}", suffix))
}

fn binary_install_path() -> PathBuf {
    std::env::current_exe().unwrap_or_else(|_| PathBuf::from(DEFAULT_INSTALL_PATH))
}

async fn download_to_file(client: &reqwest::Client, url: &str, dest: &Path) -> Result<()> {
    info!(url = %url, dest = %dest.display(), "downloading");
    let mut resp = client
        .get(url)
        .send()
        .await
        .with_context(|| format!("GET {}", url))?;

    if !resp.status().is_success() {
        bail!("download failed: HTTP {} from {}", resp.status(), url);
    }

    let mut file = fs::File::create(dest).with_context(|| format!("create {}", dest.display()))?;

    while let Some(chunk) = resp.chunk().await.context("read response chunk")? {
        file.write_all(&chunk)
            .with_context(|| format!("write {}", dest.display()))?;
    }

    Ok(())
}

fn verify_checksum(file_path: &Path, checksums: &str, binary_name: &str) -> Result<()> {
    let expected = checksums
        .lines()
        .find_map(|line| {
            let mut parts = line.split_whitespace();
            let hash = parts.next()?;
            let name = parts.next()?;
            (name == binary_name).then_some(hash.to_string())
        })
        .context(format!(
            "no checksum entry for '{}' in checksums.txt",
            binary_name
        ))?;

    let actual = {
        let bytes = fs::read(file_path).with_context(|| format!("read {}", file_path.display()))?;
        hex::encode(Sha256::digest(&bytes))
    };

    if expected.to_lowercase() != actual.to_lowercase() {
        bail!(
            "SHA-256 mismatch for {}: expected {}, got {}",
            binary_name,
            expected,
            actual
        );
    }

    Ok(())
}

fn replace_binary(tmp_path: &Path, install_path: &Path, backup_path: &Path) -> Result<()> {
    if install_path.exists() {
        fs::rename(install_path, backup_path).with_context(|| {
            format!(
                "backup {} to {}",
                install_path.display(),
                backup_path.display()
            )
        })?;
    }

    fs::rename(tmp_path, install_path).with_context(|| {
        format!(
            "replace {} with {}",
            install_path.display(),
            tmp_path.display()
        )
    })?;

    let mut perms = fs::metadata(install_path)
        .with_context(|| format!("stat {}", install_path.display()))?
        .permissions();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        perms.set_mode(0o755);
        fs::set_permissions(install_path, perms)
            .with_context(|| format!("chmod 0755 {}", install_path.display()))?;
    }

    Ok(())
}

fn spawn_restart_with_rollback(install_path: &Path, backup_path: &Path) -> Result<()> {
    let script = format!(
        r#"sleep {delay}
if systemctl restart {service}; then
  sleep {delay}
  if systemctl is-active --quiet {service}; then
    rm -f {backup}
    exit 0
  fi
fi

if [ -f {backup} ]; then
  mv {backup} {install}
  systemctl restart {service} || true
fi
"#,
        delay = HEALTH_CHECK_DELAY_SECS,
        service = SYSTEMD_SERVICE,
        backup = shell_escape_path(backup_path),
        install = shell_escape_path(install_path),
    );

    std::process::Command::new("sh")
        .arg("-c")
        .arg(script)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .context("spawn detached restart/rollback helper")?;

    Ok(())
}

fn shell_escape_path(path: &Path) -> String {
    let s = path.display().to_string().replace('\'', "'\\''");
    format!("'{}'", s)
}
