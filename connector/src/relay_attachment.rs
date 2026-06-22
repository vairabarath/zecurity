// Shared state describing the connector's current relay attachment.
//
// Written by relay_client::maintain_registration (on register success / on
// any session-ending error) and read by control_stream::run_once when it
// builds each ConnectorHealthReport. The lifecycle-change emitter in
// relay_client.rs also produces a ConnectorRelayState message on every
// write; the heartbeat carries the same data as a self-healing backstop.

use std::sync::Arc;

use tokio::sync::RwLock;

#[derive(Debug, Clone)]
pub struct RelayAttachment {
    pub relay_id: String,
    pub relay_spiffe_id: String,
    pub attached_at: i64, // unix seconds
}

/// Single shared slot — `None` means the connector is not currently attached
/// to any relay. RwLock keeps the heartbeat hot-path cheap (read-only).
pub type RelayAttachmentSlot = Arc<RwLock<Option<RelayAttachment>>>;

pub fn new_slot() -> RelayAttachmentSlot {
    Arc::new(RwLock::new(None))
}
