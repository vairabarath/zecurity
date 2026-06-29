---
type: phase
member: M3
sprint: 11
phase: 3
title: Connector Integration & End-to-End Validation
depends_on:
  - Sprint11/Member3-Rust/Phase2-Selector-Migration
  - Sprint11/Member2-Go/Phase3-Integration
---

# Phase 3 — Connector Integration & End-to-End Validation

## Goal

Validate the full connector relay selection lifecycle against a real controller and relay.

## Scenarios

### Scenario 1 — Cold start, no state file

1. No `relay_ranking.json` exists.
2. Connector starts → receives `LabelledRelayList` with 3 Tier 1 relays.
3. Picks one at random → registers → reports `ConnectorRelayState`.
4. Background probe fires → scores all relays → persists top 5 to state file.

### Scenario 2 — Warm restart

1. `relay_ranking.json` exists and is fresh.
2. Connector restarts → reads `ranked[0]` → connects immediately without probe.
3. Background re-probe fires → refreshes ranking.

### Scenario 3 — Background migration

1. Connector connected to relay scoring 30ms.
2. Background probe finds relay scoring 10ms — improvement = 67% and 20ms absolute → both thresholds met.
3. Phase 3 migration → make-before-break → no active stream drops.
4. Controller receives `ConnectorRelayState` → `connector_relay_placement` updated.
5. Client fetches ACL within 15s → new `relay_addr` in snapshot.

### Scenario 4 — Probe security

1. Probe response with wrong `request_id` → discarded, relay not scored.
2. Probe to relay with wrong SPIFFE cert → mTLS failure → treated as unreachable.

### Scenario 5 — 1,000 connector simulation

1. 1,000 connectors boot simultaneously against 5 Tier 1 relays.
2. Assert distribution: no relay receives > 2× average (200 × 1.5 = 300 max).
3. Assert no probe storm: connectors connect before probing.

## Build Check

```bash
cd connector && cargo build
cd connector && cargo test
```

## Implementation Checklist

- [ ] **TEAM-E3** Connector restart → reads persisted ranking → connects to `ranked[0]` immediately → background re-probe fires; no traffic loss during 15s ACL sync window
- [ ] **TEAM-E5** Probe with wrong `request_id` → discarded; probe to wrong SPIFFE peer → mTLS failure → treated as unreachable
- [ ] **TEAM-E6** 1,000 simulated connectors boot simultaneously → no single Tier 1 relay receives > 2× average connections
- [ ] **TEAM-E7** Background optimization finds > 15% + 10ms improvement → make-before-break migration → zero active stream drops
- [ ] **Build gate:** `cd connector && cargo build` and `cargo test` pass
