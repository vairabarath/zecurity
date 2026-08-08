// resolver.rs — Sprint 16 Phase 6 (PENDING-14 Stage 2)
//
// Turns (hostname, resolver) into a current IPv4 address, cached by TTL.
//
// WHY THIS LIVES AT THE CONNECTOR, NOT THE CONTROLLER: writing a resolved IP to the
// database bumps the ACL snapshot version, and an ACL version change triggers the
// client's `restart_tunnel_if_running`. A backend re-resolved every 60s would
// therefore restart every tunnel in the fleet every 60s. Nothing here touches
// controller state, and the cache is process-local — a resolver failure can never
// poison ACL state.
//
// This module performs no dialing and holds no policy about *who* may reach a
// resource. Delivery (`route_type`) and authorization are decided before anything
// here is called; see device_tunnel.

use std::collections::HashMap;
use std::future::Future;
use std::net::Ipv4Addr;
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use tokio::sync::Mutex as AsyncMutex;

use crate::client::v1::AclResolver;

/// TTL floor. A 1s record would otherwise make the connector query DNS on
/// essentially every connection.
pub const TTL_MIN: Duration = Duration::from_secs(5);
/// TTL ceiling. A 24h record would defeat the entire point of the sprint.
pub const TTL_MAX: Duration = Duration::from_secs(300);
/// Negative-answer suppression window, so one misconfigured resource cannot become
/// a DNS flood. Applies to both NXDOMAIN and NODATA — they are equally authoritative
/// and equally likely to be hit on every connection attempt.
pub const NEGATIVE_TTL: Duration = Duration::from_secs(5);
/// While the resolver is unreachable, how long a stale address is served from the
/// *fast path* before another query is attempted.
///
/// Without this, every connection during a DNS outage pays a full resolver timeout
/// before receiving the stale address — technically "serving stale", but with the
/// latency of the outage attached to every request.
pub const STALE_RETRY: Duration = Duration::from_secs(5);
/// How long a last-known-good address may be served after expiry while the resolver
/// is unreachable. Bounded, so a permanently dead resolver eventually fails closed
/// instead of pinning a stale address forever.
pub const STALE_MAX: Duration = Duration::from_secs(3600);

// ── Errors ───────────────────────────────────────────────────────────────────

/// Resolution failures, deliberately non-collapsible: each one sends an operator to
/// a *different* system, which is the whole reason they are typed.
///
/// There is intentionally **no `DialFailed` variant**. A failed TCP connect means
/// resolution *succeeded* — reporting it here would conflate "DNS is broken" with
/// "the resource is down". The dial and its `dial_failed` reason belong to the
/// caller (Phase 7).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ResolveError {
    /// The name does not exist. An **answer**, not a failure — a config error.
    NxDomain,
    /// Nothing answered: timeout, no route, connection refused, no nameserver.
    /// Says nothing about the name.
    ResolverUnavailable,
    /// The resolver answered that it failed — SERVFAIL or REFUSED. Also says nothing
    /// about the name, but it is kept distinct from `ResolverUnavailable` because it
    /// points at a different system: the resolver or the zone, rather than the
    /// network path to it. (DNSSEC validation failure lands here.)
    ResolverFailure,
    /// NODATA — the name exists but has no A record. An **answer**: the record was
    /// removed.
    NoAddressRecord,
    /// `resolver.type` is not one this connector implements.
    UnsupportedResolver,
    /// `resolver.config` is missing or malformed for its type.
    InvalidResolverConfig,
}

impl ResolveError {
    /// Stable snake_case token for deny reasons, logs and metrics. Phase 7 logs this
    /// verbatim so denials stay countable from the audit trail rather than only from
    /// stderr.
    pub fn reason(&self) -> &'static str {
        match self {
            Self::NxDomain => "nxdomain",
            Self::ResolverUnavailable => "resolver_unavailable",
            Self::ResolverFailure => "resolver_failure",
            Self::NoAddressRecord => "no_address_record",
            Self::UnsupportedResolver => "unsupported_resolver",
            Self::InvalidResolverConfig => "invalid_resolver_config",
        }
    }

    /// May a last-known-good address be served for this failure?
    ///
    /// Only when the failure carries **no authoritative information about the name** —
    /// i.e. the resolver never got to answer. A DNS blip must not become an outage.
    pub fn may_serve_stale(&self) -> bool {
        matches!(self, Self::ResolverUnavailable | Self::ResolverFailure)
    }

    /// Is this authoritative evidence that the endpoint is gone?
    ///
    /// Then the cached address **must** be discarded. Both NXDOMAIN (name removed)
    /// and NODATA (A record removed) are answers, and continuing to dial a
    /// previously-cached address after either is a failure to converge — the
    /// security-relevant direction to get wrong.
    pub fn invalidates_cache(&self) -> bool {
        matches!(self, Self::NxDomain | Self::NoAddressRecord)
    }
}

