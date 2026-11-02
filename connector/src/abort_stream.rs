//! FU-15: ending a device stream abruptly when its session's authorization
//! is revoked.
//!
//! Dropping a device stream ends it *normally*: quinn finishes a dropped
//! `SendStream`, so everything the connector had queued still reaches the
//! client, which then hands its own backlog to the app. After a member
//! removal a rate-limited download kept receiving 5.6 MB for 18.6 s (live E2E
//! run, O-6). `abort` discards instead: QUIC reset (the peer drops what it
//! hasn't delivered and sees an error), or a TCP RST on the direct TLS path.

use std::time::Duration;

/// QUIC application error code for a session whose authorization was revoked
/// ("RVKD").
pub const SESSION_REVOKED_CODE: u32 = 0x5256_4b44;

/// A device stream that can be ended abruptly. Best effort: a stream that is
/// already closed has nothing left to discard.
pub trait AbortStream {
    fn abort(&mut self);
}

/// Direct QUIC device stream (`quic_listener`).
impl AbortStream for tokio::io::Join<quinn::RecvStream, quinn::SendStream> {
    fn abort(&mut self) {
        let code = quinn::VarInt::from_u32(SESSION_REVOKED_CODE);
        let _ = self.writer_mut().reset(code);
        let _ = self.reader_mut().stop(code);
    }
}

/// Direct TLS-over-TCP device stream: an RST when the socket is dropped.
impl AbortStream for tokio::net::TcpStream {
    fn abort(&mut self) {
        let _ = self.set_zero_linger();
    }
}

/// Inner mTLS stream (relay path, TLS over TCP): abort the transport under it.
impl<IO: AbortStream> AbortStream for tokio_rustls::server::TlsStream<IO> {
    fn abort(&mut self) {
        self.get_mut().0.abort();
    }
}

/// In-memory streams (tests): nothing to discard.
impl AbortStream for tokio::io::DuplexStream {
    fn abort(&mut self) {}
}

/// How long `relay_stream` waits for its device→shield task to hand the read
/// half back after a cancel before giving up on the abort.
pub const ABORT_REJOIN_GRACE: Duration = Duration::from_secs(1);
