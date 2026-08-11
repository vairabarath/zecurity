use std::collections::HashMap;
use std::sync::Arc;

use dashmap::DashMap;
use tokio_util::sync::CancellationToken;
use tracing::info;
use uuid::Uuid;

/// (spiffe_id, resource_id) — the same pair an ACL entry's allowed_spiffe_ids
/// check already resolves against. This is the registry's key shape, not a
/// per-device key, so a device can be authorized for one resource and lose
/// another without affecting both.
pub type SessionKey = (String, String);

/// Fresh per registration, not per (spiffe, resource) pair — two tunnels can
/// share the same pair and must not share an id.
pub type SessionId = Uuid;

/// How a session's tunnel was delivered to the connector. Carried on the
/// cancellation log lines so ACL-diff teardown is observable per path —
/// relay-routed cancellations should track roughly with direct ones, not lag
/// or go missing (the fastest signal that the Phase 2 relay child-task fix
/// regressed).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub enum SessionTransport {
    /// Direct connector-route tunnel over the TLS/TCP accept path.
    Tcp,
    /// Direct connector-route tunnel over the QUIC accept path.
    Quic,
    /// Shield-relay-routed tunnel (the `RelaySession`/`d2s` child-task path)
    /// or a connector endpoint reached through a Relay.
    Relay,
}

impl SessionTransport {
    pub fn as_str(&self) -> &'static str {
        match self {
            SessionTransport::Tcp => "tcp",
            SessionTransport::Quic => "quic",
            SessionTransport::Relay => "relay",
        }
    }
}

/// Live index of open tunnels, keyed by (spiffe_id, resource_id). Nested
/// (not a flat Vec/DashMap<SessionKey, CancellationToken>) because a device
/// can legitimately hold more than one live tunnel to the same resource at
/// once — a flat design would let one tunnel's cleanup remove a sibling's
/// still-live token, making it silently uncancellable.
#[derive(Default)]
pub struct SessionRegistry {
    inner: DashMap<SessionKey, HashMap<SessionId, (CancellationToken, SessionTransport)>>,
}

impl SessionRegistry {
    pub fn new() -> Self {
        Self::default()
    }

    /// Registers a new session under `key`. Returns the token to race against
    /// the tunnel's actual I/O, and a guard that deregisters this session (and
    /// only this session) when dropped — covering every exit path (normal
    /// close, error, external cancellation) without relying on a manual
    /// cleanup call that could be forgotten on some path.
    pub fn register(
        self: &Arc<Self>,
        key: SessionKey,
        transport: SessionTransport,
    ) -> (CancellationToken, RegistryGuard) {
        let token = CancellationToken::new();
        let session_id = Uuid::new_v4();

        self.inner
            .entry(key.clone())
            .or_default()
            .insert(session_id, (token.clone(), transport));

        let total_sessions: usize = self.inner.iter().map(|e| e.value().len()).sum();
        info!(
            spiffe_id = %key.0,
            resource_id = %key.1,
            session_id = %session_id,
            transport = transport.as_str(),
            total_sessions,
            "session registered",
        );

        let guard = RegistryGuard {
            key,
            session_id,
            registry: Arc::clone(self),
        };

        (token, guard)
    }

    /// Cancels every session currently registered under `key`. Inert until
    /// something calls it — Phase 1 never does; this is Phase 2's ACL-diff
    /// teardown entry point. Does not remove entries itself: each cancelled
    /// session's own RegistryGuard removes its entry as it unwinds. One
    /// structured line is logged per cancelled session, labeled by transport
    /// (tcp/quic/relay) so teardown is observable per delivery path.
    pub fn cancel_all(&self, key: &SessionKey) {
        let cancelled: Vec<(SessionId, SessionTransport)> = {
            let Some(sessions) = self.inner.get(key) else {
                return;
            };
            sessions
                .value()
                .iter()
                .map(|(id, (token, transport))| {
                    token.cancel();
                    (*id, *transport)
                })
                .collect()
        };
        for (session_id, transport) in cancelled {
            info!(
                spiffe_id = %key.0,
                resource_id = %key.1,
                session_id = %session_id,
                reason = "acl_diff",
                transport = transport.as_str(),
                "ACL diff: session cancelled",
            );
        }
    }