/// A successful resolution. `stale` is true when the resolver was unreachable and
/// this is a last-known-good address: dial it, but the degradation is worth
/// surfacing.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Resolved {
    pub address: Ipv4Addr,
    pub stale: bool,
}

// ── DNS backend boundary ─────────────────────────────────────────────────────

type BoxFut<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// The only part of this module that talks to the network.
///
/// Behind a trait so the cache, TTL clamp, negative cache, single-flight and
/// stale-on-error logic are all unit-testable with no DNS server — and so this
/// module does not depend on any particular DNS crate's API surface.
pub trait DnsBackend: Send + Sync + 'static {
    /// Resolve A records. Returns `(address, record TTL)` pairs; empty means NODATA.
    fn lookup_a<'a>(
        &'a self,
        name: &'a str,
    ) -> BoxFut<'a, Result<Vec<(Ipv4Addr, Duration)>, ResolveError>>;
}

// ── Cache ────────────────────────────────────────────────────────────────────

/// IPv4-only this sprint — stated in the cache key rather than left as an accident
/// of taking the first result. Stage 3 answers AAAA with NODATA, which only makes
/// sense because this layer is deliberately v4.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Family {
    V4,
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
struct CacheKey {
    remote_network_id: String,
    name: String,
    family: Family,
}

#[derive(Debug, Clone, Copy)]
struct Good {
    address: Ipv4Addr,
    expires_at: Instant,
}

#[derive(Debug, Default)]
struct Entry {
    /// Last known good, retained past `expires_at` to back stale-on-error. Cleared
    /// whenever an answer tells us the endpoint is gone.
    good: Option<Good>,
    /// A cached authoritative "no endpoint" answer and the reason to report, held
    /// until the given instant. Covers **both** NXDOMAIN and NODATA — the reason is
    /// stored rather than assumed, so a suppressed NODATA is not later reported as
    /// NXDOMAIN.
    negative: Option<(ResolveError, Instant)>,
    /// Do not re-query before this instant; serve `good` as stale instead. Set when
    /// the resolver was unreachable, so a DNS outage does not make every connection
    /// wait out a resolver timeout to learn what we already knew.
    retry_after: Option<Instant>,
}

// ── Resolver ─────────────────────────────────────────────────────────────────

pub struct Resolver {
    backend: Arc<dyn DnsBackend>,
    cache: Mutex<HashMap<CacheKey, Entry>>,
    /// Per-key gate so N concurrent connections to one cold name issue ONE query.
    gates: Mutex<HashMap<CacheKey, Arc<AsyncMutex<()>>>>,
}

impl Resolver {
    pub fn new(backend: Arc<dyn DnsBackend>) -> Self {
        Self {
            backend,
            cache: Mutex::new(HashMap::new()),
            gates: Mutex::new(HashMap::new()),
        }
    }

    /// Resolve a name-addressed resource's current endpoint.
    ///
    /// `hostname` is the client-facing name from the ACL entry; the *backend* name
    /// may differ and is taken from `resolver.config["name"]` when present. The two
    /// are frequently different strings, which is why the resource model separates
    /// them.
    pub async fn resolve(
        &self,
        remote_network_id: &str,
        hostname: &str,
        resolver: &AclResolver,
    ) -> Result<Resolved, ResolveError> {
        self.resolve_at(remote_network_id, hostname, resolver, Instant::now())
            .await
    }

