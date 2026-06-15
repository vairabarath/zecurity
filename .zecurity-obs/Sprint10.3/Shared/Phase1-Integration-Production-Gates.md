---
type: phase
status: planned
sprint: 10.3
member: shared
phase: 1
depends_on:
  - Sprint10.3-M2-Phase1
  - Sprint10.3-M3-Phase1
  - Sprint10.3-M3-Phase2
unlocks: []
---

# Shared Phase 1 — Integration & Production Security Gates

## Goal

Prove the hardened Controller, Relay, and Connector behavior using real
certificate chains, failure injection, concurrency, and lifecycle tests.

## Integration Matrix

| Case | Expected Result |
|------|-----------------|
| Valid single-use provisioning token | Relay certificate issued once |
| Expired, mismatched, or replayed token | Provision rejected |
| Valid Relay mTLS heartbeat | Correct Relay health record updated |
| Wrong-role heartbeat certificate | Heartbeat rejected |
| Slow first message or inner TLS handshake | Deadline closes work and releases capacity |
| Excess connections or Lookup streams | Bounded rejection without task growth |
| Connector disconnect during Lookup | Stale registration evicted; Client receives negative ACK |
| Connector certificate renewal | Relay reconnect and inner TLS use new certificate without restart |
| Relay certificate renewal | New outer certificate served without restart |
| Reordered valid CA bundle | Correct CAs selected by validation |
| Unrelated or ambiguous CA bundle | Startup/reload fails closed |
| Uppercase UUID SPIFFE | Rejected |

## Security Regression Gates

- Relay still trusts only the Platform Intermediate CA for outer peers.
- Connector still verifies the exact Relay SPIFFE identity.
- Register remains bound to the authenticated Connector certificate.
- Cross-workspace Lookup remains denied.
- Inner Client-to-Connector TLS remains TLS 1.3 mTLS.
- Relay-observed bridge bytes contain no tunnel or resource plaintext.

## Load & Failure Validation

1. Exercise connection and stream limits at and above configured thresholds.
2. Hold incomplete handshakes until timeout and verify stable task/memory use.
3. Disconnect and replace Connector registrations during concurrent Lookups.
4. Interrupt certificate writes and renewal/reload operations.
5. Restart Controller while Relay continues serving existing data-plane
   connections, then verify heartbeat recovery.

## Final Build Check

```bash
buf generate
cd controller && go test ./internal/relay/... ./internal/pki/... && go build ./...
cd relay && cargo test && cargo build
cd connector && cargo test && cargo build
```

## Completion Rule

Sprint 10.3 is complete only when every finding in `Sprint10.3/path.md` has an
automated regression test and all security regression gates pass.

