//! Android integration points, set once by the app's native library
//! (`client/android`) before anything else runs.
//!
//! - The app's private files directory, which replaces the Linux config,
//!   state, socket and log locations (`dirs` has none of them on Android).
//! - The TUN provider: only Android's `VpnService` can create a VPN
//!   interface, and its routes are fixed when it is created, so the data
//!   plane (`tun_android.rs`) asks the app for an interface with exactly the
//!   allowed resource routes and gets back its file descriptor.
//! - The URL opener: login opens the identity provider in the browser,
//!   which only the app can do.
//!
//! These are `OnceLock`s rather than environment variables: the JVM and
//! Flutter threads already run when the library is loaded, and setenv is not
//! thread-safe against getenv on bionic.

use std::net::Ipv4Addr;
use std::path::PathBuf;
use std::sync::OnceLock;

use anyhow::{anyhow, Result};

/// Creates the VPN interface routing exactly `routes` (one /32 each) and
/// returns its file descriptor, owned by the caller from then on.
pub type TunProvider = Box<dyn Fn(&[Ipv4Addr]) -> Result<i32> + Send + Sync>;

/// Opens a URL in the user's browser.
pub type UrlOpener = Box<dyn Fn(&str) + Send + Sync>;

static FILES_DIR: OnceLock<PathBuf> = OnceLock::new();
static TUN_PROVIDER: OnceLock<TunProvider> = OnceLock::new();
static URL_OPENER: OnceLock<UrlOpener> = OnceLock::new();

/// Sets the app's private files directory. The first call wins.
pub fn init(files_dir: PathBuf) {
    let _ = FILES_DIR.set(files_dir);
}

/// The app's private files directory. Before `init`, a path that exists
/// nowhere, so nothing is ever written to the process's working directory.
pub fn files_dir() -> PathBuf {
    FILES_DIR
        .get()
        .cloned()
        .unwrap_or_else(|| PathBuf::from("/nonexistent/zecurity-not-initialised"))
}

/// Registers the function that creates the VPN interface (service process).
pub fn set_tun_provider(provider: TunProvider) {
    let _ = TUN_PROVIDER.set(provider);
}

/// Registers the function that opens the login URL (UI process).
pub fn set_url_opener(opener: UrlOpener) {
    let _ = URL_OPENER.set(opener);
}

pub(crate) fn establish_tun(routes: &[Ipv4Addr]) -> Result<i32> {
    let provider = TUN_PROVIDER
        .get()
        .ok_or_else(|| anyhow!("no VPN service is attached to the daemon"))?;
    provider(routes)
}

pub(crate) fn open_url(url: &str) {
    match URL_OPENER.get() {
        Some(open) => open(url),
        None => tracing::warn!("no URL opener registered; cannot open the login page"),
    }
}
