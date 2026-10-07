use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicI64, AtomicU32, Ordering};
use std::time::Duration;

use anyhow::{anyhow, Result};
use async_trait::async_trait;
use tokio::time::timeout;
use tracing::warn;

use crate::relay_pool::RelayPool;
use crate::tunnel_pool::{AuthenticatedStream, PathProbe, TunnelOpenError, TunnelPool};

pub const DIRECT_TIMEOUT: Duration = Duration::from_secs(2);
pub const RELAY_TIMEOUT: Duration = Duration::from_secs(5);
pub const DIRECT_RETRY_INITIAL_COOLDOWN: Duration = Duration::from_secs(30);
pub const DIRECT_RETRY_MAX_COOLDOWN: Duration = Duration::from_secs(2 * 60 * 60);

#[async_trait]
pub trait DirectOpener: Send + Sync + 'static {
    async fn open(&self, addr: SocketAddr) -> Result<AuthenticatedStream, TunnelOpenError>;

    /// Fix 05-B: open and also return a [`PathProbe`] for the pooled
    /// connection the stream rides on. Openers without a connection pool
    /// (test doubles) return `None`, which disables stall eviction for them.
    async fn open_probed(
        &self,
        addr: SocketAddr,
    ) -> Result<(AuthenticatedStream, Option<PathProbe>), TunnelOpenError> {
        self.open(addr).await.map(|stream| (stream, None))
    }

    /// Fix 05-B: evict the pooled connection named by `probe` if its peer has
    /// been silent since the probe was taken. Returns `true` only on eviction.
    /// Must never close the connection.
    async fn evict_if_silent(&self, _addr: SocketAddr, _probe: &PathProbe) -> bool {
        false
    }
}

#[async_trait]
pub trait RelayOpener: Send + Sync + 'static {
    async fn open(&self, ctx: &RelayContext) -> Result<AuthenticatedStream, TunnelOpenError>;
}

#[async_trait]
impl DirectOpener for TunnelPool {
    async fn open(&self, addr: SocketAddr) -> Result<AuthenticatedStream, TunnelOpenError> {
        self.open_authenticated_stream(addr).await
    }

    async fn open_probed(
        &self,
        addr: SocketAddr,
    ) -> Result<(AuthenticatedStream, Option<PathProbe>), TunnelOpenError> {
        self.open_probed_stream(addr)
            .await
            .map(|(stream, probe)| (stream, Some(probe)))
    }

    async fn evict_if_silent(&self, addr: SocketAddr, probe: &PathProbe) -> bool {
        TunnelPool::evict_if_silent(self, addr, probe).await
    }
}

#[async_trait]
impl RelayOpener for RelayPool {
    async fn open(&self, ctx: &RelayContext) -> Result<AuthenticatedStream, TunnelOpenError> {
        self.open_authenticated_stream(&ctx.relay_addr, &ctx.connector_id, &ctx.connector_spiffe)
            .await
    }
}

pub struct RelayContext {
    pub pool: Arc<dyn RelayOpener>,
    pub relay_addr: String,
    pub connector_id: String,
    pub connector_spiffe: String,
}

pub struct ClientTransport {
    direct: Arc<dyn DirectOpener>,
    direct_addr: SocketAddr,
    relay: Option<RelayContext>,
    direct_failure_count: AtomicU32,
    direct_unhealthy_until: AtomicI64
}

impl ClientTransport {
    pub fn new(
        direct: Arc<dyn DirectOpener>,
        direct_addr: SocketAddr,
        relay: Option<RelayContext>,
    ) -> Self {
        Self {
            direct,
            direct_addr,
            relay,
            direct_failure_count: AtomicU32::new(0),
            direct_unhealthy_until: AtomicI64::new(0),
        }
    }
    fn now_unix() -> i64 {
      std::time::SystemTime::now()
          .duration_since(std::time::UNIX_EPOCH)
          .map(|d| d.as_secs() as i64)
          .unwrap_or(0)
  }

  fn direct_is_in_cooldown(&self) -> bool {
      Self::now_unix() < self.direct_unhealthy_until.load(Ordering::Relaxed)
  }

  fn mark_direct_success(&self) {
      self.direct_failure_count.store(0, Ordering::Relaxed);
      self.direct_unhealthy_until.store(0, Ordering::Relaxed);
  }

  fn mark_direct_failure(&self) {
      let failures = self
          .direct_failure_count
          .fetch_add(1, Ordering::Relaxed)
          .saturating_add(1);

      let shift = failures.saturating_sub(1).min(16);
      let multiplier = 1u64 << shift;
      let cooldown_secs = DIRECT_RETRY_INITIAL_COOLDOWN
          .as_secs()
          .saturating_mul(multiplier)
          .min(DIRECT_RETRY_MAX_COOLDOWN.as_secs());

      let until = Self::now_unix().saturating_add(cooldown_secs as i64);
      self.direct_unhealthy_until.store(until, Ordering::Relaxed);
  }


