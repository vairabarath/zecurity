---
type: phase
member: M2
sprint: 11
phase: 3
title: Controller Integration & Validation
depends_on:
  - Sprint11/Member2-Go/Phase2-Label-StateMachine
  - Sprint11/Member3-Rust/Phase2-Selector-Migration
---

# Phase 3 — Controller Integration & Validation

## Goal

Verify end-to-end: controller pushes correct `LabelledRelayList`, connector reports `ConnectorRelayState`, `connector_relay_placement` is updated, and ACL snapshot reflects the new relay per connector.

## Integration Scenarios

### Scenario 1 — Two connectors, two relays

1. Start Relay A (30% capacity) and Relay B (40% capacity).
2. Start Connector 1 → connects to relay (random Tier 1).
3. Start Connector 2 → connects to relay (random Tier 1).
4. Assert `connector_relay_placement` has distinct relay entries.
5. Assert `GetACLSnapshot` shows each `ACLConnector` with its own `relay_addr`.

### Scenario 2 — Relay becomes exhausted

1. Relay A crosses 80% capacity → label promoted to exhausted after 60s hold-down.
2. Controller pushes updated `LabelledRelayList` (Relay A absent).
3. Connectors on Relay A detect absence → immediate migration.
4. Assert `connector_relay_placement` updated to Relay B.
5. Assert ACL recompiled and clients receive new snapshot.

### Scenario 3 — Controller alert on no Tier 1

1. All relays at > 50% capacity → no Tier 1 relays in list.
2. Controller logs warning: "no Tier 1 relays available; connectors will start on Tier 2".
3. Connectors start on Tier 2 relay → warning logged on connector side.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/... ./internal/connector/... ./internal/policy/...
```

## Implementation Checklist

- [ ] **TEAM-E1** Two connectors on different relays → each `ConnectorRelayState` report recorded → ACL snapshot shows distinct `relay_addr` per `ACLConnector`
- [ ] **TEAM-E2** Relay crosses 80% → exhausted after hold-down → dropped from list → connectors migrate → placement updated
- [ ] **TEAM-E3** Connector restart → reads persisted ranking → online immediately → background re-probe fires
- [ ] **TEAM-E4** No Tier 1 relays → Tier 2 fallback for startup → controller alert logged
- [ ] **Build gate:** `cd controller && go build ./...` and `go test ./internal/relay/... ./internal/connector/... ./internal/policy/...` pass
