//! FU-16: a tunnel stream that knows when its QUIC transport has ended.
//!
//! Two gaps let data reach the application after the connector had cut a
//! tunnel (measured live, FU-16):
//!
//! 1. **Connection lost.** After a QUIC connection closes, quinn still hands
//!    out the data already buffered for a stream (up to its 1.25 MB receive
//!    window) and reports `ConnectionLost` only once it is drained. The relay
//!    kept forwarding that data at the application's pace: 0.84–1.1 MB for up
//!    to 30 s after a connector stop.
//! 2. **Reset not seen while the app is slow.** The relay task notices a
//!    stream reset only when it reads, and it doesn't read while its queue to
//!    the application is full.
//!
//! [`WatchedStream`] closes both without consuming data:
//! - a normal read checks the connection first: once it is closed, the read
//!   fails, so nothing buffered on a dead connection is handed out (QUIC: a
//!   closed connection abruptly ends every stream on it, like a reset);
//! - a **zero-length read** is a "watch": it completes only with the error
//!   that ends the tunnel (connection closed, or — through [`ResetAwareRecv`]
//!   — the stream reset), and stays pending otherwise. The relay task waits on
//!   it while it waits for room in its queue to the application.

use std::future::Future;
use std::io;
use std::pin::Pin;
use std::task::{Context, Poll};

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

/// Resolves when the transport under a tunnel stream is gone.
pub type TransportGone = Pin<Box<dyn Future<Output = ()> + Send>>;

/// [`TransportGone`] for a QUIC connection: resolves when it closes, for any
/// reason (peer close, idle timeout, local close).
pub fn quic_connection_gone(conn: &quinn::Connection) -> TransportGone {
    let conn = conn.clone();
    Box::pin(async move {
        conn.closed().await;
    })
}

fn transport_closed() -> io::Error {
    io::Error::new(io::ErrorKind::ConnectionAborted, "tunnel transport closed")
}

/// A tunnel stream that fails as soon as its transport is gone. See the
/// module documentation.
pub struct WatchedStream<S> {
    inner: S,
    gone: TransportGone,
    fired: bool,
    /// Pass zero-length "watch" reads to `inner` (for a [`ResetAwareRecv`]
    /// underneath). Off for an inner TLS stream, which would read and buffer
    /// records to answer it.
    watch_inner: bool,
}

impl<S> WatchedStream<S> {
    pub fn new(inner: S, gone: TransportGone, watch_inner: bool) -> Self {
        Self {
            inner,
            gone,
            fired: false,
            watch_inner,
        }
    }

    /// Whether the transport is gone (registers `cx` for when it goes).
    fn poll_gone(&mut self, cx: &mut Context<'_>) -> bool {
        if !self.fired && self.gone.as_mut().poll(cx).is_ready() {
            self.fired = true;
        }
        self.fired
    }
}

impl<S: AsyncRead + Unpin> AsyncRead for WatchedStream<S> {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let this = &mut *self;
        if this.poll_gone(cx) {
            return Poll::Ready(Err(transport_closed()));
        }
        if buf.remaining() == 0 {
            // A watch: only an error ends it.
            if this.watch_inner {
                if let Poll::Ready(Err(e)) = Pin::new(&mut this.inner).poll_read(cx, buf) {
                    return Poll::Ready(Err(e));
                }
            }
            return Poll::Pending;
        }
        Pin::new(&mut this.inner).poll_read(cx, buf)
    }
}

impl<S: AsyncWrite + Unpin> AsyncWrite for WatchedStream<S> {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write(cx, buf)
    }
    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_flush(cx)
    }
    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.inner).poll_shutdown(cx)
    }
    fn poll_write_vectored(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        bufs: &[io::IoSlice<'_>],
    ) -> Poll<io::Result<usize>> {
        Pin::new(&mut self.inner).poll_write_vectored(cx, bufs)
    }
    fn is_write_vectored(&self) -> bool {
        self.inner.is_write_vectored()
    }
}

/// A quinn receive stream whose zero-length read reports a peer reset
/// without consuming data (quinn's cancel-safe `received_reset`). Normal reads
/// are unchanged.
pub struct ResetAwareRecv(pub quinn::RecvStream);

impl AsyncRead for ResetAwareRecv {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        if buf.remaining() > 0 {
            return Pin::new(&mut self.0).poll_read(cx, buf);
        }
        let reset = self.0.received_reset();
        tokio::pin!(reset);
        match reset.poll(cx) {
            Poll::Ready(Ok(Some(code))) => Poll::Ready(Err(io::Error::new(
                io::ErrorKind::ConnectionReset,
                format!("tunnel stream reset by peer: {code}"),
            ))),
            // Finished by the peer with all data received, or stopped: no
            // reset to report.
            Poll::Ready(Ok(None)) => Poll::Ready(Ok(())),
            Poll::Ready(Err(e)) => {
                Poll::Ready(Err(io::Error::new(io::ErrorKind::ConnectionAborted, e)))
            }
            Poll::Pending => Poll::Pending,
        }
    }
}

/// Resolves with the error that ends the tunnel under `recv` (a zero-length
/// "watch" read); never resolves while the tunnel is up. Consumes no data.
pub async fn tunnel_ended<R: AsyncRead + Unpin>(recv: &mut R) -> io::Error {
    std::future::poll_fn(|cx| {
        let mut empty: [u8; 0] = [];
        let mut buf = ReadBuf::new(&mut empty);
        match Pin::new(&mut *recv).poll_read(cx, &mut buf) {
            Poll::Ready(Err(e)) => Poll::Ready(e),
            _ => Poll::Pending,
        }
    })
    .await
}

#[cfg(test)]
#[path = "stream_watch_tests.rs"]
mod tests;
