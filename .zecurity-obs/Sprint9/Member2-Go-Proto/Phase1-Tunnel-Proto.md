---
type: phase
status: pending
sprint: 9
member: M2
phase: Phase1-Tunnel-Proto
depends_on: []
tags:
  - proto
  - day1
  - rde
  - tunnel
---

# M2 Phase 1 — Tunnel Proto (Day 1)

> **This is Day 1 work. M3 and M4 are blocked until you commit.**

---

## What You're Building

Activate the four shield.proto fields that were reserved in Sprint 6. These messages carry the RDE tunnel session lifecycle between Connector and Shield over the existing Control stream.

---

## Files to Touch

### `proto/shield/v1/shield.proto`

Add four new messages after the existing `DiscoveryReport` message:

```proto
message TunnelOpen {
  string connection_id = 1;  // UUID identifying this tunnel session
  string destination   = 2;  // target IP or hostname
  uint32 port          = 3;
  string protocol      = 4;  // "tcp" or "udp"
}

message TunnelOpened {
  string connection_id = 1;
  bool   ok            = 2;
  string error         = 3;  // set only on failure
}

message TunnelData {
  string connection_id = 1;
  bytes  data          = 2;  // raw TCP bytes (max 16 KB chunks)
}

message TunnelClose {
  string connection_id = 1;
  string error         = 2;  // empty = clean close
}
```

Add to `ShieldControlMessage.oneof body` after `discovery_report = 7`:

```proto
// Connector → Shield
TunnelOpen   tunnel_open   = 8;
// Shield → Connector
TunnelOpened tunnel_opened = 9;
// Bidirectional
TunnelData   tunnel_data   = 10;
TunnelClose  tunnel_close  = 11;
```

> **Rule:** fields 8–11 are the last reserved slots. Never reuse 1–11.

---

## After Your Commit

Notify the team. They run:

```bash
buf generate                          # from repo root
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

## Build Check

```bash
buf generate    # must be clean
cd controller && go build ./...
```
