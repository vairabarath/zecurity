use std::fs;
use std::path::Path;

/// Raw PEM material loaded from the connector state directory.
/// Passed to TLS server/client config builders so they don't need direct
/// filesystem access.
#[derive(Clone)]
pub struct CertStore {
    pub cert_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
    pub workspace_ca_pem: Vec<u8>,
}

impl CertStore {
    /// Load connector cert, key, and workspace CA from `state_dir`.
    pub fn load(state_dir: &str) -> anyhow::Result<Self> {
        let dir = Path::new(state_dir);
        Ok(Self {
            cert_pem: fs::read(dir.join("connector.crt"))?,
            key_pem: fs::read(dir.join("connector.key"))?,
            workspace_ca_pem: fs::read(dir.join("workspace_ca.crt"))?,
        })
    }
}
