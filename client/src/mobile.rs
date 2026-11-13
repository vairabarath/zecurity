//! Mobile (Android and iOS) integration points, set once by the app's
//! native library (`client/ffi`) before anything else runs.
//!
//! - The app's files directory, which replaces the Linux config, state,
//!   socket and log locations (`dirs` has none of them on mobile). On iOS it
//!   is the App Group container shared by the app and its tunnel extension.
//! - The TUN provider: only the platform's VPN API (Android `VpnService`,
//!   iOS `NEPacketTunnelProvider`) can create the VPN interface, and the
//!   data plane (`tun_mobile.rs`) asks the app for one routing exactly the
//!   allowed resource IPs.
//! - The TUN release hook (iOS): the interface belongs to the extension and
//!   outlives the daemon's copy of it, so tunnel-down must remove its routes
//!   or resource traffic would be black-holed. Android needs none: closing
//!   the descriptor tears the interface down.
//! - The URL opener: login opens the identity provider in a browser, which
//!   only the app can do.
//! - The OS version (iOS), for the posture report.
//!
//! These are `OnceLock`s rather than environment variables: the platform's
//! threads already run when the library is loaded, and setenv is not
//! thread-safe against getenv.

use std::net::Ipv4Addr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::OnceLock;

use anyhow::{anyhow, Result};

/// Creates (or, on iOS, reconfigures) the VPN interface routing exactly
/// `routes` (one /32 each) and returns a file descriptor the caller owns.
pub type TunProvider = Box<dyn Fn(&[Ipv4Addr]) -> Result<i32> + Send + Sync>;

/// Removes the VPN interface's routes (iOS).
pub type TunRelease = Box<dyn Fn() + Send + Sync>;

/// Opens a URL in a browser.
pub type UrlOpener = Box<dyn Fn(&str) + Send + Sync>;

/// One posture check read by the platform (ADR-031 D-4): check id, status
/// (`PASS`, `FAIL`, `UNSUPPORTED`, `ERROR`) and a short detail.
pub type PlatformCheck = (String, String, String);

/// Reads the platform's posture checks (Android: the VPN service, Kotlin).
pub type PostureProvider = Box<dyn Fn() -> Vec<PlatformCheck> + Send + Sync>;

static FILES_DIR: OnceLock<PathBuf> = OnceLock::new();
static TUN_PROVIDER: OnceLock<TunProvider> = OnceLock::new();
static TUN_RELEASE: OnceLock<TunRelease> = OnceLock::new();
static URL_OPENER: OnceLock<UrlOpener> = OnceLock::new();
static OS_VERSION: OnceLock<String> = OnceLock::new();
static POSTURE_PROVIDER: OnceLock<PostureProvider> = OnceLock::new();
/// Number of the latest VPN interface handed out; 0 = none yet.
static TUN_GENERATION: AtomicU64 = AtomicU64::new(0);

/// Sets the app's files directory. The first call wins.
pub fn init(files_dir: PathBuf) {
    let _ = FILES_DIR.set(files_dir);
}

/// The app's files directory. Before `init`, a path that exists nowhere, so
/// nothing is ever written to the process's working directory.
pub fn files_dir() -> PathBuf {
    FILES_DIR
        .get()
        .cloned()
        .unwrap_or_else(|| PathBuf::from("/nonexistent/zecurity-not-initialised"))
}

/// Registers the function that creates the VPN interface (tunnel process).
pub fn set_tun_provider(provider: TunProvider) {
    let _ = TUN_PROVIDER.set(provider);
}

/// Registers the function that removes the interface's routes (iOS).
pub fn set_tun_release(release: TunRelease) {
    let _ = TUN_RELEASE.set(release);
}

/// Registers the function that opens the login URL (UI process).
pub fn set_url_opener(opener: UrlOpener) {
    let _ = URL_OPENER.set(opener);
}

/// Registers the function that reads the platform's posture checks.
pub fn set_posture_provider(provider: PostureProvider) {
    let _ = POSTURE_PROVIDER.set(provider);
}

/// The platform's posture checks; none before a provider is registered.
pub(crate) fn platform_posture() -> Vec<PlatformCheck> {
    POSTURE_PROVIDER.get().map(|p| p()).unwrap_or_default()
}

/// Sets the OS version reported in posture (iOS; Android reads it itself).
pub fn set_os_version(version: String) {
    let _ = OS_VERSION.set(version);
}

#[cfg_attr(target_os = "android", allow(dead_code))]
pub(crate) fn os_version() -> Option<String> {
    OS_VERSION.get().cloned()
}

/// Asks the app for a VPN interface routing `routes`. Returns the file
/// descriptor and the interface's number, which `release_tun` takes.
pub(crate) fn establish_tun(routes: &[Ipv4Addr]) -> Result<(i32, u64)> {
    let provider = TUN_PROVIDER
        .get()
        .ok_or_else(|| anyhow!("no VPN service is attached to the daemon"))?;
    let fd = provider(routes)?;
    let generation = TUN_GENERATION.fetch_add(1, Ordering::SeqCst) + 1;
    Ok((fd, generation))
}

/// The tunnel built on interface `generation` went down: remove the routes,
/// unless a newer interface has replaced it since (a tunnel restart may
/// drop the old manager after the new one is up).
pub(crate) fn release_tun(generation: u64) {
    if generation == 0 || TUN_GENERATION.load(Ordering::SeqCst) != generation {
        return;
    }
    if let Some(release) = TUN_RELEASE.get() {
        release();
    }
}

pub(crate) fn open_url(url: &str) {
    match URL_OPENER.get() {
        Some(open) => open(url),
        None => tracing::warn!("no URL opener registered; cannot open the login page"),
    }
}
