// Sprint 11 ADR-016 Phase 1 — persisted relay ranking.
//
// Stores the connector's top-N probed relays so a restart can connect to a
// known-good relay immediately, before the background probe loop refreshes
// the data. Phase 1 ships the file format and the freshness/version checks;
// Phase 2's selector consumes it.
//
// File path: <state_dir>/relay_ranking.json.
// Atomic write: write .tmp + fsync, then rename — crash-during-write leaves
// any prior file intact.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};

use crate::proto::LabelledRelayList;

const RANKING_FILE: &str = "relay_ranking.json";
const RANKING_TMP_FILE: &str = "relay_ranking.json.tmp";
const FRESHNESS_SECS: i64 = 60 * 60; // 1 hour

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RankedEntry {
    pub rank: usize,
    pub relay_id: String,
    pub relay_addr: String,
    pub spiffe_id: String,
    pub score: u64,
    pub rtt_ms: u64,
    pub fill_ratio: f64,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct RelayRanking {
    pub list_version: u64,
    /// Unix seconds at the time the probe sweep completed.
    pub probed_at_unix: i64,
    pub entries: Vec<RankedEntry>,
}

impl RelayRanking {
    pub fn save(&self, state_dir: &Path) -> Result<()> {
        fs::create_dir_all(state_dir)
            .with_context(|| format!("create state dir {}", state_dir.display()))?;
        let final_path = state_dir.join(RANKING_FILE);
        let tmp_path = state_dir.join(RANKING_TMP_FILE);

        let body = serde_json::to_vec_pretty(self).context("encode RelayRanking JSON")?;
        {
            let mut tmp = fs::File::create(&tmp_path)
                .with_context(|| format!("create {}", tmp_path.display()))?;
            tmp.write_all(&body)
                .with_context(|| format!("write {}", tmp_path.display()))?;
            tmp.sync_all()
                .with_context(|| format!("fsync {}", tmp_path.display()))?;
        }
        fs::rename(&tmp_path, &final_path).with_context(|| {
            format!(
                "rename {} -> {}",
                tmp_path.display(),
                final_path.display()
            )
        })?;
        Ok(())
    }

    pub fn load(state_dir: &Path) -> Option<Self> {
        let path: PathBuf = state_dir.join(RANKING_FILE);
        let body = fs::read(&path).ok()?;
        serde_json::from_slice(&body).ok()
    }

    /// Filter to entries whose `relay_id` is present in the currently-pushed
    /// labelled list. The controller already drops exhausted relays from the
    /// list, so "present in `current_list`" is itself the tier check.
    pub fn valid_entries<'a>(&'a self, current_list: &LabelledRelayList) -> Vec<&'a RankedEntry> {
        self.entries
            .iter()
            .filter(|entry| {
                current_list
                    .relays
                    .iter()
                    .any(|info| info.relay_id == entry.relay_id)
            })
            .collect()
    }

    /// Ranking is fresh enough for the cold-start fast path.
    pub fn is_fresh(&self) -> bool {
        let now = now_unix_seconds();
        let age = now.saturating_sub(self.probed_at_unix);
        age >= 0 && age < FRESHNESS_SECS
    }

    pub fn version_matches(&self, current_version: u64) -> bool {
        self.list_version == current_version
    }
}

