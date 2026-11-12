mod appmeta;
mod auth;
mod change_watch;
mod config;
mod crl;
mod daemon;
mod dns;
mod dns_link;
#[cfg(test)]
mod daemon_tests;
mod error;
mod grpc;
mod ipc;
mod login;
mod name_map;
mod net_stack;
mod relay_pool;
mod runtime;
mod state_store;
mod stream_watch;
mod transport;
mod trust;
// The TUN data plane: nftables/fwmark/netlink on Linux, Wintun + per-IP
// routes on Windows. Both expose the same surface (see tun_windows.rs).
#[cfg_attr(windows, path = "tun_windows.rs")]
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
mod win_service;
#[cfg(target_os = "linux")]
mod tpm;

mod cmd {
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

use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(name = "zecurity-client", about = "Zecurity ZTNA client")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Write workspace name (and optional dev overrides) to the client config
    /// (/etc/zecurity/client.conf; %ProgramData%\Zecurity\client.conf on Windows)
    Setup {
        #[arg(long)]
        workspace: String,
        /// Dev only: override compiled-in controller address
        #[arg(long)]
        controller: Option<String>,
        /// Dev only: override compiled-in connector address
        #[arg(long)]
        connector: Option<String>,
        /// Dev only: override compiled-in controller HTTP base URL
        #[arg(long = "http-base")]
        http_base: Option<String>,
    },
    /// Authenticate via OAuth, enroll device cert, and save encrypted local state
    Login,
    /// Show current connection status
    Status,
    /// List resources this device has access to
    Resources,
    /// Refresh ACL/resource snapshot from the controller
    Sync,
    /// Clear saved session and device state
    Logout,
    /// Connect to Zecurity and make resources accessible by IP
    Up,
    /// Disconnect and remove resource routes
    Down,
    /// Run as background daemon (launched by systemd, or the Windows Service
    /// Control Manager — not for direct use)
    #[command(hide = true)]
    Daemon {
        /// Windows: SID of the enrolling user, the one non-admin account the
        /// IPC pipe and data directory admit. Set in the service command line
        /// by client-install.ps1 (admin-only); ignored on Linux.
        #[arg(long)]
        ipc_user_sid: Option<String>,
    },
}

fn main() -> anyhow::Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    let cli = Cli::parse();

    // On Windows the daemon is a service: the Service Control Manager's
    // dispatcher must own this thread, and it starts its own runtime.
    #[cfg(windows)]
    if let Commands::Daemon { ipc_user_sid } = cli.command {
        return win_service::run(ipc_user_sid);
    }

    // What #[tokio::main] expanded to before the Windows port.
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .expect("failed building the Runtime")
        .block_on(run_command(cli))
}

async fn run_command(cli: Cli) -> anyhow::Result<()> {
    match cli.command {
        Commands::Setup {
            workspace,
            controller,
            connector,
            http_base,
        } => cmd::setup::run(workspace, controller, connector, http_base).await,
        Commands::Login => cmd::login::run().await,
        Commands::Status => cmd::status::run().await,
        Commands::Resources => cmd::resources::run().await,
        Commands::Sync => cmd::sync::run().await,
        Commands::Logout => cmd::logout::run().await,
        Commands::Up => cmd::up::run().await,
        Commands::Down => cmd::down::run().await,
        Commands::Daemon { .. } => daemon::run().await,
    }
}

/// Ends the daemon process. On Windows it first reports the service as
/// stopped, so the Service Control Manager does not treat a deliberate exit
/// (Shutdown request, revoked device) as a crash and restart it; on Linux it
/// is `std::process::exit`, as before.
pub(crate) fn exit_daemon(code: i32) -> ! {
    #[cfg(windows)]
    win_service::report_stopped(code);
    std::process::exit(code)
}