    /// Open a byte-zero authenticated stream to the connector, preferring the
    /// direct LAN path and falling back to the relay only when the direct
    /// attempt times out or fails for a transport-layer reason. Identity /
    /// authentication failures surface verbatim and never trigger relay
    /// retry — the relay path would just fail the same way.
    ///
    /// Fix 05-B: production callers (`net_stack`) use
    /// [`Self::open_authenticated_stream_probed`]; this probe-less wrapper is
    /// kept for the transport unit tests.
    #[cfg(test)]
    pub async fn open_authenticated_stream(&self) -> Result<AuthenticatedStream> {
        self.open_authenticated_stream_probed()
            .await
            .map(|(stream, _probe)| stream)
    }

    /// Fix 05-B: `open_authenticated_stream`, plus the [`PathProbe`] of the
    /// pooled direct connection the stream was opened on (`None` for relay
    /// streams and for openers without a pool). Pass it back to
    /// [`Self::report_handshake_stall`] if the tunnel handshake on this stream
    /// times out.
    pub async fn open_authenticated_stream_probed(
        &self,
    ) -> Result<(AuthenticatedStream, Option<PathProbe>)> {
      let direct_err: anyhow::Error = if self.direct_is_in_cooldown() {
          anyhow!("direct path is in cooldown")
      } else {
          let attempt = timeout(DIRECT_TIMEOUT, self.direct.open_probed(self.direct_addr)).await;

          match attempt {
              Ok(Ok(opened)) => {
                  self.mark_direct_success();
                  return Ok(opened);
              }
              Ok(Err(err)) => match err {
                  TunnelOpenError::Authenticate(_) => {
                      // Identity/auth failures surface verbatim — no relay retry.
                      return Err(anyhow::Error::new(err));
                  }
                  TunnelOpenError::Connect(_) => {
                      self.mark_direct_failure();
                      anyhow::Error::new(err)
                  }
              },
              Err(_) => {
                  self.mark_direct_failure();
                  anyhow!("direct stream establishment exceeded {:?}", DIRECT_TIMEOUT)
              }
          }
      };

      match &self.relay {
          Some(r) => match timeout(RELAY_TIMEOUT, r.pool.open(r)).await {
              Ok(Ok(stream)) => {
                  warn!(
                      direct_err = %direct_err,
                      relay_addr = %r.relay_addr,
                      "direct path failed; used relay fallback"
                  );
                  Ok((stream, None))
              }
              Ok(Err(relay_err)) => Err(anyhow::Error::new(relay_err)
                  .context(format!("direct attempt: {direct_err}"))),
              Err(_) => Err(
                  anyhow!("relay stream establishment exceeded {:?}", RELAY_TIMEOUT)
                      .context(format!("direct attempt: {direct_err}")),
              ),
          },
          None => Err(direct_err),
      }
  }

    /// Fix 05-B: the tunnel handshake on a stream from
    /// [`Self::open_authenticated_stream_probed`] hit `TUNNEL_HANDSHAKE_TIMEOUT`.
    ///
    /// If the pooled direct connection it ran on is still pooled (same
    /// `stable_id`) and has received no datagram since the stream was opened,
    /// the connector is silent: the connection is removed from the pool (NOT
    /// closed, so other flows on it are untouched) and the direct path is put
    /// into the existing cooldown, so later flows stop paying the timeout on
    /// it. Otherwise (relay stream, a replacement is already pooled, or the
    /// peer is alive and only the resource/shield is slow) nothing changes.
    /// Returns `true` when the connection was evicted.
    pub async fn report_handshake_stall(&self, probe: Option<&PathProbe>) -> bool {
        let Some(probe) = probe else {
            return false;
        };
        if !self.direct.evict_if_silent(self.direct_addr, probe).await {
            return false;
        }
        self.mark_direct_failure();
        warn!(
            direct_addr = %self.direct_addr,
            "connector silent since stream open: stale pooled QUIC connection evicted (not closed), direct path cooling down"
        );
        true
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tokio::io::duplex;

    fn fake_stream() -> AuthenticatedStream {
        let (a, _b) = duplex(64);
        Box::new(a)
    }

    fn loopback() -> SocketAddr {
        "127.0.0.1:9092".parse().unwrap()
    }

    enum DirectBehavior {
        Ok,
        ConnectErr,
        AuthenticateErr,
        Sleep(Duration),
    }

    struct MockDirect {
        behavior: DirectBehavior,
        calls: Arc<AtomicUsize>,
    }

    impl MockDirect {
        fn new(behavior: DirectBehavior) -> (Arc<Self>, Arc<AtomicUsize>) {
            let calls = Arc::new(AtomicUsize::new(0));
            (
                Arc::new(Self {
                    behavior,
                    calls: calls.clone(),
                }),
                calls,
            )
        }
    }

    #[async_trait]
    impl DirectOpener for MockDirect {
        async fn open(&self, _addr: SocketAddr) -> Result<AuthenticatedStream, TunnelOpenError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            match &self.behavior {
                DirectBehavior::Ok => Ok(fake_stream()),
                DirectBehavior::ConnectErr => {
                    Err(TunnelOpenError::Connect(anyhow!("ECONNREFUSED")))
                }
                DirectBehavior::AuthenticateErr => Err(TunnelOpenError::Authenticate(anyhow!(
                    "bad certificate (TLS alert 42)"
                ))),
                DirectBehavior::Sleep(d) => {
                    tokio::time::sleep(*d).await;
                    Ok(fake_stream())
                }
            }
        }
    }

