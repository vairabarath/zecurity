---
type: phase
member: M2
sprint: 11
phase: 1
title: Proto Changes
depends_on: []
---

# Phase 1 — Proto Changes

## Goal

Add all protobuf messages required by ADR-016 so every component can compile against the new contract before implementation begins.

## Files

| File | Change |
|---|---|
| `proto/relay/v1/relay.proto` | Add `connection_count`, `max_connections` to relay heartbeat; add `ProbeRequest`, `ProbeResponse` |
| `proto/connector/v1/connector.proto` | Add `RelayCapacityLabel`, `LabelledRelayInfo`, `LabelledRelayList`; add field 17 to `ConnectorControlMessage` |

## Relay Proto

```protobuf
// In RelayHeartbeat message — add after existing fields:
uint32 connection_count = N;    // active bridged client relay streams
uint32 max_connections  = N+1;  // RELAY_MAX_CONNECTIONS ceiling

// New probe messages:
message ProbeRequest {
  string connector_id = 1;
  uint64 request_id   = 2; // random nonce; relay must echo it
}
message ProbeResponse {
  uint32 connection_count = 1;
  uint32 capacity         = 2;
  uint64 request_id       = 3; // must match ProbeRequest.request_id
}
```

Find current highest field number in `RelayHeartbeat` before assigning N.

## Connector Control Stream Proto

```protobuf
enum RelayCapacityLabel {
  RELAY_CAPACITY_HIGH   = 0;
  RELAY_CAPACITY_MEDIUM = 1;
}

message LabelledRelayInfo {
  string             relay_id   = 1;
  string             relay_addr = 2; // host:9093
  string             spiffe_id  = 3;
  RelayCapacityLabel label      = 4;
}

message LabelledRelayList {
  repeated LabelledRelayInfo relays  = 1;
  uint64                     version = 2; // monotonic; connector skips re-probe if unchanged
}

// In ConnectorControlMessage oneof body — field 16 is reserved for TransportSnapshot:
LabelledRelayList relay_list = 17;
```

## Build Check

```bash
buf generate
cd controller && go build ./...
```

Confirm Rust prost stubs also regenerate cleanly for relay and connector crates.

## Implementation Checklist

- [x] **M2-A1** `proto/relay/v1/relay.proto` — add `connection_count = 6` and `max_connections = 7` to `HeartbeatRequest`
- [x] **M2-A2** `proto/relay/v1/relay.proto` — add `ProbeRequest` (with `request_id`) and `ProbeResponse` (echoing `request_id`) messages
- [x] **M2-A3** `proto/connector/v1/connector.proto` — add `RelayCapacityLabel` enum, `LabelledRelayInfo`, `LabelledRelayList` messages
- [x] **M2-A4** `proto/connector/v1/connector.proto` — add `relay_list = 17` to `ConnectorControlMessage` oneof body; field 16 reserved for TransportSnapshot
- [x] **M2-A5** `buf generate` — Go stubs regenerated; Rust prost stubs regenerated
- [x] **Build gate:** `cd controller && go build ./...` passes
