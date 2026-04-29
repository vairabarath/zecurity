mod appmeta;
mod config;
mod runtime;
mod error;
mod grpc;
mod login;
mod ipc;    // stub for Phase 4
mod cmd {
    pub mod setup;
    pub mod status;
    pub mod logout;
    pub mod login;    // stub
    pub mod invite;   // stub
    pub mod connect;  // stub
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
        #[arg(long)] workspace:  String,
        /// Dev only: override compiled-in controller address
        #[arg(long)] controller: Option<String>,
        /// Dev only: override compiled-in connector address
        #[arg(long)] connector:  Option<String>,
    },
    /// Authenticate and start the tunnel (long-running)
    Connect,
    /// Show current connection status (queries running daemon)
    Status,
    /// Stop the running daemon and clear the in-memory session
    Logout,
    /// Invite a user to the workspace (admin only)
    Invite {
        #[arg(long)] email: String,
    },
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Commands::Setup { workspace, controller, connector } =>
            cmd::setup::run(workspace, controller, connector).await,
        Commands::Connect =>
            cmd::connect::run().await,
        Commands::Status =>
            cmd::status::run().await,
        Commands::Logout =>
            cmd::logout::run().await,
        Commands::Invite { email } =>
            cmd::invite::run(email).await,
    }
}