use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU32, Ordering};
use std::time::Duration;

use anyhow::{anyhow, Result};
use async_trait::async_trait;
use tokio::time::timeout;
use tracing::{info, warn};

use crate::relay_pool::RelayPool;
use crate::tunnel_pool::{AuthenticatedStream, PathProbe, TunnelOpenError, TunnelPool};

pub const DIRECT_TIMEOUT: Duration = Duration::from_secs(2);
pub const RELAY_TIMEOUT: Duration = Duration::from_secs(5);
pub const DIRECT_RETRY_INITIAL_COOLDOWN: Duration = Duration::from_secs(30);
/// Sprint 20 F43: was 2 h. With a relay, an expired cooldown is re-checked by
/// one background probe, never by a user flow, so a short cap only costs a
/// QUIC dial per window and lets a recovered connector be used again soon.
pub const DIRECT_RETRY_MAX_COOLDOWN: Duration = Duration::from_secs(2 * 60);

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

    /// Sprint 20 F43: check that the direct path works again, without a user
    /// flow. The default opens (and drops) a stream; the pool overrides it with
    /// an authenticated connect that is pooled for the next flow.
    async fn probe(&self, addr: SocketAddr) -> Result<(), TunnelOpenError> {
        self.open(addr).await.map(|_| ())
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

    async fn probe(&self, addr: SocketAddr) -> Result<(), TunnelOpenError> {
        self.get_or_connect(addr).await.map(|_| ())
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

/// Direct-path health, shared with the background probe (Sprint 20 F43).
struct DirectHealth {
    failure_count: AtomicU32,
    unhealthy_until: AtomicI64,
    /// A background probe is in flight; at most one per transport.
    probing: AtomicBool,
}

impl DirectHealth {
    fn now_unix() -> i64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_secs() as i64)
            .unwrap_or(0)
    }

    fn in_cooldown(&self) -> bool {
        Self::now_unix() < self.unhealthy_until.load(Ordering::SeqCst)
    }

    fn mark_success(&self) {
        self.failure_count.store(0, Ordering::SeqCst);
        self.unhealthy_until.store(0, Ordering::SeqCst);
    }

    fn mark_failure(&self) {
        let failures = self
            .failure_count
            .fetch_add(1, Ordering::SeqCst)
            .saturating_add(1);
        let shift = failures.saturating_sub(1).min(16);
        let cooldown_secs = DIRECT_RETRY_INITIAL_COOLDOWN
            .as_secs()
            .saturating_mul(1u64 << shift)
            .min(DIRECT_RETRY_MAX_COOLDOWN.as_secs());
        self.unhealthy_until
            .store(Self::now_unix().saturating_add(cooldown_secs as i64), Ordering::SeqCst);
    }
}

/// Clears `probing` when the probe task ends, however it ends.
struct ProbeDone(Arc<DirectHealth>);

impl Drop for ProbeDone {
    fn drop(&mut self) {
        self.0.probing.store(false, Ordering::SeqCst);
    }
}

