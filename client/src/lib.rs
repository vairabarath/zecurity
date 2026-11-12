//! zecurity-client as a library: the whole client (CLI commands, daemon, data
//! plane), used by the `zecurity-client` binary (`main.rs`) and by the Android
//! app's native library (`client/android`).

mod appmeta;
mod auth;
mod change_watch;
pub mod config;
mod crl;
pub mod daemon;
mod dns;
mod dns_link;
#[cfg(test)]
mod daemon_tests;
mod error;
mod grpc;
pub mod ipc;
pub mod login;
mod name_map;
mod net_stack;
mod relay_pool;
mod runtime;
pub mod state_store;
mod stream_watch;
mod transport;
mod trust;
// The TUN data plane: nftables/fwmark/netlink on Linux, Wintun + per-IP
// routes on Windows, a VpnService interface on Android. All three expose the
// same surface (see tun_windows.rs, tun_android.rs).
#[cfg_attr(windows, path = "tun_windows.rs")]
#[cfg_attr(target_os = "android", path = "tun_android.rs")]
mod tun;
mod tun_shared;
// The Windows module's command renderers are tested on Linux too.
#[cfg(all(test, not(windows)))]
#[path = "tun_windows.rs"]
#[allow(dead_code)]
mod tun_windows_tests;
mod tunnel_pool;
#[cfg(any(windows, test))]
mod win_security;
#[cfg(windows)]
pub mod win_service;
#[cfg(target_os = "linux")]
mod tpm;

#[cfg(target_os = "android")]
pub mod android;

pub mod cmd {
    pub mod down;
    pub mod login;
    pub mod logout;
    pub mod resources;
    pub mod setup;
    pub mod status;
    pub mod sync;
    pub mod up;
}

mod posture;
// The Linux collectors' tests use Unix process APIs.
#[cfg(all(test, unix))]
mod posture_tests;

// Certificate audit bug 1 (client): bootstrap-trust tests and their fixtures.
#[cfg(test)]
mod bootstrap_tests;
#[cfg(test)]
mod test_controller;
#[cfg(test)]
mod test_pki;

/// Ends the daemon process. On Windows it first reports the service as
/// stopped, so the Service Control Manager does not treat a deliberate exit
/// (Shutdown request, revoked device) as a crash and restart it; on Linux it
/// is `std::process::exit`, as before.
pub(crate) fn exit_daemon(code: i32) -> ! {
    #[cfg(windows)]
    win_service::report_stopped(code);
    std::process::exit(code)
}