    /// `now` is injected so TTL expiry, the clamp and the stale window are testable
    /// without sleeping.
    pub async fn resolve_at(
        &self,
        remote_network_id: &str,
        hostname: &str,
        resolver: &AclResolver,
        now: Instant,
    ) -> Result<Resolved, ResolveError> {
        match resolver.r#type.trim() {
            "static" => resolve_static(resolver),
            "dns" => {
                self.resolve_dns(remote_network_id, hostname, resolver, now)
                    .await
            }
            // Never fall back to a default. An unknown type must not silently become
            // a DNS lookup against the connector's own system resolver.
            _ => Err(ResolveError::UnsupportedResolver),
        }
    }

    async fn resolve_dns(
        &self,
        remote_network_id: &str,
        hostname: &str,
        resolver: &AclResolver,
        now: Instant,
    ) -> Result<Resolved, ResolveError> {
        let name = dns_query_name(hostname, resolver)?;
        let key = CacheKey {
            remote_network_id: remote_network_id.to_string(),
            name: name.clone(),
            family: Family::V4,
        };

        // 1. Fast path — no gate contention, no await.
        if let Some(hit) = self.check_cache(&key, now) {
            return hit;
        }

        // 2. Single-flight gate. Cloned out of the map so no sync lock is held
        //    across the await below.
        let gate = self.gate_for(&key);
        let _held = gate.lock().await;

        // 3. Double-check: a task ahead of us in the gate may have filled the cache.
        if let Some(hit) = self.check_cache(&key, now) {
            return hit;
        }

        // 4. The only await in this module that touches the network.
        let outcome = self.backend.lookup_a(&name).await;
        let result = self.store(&key, outcome, now);

        // 5. Drop the gate entry once nobody is waiting on it (map + our local = 2).
        //    Safe to do here because `store` has already populated the cache, so a
        //    task that creates a fresh gate afterwards hits the cache immediately.
        if Arc::strong_count(&gate) == 2 {
            self.gates.lock().remove(&key);
        }
        result
    }

    /// `Some(..)` when the cache can answer without a query; `None` means "must query".
    fn check_cache(&self, key: &CacheKey, now: Instant) -> Option<Result<Resolved, ResolveError>> {
        let cache = self.cache.lock();
        let entry = cache.get(key)?;

        // A suppressed authoritative negative answer, reported with its own reason.
        if let Some((err, until)) = entry.negative {
            if now < until {
                return Some(Err(err));
            }
        }

        let good = entry.good?;
        if now < good.expires_at {
            return Some(Ok(Resolved {
                address: good.address,
                stale: false,
            }));
        }

        // Expired, but the resolver was unreachable moments ago. Serve the stale
        // address straight away instead of making this connection pay another
        // resolver timeout for an answer we already have.
        let backoff_active = entry.retry_after.is_some_and(|t| now < t);
        if backoff_active && now < good.expires_at + STALE_MAX {
            return Some(Ok(Resolved {
                address: good.address,
                stale: true,
            }));
        }
        None
    }

    fn store(
        &self,
        key: &CacheKey,
        outcome: Result<Vec<(Ipv4Addr, Duration)>, ResolveError>,
        now: Instant,
    ) -> Result<Resolved, ResolveError> {
        let mut cache = self.cache.lock();
        let entry = cache.entry(key.clone()).or_default();

        match outcome {
            Ok(addrs) => match addrs.first() {
                Some(&(address, ttl)) => {
                    entry.good = Some(Good {
                        address,
                        expires_at: now + ttl.clamp(TTL_MIN, TTL_MAX),
                    });
                    entry.negative = None;
                    entry.retry_after = None;
                    Ok(Resolved {
                        address,
                        stale: false,
                    })
                }
                // NODATA: an authoritative NOERROR answer. The name exists, its A
                // record is gone. Same class as NXDOMAIN — discard the cached
                // address (otherwise a later ResolverUnavailable would resurrect a
                // deleted endpoint through the stale path) and suppress briefly, or
                // every connection attempt re-queries.
                None => {
                    entry.good = None;
                    entry.retry_after = None;
                    entry.negative = Some((ResolveError::NoAddressRecord, now + NEGATIVE_TTL));
                    Err(ResolveError::NoAddressRecord)
                }
            },

            // An answer that says the endpoint is gone is a DECISION. Never serve
            // stale for it.
            Err(e) if e.invalidates_cache() => {
                entry.good = None;
                entry.retry_after = None;
                entry.negative = Some((e, now + NEGATIVE_TTL));
                Err(e)
            }

            // Nothing was learned about the name: serve last-known-good inside a
            // bounded window, then fail closed. Copy first — `Good` is Copy, and the
            // arm needs to mutate `entry` while deciding.
            Err(e) if e.may_serve_stale() => {
                let cached = entry.good;
                match cached {
                    Some(good) if now < good.expires_at + STALE_MAX => {
                        // Short backoff so the next connection is served from the
                        // fast path rather than waiting out another timeout.
                        entry.retry_after = Some(now + STALE_RETRY);
                        Ok(Resolved {
                            address: good.address,
                            stale: true,
                        })
                    }
                    _ => Err(e),
                }
            }

            // Config errors: nothing was ever asked of the resolver, so no cache
            // interaction at all.
            Err(e) => Err(e),
        }
    }

    fn gate_for(&self, key: &CacheKey) -> Arc<AsyncMutex<()>> {
        self.gates
            .lock()
            .entry(key.clone())
            .or_insert_with(|| Arc::new(AsyncMutex::new(())))
            .clone()
    }
}

// ── Resolver types ───────────────────────────────────────────────────────────

/// No network I/O, so no cache and no TTL. Also what makes this module exercisable
/// end-to-end without a DNS server.
fn resolve_static(resolver: &AclResolver) -> Result<Resolved, ResolveError> {
    let raw = resolver
        .config
        .get("address")
        .or_else(|| resolver.config.get("addresses"))
        .ok_or(ResolveError::InvalidResolverConfig)?;

    raw.split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .find_map(|s| s.parse::<Ipv4Addr>().ok())
        .map(|address| Resolved {
            address,
            stale: false,
        })
        .ok_or(ResolveError::InvalidResolverConfig)
}

