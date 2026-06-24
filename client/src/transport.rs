use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Result};
use async_trait::async_trait;
use tokio::time::timeout;
use tracing::warn;

use crate::relay_pool::RelayPool;
use crate::tunnel_pool::{AuthenticatedStream, TunnelOpenError, TunnelPool};

pub const DIRECT_TIMEOUT: Duration = Duration::from_secs(2);

#[async_trait]
pub trait DirectOpener: Send + Sync + 'static {
    async fn open(&self, addr: SocketAddr) -> Result<AuthenticatedStream, TunnelOpenError>;
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
        }
    }

    /// Open a byte-zero authenticated stream to the connector, preferring the
    /// direct LAN path and falling back to the relay only when the direct
    /// attempt times out or fails for a transport-layer reason. Identity /
    /// authentication failures surface verbatim and never trigger relay
    /// retry — the relay path would just fail the same way.
    pub async fn open_authenticated_stream(&self) -> Result<AuthenticatedStream> {
        let attempt = timeout(DIRECT_TIMEOUT, self.direct.open(self.direct_addr)).await;

        let direct_err: anyhow::Error = match attempt {
            Ok(Ok(stream)) => return Ok(stream),
            Ok(Err(err)) => match err {
                TunnelOpenError::Authenticate(_) => {
                    // Identity/auth failures surface verbatim — no relay retry.
                    return Err(anyhow::Error::new(err));
                }
                TunnelOpenError::Connect(_) => anyhow::Error::new(err),
            },
            Err(_) => anyhow!("direct stream establishment exceeded {:?}", DIRECT_TIMEOUT),
        };

        match &self.relay {
            Some(r) => {
                match r.pool.open(r).await {
                    Ok(stream) => {
                        warn!(
                            direct_err = %direct_err,
                            relay_addr = %r.relay_addr,
                            "direct path failed; used relay fallback"
                        );
                        Ok(stream)
                    }
                    Err(relay_err) => Err(anyhow::Error::new(relay_err)
                        .context(format!("direct attempt: {direct_err}"))),
                }
            }
            None => Err(direct_err),
        }
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
