use thiserror::Error;

#[derive(Debug, Error)]
pub enum ClientError {
    #[error("Not configured. Run `zecurity-client setup --workspace <name>` first.")]
    NotConfigured,
    #[error("Not connected. Run `zecurity-client connect` first.")]
    NotConnected,
    #[error("IO error: {0}")]
    Io(#[from] std::io::Error),
    #[error("Config parse error: {0}")]
    Toml(#[from] toml::de::Error),
    #[error("{0}")]
    Other(String),
}