/// The backend name to query: `config["name"]` when present, else the resource's
/// client-facing `hostname`.
///
/// A `server` key is **rejected rather than ignored**. Honouring it would need a
/// per-resource resolver instance (not in this sprint), and silently ignoring it
/// would resolve against the connector's system resolver instead — a different
/// answer than the operator asked for. Failing closed here is the same rule as
/// `ambiguous_addressing` in policy::ResourceAcl::addressing.
fn dns_query_name(hostname: &str, resolver: &AclResolver) -> Result<String, ResolveError> {
    if resolver.config.contains_key("server") {
        return Err(ResolveError::InvalidResolverConfig);
    }
    let name = resolver
        .config
        .get("name")
        .map(String::as_str)
        .unwrap_or(hostname)
        .trim();
    if name.is_empty() {
        return Err(ResolveError::InvalidResolverConfig);
    }
    Ok(name.to_string())
}

// ── Hickory backend ──────────────────────────────────────────────────────────
//
// The only version-sensitive code in this module. Written against
// hickory-resolver 0.26.1: `TokioResolver::builder_tokio()?.build()?`,
// `ipv4_lookup(..) -> Result<Lookup, NetError>`, `Lookup::valid_until()`.

use hickory_resolver::lookup::Lookup;
use hickory_resolver::net::{DnsError, NetError};
use hickory_resolver::proto::op::ResponseCode;
use hickory_resolver::proto::rr::RData;
use hickory_resolver::TokioResolver;

pub struct HickoryBackend {
    inner: TokioResolver,
}

impl HickoryBackend {
    /// Reads the system resolver configuration (`/etc/resolv.conf`). Build once at
    /// startup and share the `Arc` — the inner resolver holds its own connection
    /// pool and internal cache.
    pub fn from_system() -> anyhow::Result<Self> {
        Ok(Self {
            inner: TokioResolver::builder_tokio()?.build()?,
        })
    }
}

impl DnsBackend for HickoryBackend {
    fn lookup_a<'a>(
        &'a self,
        name: &'a str,
    ) -> BoxFut<'a, Result<Vec<(Ipv4Addr, Duration)>, ResolveError>> {
        Box::pin(async move {
            match self.inner.ipv4_lookup(name).await {
                Ok(lookup) => Ok(a_records(&lookup, Instant::now())),
                Err(e) => Err(classify(&e)),
            }
        })
    }
}

/// Extract A records and their shared caching horizon from a hickory `Lookup`.
///
/// Factored out of `lookup_a` so the success path is testable without a DNS server —
/// it was otherwise the one piece of real logic in this module with no coverage.
///
/// `valid_until` is the min-TTL deadline across the whole answer set, which is
/// exactly the caching horizon we want; our own clamp is applied later in `store`.
/// Non-A records (a CNAME chain's intermediate records) are skipped, so a
/// CNAME-only answer yields an empty vec and is treated as NODATA upstream.
fn a_records(lookup: &Lookup, now: Instant) -> Vec<(Ipv4Addr, Duration)> {
    let ttl = lookup.valid_until().saturating_duration_since(now);
    lookup
        .answers()
        .iter()
        // `data` is a public field in hickory 0.26, not a method.
        .filter_map(|r| match &r.data {
            RData::A(a) => Some((a.0, ttl)),
            _ => None,
        })
        .collect()
}

/// Map a hickory error onto our policy classes.
///
/// ⚠️ **This is the highest-stakes function in the module.** Hickory folds
/// "the name does not exist" and "the name exists but has no A record" into the
/// *same* `NoRecordsFound` variant, separated only by `response_code`. Getting it
/// wrong inverts the stale decision:
///
/// * SERVFAIL misread as NXDOMAIN → a transient blip discards the last-known-good
///   and denies (availability hit, brief).
/// * NXDOMAIN misread as a transient failure → a **deleted** resource stays
///   reachable for up to `STALE_MAX` (the security-relevant direction).
///
/// So the default for anything unrecognised is a *stale-eligible,
/// non-invalidating* class: we discard a cached address only on an explicit
/// authoritative signal, never on a guess.
fn classify(e: &NetError) -> ResolveError {
    match e {
        // An answer arrived. Branch on what it actually said.
        NetError::Dns(DnsError::NoRecordsFound(no_records)) => {
            from_response_code(no_records.response_code)
        }
        NetError::Dns(DnsError::ResponseCode(rc)) => from_response_code(*rc),

        // Nothing answered.
        NetError::Timeout | NetError::Busy | NetError::NoConnections => {
            ResolveError::ResolverUnavailable
        }
        NetError::Io(_) | NetError::Proto(_) => ResolveError::ResolverUnavailable,

        // Both enums are #[non_exhaustive]; unknown errors are transport or protocol
        // problems, never authoritative answers, so they must not invalidate.
        _ => ResolveError::ResolverUnavailable,
    }
}

