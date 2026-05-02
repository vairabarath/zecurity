use std::collections::HashSet;
use std::sync::{Arc, RwLock};
use std::time::Duration;

use tokio::time::interval;
use tracing::warn;
use x509_parser::prelude::*;

/// Caches revoked certificate serials fetched from the controller's /ca.crl endpoint.
///
/// The controller serves `GET /ca.crl?workspace_id=<uuid>` → DER-encoded CRL signed
/// by the workspace CA. Connector fetches on startup then refreshes every 5 minutes.
///
/// M4 calls `is_revoked(serial_bytes)` inside `device_tunnel::handle_stream` after
/// extracting the peer cert serial from the TLS handshake.
#[derive(Clone, Debug, Default)]
pub struct CrlManager {
    revoked: Arc<RwLock<HashSet<Vec<u8>>>>,
}

impl CrlManager {
    pub fn new() -> Self {
        Self::default()
    }

    /// Returns true if `serial` (raw big-endian bytes from the peer cert) is revoked.
    pub fn is_revoked(&self, serial: &[u8]) -> bool {
        self.revoked.read().unwrap().contains(serial)
    }

    /// Fetch the DER CRL from `url`, parse it, and replace the cached revoked set.
    ///
    /// Errors are non-fatal — the existing cache is kept on failure so a transient
    /// network blip does not grant access to revoked devices.
    pub async fn refresh(&self, url: &str) -> anyhow::Result<()> {
        let bytes = reqwest::get(url)
            .await
            .map_err(|e| anyhow::anyhow!("CRL fetch error: {e}"))?
            .error_for_status()
            .map_err(|e| anyhow::anyhow!("CRL fetch HTTP error: {e}"))?
            .bytes()
            .await
            .map_err(|e| anyhow::anyhow!("CRL body read error: {e}"))?;

        let (_, crl) = parse_x509_crl(&bytes)
            .map_err(|e| anyhow::anyhow!("CRL parse error: {:?}", e))?;

        let serials: HashSet<Vec<u8>> = crl
            .iter_revoked_certificates()
            .map(|r| r.raw_serial().to_vec())
            .collect();

        *self.revoked.write().unwrap() = serials;
        Ok(())
    }

    /// Spawn a background task that calls `refresh` every `interval_secs`.
    /// Logs a warning on failure but never panics — stale cache is kept.
    pub fn spawn_refresh(self, url: String, interval_secs: u64) {
        tokio::spawn(async move {
            let mut tick = interval(Duration::from_secs(interval_secs));
            loop {
                tick.tick().await;
                if let Err(e) = self.refresh(&url).await {
                    warn!("CRL refresh failed: {e}");
                }
            }
        });
    }
}
