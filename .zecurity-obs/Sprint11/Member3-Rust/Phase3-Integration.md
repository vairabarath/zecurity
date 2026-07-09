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

- [ ] **TEAM-E3** Connector restart → reads persisted ranking → connects to `ranked[0]` immediately → background re-probe fires; no traffic loss during 15s ACL sync window — _planned as `connector/tests/scenario2_warm_restart.rs`. Pending: test relay needs a register-path handler + accept-loop extension._
- [x] **TEAM-E5** Probe with wrong `request_id` → discarded; probe to wrong SPIFFE peer → mTLS failure → treated as unreachable. **Ships as `connector/tests/scenario4_probe_security.rs`** — 4 tests green: baseline correct probe succeeds, wrong `request_id` rejected, wrong SPIFFE → `ExactRelaySpiffeVerifier` rejects mTLS, silent relay (no response) dropped. Tests run against a real in-process QUIC mTLS relay built with `rcgen` (workspace CA + leaves with SPIFFE URI SANs).
- [ ] **TEAM-E6** 1,000 simulated connectors boot simultaneously → no single Tier 1 relay receives > 2× average connections — _planned as `connector/examples/load_test.rs`. Pending: needs the register-path relay so the boot path completes._
- [ ] **TEAM-E7** Background optimization finds > 15% + 10ms improvement → make-before-break migration → zero active stream drops — _planned as `connector/tests/scenario3_migration.rs` (control-plane assertions in v1; data-plane stream-drop assertion `#[ignore]`-scaffolded for follow-up). Pending: register-path relay + injectable latency._
- [~] **Build gate:** `cd connector && cargo build` and `cargo test` pass. **Currently green:** 52 unit tests + 4 scenario4 integration tests + 1 doctest (`ignore`d after lib restructure made it runnable). Will re-verify after scenarios 1/2/3 land.

**Phase 3 status:** **Partially done.**
- ✅ Foundation merged: `src/lib.rs` (library surface for integration tests), `connector/Cargo.toml` dev-deps + `test-hooks` feature + `[[example]]`, `tests/common/mod.rs` shared harness with cert factory + probe-only in-process QUIC mTLS relay.
- ✅ Scenario 4 (TEAM-E5) shipped — proves the harness works end-to-end and validates the connector's `ExactRelaySpiffeVerifier` + `request_id` echo check against a real QUIC peer.
- ⏳ Scenarios 1/2/3 + load test need an extension to `tests/common/mod.rs`: a register-path test relay (responds to `HandshakeMsg::Register` with `RelayAck{ok:true}` and keeps the connection alive for the connector's `RelayHandler::run` accept loop), plus a `boot_selector` helper that constructs `RelaySelectorConfig` + spawns `relay_selector::run` and surfaces the `SelectorEvent` broadcast subscriber.

Preconditions complete: M2 proto (✅ `c6e4ab4`), M2 controller Phase C (✅ `bee9884`), M4 relay probe responder (✅ `7e07893`), M3 Phase 1 + Phase 2 (✅ `9de4f50`).