fn from_response_code(rc: ResponseCode) -> ResolveError {
    match rc {
        // The name does not exist. An answer — invalidates the cache.
        ResponseCode::NXDomain => ResolveError::NxDomain,
        // NODATA: the name exists, but not with an A record. Also an answer.
        ResponseCode::NoError => ResolveError::NoAddressRecord,
        // The resolver answered that it could not answer. Says nothing about the
        // name, so stale-eligible — DNSSEC validation failures land here.
        ResponseCode::ServFail | ResponseCode::Refused => ResolveError::ResolverFailure,
        _ => ResolveError::ResolverFailure,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Scripted DNS backend: counts queries and replays queued outcomes, so cache
    /// behaviour is observable without a DNS server. The final scripted entry
    /// repeats forever.
    type LookupOutcome = Result<Vec<(Ipv4Addr, Duration)>, ResolveError>;

    struct FakeDns {
        queries: AtomicUsize,
        script: Mutex<Vec<LookupOutcome>>,
        last_name: Mutex<Option<String>>,
    }

    impl FakeDns {
        fn new(script: Vec<LookupOutcome>) -> Arc<Self> {
            Arc::new(Self {
                queries: AtomicUsize::new(0),
                script: Mutex::new(script),
                last_name: Mutex::new(None),
            })
        }
        fn ok(ip: [u8; 4], ttl_secs: u64) -> LookupOutcome {
            Ok(vec![(Ipv4Addr::from(ip), Duration::from_secs(ttl_secs))])
        }
        fn count(&self) -> usize {
            self.queries.load(Ordering::SeqCst)
        }
    }

    impl DnsBackend for FakeDns {
        fn lookup_a<'a>(
            &'a self,
            name: &'a str,
        ) -> BoxFut<'a, Result<Vec<(Ipv4Addr, Duration)>, ResolveError>> {
            Box::pin(async move {
                self.queries.fetch_add(1, Ordering::SeqCst);
                *self.last_name.lock() = Some(name.to_string());
                let mut s = self.script.lock();
                if s.len() > 1 {
                    s.remove(0)
                } else {
                    s[0].clone()
                }
            })
        }
    }

    fn res(kind: &str, kv: &[(&str, &str)]) -> AclResolver {
        AclResolver {
            r#type: kind.into(),
            config: kv
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
        }
    }

    async fn resolve(
        r: &Resolver,
        rn: &str,
        host: &str,
        cfg: &AclResolver,
        now: Instant,
    ) -> Result<Resolved, ResolveError> {
        r.resolve_at(rn, host, cfg, now).await
    }

    // ── dispatch ──

    #[tokio::test]
    async fn static_resolver_returns_configured_address() {
        let r = Resolver::new(FakeDns::new(vec![FakeDns::ok([1, 1, 1, 1], 60)]));
        let out = resolve(
            &r,
            "rn-1",
            "app.internal",
            &res("static", &[("address", "10.9.9.9")]),
            Instant::now(),
        )
        .await
        .unwrap();
        assert_eq!(out.address, Ipv4Addr::new(10, 9, 9, 9));
        assert!(!out.stale);
    }

    #[tokio::test]
    async fn static_resolver_rejects_missing_or_malformed_address() {
        let r = Resolver::new(FakeDns::new(vec![FakeDns::ok([1, 1, 1, 1], 60)]));
        for cfg in [
            res("static", &[]),
            res("static", &[("address", "not-an-ip")]),
        ] {
            assert_eq!(
                resolve(&r, "rn-1", "app.internal", &cfg, Instant::now()).await,
                Err(ResolveError::InvalidResolverConfig)
            );
        }
    }

    #[tokio::test]
    async fn unknown_resolver_type_is_unsupported_never_defaulted() {
        let r = Resolver::new(FakeDns::new(vec![FakeDns::ok([1, 1, 1, 1], 60)]));
        // "shield" is NOT a resolver type — delivery is route_type, an orthogonal axis.
        for kind in ["shield", "k8s", ""] {
            assert_eq!(
                resolve(&r, "rn-1", "app.internal", &res(kind, &[]), Instant::now()).await,
                Err(ResolveError::UnsupportedResolver)
            );
        }
    }

    #[tokio::test]
    async fn dns_config_name_overrides_client_facing_hostname() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 60)]);
        let r = Resolver::new(dns.clone());
        resolve(
            &r,
            "rn-1",
            "app.internal",
            &res("dns", &[("name", "backend.svc.cluster.local")]),
            Instant::now(),
        )
        .await
        .unwrap();
        assert_eq!(
            dns.last_name.lock().as_deref(),
            Some("backend.svc.cluster.local")
        );
    }

    #[tokio::test]
    async fn dns_rejects_unsupported_server_key_rather_than_ignoring_it() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 60)]);
        let r = Resolver::new(dns.clone());
        assert_eq!(
            resolve(
                &r,
                "rn-1",
                "app.internal",
                &res("dns", &[("server", "10.0.0.53")]),
                Instant::now()
            )
            .await,
            Err(ResolveError::InvalidResolverConfig)
        );
        assert_eq!(dns.count(), 0, "must not query the system resolver instead");
    }

    // ── cache + TTL ──

    #[tokio::test]
    async fn cache_hit_within_ttl_issues_no_second_query() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 60)]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "app.internal", &cfg, t0).await.unwrap();
        resolve(
            &r,
            "rn-1",
            "app.internal",
            &cfg,
            t0 + Duration::from_secs(30),
        )
        .await
        .unwrap();
        assert_eq!(dns.count(), 1);
    }

    #[tokio::test]
    async fn expiry_requeries_and_picks_up_the_new_address() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            FakeDns::ok([10, 0, 0, 9], 60),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap().address,
            Ipv4Addr::new(10, 0, 0, 5)
        );
        // This is the sprint's whole point: the backend moved, nothing upstream changed.
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61))
                .await
                .unwrap()
                .address,
            Ipv4Addr::new(10, 0, 0, 9)
        );
        assert_eq!(dns.count(), 2);
    }

    #[tokio::test]
    async fn ttl_below_floor_is_clamped_up() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 1)]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        // A 1s record would have expired; the 5s floor keeps it cached.
        resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(3))
            .await
            .unwrap();
        assert_eq!(dns.count(), 1);
    }

    #[tokio::test]
    async fn ttl_above_ceiling_is_clamped_down() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 86_400)]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        // A 24h record must not pin the address past the 300s ceiling.
        resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(301))
            .await
            .unwrap();
        assert_eq!(dns.count(), 2);
    }

    #[tokio::test]
    async fn same_name_in_two_remote_networks_is_isolated() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            FakeDns::ok([10, 1, 1, 5], 60),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        let a = resolve(&r, "rn-1", "app.internal", &cfg, t0).await.unwrap();
        let b = resolve(&r, "rn-2", "app.internal", &cfg, t0).await.unwrap();
        assert_ne!(
            a.address, b.address,
            "remote networks must not share cache entries"
        );
        assert_eq!(dns.count(), 2);
    }

    // ── failure handling ──

    #[tokio::test]
    async fn empty_answer_is_no_address_record_not_nxdomain() {
        let r = Resolver::new(FakeDns::new(vec![Ok(vec![])]));
        assert_eq!(
            resolve(&r, "rn-1", "h", &res("dns", &[]), Instant::now()).await,
            Err(ResolveError::NoAddressRecord)
        );
    }

    #[tokio::test]
    async fn nxdomain_is_negative_cached() {
        let dns = FakeDns::new(vec![Err(ResolveError::NxDomain)]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0).await,
            Err(ResolveError::NxDomain)
        );
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(1)).await,
            Err(ResolveError::NxDomain)
        );
        assert_eq!(dns.count(), 1, "a bad name must not become a DNS flood");
    }

    #[tokio::test]
    async fn resolver_unavailable_serves_last_known_good_as_stale() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::ResolverUnavailable),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        let out = resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61))
            .await
            .unwrap();
        assert_eq!(out.address, Ipv4Addr::new(10, 0, 0, 5));
        assert!(out.stale, "a DNS blip must not become an outage");
    }

    #[tokio::test]
    async fn servfail_serves_stale_like_unavailable() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::ResolverFailure),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        let out = resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61))
            .await
            .unwrap();
        assert!(out.stale);
        assert_eq!(out.address, Ipv4Addr::new(10, 0, 0, 5));
    }

    #[tokio::test]
    async fn stale_serving_is_bounded_then_fails_closed() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::ResolverUnavailable),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        let far = t0 + Duration::from_secs(60) + STALE_MAX + Duration::from_secs(1);
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, far).await,
            Err(ResolveError::ResolverUnavailable)
        );
    }

    /// A removed name is a decision. Honouring it after deletion is a failure to
    /// converge, so stale-on-error must NOT apply to NXDOMAIN.
    #[tokio::test]
    async fn nxdomain_does_not_serve_stale() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::NxDomain),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61)).await,
            Err(ResolveError::NxDomain)
        );
    }

    /// The hole this closes: the A record is deleted (an authoritative answer), then
    /// the resolver goes down — stale-on-error must not resurrect the dead address.
    #[tokio::test]
    async fn no_address_record_invalidates_cached_good() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Ok(vec![]),                             // A record removed
            Err(ResolveError::ResolverUnavailable), // then DNS goes down
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61)).await,
            Err(ResolveError::NoAddressRecord)
        );
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(122)).await,
            Err(ResolveError::ResolverUnavailable),
            "must not serve an address we were authoritatively told is gone"
        );
    }

    /// The two policies must partition cleanly: no failure may both serve stale and
    /// invalidate the cache.
    #[tokio::test]
    async fn stale_and_invalidate_policies_are_disjoint() {
        use ResolveError::*;
        for e in [
            NxDomain,
            ResolverUnavailable,
            ResolverFailure,
            NoAddressRecord,
            UnsupportedResolver,
            InvalidResolverConfig,
        ] {
            assert!(!(e.may_serve_stale() && e.invalidates_cache()), "{e:?}");
        }
        assert!(NxDomain.invalidates_cache() && !NxDomain.may_serve_stale());
        assert!(NoAddressRecord.invalidates_cache() && !NoAddressRecord.may_serve_stale());
        assert!(ResolverFailure.may_serve_stale());
        assert!(ResolverUnavailable.may_serve_stale());
    }

    #[tokio::test]
    async fn reason_tokens_are_distinct() {
        use ResolveError::*;
        let all = [
            NxDomain,
            ResolverUnavailable,
            ResolverFailure,
            NoAddressRecord,
            UnsupportedResolver,
            InvalidResolverConfig,
        ];
        let tokens: std::collections::HashSet<_> = all.iter().map(|e| e.reason()).collect();
        assert_eq!(tokens.len(), all.len(), "deny reasons must stay countable");
    }

    /// Regression guard for the fix: during a DNS outage the *second* connection must
    /// be served from the cache, not pay another resolver timeout.
    #[tokio::test]
    async fn stale_is_served_from_the_fast_path_without_requerying() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::ResolverUnavailable),
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();

        // First request after expiry queries and gets stale.
        let a = resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61))
            .await
            .unwrap();
        assert!(a.stale);
        assert_eq!(dns.count(), 2);

        // Inside STALE_RETRY: served from cache, no third query.
        let b = resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(63))
            .await
            .unwrap();
        assert!(b.stale);
        assert_eq!(b.address, Ipv4Addr::new(10, 0, 0, 5));
        assert_eq!(
            dns.count(),
            2,
            "an outage must not cost a timeout per connection"
        );
    }

    #[tokio::test]
    async fn stale_backoff_expires_and_recovery_is_picked_up() {
        let dns = FakeDns::new(vec![
            FakeDns::ok([10, 0, 0, 5], 60),
            Err(ResolveError::ResolverUnavailable),
            FakeDns::ok([10, 0, 0, 9], 60), // resolver recovers, backend moved
        ]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        resolve(&r, "rn-1", "h", &cfg, t0).await.unwrap();
        assert!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(61))
                .await
                .unwrap()
                .stale
        );
        // Past STALE_RETRY: re-query, and the fresh answer clears the stale flag.
        let out = resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(70))
            .await
            .unwrap();
        assert!(!out.stale);
        assert_eq!(out.address, Ipv4Addr::new(10, 0, 0, 9));
        assert_eq!(dns.count(), 3);
    }

    /// NODATA is as authoritative as NXDOMAIN and just as likely to be retried on
    /// every connection, so it must be suppressed too — and reported as *itself*,
    /// not as NXDOMAIN.
    #[tokio::test]
    async fn nodata_is_negative_cached_with_its_own_reason() {
        let dns = FakeDns::new(vec![Ok(vec![])]);
        let r = Resolver::new(dns.clone());
        let cfg = res("dns", &[]);
        let t0 = Instant::now();
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0).await,
            Err(ResolveError::NoAddressRecord)
        );
        assert_eq!(
            resolve(&r, "rn-1", "h", &cfg, t0 + Duration::from_secs(1)).await,
            Err(ResolveError::NoAddressRecord),
            "a suppressed NODATA must not be reported as NXDOMAIN"
        );
        assert_eq!(dns.count(), 1);
    }

    // ── hickory answer extraction ──
    //
    // The success path of the hickory glue. Previously the only real logic in this
    // module with no coverage: a mistake here surfaces first at Phase 7 E2E.

    use hickory_resolver::lookup::Lookup;
    use hickory_resolver::proto::rr::rdata::{A, CNAME};
    use hickory_resolver::proto::rr::Record;

    fn lookup_with(records: Vec<Record>) -> Lookup {
        let q = Query::query(Name::from_ascii("app.internal.").unwrap(), RecordType::A);
        Lookup::new_with_max_ttl(q, records)
    }

    fn a_rec(ip: [u8; 4]) -> Record {
        Record::from_rdata(
            Name::from_ascii("app.internal.").unwrap(),
            60,
            RData::A(A(Ipv4Addr::from(ip))),
        )
    }

    #[tokio::test]
    async fn a_records_extracts_every_a_answer() {
        let l = lookup_with(vec![a_rec([10, 0, 0, 5]), a_rec([10, 0, 0, 6])]);
        let got = a_records(&l, Instant::now());
        assert_eq!(
            got.iter().map(|(ip, _)| *ip).collect::<Vec<_>>(),
            vec![Ipv4Addr::new(10, 0, 0, 5), Ipv4Addr::new(10, 0, 0, 6)]
        );
        assert!(got.iter().all(|(_, ttl)| *ttl > Duration::ZERO));
    }

    /// A CNAME chain puts intermediate records in the answer set. Skipping them is
    /// what keeps a resolved alias from being read as NODATA.
    #[tokio::test]
    async fn a_records_skips_non_a_records_in_a_cname_chain() {
        let cname = Record::from_rdata(
            Name::from_ascii("app.internal.").unwrap(),
            60,
            RData::CNAME(CNAME(Name::from_ascii("real.internal.").unwrap())),
        );
        let l = lookup_with(vec![cname, a_rec([10, 0, 0, 7])]);
        let got = a_records(&l, Instant::now());
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].0, Ipv4Addr::new(10, 0, 0, 7));
    }

    #[tokio::test]
    async fn a_records_is_empty_for_no_answers_which_drives_nodata() {
        assert!(a_records(&lookup_with(vec![]), Instant::now()).is_empty());
    }

    /// A deadline already in the past must not underflow; it clamps to zero here and
    /// is lifted to TTL_MIN by `store`.
    #[tokio::test]
    async fn a_records_ttl_saturates_instead_of_underflowing() {
        let l = lookup_with(vec![a_rec([10, 0, 0, 5])]);
        let far_future = Instant::now() + Duration::from_secs(86_400 * 2);
        assert_eq!(a_records(&l, far_future)[0].1, Duration::ZERO);
    }

    // ── hickory error classification ──
    //
    // Hickory folds NXDOMAIN, NODATA and SERVFAIL into ONE `NoRecordsFound`
    // variant, separated only by `response_code`. One test per branch, because a
    // swap here silently inverts the stale-serving decision.

    use hickory_resolver::proto::op::{Query, ResponseCode};
    use hickory_resolver::proto::rr::{Name, RecordType};

    fn no_records(rc: ResponseCode) -> NetError {
        let q = Query::query(Name::from_ascii("app.internal.").unwrap(), RecordType::A);
        NetError::Dns(DnsError::NoRecordsFound(
            hickory_resolver::net::NoRecords::new(q, rc),
        ))
    }

    #[tokio::test]
    async fn classify_nxdomain_invalidates_and_never_serves_stale() {
        let e = classify(&no_records(ResponseCode::NXDomain));
        assert_eq!(e, ResolveError::NxDomain);
        assert!(e.invalidates_cache() && !e.may_serve_stale());
    }

    /// NoError inside NoRecordsFound is NODATA — the name exists, the A record does
    /// not. Also authoritative, so it must invalidate.
    #[tokio::test]
    async fn classify_nodata_is_no_address_record_not_nxdomain() {
        let e = classify(&no_records(ResponseCode::NoError));
        assert_eq!(e, ResolveError::NoAddressRecord);
        assert!(e.invalidates_cache() && !e.may_serve_stale());
    }

    #[tokio::test]
    async fn classify_servfail_and_refused_are_stale_eligible() {
        for rc in [ResponseCode::ServFail, ResponseCode::Refused] {
            let e = classify(&no_records(rc));
            assert_eq!(e, ResolveError::ResolverFailure, "{rc:?}");
            assert!(e.may_serve_stale() && !e.invalidates_cache(), "{rc:?}");
        }
    }

    #[tokio::test]
    async fn classify_timeout_is_resolver_unavailable() {
        let e = classify(&NetError::Timeout);
        assert_eq!(e, ResolveError::ResolverUnavailable);
        assert!(e.may_serve_stale());
    }

    /// Both hickory enums are #[non_exhaustive]. An error we don't recognise is a
    /// transport problem, never an authoritative answer — so it must never discard a
    /// cached address.
    #[tokio::test]
    async fn classify_unknown_errors_never_invalidate_the_cache() {
        for e in [
            classify(&NetError::Message("something new")),
            classify(&NetError::NoConnections),
            classify(&NetError::Busy),
        ] {
            assert!(
                !e.invalidates_cache(),
                "{e:?} must not discard a good address"
            );
            assert!(e.may_serve_stale(), "{e:?}");
        }
    }

    // ── single-flight ──

    #[tokio::test(flavor = "multi_thread")]
    async fn concurrent_resolves_of_a_cold_name_issue_one_query() {
        let dns = FakeDns::new(vec![FakeDns::ok([10, 0, 0, 5], 60)]);
        let r = Arc::new(Resolver::new(dns.clone()));
        let cfg = res("dns", &[]);
        let t0 = Instant::now();

        let mut set = tokio::task::JoinSet::new();
        for _ in 0..10 {
            let (r, cfg) = (r.clone(), cfg.clone());
            set.spawn(async move { r.resolve_at("rn-1", "h", &cfg, t0).await });
        }
        while let Some(joined) = set.join_next().await {
            assert_eq!(joined.unwrap().unwrap().address, Ipv4Addr::new(10, 0, 0, 5));
        }
        assert_eq!(
            dns.count(),
            1,
            "10 cold connections must issue 1 query, not 10"
        );
    }
}
