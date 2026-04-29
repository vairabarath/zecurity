mod appmeta;
mod config;
mod error;
mod grpc;
mod login;
mod runtime;
mod state_store;
mod cmd {
    pub mod invite;
    pub mod login;
    pub mod logout;
    pub mod setup;
    pub mod status;
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
    /// Clear saved session and device state
    Logout,
    /// Invite a user to the workspace (admin only)
    Invite {
        #[arg(long)]
        email: String,
    },
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    let cli = Cli::parse();
    match cli.command {
        Commands::Setup {
            workspace,
            controller,
            connector,
            http_base,
        } => cmd::setup::run(workspace, controller, connector, http_base).await,
        Commands::Login => cmd::login::run().await,
        Commands::Status => cmd::status::run().await,
        Commands::Logout => cmd::logout::run().await,
        Commands::Invite { email } => cmd::invite::run(email).await,
    }
}
