//! Sprint 24 R1 (Decision Record D-54): the F5 byte counters split by the
//! workspace of each bridged stream, plus its open streams, reported in the
//! heartbeat (`workspace_traffic`, field 13).
//!
//! One entry per trust domain the relay has bridged for. Counters are
//! cumulative for the life of the entry, like the process-wide F5 counters:
//! the controller derives rates from two samples and treats a decrease as a
//! restart. An entry is dropped only when it has no open stream and its
//! workspace is no longer allowed on this relay (D-40 policy), so the table
//! stays bounded by the workspaces the relay actually serves.

use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, LazyLock};

use dashmap::DashMap;

use crate::relay::v1::WorkspaceTraffic;

/// The relay's table. Bridges add to it next to the process-wide counters.
pub static TRAFFIC: LazyLock<TrafficTable> = LazyLock::new(TrafficTable::new);

/// One workspace's counters.
#[derive(Default)]
pub struct WorkspaceCounters {
    /// Lower-case workspace id, as the D-40 policy lists it.
    workspace_id: String,
    pub client_to_connector: AtomicU64,
    pub connector_to_client: AtomicU64,
    active_streams: AtomicU32,
}

/// Held by a bridge for its lifetime: one open stream of its workspace.
pub struct StreamGuard {
    counters: Arc<WorkspaceCounters>,
}

impl StreamGuard {
    pub fn counters(&self) -> &WorkspaceCounters {
        &self.counters
    }
}

impl Drop for StreamGuard {
    fn drop(&mut self) {
        self.counters.active_streams.fetch_sub(1, Ordering::Relaxed);
    }
}

pub struct TrafficTable {
    entries: DashMap<String, Arc<WorkspaceCounters>>,
}

impl TrafficTable {
    pub fn new() -> Self {
        Self {
            entries: DashMap::new(),
        }
    }

    /// Opens a stream for `trust_domain`. The stream is counted while the
    /// entry's map lock is held, so a concurrent `prune` either sees the open
    /// stream or ran before the entry was (re)created: a bridge never counts
    /// into an entry that has already left the table.
    pub fn open_stream(&self, trust_domain: &str, workspace_id: &str) -> StreamGuard {
        let entry = self
            .entries
            .entry(trust_domain.to_owned())
            .or_insert_with(|| {
                Arc::new(WorkspaceCounters {
                    workspace_id: workspace_id.to_ascii_lowercase(),
                    ..Default::default()
                })
            });
        entry.active_streams.fetch_add(1, Ordering::Relaxed);
        StreamGuard {
            counters: entry.value().clone(),
        }
    }

    /// Every entry with traffic or an open stream, ordered by trust domain.
    pub fn snapshot(&self) -> Vec<WorkspaceTraffic> {
        let mut out: Vec<WorkspaceTraffic> = self
            .entries
            .iter()
            .filter_map(|e| {
                let c = e.value();
                let up = c.client_to_connector.load(Ordering::Relaxed);
                let down = c.connector_to_client.load(Ordering::Relaxed);
                let streams = c.active_streams.load(Ordering::Relaxed);
                (up > 0 || down > 0 || streams > 0).then(|| WorkspaceTraffic {
                    trust_domain: e.key().clone(),
                    bytes_client_to_connector: up,
                    bytes_connector_to_client: down,
                    active_streams: streams,
                })
            })
            .collect();
        out.sort_by(|a, b| a.trust_domain.cmp(&b.trust_domain));
        out
    }

    /// Drops idle entries whose workspace `allowed` refuses. Returns how many.
    pub fn prune(&self, allowed: impl Fn(&str) -> bool) -> usize {
        let before = self.entries.len();
        self.entries.retain(|_, c| {
            c.active_streams.load(Ordering::Relaxed) > 0 || allowed(&c.workspace_id)
        });
        before.saturating_sub(self.entries.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn add(guard: &StreamGuard, up: u64, down: u64) {
        guard
            .counters()
            .client_to_connector
            .fetch_add(up, Ordering::Relaxed);
        guard
            .counters()
            .connector_to_client
            .fetch_add(down, Ordering::Relaxed);
    }

    #[test]
    fn workspaces_are_counted_separately() {
        let t = TrafficTable::new();
        let a1 = t.open_stream("ws-a.zecurity.in", "AAAA");
        let a2 = t.open_stream("ws-a.zecurity.in", "aaaa");
        let b = t.open_stream("ws-b.zecurity.in", "bbbb");
        add(&a1, 10, 100);
        add(&a2, 1, 1);
        add(&b, 5, 50);
        let snap = t.snapshot();
        assert_eq!(
            snap,
            vec![
                WorkspaceTraffic {
                    trust_domain: "ws-a.zecurity.in".into(),
                    bytes_client_to_connector: 11,
                    bytes_connector_to_client: 101,
                    active_streams: 2,
                },
                WorkspaceTraffic {
                    trust_domain: "ws-b.zecurity.in".into(),
                    bytes_client_to_connector: 5,
                    bytes_connector_to_client: 50,
                    active_streams: 1,
                },
            ]
        );
        drop(a1);
        assert_eq!(
            t.snapshot()[0].active_streams,
            1,
            "a closed stream is no longer open"
        );
        assert_eq!(
            t.snapshot()[0].bytes_connector_to_client,
            101,
            "counters stay cumulative after a stream closes"
        );
    }

    #[test]
    fn an_entry_without_traffic_or_streams_is_not_reported() {
        let t = TrafficTable::new();
        drop(t.open_stream("ws-idle.zecurity.in", "cccc"));
        assert!(t.snapshot().is_empty());
    }

    #[test]
    fn prune_drops_only_idle_entries_of_disallowed_workspaces() {
        let t = TrafficTable::new();
        let open = t.open_stream("ws-open.zecurity.in", "1111");
        add(&open, 1, 1);
        add(&t.open_stream("ws-kept.zecurity.in", "2222"), 1, 1);
        add(&t.open_stream("ws-gone.zecurity.in", "3333"), 1, 1);
        // Only 2222 is allowed; 1111 is refused too but still has a stream.
        let dropped = t.prune(|ws| ws == "2222");
        assert_eq!(dropped, 1);
        let left: Vec<_> = t.snapshot().into_iter().map(|w| w.trust_domain).collect();
        assert_eq!(left, vec!["ws-kept.zecurity.in", "ws-open.zecurity.in"]);
        drop(open);
        assert_eq!(t.prune(|ws| ws == "2222"), 1, "idle now, so it goes");
    }

    #[test]
    fn prune_matches_the_lower_cased_workspace_id() {
        let t = TrafficTable::new();
        add(&t.open_stream("ws-x.zecurity.in", "ABCD-EF"), 1, 1);
        assert_eq!(t.prune(|ws| ws == "abcd-ef"), 0);
    }
}
