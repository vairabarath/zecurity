mod appmeta;
mod config;
mod daemon;
mod error;
mod grpc;
mod ipc;
mod login;
mod net_stack;
mod runtime;
mod state_store;
mod tun;
mod tunnel_pool;
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

use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(name = "zecurity-client", about = "Zecurity ZTNA client")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Write workspace name (and optional dev overrides) to /etc/zecurity/client.conf
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
    /// Run as background daemon (launched by systemd — not for direct use)
    #[command(hide = true)]
    Daemon,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    let cli = Cli::parse(); // cli {command: Commands::Daemon}
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
        Commands::Daemon => daemon::run().await, //matches Commands::Daemon && calls run function in the daemon package
    }
}
