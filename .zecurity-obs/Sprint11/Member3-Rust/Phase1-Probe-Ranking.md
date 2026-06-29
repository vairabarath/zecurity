---
type: phase
member: M3
sprint: 11
phase: 1
title: Connector Probe & Persisted Ranking
depends_on:
  - Sprint11/Member2-Go/Phase1-Proto
  - Sprint11/Member4-Relay/Phase1-Telemetry
---

# Phase 1 — Connector Probe & Persisted Ranking

## Goal

Implement the relay probe module and the persisted ranking state file. These are the building blocks for the selector in Phase 2.

## Files

| File | Change |
|---|---|
| `connector/src/relay_probe.rs` (new) | Parallel QUIC probe, RTT, scoring, validation |
| `connector/src/relay_ranking.rs` (new) | RelayRanking struct, atomic state file, staleness check |
| `connector/src/config.rs` | Add all `RELAY_*` config vars |

## relay_probe.rs

```rust
pub struct RelayProbeResult {
    pub relay_id:   String,
    pub relay_addr: String,
    pub spiffe_id:  String,
    pub rtt_ms:     u64,
    pub fill_ratio: f64,   // connection_count / capacity from ProbeResponse
    pub score:      u64,   // rtt_ms + ceil(fill_ratio * 50)
}

/// Probe all candidates concurrently, up to RELAY_MAX_CONCURRENT_PROBES in parallel.
/// Each probe:
///   1. Dial relay_addr via QUIC mTLS
///   2. Validate QUIC peer SPIFFE == LabelledRelayInfo.spiffe_id → failure if mismatch
///   3. Send ProbeRequest { connector_id, request_id: random_u64() }
///   4. Receive ProbeResponse; validate request_id matches → failure if mismatch
///   5. Measure RTT = wall_clock_from_dial_start_to_probe_response_received
///   6. Compute score
/// Unreachable / timeout / mismatch → silently dropped from results
pub async fn probe_relays(
    candidates: &[LabelledRelayInfo],
    connector_id: &str,
    device: &DeviceInfo,
    max_concurrent: usize,
) -> Vec<RelayProbeResult>
```

## relay_ranking.rs

```rust
pub struct RankedEntry {
    pub rank:       usize,
    pub relay_id:   String,
    pub relay_addr: String,
    pub spiffe_id:  String,
    pub score:      u64,
    pub rtt_ms:     u64,
    pub fill_ratio: f64,
}

pub struct RelayRanking {
    pub list_version: u64,
    pub probed_at:    DateTime<Utc>,
    pub entries:      Vec<RankedEntry>, // top 5
}

impl RelayRanking {
    /// Atomically write to <state_dir>/relay_ranking.json (write .tmp then rename)
    pub fn save(&self, state_dir: &Path) -> Result<()>

    /// Read from state file. Returns None if missing or corrupt.
    pub fn load(state_dir: &Path) -> Option<Self>

    /// Filter entries to those still present in current LabelledRelayList as Tier1 or Tier2.
    pub fn valid_entries<'a>(&'a self, current_list: &LabelledRelayList) -> Vec<&'a RankedEntry>

    /// Is the ranking fresh enough to use directly (< 1h old)?
    pub fn is_fresh(&self) -> bool

    /// Does the list_version match the current pushed list?
    pub fn version_matches(&self, current_version: u64) -> bool
}
```

## State File Format

```json
{
  "list_version": 7,
  "probed_at": "2026-06-27T10:34:00Z",
  "entries": [
    {
      "rank": 0,
      "relay_id": "<uuid>",
      "relay_addr": "relay1.example.com:9093",
      "spiffe_id": "spiffe://zecurity.in/relay/<uuid>",
      "score": 12,
      "rtt_ms": 8,
      "fill_ratio": 0.08
    }
  ]
}
```

## Config

```
RELAY_REPROBE_INTERVAL_SECS   = 300
RELAY_MAX_CONCURRENT_PROBES   = 5
RELAY_RECONNECT_BASE_SECS     = 5
RELAY_RECONNECT_MAX_SECS      = 120
RELAY_RECONNECT_BACKOFF_FACTOR = 2.0
RELAY_DRAIN_TIMEOUT_SECS      = 30
```

## Tests

- Probe with correct `request_id` → result accepted
- Probe with mismatched `request_id` → result discarded
- Probe with wrong SPIFFE peer → treated as failure
- `valid_entries` filters absent/exhausted relays correctly
- `is_fresh` returns false for entries > 1h old
- Atomic write: partial write failure leaves previous file intact

## Build Check

```bash
cd connector && cargo build
```

## Implementation Checklist

- [ ] **M3-D1** `connector/src/relay_probe.rs` (new) — `probe_relays()`: parallel QUIC mTLS dial, `request_id` generate + echo validate, QUIC peer SPIFFE validate against `LabelledRelayInfo.spiffe_id`, RTT measurement, score = `rtt_ms + ceil(fill_ratio × 50)`, concurrent cap via semaphore
- [ ] **M3-D2** `connector/src/relay_ranking.rs` (new) — `RelayRanking` struct; `save()` atomic write (`.tmp` → rename); `load()` returning `None` on missing/corrupt; `valid_entries()` filtering absent/exhausted; `is_fresh()` (< 1h); `version_matches()`
- [ ] **M3-D3** `connector/src/config.rs` — add `RELAY_REPROBE_INTERVAL_SECS` (300), `RELAY_MAX_CONCURRENT_PROBES` (5), `RELAY_RECONNECT_BASE_SECS` (5), `RELAY_RECONNECT_MAX_SECS` (120), `RELAY_RECONNECT_BACKOFF_FACTOR` (2.0), `RELAY_DRAIN_TIMEOUT_SECS` (30)
- [ ] **Tests:** `request_id` mismatch → dropped; SPIFFE mismatch → failure; `valid_entries` filters correctly; `is_fresh` false for > 1h; atomic write leaves previous file on partial failure
- [ ] **Build gate:** `cd connector && cargo build` passes
