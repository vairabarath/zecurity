use std::collections::HashMap;
use std::sync::Arc;

use dashmap::DashMap;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

/// (spiffe_id, resource_id) — the same pair an ACL entry's allowed_spiffe_ids
/// check already resolves against. This is the registry's key shape, not a
/// per-device key, so a device can be authorized for one resource and lose
/// another without affecting both.
pub type SessionKey = (String, String);

/// Fresh per registration, not per (spiffe, resource) pair — two tunnels can
/// share the same pair and must not share an id.
pub type SessionId = Uuid;

/// Live index of open tunnels, keyed by (spiffe_id, resource_id). Nested
/// (not a flat Vec/DashMap<SessionKey, CancellationToken>) because a device
/// can legitimately hold more than one live tunnel to the same resource at
/// once — a flat design would let one tunnel's cleanup remove a sibling's
/// still-live token, making it silently uncancellable.
#[derive(Default)]
pub struct SessionRegistry {
    inner: DashMap<SessionKey, HashMap<SessionId, CancellationToken>>,
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
    pub fn register(self: &Arc<Self>, key: SessionKey) -> (CancellationToken, RegistryGuard) {
        let token = CancellationToken::new();
        let session_id = Uuid::new_v4();

        self.inner
            .entry(key.clone())
            .or_default()
            .insert(session_id, token.clone());

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
    /// session's own RegistryGuard removes its entry as it unwinds.
    pub fn cancel_all(&self, key: &SessionKey) {
        if let Some(sessions) = self.inner.get(key) {
            for token in sessions.value().values() {
                token.cancel();
            }
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
        let now_empty = match self.registry.inner.get_mut(&self.key) {
            Some(mut sessions) => {
                sessions.value_mut().remove(&self.session_id);
                sessions.value().is_empty()
            }
            None => return,
        };

        if now_empty {
            // Re-check-and-remove atomically under the map's own lock, not
            // based on the `now_empty` snapshot above — a concurrent register
            // could have added a new session for this key in between.
            self.registry.inner.remove_if(&self.key, |_, v| v.is_empty());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn key(spiffe: &str, resource: &str) -> SessionKey {
        (spiffe.to_string(), resource.to_string())
    }

    #[test]
    fn two_resources_for_same_spiffe_are_distinct_entries() {
        let registry = Arc::new(SessionRegistry::new());
        let (_t1, _g1) = registry.register(key("spiffe://a", "resource-1"));
        let (_t2, _g2) = registry.register(key("spiffe://a", "resource-2"));

        assert!(registry.contains_key(&key("spiffe://a", "resource-1")));
        assert!(registry.contains_key(&key("spiffe://a", "resource-2")));
    }

    #[test]
    fn closing_one_of_two_sessions_on_the_same_key_keeps_the_other_cancellable() {
        let registry = Arc::new(SessionRegistry::new());
        let k = key("spiffe://a", "resource-1");

        let (token1, guard1) = registry.register(k.clone());
        let (token2, _guard2) = registry.register(k.clone());

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

        let (_token, guard) = registry.register(k.clone());
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

        let (token, guard) = registry.register(k.clone());
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
                let (_token, guard) = registry.register(k.clone());
                tokio::task::yield_now().await;
                registry.cancel_all(&k);
                drop(guard);
            }));
        }
        for h in handles {
            h.await.unwrap();
        }
    }
}
