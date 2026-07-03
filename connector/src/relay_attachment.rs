// Shared state describing the connector's current relay attachment.
//
// A single `active` slot holds the relay the heartbeat reports to the
// controller. During make-before-break migration, `run_session` flips
// `active` to the new relay the instant registration on it succeeds, so
// new client streams route to the new relay immediately. The old relay's
// connection is drained and aborted independently by the selector — it is
// not tracked here.

use std::sync::Arc;

use tokio::sync::RwLock;

#[derive(Debug, Clone)]
pub struct RelayAttachment {
    pub relay_id: String,
    pub relay_spiffe_id: String,
    pub attached_at: i64, // unix seconds
}

#[derive(Debug, Default)]
struct Slot {
    active: Option<RelayAttachment>,
}

#[derive(Clone)]
pub struct RelayAttachmentSlot {
    inner: Arc<RwLock<Slot>>,
}

impl RelayAttachmentSlot {
    pub fn new() -> Self {
        Self {
            inner: Arc::new(RwLock::new(Slot::default())),
        }
    }

    /// Read the active attachment. The heartbeat uses this; `pending` is
    /// intentionally invisible to the controller.
    pub async fn active(&self) -> Option<RelayAttachment> {
        self.inner.read().await.active.clone()
    }

    /// Non-blocking variant for the heartbeat hot-path. Returns `None`
    /// if the lock is held — same fallback as the previous try_read pattern.
    pub fn try_active(&self) -> Option<RelayAttachment> {
        self.inner.try_read().ok().and_then(|g| g.active.clone())
    }

    pub async fn set_active(&self, attachment: Option<RelayAttachment>) {
        self.inner.write().await.active = attachment;
    }

    pub async fn clear_active_if_relay(&self, relay_id: &str) {
        let mut guard = self.inner.write().await;
        if guard
            .active
            .as_ref()
            .map(|active| active.relay_id.as_str() == relay_id)
            .unwrap_or(false)
        {
            guard.active = None;
        }
    }

}

pub fn new_slot() -> RelayAttachmentSlot {
    RelayAttachmentSlot::new()
}
