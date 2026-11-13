//! iOS utun framing. A Darwin utun descriptor carries a 4-byte header before
//! every packet, the address family as a big-endian u32 (`AF_INET` = 2,
//! `AF_INET6` = 30 on Darwin, unlike Linux's 10). The net_stack reads and
//! writes bare IP packets, so `Utun` strips the header on read and adds it on
//! write. One read or write is one packet, as with every TUN device.
//!
//! Generic over the inner device and compiled on every platform's tests, so
//! the framing is unit-tested on Linux.

use std::io;
use std::pin::Pin;
use std::task::{ready, Context, Poll};

use tokio::io::{AsyncRead, AsyncWrite, ReadBuf};

const HEADER: usize = 4;
const AF_INET: u32 = 2;
const AF_INET6_DARWIN: u32 = 30;

/// The utun header for an IP packet, from its version nibble; None for
/// anything that isn't IPv4 or IPv6.
pub(crate) fn header_for(packet: &[u8]) -> Option<[u8; HEADER]> {
    let family = match packet.first()? >> 4 {
        4 => AF_INET,
        6 => AF_INET6_DARWIN,
        _ => return None,
    };
    Some(family.to_be_bytes())
}

pub struct Utun<T> {
    inner: T,
    read_buf: Vec<u8>,
    write_buf: Vec<u8>,
}

impl<T> Utun<T> {
    pub fn new(inner: T) -> Self {
        Self {
            inner,
            read_buf: Vec::new(),
            write_buf: Vec::new(),
        }
    }
}

impl<T: AsyncRead + Unpin> AsyncRead for Utun<T> {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let this = self.get_mut();
        loop {
            this.read_buf.resize(buf.remaining() + HEADER, 0);
            let n = {
                let mut frame = ReadBuf::new(&mut this.read_buf);
                ready!(Pin::new(&mut this.inner).poll_read(cx, &mut frame))?;
                frame.filled().len()
            };
            if n == 0 {
                return Poll::Ready(Ok(())); // end of stream
            }
            if n <= HEADER {
                continue; // a frame with no packet: skip it
            }
            buf.put_slice(&this.read_buf[HEADER..n]);
            return Poll::Ready(Ok(()));
        }
    }
}

impl<T: AsyncWrite + Unpin> AsyncWrite for Utun<T> {
    fn poll_write(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        packet: &[u8],
    ) -> Poll<io::Result<usize>> {
        let this = self.get_mut();
        let Some(header) = header_for(packet) else {
            // Not an IP packet: nothing the interface could carry. Report it
            // written so the net_stack's writer keeps going.
            return Poll::Ready(Ok(packet.len()));
        };
        this.write_buf.clear();
        this.write_buf.extend_from_slice(&header);
        this.write_buf.extend_from_slice(packet);
        let n = ready!(Pin::new(&mut this.inner).poll_write(cx, &this.write_buf))?;
        if n != this.write_buf.len() {
            return Poll::Ready(Err(io::Error::new(
                io::ErrorKind::WriteZero,
                "short write on the utun interface",
            )));
        }
        Poll::Ready(Ok(packet.len()))
    }

    fn poll_flush(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_flush(cx)
    }

    fn poll_shutdown(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.get_mut().inner).poll_shutdown(cx)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::VecDeque;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    /// A packet device: each read returns one queued frame, each write is
    /// recorded as one frame (optionally truncated, to simulate a short write).
    #[derive(Default)]
    struct FakeUtun {
        frames: VecDeque<Vec<u8>>,
        written: Vec<Vec<u8>>,
        short_by: usize,
    }

    impl AsyncRead for FakeUtun {
        fn poll_read(
            self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
            buf: &mut ReadBuf<'_>,
        ) -> Poll<io::Result<()>> {
            if let Some(frame) = self.get_mut().frames.pop_front() {
                buf.put_slice(&frame);
            }
            Poll::Ready(Ok(()))
        }
    }

    impl AsyncWrite for FakeUtun {
        fn poll_write(
            self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
            buf: &[u8],
        ) -> Poll<io::Result<usize>> {
            let this = self.get_mut();
            this.written.push(buf.to_vec());
            Poll::Ready(Ok(buf.len() - this.short_by))
        }
        fn poll_flush(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
            Poll::Ready(Ok(()))
        }
        fn poll_shutdown(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<io::Result<()>> {
            Poll::Ready(Ok(()))
        }
    }

    const V4: [u8; 6] = [0x45, 0, 0, 20, 0xaa, 0xbb];
    const V6: [u8; 4] = [0x60, 0, 0, 0];

    #[test]
    fn header_is_the_darwin_family_big_endian() {
        assert_eq!(header_for(&V4), Some([0, 0, 0, 2]));
        assert_eq!(header_for(&V6), Some([0, 0, 0, 30]));
        assert_eq!(header_for(&[0x15]), None);
        assert_eq!(header_for(&[]), None);
    }

    #[tokio::test]
    async fn reads_strip_the_header_one_packet_at_a_time() {
        let mut inner = FakeUtun::default();
        inner.frames.push_back([&[0, 0, 0, 2][..], &V4].concat());
        inner.frames.push_back(vec![0, 0, 0, 2]); // header only: skipped
        inner.frames.push_back([&[0, 0, 0, 30][..], &V6].concat());
        let mut utun = Utun::new(inner);

        let mut buf = [0u8; 64];
        let n = utun.read(&mut buf).await.unwrap();
        assert_eq!(&buf[..n], &V4);
        let n = utun.read(&mut buf).await.unwrap();
        assert_eq!(&buf[..n], &V6);
        assert_eq!(utun.read(&mut buf).await.unwrap(), 0, "end of stream");
    }

    #[tokio::test]
    async fn writes_add_the_header_in_one_frame() {
        let mut utun = Utun::new(FakeUtun::default());
        assert_eq!(utun.write(&V4).await.unwrap(), V4.len());
        assert_eq!(utun.write(&V6).await.unwrap(), V6.len());
        assert_eq!(utun.inner.written[0], [&[0, 0, 0, 2][..], &V4].concat());
        assert_eq!(utun.inner.written[1], [&[0, 0, 0, 30][..], &V6].concat());
    }

    #[tokio::test]
    async fn a_non_ip_packet_is_dropped_not_written() {
        let mut utun = Utun::new(FakeUtun::default());
        assert_eq!(utun.write(&[0x15, 1, 2]).await.unwrap(), 3);
        assert!(utun.inner.written.is_empty());
    }

    #[tokio::test]
    async fn a_short_write_is_an_error() {
        let mut utun = Utun::new(FakeUtun {
            short_by: 1,
            ..Default::default()
        });
        assert!(utun.write(&V4).await.is_err());
    }
}
