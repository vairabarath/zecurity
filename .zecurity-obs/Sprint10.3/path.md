---
type: planning
status: planned
sprint: 10.3
tags:
  - sprint10_3
  - relay
  - connector
  - hardening
  - operations
  - security
---

# Sprint 10.3 — Relay & Connector Production Hardening

## Sprint Goal

Close the security, lifecycle, resource-exhaustion, and operational gaps found
after the Relay and Connector runtime review.

The existing trust model remains unchanged:

```text
Client/Connector -- outer QUIC mTLS --> Relay
Client <---------- inner TLS 1.3 mTLS ----------> Connector
```

The Relay continues to enforce authenticated SPIFFE roles and same-workspace
Lookup while remaining unable to observe inner tunnel plaintext.

## Baseline

- Outer QUIC mTLS authentication works.
- SPIFFE role validation and workspace isolation work.
- Connector persistent Relay registration and `RelayHandler` are wired.
- Relay forwards only inner-TLS ciphertext after Lookup.
- Relay tests: 25 passed.
- Connector tests: 20 passed.
- Client Relay fallback remains planned separately in Sprint 10.2.

## Findings Addressed

| ID | Severity | Finding | Owning Phase |
|----|----------|---------|--------------|
| F1 | Critical | Relay Provision accepts an empty/ignored provisioning token | M2 Phase 1 |
| F2 | High | Relay and Connector lack handshake timeouts and concurrency limits | M3 Phase 1 |
| F3 | High | Connector Relay connections keep old certificate material after renewal | M3 Phase 2 |
| F4 | Medium | Relay certificate existence check, renewal, and writes are insufficient | M3 Phase 2 |
| F5 | Medium | Connector CA selection depends on bundle ordering | M3 Phase 2 |
| F6 | Medium | Closed Connector registrations can remain temporarily routable | M3 Phase 1 |
| F7 | Low | Relay SPIFFE parser accepts uppercase UUID text | M3 Phase 1 |
| F8 | Operational | Relay mTLS heartbeat is not implemented | M2 Phase 1 |

## Key Decisions

| Decision | Requirement |
|----------|-------------|
| Provision authentication | Require a short-lived, Relay-bound, single-use provisioning token |
| Token replay protection | Burn the stored JTI atomically before certificate issuance succeeds |
| Runtime limits | Bound connections, streams, pending handshakes, and idle time |
| Protocol failures | Return structured negative ACKs when possible |
| Certificate writes | Use restrictive permissions and atomic temp-file rename |
| Certificate lifecycle | Renew before expiry and reload Relay/Connector runtime material |
| CA selection | Validate certificate properties and chain relationships, never first/last position |
| Relay health | Send mTLS heartbeat and persist last-seen/status in Controller |

## Execution Path

### Phase A — M2: Authenticated Provisioning & Relay Heartbeat

> See [[Sprint10.3/Member2-Go/Phase1-Authenticated-Provisioning-Heartbeat]].

- [ ] **M2-A1** Require and verify a Relay-bound provisioning token.
- [ ] **M2-A2** Atomically burn the provisioning JTI and reject replay.
- [x] **M2-A3** Define and generate the Relay heartbeat protobuf contract.
- [x] **M2-A4** Authenticate heartbeat identity through mTLS and persist health.
- [ ] **M2-A5** Add provisioning, replay, identity, and heartbeat tests.

### Phase B — M3: Relay Runtime Resource & Routing Hardening

> Can proceed with Phase A.
> See [[Sprint10.3/Member3-Rust/Phase1-Relay-Runtime-Hardening]].

- [x] **M3-B1** Add QUIC/session/handshake idle timeouts.
- [x] **M3-B2** Add bounded connection, stream, and pending-handshake concurrency.
- [ ] **M3-B3** Evict closed Connector registrations during Lookup/open failure.
- [ ] **M3-B4** Return structured negative ACKs for routable protocol failures.
- [ ] **M3-B5** Enforce canonical lowercase UUIDs in Relay SPIFFE parsing.

### Phase C — M3: Certificate Lifecycle & Trust-Bundle Hardening

> Depends on the Phase A provisioning and heartbeat contracts.
> See [[Sprint10.3/Member3-Rust/Phase2-Certificate-Lifecycle-Trust-Bundles]].

- [ ] **M3-C1** Validate stored Relay certificate identity, key match, chain, and expiry.
- [ ] **M3-C2** Store Relay certificate material atomically with restrictive permissions.
- [ ] **M3-C3** Renew Relay certificates before expiry and reload runtime TLS material.
- [ ] **M3-C4** Reload Connector Relay client/handler material after Connector renewal.
- [ ] **M3-C5** Select Workspace and Intermediate CAs by validated certificate relationships.

### Phase D — M2/M3: Integration & Production Security Gates

> Depends on Phases A–C.
> See [[Sprint10.3/Shared/Phase1-Integration-Production-Gates]].

- [ ] **S-D1** Test provisioning token expiry, mismatch, and replay rejection.
- [ ] **S-D2** Test connection/stream exhaustion limits and handshake timeouts.
- [ ] **S-D3** Test stale registration eviction and negative ACK behavior.
- [ ] **S-D4** Test Relay and Connector certificate renewal without process restart.
- [ ] **S-D5** Test reordered/malformed CA bundles and complete real certificate chains.
- [ ] **S-D6** Test mTLS Relay heartbeat identity and persisted health transitions.

## Dependency Graph

```text
M2-A authenticated provisioning + heartbeat ──> M3-C lifecycle/trust hardening ──┐
                                                                                  │
M3-B runtime/resource hardening ──────────────────────────────────────────────────┤
                                                                                  ▼
                                                    Shared integration/security gates
```

## Final Build Gates

- [ ] `buf generate`
- [ ] `cd controller && go test ./internal/relay/... ./internal/pki/... && go build ./...`
- [ ] `cd relay && cargo test && cargo build`
- [ ] `cd connector && cargo test && cargo build`
- [ ] Run end-to-end authenticated provisioning, heartbeat, renewal, and limit tests.

## Acceptance Criteria

- [ ] Controller never signs a Relay CSR without a valid Relay-bound single-use token.
- [ ] Reusing, modifying, or using an expired provisioning token fails closed.
- [ ] Relay heartbeat uses mTLS identity and Controller persists Relay health.
- [ ] Slow or excessive connections/streams cannot grow tasks without bounds.
- [ ] Connector inner TLS handshakes time out and release capacity.
- [ ] Lookup never routes to a known-closed Connector registration.
- [ ] Relay and Connector certificate renewal takes effect without process restart.
- [ ] Partial certificate writes cannot replace valid runtime material.
- [ ] Reordered or malformed CA bundles fail safely or are selected by chain validation.
- [ ] Relay rejects noncanonical uppercase UUID identities.
- [ ] Existing workspace isolation and inner-TLS confidentiality tests continue to pass.

## Deferred

- Geographic Relay selection and load balancing.
- Hardware-backed Relay private keys.
- Relay-local OCSP distribution.
- Automated Platform Intermediate CA rotation.
- Client Relay fallback implementation, which remains in Sprint 10.2.