    #[cfg(test)]
    pub fn session_count(&self, key: &SessionKey) -> usize {
        self.inner.get(key).map(|s| s.value().len()).unwrap_or(0)
    }

    #[cfg(test)]
    pub fn contains_key(&self, key: &SessionKey) -> bool {
        self.inner.contains_key(key)
    }
}

pub struct RegistryGuard {
    key: SessionKey,
    session_id: SessionId,
    registry: Arc<SessionRegistry>,
}

impl Drop for RegistryGuard {
    fn drop(&mut self) {
        let removed_session = self.session_id;
        let now_empty = match self.registry.inner.get_mut(&self.key) {
            Some(mut sessions) => {
                sessions.value_mut().remove(&self.session_id);
                sessions.value().is_empty()
            }
            None => return,
        };

        if now_empty {
            self.registry.inner.remove_if(&self.key, |_, v| v.is_empty());
        }

        let remaining_sessions: usize = self
            .registry
            .inner
            .iter()
            .map(|e| e.value().len())
            .sum();
        info!(
            spiffe_id = %self.key.0,
            resource_id = %self.key.1,
            session_id = %removed_session,
            remaining_sessions,
            "session unregistered",
        );
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{Mutex, OnceLock};

    use super::*;

    fn key(spiffe: &str, resource: &str) -> SessionKey {
        (spiffe.to_string(), resource.to_string())
    }

    #[test]
    fn two_resources_for_same_spiffe_are_distinct_entries() {
        let registry = Arc::new(SessionRegistry::new());
        let (_t1, _g1) = registry.register(key("spiffe://a", "resource-1"), SessionTransport::Tcp);
        let (_t2, _g2) = registry.register(key("spiffe://a", "resource-2"), SessionTransport::Quic);

        assert!(registry.contains_key(&key("spiffe://a", "resource-1")));
        assert!(registry.contains_key(&key("spiffe://a", "resource-2")));
    }

    #[test]
    fn closing_one_of_two_sessions_on_the_same_key_keeps_the_other_cancellable() {
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "resource-1");

        let (token1, guard1) = registry.register(k.clone(), SessionTransport::Tcp);
        let (token2, _guard2) = registry.register(k.clone(), SessionTransport::Tcp);

        assert_eq!(registry.session_count(&k), 2);

        drop(guard1); // simulates one tunnel closing

        assert_eq!(
            registry.session_count(&k),
            1,
            "closing one session must not remove the other"
        );
        assert!(!token1.is_cancelled());
        assert!(
            !token2.is_cancelled(),
            "the surviving session's token must still be live and cancellable"
        );

        registry.cancel_all(&k);
        assert!(
            token2.is_cancelled(),
            "the surviving session must still be reachable via cancel_all"
        );
    }

    #[test]
    fn entry_is_removed_on_normal_drop_no_leak() {
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "resource-1");

        let (_token, guard) = registry.register(k.clone(), SessionTransport::Tcp);
        assert!(registry.contains_key(&k));

        drop(guard);