pub struct ClientTransport {
    direct: Arc<dyn DirectOpener>,
    direct_addr: SocketAddr,
    relay: Option<RelayContext>,
    health: Arc<DirectHealth>,
    /// Connector-revocation Phase C: the connector this transport reaches
    /// (empty when unknown). Lets a flow record which connector carries it.
    connector_id: String,
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
            health: Arc::new(DirectHealth {
                failure_count: AtomicU32::new(0),
                unhealthy_until: AtomicI64::new(0),
                probing: AtomicBool::new(false),
            }),
            connector_id: String::new(),
        }
    }

    /// Phase C: tag this transport with its connector id.
    pub fn with_connector_id(mut self, connector_id: String) -> Self {
        self.connector_id = connector_id;
        self
    }

    pub fn connector_id(&self) -> &str {
        &self.connector_id
    }
  fn direct_is_in_cooldown(&self) -> bool {
      self.health.in_cooldown()
  }

  fn mark_direct_success(&self) {
      self.health.mark_success();
  }

  fn mark_direct_failure(&self) {
      self.health.mark_failure();
  }

  /// Sprint 20 F43: the cooldown expired after a failure. Instead of making
  /// user flows re-dial a possibly dead connector (each paying up to
  /// `DIRECT_TIMEOUT`), one background probe re-checks the direct path while
  /// flows keep using the relay. Success clears the cooldown; any failure,
  /// including an identity failure, re-arms it with backoff.
  fn spawn_direct_probe(&self) {
      if self
          .health
          .probing
          .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
          .is_err()
      {
          return;
      }
      let health = self.health.clone();
      let direct = self.direct.clone();
      let addr = self.direct_addr;
      tokio::spawn(async move {
          let _done = ProbeDone(health.clone());
          match timeout(DIRECT_TIMEOUT, direct.probe(addr)).await {
              Ok(Ok(())) => {
                  health.mark_success();
                  info!(direct_addr = %addr, "direct path healthy again (background probe)");
              }
              Ok(Err(TunnelOpenError::Authenticate(e))) => {
                  health.mark_failure();
                  warn!(direct_addr = %addr, error = %e, "direct path probe refused the connector's identity; staying on the relay");
              }
              Ok(Err(TunnelOpenError::Connect(_))) | Err(_) => health.mark_failure(),
          }
      });
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
      } else if self.relay.is_some() && self.health.failure_count.load(Ordering::SeqCst) > 0 {
          // F43: cooldown expired after a failure — a background probe
          // re-checks the direct path; this flow takes the relay.
          self.spawn_direct_probe();
          anyhow!("direct path is being re-checked")
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

    /// Sprint 20 F43: put the transport in the state "a failure happened and
    /// its cooldown has just expired".
    fn expire_cooldown_after_failure(t: &ClientTransport) {
        t.health.failure_count.store(1, Ordering::SeqCst);
        t.health.unhealthy_until.store(0, Ordering::SeqCst);
    }

    async fn wait_probe_done(t: &ClientTransport) {
        let deadline = std::time::Instant::now() + Duration::from_secs(5);
        while t.health.probing.load(Ordering::SeqCst) {
            assert!(std::time::Instant::now() < deadline, "probe never finished");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    /// F43: while the cooldown is active, flows take the relay without dialing.
    #[tokio::test]
    async fn cooldown_flows_use_relay_without_dialing() {
        let (direct, dcalls) = MockDirect::new(DirectBehavior::ConnectErr);
        let (relay, rcalls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        t.open_authenticated_stream().await.unwrap();
        assert_eq!(dcalls.load(Ordering::SeqCst), 1, "first flow dials and fails");
        for _ in 0..3 {
            t.open_authenticated_stream().await.unwrap();
        }
        assert_eq!(dcalls.load(Ordering::SeqCst), 1, "no dial while cooling down");
        assert_eq!(rcalls.load(Ordering::SeqCst), 4);
    }

    /// F43: after the cooldown expires, flows don't pay a direct dial: they
    /// take the relay at once and exactly one background probe dials.
    #[tokio::test]
    async fn expired_cooldown_probes_once_in_background() {
        let (direct, dcalls) = MockDirect::new(DirectBehavior::Sleep(Duration::from_secs(10)));
        let (relay, rcalls) = MockRelay::new(true);
        let t = Arc::new(ClientTransport::new(direct, loopback(), Some(relay_ctx(relay))));
        expire_cooldown_after_failure(&t);

        let started = std::time::Instant::now();
        let flows: Vec<_> = (0..5)
            .map(|_| {
                let t = t.clone();
                tokio::spawn(async move { t.open_authenticated_stream().await.map(|_| ()) })
            })
            .collect();
        for f in flows {
            f.await.unwrap().unwrap();
        }
        assert!(started.elapsed() < Duration::from_millis(500), "flows waited on the direct dial");
        assert_eq!(rcalls.load(Ordering::SeqCst), 5);
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(dcalls.load(Ordering::SeqCst), 1, "exactly one probe for all flows");

        // The probe times out (dead connector): the cooldown is re-armed with backoff.
        wait_probe_done(&t).await;
        assert!(t.direct_is_in_cooldown());
        assert_eq!(t.health.failure_count.load(Ordering::SeqCst), 2);
    }

    /// F43: a successful probe clears the cooldown; the next flow goes direct.
    #[tokio::test]
    async fn successful_probe_restores_the_direct_path() {
        let (direct, dcalls) = MockDirect::new(DirectBehavior::Ok);
        let (relay, rcalls) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        expire_cooldown_after_failure(&t);

        t.open_authenticated_stream().await.unwrap();
        assert_eq!(rcalls.load(Ordering::SeqCst), 1, "the flow during the probe takes the relay");
        wait_probe_done(&t).await;
        assert!(!t.direct_is_in_cooldown());
        assert_eq!(t.health.failure_count.load(Ordering::SeqCst), 0);

        t.open_authenticated_stream().await.unwrap();
        assert_eq!(dcalls.load(Ordering::SeqCst), 2, "probe + the next flow dial direct");
        assert_eq!(rcalls.load(Ordering::SeqCst), 1);
    }

    /// F43: a probe that meets an identity failure (e.g. a revoked connector)
    /// never marks the direct path healthy.
    #[tokio::test]
    async fn probe_identity_failure_keeps_the_cooldown() {
        let (direct, _) = MockDirect::new(DirectBehavior::AuthenticateErr);
        let (relay, _) = MockRelay::new(true);
        let t = ClientTransport::new(direct, loopback(), Some(relay_ctx(relay)));
        expire_cooldown_after_failure(&t);
        t.open_authenticated_stream().await.unwrap();
        wait_probe_done(&t).await;
        assert!(t.direct_is_in_cooldown());
    }

    /// F43: without a relay there is nothing to fall back to, so a flow after
    /// an expired cooldown still dials direct itself (old behaviour).
    #[tokio::test]
    async fn without_relay_an_expired_cooldown_dials_in_the_flow() {
        let (direct, dcalls) = MockDirect::new(DirectBehavior::Ok);
        let t = ClientTransport::new(direct, loopback(), None);
        expire_cooldown_after_failure(&t);
        t.open_authenticated_stream().await.unwrap();
        assert_eq!(dcalls.load(Ordering::SeqCst), 1);
        assert!(!t.health.probing.load(Ordering::SeqCst), "no background probe");
    }

    /// F43: the backoff is capped at two minutes.
    #[test]
    fn cooldown_backoff_is_capped() {
        let (direct, _) = MockDirect::new(DirectBehavior::Ok);
        let t = ClientTransport::new(direct, loopback(), None);
        for _ in 0..20 {
            t.mark_direct_failure();
        }
        let left = t.health.unhealthy_until.load(Ordering::SeqCst) - DirectHealth::now_unix();
        assert!((119..=120).contains(&left), "cooldown {left}s, want 120s");
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
