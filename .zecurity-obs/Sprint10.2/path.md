---
type: planning
status: planned
sprint: 10.2
tags:
  - sprint10_2
  - relay
  - client
  - fallback
  - inner-mtls
---

# Sprint 10.2 — Client Relay Fallback & End-to-End Completion

## Sprint Goal

Complete the Client side of the Relay dataplane without weakening the existing
direct Client-to-Connector QUIC path.

```text
Preferred:
Client -- direct QUIC mTLS --> Connector

Fallback after direct connection failure or 2-second timeout:
Client -- outer QUIC mTLS --> Relay -- outer QUIC mTLS --> Connector
       <----------- inner Client-to-Connector TLS 1.3 mTLS ----------->
```

The Relay may observe Lookup metadata, but must never observe `TunnelRequest`,
`TunnelResponse`, or resource payload plaintext.

## Current Prerequisites

- Relay listener, registration state, Lookup bridging, and workspace isolation
  exist.
- Connector persistent Relay registration and `RelayHandler` inner-mTLS server
  exist.
- Controller-issued Client SPIFFE role is authoritative:
  `spiffe://<workspace>/client/<device-uuid>`.
- Client direct QUIC tunnel path exists and must remain operational.

## Key Decisions

| Decision | Requirement |
|----------|-------------|
| Discovery source | Controller ACL snapshot provides all Relay and Connector identity fields |
| Required ACL fields | `relay_addr`, `relay_spiffe_id`, `connector_id`, `connector_spiffe` |
| Outer Relay authentication | Client presents `leaf + Workspace CA`, trusts only Platform Intermediate, verifies exact Relay SPIFFE |
| Inner Connector authentication | Client trusts its Workspace CA and verifies exact `connector_spiffe` |
| Inner protocol | TLS 1.3 mTLS over the Relay-bridged QUIC stream |
| Fallback trigger | Direct stream connection error or 2-second timeout only |
| No fallback after denial | Connector ACL denial, revoked certificate, or invalid identity is final |
| Stream abstraction | Direct QUIC and Relay inner-TLS paths return one common authenticated bidirectional stream |
| Pooling | Reuse one healthy outer QUIC connection per Relay |

## Execution Path

### Phase A — M2: ACL Relay Discovery Contract

> See [[Sprint10.2/Member2-Go/Phase1-ACL-Relay-Discovery]].

- [x] **M2-A1** Add ACL snapshot fields 6–9 without changing existing numbers.
- [x] **M2-A2** Populate active Connector ID and exact Connector SPIFFE.
- [x] **M2-A3** Relay address resolved from DB (`connector_relay_placement` JOIN `relays`) — not from env var (superseded by ADR-014 Gap 1; env var approach dropped).
- [x] **M2-A4** Per-connector relay coords populated on each `ACLConnector` in compiler loop; client reads from `connector.relay_addr` / `connector.relay_spiffe_id` (ADR-014 Gap 4).
- [x] **M2-A5** Regenerate protobuf stubs and pass Controller tests/build.

### Phase B — M3: Client RelayPool & Inner mTLS

> Depends on Phase A. See [[Sprint10.2/Member3-Rust/Phase1-Client-RelayPool-Inner-mTLS]].

- [x] **M3-B1** Add `client/src/relay_pool.rs`.
- [x] **M3-B2** Implement pooled outer Relay QUIC mTLS with exact Relay identity.
- [x] **M3-B3** Send framed `Lookup { connector_id }` and validate framed ACK.
- [x] **M3-B4** Establish inner TLS 1.3 mTLS and verify exact Connector SPIFFE.
- [x] **M3-B5** Return a common authenticated stream ready for `TunnelRequest`.

### Phase C — M3: Direct-First Fallback Wiring

> Depends on Phase B. See [[Sprint10.2/Member3-Rust/Phase2-Direct-First-Fallback]].

- [x] **M3-C1** Preserve direct `TunnelPool` behavior behind the common stream API.
- [x] **M3-C2** Apply a 2-second timeout to direct stream establishment.
- [x] **M3-C3** Fall back to Relay only for direct connection failure/timeout.
- [x] **M3-C4** Thread optional `RelayPool` through daemon and net stack.
- [x] **M3-C5** Keep `RELAY_ADDR` empty behavior direct-only.

### Phase D — M3: Security & End-to-End Validation

> Depends on Phases A–C. See [[Sprint10.2/Member3-Rust/Phase3-Integration-Security]].

- [ ] **M3-D1** Test exact Relay and Connector SPIFFE rejection.
- [ ] **M3-D2** Test wrong-workspace and wrong-role rejection.
- [ ] **M3-D3** Prove Relay-observed bridged bytes contain no plaintext markers.
- [ ] **M3-D4** Test direct-first selection and Relay fallback.
- [ ] **M3-D5** Test reconnect and pooled concurrent Lookup streams.

## Dependency Graph

```text
Completed Relay + Connector runtime
              │
              ▼
   M2-A ACL discovery contract
              │
              ▼
 M3-B RelayPool + inner TLS client
              │
              ▼
   M3-C direct-first fallback
              │
              ▼
 M3-D integration/security gates
```

## Final Build Gates

- [x] `buf generate`
- [ ] `cd controller && go test ./internal/policy/... ./internal/client/... ./internal/connector/...`
- [x] `cd controller && go build ./...`
- [ ] `cd client && cargo test && cargo build`
- [ ] `cd connector && cargo test && cargo build`
- [ ] `cd relay && cargo test && cargo build`

## Acceptance Criteria

- [ ] Same-network Client uses direct Connector QUIC path.
- [ ] Unreachable direct Connector falls back through Relay after at most two seconds.
- [ ] Client verifies exact Relay SPIFFE before sending Lookup.
- [ ] Client verifies exact Connector SPIFFE inside the bridged stream.
- [ ] Relay sees no plaintext tunnel request or resource payload.
- [ ] Cross-workspace Lookup and inner mTLS fail closed.
- [ ] ACL denial never triggers a Relay retry.
- [ ] Missing Relay discovery fields leave Client in direct-only mode.

## Deferred

- Multiple Relay selection and geographic routing.
- Relay load balancing and health scoring.
- Online certificate hot reload.
- UDP Relay fallback validation.