        assert!(
            !registry.contains_key(&k),
            "outer key must be removed once its last session closes"
        );
    }

    #[test]
    fn cancel_all_marks_tokens_cancelled_but_guard_still_owns_removal() {
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "resource-1");

        let (token, guard) = registry.register(k.clone(), SessionTransport::Relay);
        registry.cancel_all(&k);

        assert!(token.is_cancelled());
        // cancel_all only signals — the entry is removed by the guard, not by
        // cancel_all itself.
        assert!(registry.contains_key(&k));

        drop(guard);
        assert!(!registry.contains_key(&k));
    }

    #[tokio::test]
    async fn concurrent_register_and_cancel_do_not_panic() {
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "resource-1");

        let mut handles = Vec::new();
        for _ in 0..50 {
            let registry = Arc::clone(&registry);
            let k = k.clone();
            handles.push(tokio::spawn(async move {
                let (_token, guard) = registry.register(k.clone(), SessionTransport::Tcp);
                tokio::task::yield_now().await;
                registry.cancel_all(&k);
                drop(guard);
            }));
        }
        for h in handles {
            h.await.unwrap();
        }
    }

    /// Minimal `io::Write` used by the test-only global fmt subscriber.
    ///
    /// Bytes are staged in a per-event `local` buffer and flushed to the shared
    /// capture buffer atomically in `Drop`, so one formatted log line can never
    /// be interleaved with another thread's line.
    struct RecordingWriter {
        shared: Arc<Mutex<Vec<u8>>>,
        local: Vec<u8>,
    }

    impl std::io::Write for RecordingWriter {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.local.extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    impl Drop for RecordingWriter {
        fn drop(&mut self) {
            if !self.local.is_empty() {
                self.shared.lock().unwrap().extend_from_slice(&self.local);
            }
        }
    }

    /// Returns the shared capture buffer, installing the process-wide test
    /// subscriber on first use.
    ///
    /// The subscriber is registered via `set_global_default` **exactly once**.
    /// Registering a fresh subscriber per test (the obvious `with_default`
    /// approach) makes `tracing` rebuild its global callsite-interest cache on
    /// every test, and concurrent rebuilds race — some threads' `info!` events
    /// get statically filtered and silently dropped, producing intermittently
    /// empty captures. A single global subscriber avoids that entirely.
    fn test_logs() -> &'static Arc<Mutex<Vec<u8>>> {
        static LOGS: OnceLock<Arc<Mutex<Vec<u8>>>> = OnceLock::new();
        static INIT: OnceLock<()> = OnceLock::new();
        INIT.get_or_init(|| {
            let logs = LOGS.get_or_init(|| Arc::new(Mutex::new(Vec::new())));
            let subscriber = tracing_subscriber::fmt()
                .with_writer({
                    let logs = Arc::clone(logs);
                    move || RecordingWriter {
                        shared: Arc::clone(&logs),
                        local: Vec::new(),
                    }
                })
                .with_ansi(false)
                .with_max_level(tracing::Level::INFO)
                .finish();
            let _ = tracing::subscriber::set_global_default(subscriber);
        });
        LOGS.get().expect("capture buffer initialized")
    }

    /// Byte offset of the end of the shared log buffer.
    fn log_len() -> usize {
        test_logs().lock().unwrap().len()
    }

    /// Bytes appended to the shared buffer since `start`.
    fn log_slice(start: usize) -> String {
        let logs = test_logs().lock().unwrap();
        String::from_utf8_lossy(&logs[start..]).into_owned()
    }

    #[test]
    fn registry_size_log_updates_on_insert_and_remove() {
        let start = log_len();
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "res-1");

        let (_t1, guard1) = registry.register(k.clone(), SessionTransport::Tcp);
        let (_t2, guard2) = registry.register(k.clone(), SessionTransport::Quic);
        assert_eq!(registry.session_count(&k), 2);

        drop(guard1);
        assert_eq!(registry.session_count(&k), 1);

        drop(guard2);
        assert_eq!(registry.session_count(&k), 0);

        let log = log_slice(start);
        assert!(log.contains("total_sessions=2"), "log was:\n{log}");
        assert!(log.contains("remaining_sessions=1"), "log was:\n{log}");
        assert!(log.contains("remaining_sessions=0"), "log was:\n{log}");
    }

    #[test]
    fn cancellation_logs_are_labeled_by_transport() {
        let start = log_len();
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "res-1");

        let (_tcp, _g_tcp) = registry.register(k.clone(), SessionTransport::Tcp);
        let (_quic, _g_quic) = registry.register(k.clone(), SessionTransport::Quic);
        let (_relay, _g_relay) = registry.register(k.clone(), SessionTransport::Relay);

        registry.cancel_all(&k);

        let log = log_slice(start);
        assert!(log.contains("transport=\"tcp\""), "log was:\n{log}");
        assert!(log.contains("transport=\"quic\""), "log was:\n{log}");
        assert!(log.contains("transport=\"relay\""), "log was:\n{log}");
        assert!(log.contains("reason=\"acl_diff\""), "log was:\n{log}");
    }
}
