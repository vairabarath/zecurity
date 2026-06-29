---
type: phase
member: M2
sprint: 11
phase: 2
title: Controller Label State Machine & LabelledRelayList Push
depends_on:
  - Sprint11/Member2-Go/Phase1-Proto
  - Sprint11/Member4-Relay/Phase1-Telemetry
---

# Phase 2 — Controller Label State Machine & LabelledRelayList Push

## Goal

The controller reads relay capacity from heartbeats, computes a tier label with hysteresis and hold-down, and pushes a versioned `LabelledRelayList` to all connector control streams when the label changes.

## DB Migration

Add to the `relays` table:

```sql
ALTER TABLE relays
  ADD COLUMN connection_count       int          NOT NULL DEFAULT 0,
  ADD COLUMN max_connections        int          NOT NULL DEFAULT 0,
  ADD COLUMN capacity_label         text         NOT NULL DEFAULT 'high',
  ADD COLUMN pending_capacity_label text,
  ADD COLUMN pending_label_since    timestamptz,
  ADD COLUMN last_label_changed_at  timestamptz;
```

## Hysteresis State Machine

Implemented in `controller/internal/relay/heartbeat.go` on each heartbeat:

```
fill_ratio = connection_count / max_connections

candidate_label:
  fill_ratio < RELAY_TIER1_ENTER (0.45)  → "high"
  fill_ratio < RELAY_TIER2_ENTER (0.75)  → "medium"
  fill_ratio < RELAY_TIER2_EXIT  (0.80)  → keep current (dead-band)
  otherwise                               → "exhausted" (drop from list)

State transitions:
  1. candidate == capacity_label         → clear pending fields
  2. candidate != capacity_label
       and candidate != pending           → set pending = candidate, pending_since = now
  3. candidate == pending
       and (now - pending_since) >= RELAY_LABEL_HOLDDOWN_SECS (60s)
                                          → promote: capacity_label = candidate
                                             last_label_changed_at = now
                                             clear pending fields
                                             increment list version
                                             push LabelledRelayList
```

## LabelledRelayList Push

Push triggered by:
- Connector control stream open → send current list snapshot
- Relay promoted to new label (heartbeat.go after promotion)
- Relay goes inactive/expired (expiry.go)
- Relay public address or SPIFFE changes

```go
// controller/internal/connector/control_stream.go
func (s *Service) pushRelayList(ctx context.Context, connStream ConnectorStream) error {
    relays, err := s.store.ListEligibleRelays(ctx) // Tier 1 + Tier 2 only
    // build LabelledRelayList proto, increment version, send on stream
}
```

`ListEligibleRelays` query:

```sql
SELECT id::text,
       COALESCE(public_addr,
         CASE WHEN address_scope = 'public' AND observed_ip IS NOT NULL
              THEN host(observed_ip) || ':9093'
         END, ''),
       COALESCE(spiffe_id, ''),
       capacity_label
  FROM relays
 WHERE status = 'active'
   AND capacity_label IN ('high', 'medium')
   AND last_heartbeat_at > NOW() - INTERVAL '90 seconds'
 ORDER BY last_heartbeat_at DESC
```

## Config

```go
// controller/internal/relay/config.go
RELAY_TIER1_ENTER           = 0.45
RELAY_TIER1_EXIT            = 0.50
RELAY_TIER2_ENTER           = 0.75
RELAY_TIER2_EXIT            = 0.80
RELAY_LABEL_HOLDDOWN_SECS   = 60
```

## Tests

- Label stays stable → no push
- fill_ratio crosses 0.46 → pending set; 59s later → no push; 61s later → promoted + pushed
- fill_ratio oscillates 0.44 → 0.51 → 0.44 within 60s → no push (dead-band + hold-down)
- Relay goes inactive → removed from next list version → push
- Control stream open → immediately receives current list

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/... ./internal/connector/...
```
