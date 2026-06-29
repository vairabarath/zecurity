---
type: phase
member: M3
sprint: 11
phase: 2
title: Connector Relay Selector & Make-Before-Break Migration
depends_on:
  - Sprint11/Member3-Rust/Phase1-Probe-Ranking
  - Sprint11/Member2-Go/Phase2-Label-StateMachine
---

# Phase 2 — Connector Relay Selector & Make-Before-Break Migration

## Goal

Implement the three-phase relay selector state machine: instant startup, background optimization, and make-before-break migration. Wire it into the connector control stream and relay client.

## Files

| File | Change |
|---|---|
| `connector/src/relay_selector.rs` (new) | Three-phase state machine |
| `connector/src/control_stream.rs` | Handle `LabelledRelayList` body variant → send to selector |
| `connector/src/relay_client.rs` | Dual-connection support for make-before-break drain |
| `connector/src/config.rs` | Remove static `RELAY_ADDR` / `RELAY_SPIFFE_ID` |

## State Machine

```
State: Disconnected
  On recv LabelledRelayList:
    → load RelayRanking from state file
    → valid_entries(current_list) → use ranked[0] if present
    → else random Tier 1
    → else random Tier 2 + log warning
    → else enter Backoff(base=RELAY_RECONNECT_BASE_SECS)
    → dial + register → Phase1Connected

State: Phase1Connected(active_relay)
  On entry:
    → report ConnectorRelayState(active_relay) to controller
    → spawn background_probe_task
  On recv new LabelledRelayList.version > current:
    → update current list
    → if active_relay absent from list → immediate migration (skip threshold)
    → else → trigger re-probe
  On active_relay connection drop:
    → Failover

State: BackgroundProbing (sub-task of Phase1Connected)
  → probe all Tier1 + Tier2 relays (RELAY_MAX_CONCURRENT_PROBES parallel)
  → persist top 5 to state file
  → if active_relay absent from current list:
      → migrate to best_valid immediately (skip threshold check)
  → else if (current_score - best_score) > max(current_score * 0.15, 10ms):
      → Phase3Migration(best_relay)
  → else:
      → sleep RELAY_REPROBE_INTERVAL_SECS → re-probe

State: Phase3Migration(new_relay, old_relay)
  1. Dial new_relay + register
  2. On registration success:
     → route new outbound streams to new_relay
     → keep old_relay alive for drain
  3. Start drain timer: RELAY_DRAIN_TIMEOUT_SECS
  4. On drain timeout OR all old streams closed:
     → force-close old_relay
  5. → Phase1Connected(new_relay)

State: Failover(active_relay dropped)
  → valid = ranking.valid_entries(current_list)
  → for each in valid: attempt within 5s; on success → Phase1Connected
  → if all fail: full probe of current_list
  → if probe yields results: Phase1Connected(best)
  → if nothing: Backoff

State: Backoff(delay)
  → sleep delay
  → next_delay = min(delay * RELAY_RECONNECT_BACKOFF_FACTOR, RELAY_RECONNECT_MAX_SECS)
  → retry startup
```

## control_stream.rs

Add handler for `ConnectorControlMessage::RelayList(list)`:

```rust
ConnectorControlMessage::RelayList(list) => {
    relay_list_tx.send(list).ok();
}
```

The `relay_selector` consumes from `relay_list_rx` watch channel.

## relay_client.rs

During Phase 3, two relay connections are alive simultaneously:
- `active_conn` — old relay, serving existing streams until drain
- `pending_conn` — new relay, receiving all new streams

After drain completes: drop `active_conn`, promote `pending_conn` to `active_conn`.

## Tests

- Startup: ranked[0] valid → connects without probe
- Startup: no ranking → random Tier 1 selected
- Startup: no Tier 1 → Tier 2 selected + warning logged
- Active relay absent from new list → immediate migration (no threshold check)
- Normal migration: improvement > max(15%, 10ms) → Phase3Migration fires
- Normal migration: improvement < threshold → hold + re-probe after 5min
- Make-before-break: new streams routed to new relay only after registration succeeds
- Drain: old connection force-closed after RELAY_DRAIN_TIMEOUT_SECS
- Failover: ranked[1] used when ranked[0] unreachable; skips entries absent from current list
- Backoff: delay doubles on each retry, caps at RELAY_RECONNECT_MAX_SECS

## Build Check

```bash
cd connector && cargo build
cd connector && cargo test
```