pub fn now_unix_seconds() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{LabelledRelayInfo, RelayCapacityLabel};

    fn make_entry(rank: usize, relay_id: &str, score: u64) -> RankedEntry {
        RankedEntry {
            rank,
            relay_id: relay_id.into(),
            relay_addr: format!("{relay_id}.example.com:9093"),
            spiffe_id: format!("spiffe://zecurity.in/relay/{relay_id}"),
            score,
            rtt_ms: score,
            fill_ratio: 0.0,
        }
    }

    fn make_ranking(entries: Vec<RankedEntry>, version: u64) -> RelayRanking {
        RelayRanking {
            list_version: version,
            probed_at_unix: now_unix_seconds(),
            entries,
        }
    }

    fn make_list(relay_ids: &[&str], version: u64) -> LabelledRelayList {
        LabelledRelayList {
            relays: relay_ids
                .iter()
                .map(|id| LabelledRelayInfo {
                    relay_id: (*id).into(),
                    relay_addr: format!("{id}.example.com:9093"),
                    spiffe_id: format!("spiffe://zecurity.in/relay/{id}"),
                    label: RelayCapacityLabel::RelayCapacityHigh as i32,
                })
                .collect(),
            version,
        }
    }

    /// Self-cleaning tempdir so we don't pull `tempfile` until Phase 3.
    struct TempDir {
        path: PathBuf,
    }
    impl TempDir {
        fn new(label: &str) -> Self {
            let mut p = std::env::temp_dir();
            p.push(format!(
                "zecurity-ranking-test-{label}-{}",
                uuid::Uuid::new_v4()
            ));
            fs::create_dir_all(&p).unwrap();
            Self { path: p }
        }
        fn path(&self) -> &Path {
            &self.path
        }
    }
    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.path);
        }
    }

    #[test]
    fn save_then_load_roundtrip() {
        let dir = TempDir::new("roundtrip");
        let r = make_ranking(
            vec![
                make_entry(0, "11111111-1111-1111-1111-111111111111", 10),
                make_entry(1, "22222222-2222-2222-2222-222222222222", 25),
            ],
            7,
        );
        r.save(dir.path()).unwrap();
        let loaded = RelayRanking::load(dir.path()).unwrap();
        assert_eq!(loaded, r);
    }

    #[test]
    fn load_missing_returns_none() {
        let dir = TempDir::new("missing");
        assert!(RelayRanking::load(dir.path()).is_none());
    }

    #[test]
    fn load_corrupt_returns_none() {
        let dir = TempDir::new("corrupt");
        fs::write(dir.path().join(RANKING_FILE), b"\xff\xff not json \x00").unwrap();
        assert!(RelayRanking::load(dir.path()).is_none());
    }

    #[test]
    fn save_cleans_up_tmp() {
        let dir = TempDir::new("tmp-cleanup");
        let r = make_ranking(vec![make_entry(0, "aaaa", 1)], 1);
        r.save(dir.path()).unwrap();
        assert!(dir.path().join(RANKING_FILE).exists());
        assert!(!dir.path().join(RANKING_TMP_FILE).exists());
    }

    #[test]
    fn save_overwrites_previous_atomically() {
        let dir = TempDir::new("overwrite");
        make_ranking(vec![make_entry(0, "first", 10)], 1)
            .save(dir.path())
            .unwrap();
        make_ranking(vec![make_entry(0, "second", 20)], 2)
            .save(dir.path())
            .unwrap();
        let loaded = RelayRanking::load(dir.path()).unwrap();
        assert_eq!(loaded.list_version, 2);
        assert_eq!(loaded.entries[0].relay_id, "second");
    }

    #[test]
    fn valid_entries_filters_absent_relays() {
        let ranking = make_ranking(
            vec![
                make_entry(0, "alpha", 10),
                make_entry(1, "beta", 20),
                make_entry(2, "gamma", 30),
            ],
            1,
        );
        let list = make_list(&["alpha", "gamma"], 1);
        let valid = ranking.valid_entries(&list);
        assert_eq!(valid.len(), 2);
        assert_eq!(valid[0].relay_id, "alpha"); // preserves original order
        assert_eq!(valid[1].relay_id, "gamma");
    }

    #[test]
    fn valid_entries_empty_when_list_disjoint() {
        let ranking = make_ranking(vec![make_entry(0, "alpha", 10)], 1);
        let list = make_list(&["beta"], 1);
        assert!(ranking.valid_entries(&list).is_empty());
    }

    #[test]
    fn is_fresh_true_for_just_now() {
        let r = make_ranking(vec![], 1);
        assert!(r.is_fresh());
    }

    #[test]
    fn is_fresh_true_under_one_hour() {
        let r = RelayRanking {
            list_version: 1,
            probed_at_unix: now_unix_seconds() - (FRESHNESS_SECS - 10),
            entries: vec![],
        };
        assert!(r.is_fresh());
    }

    #[test]
    fn is_fresh_false_over_one_hour() {
        let r = RelayRanking {
            list_version: 1,
            probed_at_unix: now_unix_seconds() - FRESHNESS_SECS - 60,
            entries: vec![],
        };
        assert!(!r.is_fresh());
    }

    #[test]
    fn is_fresh_rejects_future_timestamp() {
        // Clock skew: probed_at after now → treat as stale, force a re-probe.
        let r = RelayRanking {
            list_version: 1,
            probed_at_unix: now_unix_seconds() + 3600,
            entries: vec![],
        };
        assert!(!r.is_fresh());
    }

    #[test]
    fn version_matches_branches() {
        let r = make_ranking(vec![], 42);
        assert!(r.version_matches(42));
        assert!(!r.version_matches(43));
        assert!(!r.version_matches(0));
    }
}