    struct MockRelay {
        succeed: bool,
        calls: Arc<AtomicUsize>,
    }

    impl MockRelay {
        fn new(succeed: bool) -> (Arc<Self>, Arc<AtomicUsize>) {
            let calls = Arc::new(AtomicUsize::new(0));
            (
                Arc::new(Self {
                    succeed,
                    calls: calls.clone(),
                }),
                calls,
            )
        }
    }

    #[async_trait]
    impl RelayOpener for MockRelay {
        async fn open(&self, _ctx: &RelayContext) -> Result<AuthenticatedStream, TunnelOpenError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            if self.succeed {
                Ok(fake_stream())
            } else {
                Err(TunnelOpenError::Connect(anyhow!("relay refused")))
            }
        }
    }

    fn relay_ctx(opener: Arc<dyn RelayOpener>) -> RelayContext {
        RelayContext {
            pool: opener,
            relay_addr: "relay.x:9093".into(),
            connector_id: "conn-1".into(),
            connector_spiffe: "spiffe://td/connector/conn-1".into(),
        }
    }

    fn find_typed(err: &anyhow::Error) -> Option<&TunnelOpenError> {
        err.chain()
            .find_map(|c| c.downcast_ref::<TunnelOpenError>())
    }

    #[tokio::test]
    async fn direct_success_skips_relay() {
        let (direct, direct_calls) = MockDirect::new(DirectBehavior::Ok);
        let (relay, relay_calls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        assert!(t.open_authenticated_stream().await.is_ok());
        assert_eq!(direct_calls.load(Ordering::SeqCst), 1);
        assert_eq!(relay_calls.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn direct_timeout_falls_back_to_relay() {
        let (direct, _) = MockDirect::new(DirectBehavior::Sleep(Duration::from_secs(5)));
        let (relay, relay_calls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        assert!(t.open_authenticated_stream().await.is_ok());
        assert_eq!(relay_calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn direct_connect_error_falls_back_to_relay() {
        let (direct, _) = MockDirect::new(DirectBehavior::ConnectErr);
        let (relay, relay_calls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        assert!(t.open_authenticated_stream().await.is_ok());
        assert_eq!(relay_calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn direct_connect_error_surfaces_when_no_relay() {
        let (direct, _) = MockDirect::new(DirectBehavior::ConnectErr);
        let t = ClientTransport::new(direct, loopback(), None);
        let err = t
            .open_authenticated_stream()
            .await
            .err()
            .expect("must error");
        let typed = find_typed(&err).expect("error chain must carry TunnelOpenError");
        assert!(matches!(typed, TunnelOpenError::Connect(_)));
    }

    #[tokio::test]
    async fn direct_authenticate_error_never_falls_back() {
        let (direct, _) = MockDirect::new(DirectBehavior::AuthenticateErr);
        let (relay, relay_calls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        let err = t
            .open_authenticated_stream()
            .await
            .err()
            .expect("must error");
        let typed = find_typed(&err).expect("error chain must carry TunnelOpenError");
        assert!(
            matches!(typed, TunnelOpenError::Authenticate(_)),
            "expected Authenticate, got {typed:?}"
        );
        assert_eq!(
            relay_calls.load(Ordering::SeqCst),
            0,
            "relay must not be consulted on identity failure"
        );
    }

    #[tokio::test]
    async fn direct_only_success() {
        let (direct, direct_calls) = MockDirect::new(DirectBehavior::Ok);
        let t = ClientTransport::new(direct, loopback(), None);
        assert!(t.open_authenticated_stream().await.is_ok());
        assert_eq!(direct_calls.load(Ordering::SeqCst), 1);
    }
}
