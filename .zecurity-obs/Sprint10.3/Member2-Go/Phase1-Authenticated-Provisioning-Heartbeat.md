---
type: phase
status: in-progress
sprint: 10.3
member: M2
phase: 1
depends_on: []
unlocks:
  - Sprint10.3-M3-Phase2
  - Sprint10.3-Shared-Phase1
---

# M2 Phase 1 — Authenticated Relay Provisioning & Heartbeat

## Goal

Remove unauthenticated Relay certificate issuance and add an authenticated,
persistent Relay health signal.

## Current Risks

- `relay/src/provision.rs` sends an empty `provisioning_token`.
- `controller/internal/relay/provision.go` ignores the token and signs a
  validated Relay CSR for any caller that can reach the gRPC endpoint.
- Provisioning-token issue, verification, storage, and burn helpers already
  exist under `controller/internal/relay/` but are not wired into Provision.
- `relay.v1.RelayService.Heartbeat` is commented out.

## Required Provisioning Flow

```text
Operator/Admin
    │ issues short-lived Relay-bound single-use token
    ▼
Relay ProvisionRequest(token, relay_id, CSR)
    │
    ▼
Controller verifies signature, expiry, purpose, and relay_id
    │
    ▼
Controller atomically burns JTI
    │
    ▼
Controller validates/signs CSR and returns Relay certificate
```

## Requirements

1. Require a non-empty provisioning token in `Provision`.
2. Verify token signature, expiration, purpose, and canonical Relay ID binding.
3. Atomically burn the stored JTI and reject missing or already-used JTIs.
4. Do not permit token reuse after a failed signing attempt unless a new token
   is explicitly issued.
5. Map authentication failures to appropriate gRPC status codes without
   exposing token internals.
6. Add Relay configuration for receiving the operator-issued token without
   logging or persisting it unnecessarily.
7. Define `HeartbeatRequest` and `HeartbeatResponse` in
   `proto/relay/v1/relay.proto`.
8. Require mTLS for Heartbeat and derive Relay ID from the authenticated Relay
   SPIFFE certificate, not from an untrusted request field.
9. Persist last-seen time, version, hostname, status, and certificate metadata.
10. Add health transitions for healthy, stale, and offline Relay states.

## Required Tests

- Valid token signs the expected Relay CSR once.
- Empty, malformed, expired, wrong-purpose, and wrong-Relay tokens fail.
- Replayed token fails.
- Concurrent replay attempts allow only one successful burn.
- Heartbeat rejects missing, wrong-role, or mismatched Relay identity.
- Valid heartbeat updates the correct Relay health record.

## Build Check

```bash
buf generate
cd controller
go test ./internal/relay/... ./internal/pki/...
go build ./...
```

## Implementation Progress

### Completed: mTLS Relay Heartbeat

- Defined and generated `relay.v1.RelayService.Heartbeat`.
- Relay starts a periodic Controller heartbeat task after provisioning and
  reports version, hostname, uptime, and registered Connector count.
- Relay heartbeat uses `relay.crt` and `relay.key` as its mTLS client identity
  and trusts the Platform Intermediate CA.
- Controller verifies Relay certificates against the Platform Intermediate CA
  before injecting the Relay SPIFFE identity.
- Controller derives Relay ID from the authenticated SPIFFE identity, verifies
  it matches the presented leaf, and never trusts a request Relay ID.
- Controller records active status, `last_heartbeat_at`, version, hostname,
  certificate serial, and certificate expiry.
- Controller records every authenticated Relay heartbeat into Valkey as a
  short-lived liveness key.
- Controller writes Relay health/address metadata to Postgres only when
  heartbeat metadata changes or the DB write marker is older than
  `RELAY_HEARTBEAT_DB_WRITE_INTERVAL`.
- Controller records the authenticated heartbeat peer address into Relay DB
  metadata when a DB write is due: `observed_ip`, `observed_port`, and
  `address_scope`.
- Controller sets `public_addr` only when the observed heartbeat peer IP is
  public/global; private, loopback, link-local, or unknown addresses are
  recorded for operations but are not treated as client-discovery addresses.
- Relay heartbeat reconnects independently after Controller/network failure;
  the Relay QUIC listener continues running.

### Patch: Relay Heartbeat Address Observation

**Issue:** Relay heartbeat proved the Relay identity and health, but the
Controller did not persist the address it observed for the authenticated Relay
connection.

**Fix Applied:**
- Added Relay DB metadata columns via
  `controller/migrations/020_relay_update_table.sql`:
  `public_addr`, `observed_ip`, `observed_port`, and `address_scope`.
- Added heartbeat peer-address extraction in
  `controller/internal/relay/heartbeat.go`.
- Added address classification:
  `public`, `private`, `loopback`, `link_local`, or `unknown`.
- Added Valkey heartbeat liveness keys in
  `controller/internal/relay/heartbeat.go`; Postgres writes are throttled by
  `RELAY_HEARTBEAT_DB_WRITE_INTERVAL`.
- Updated `controller/internal/relay/store.go` so `RecordHeartbeat` persists
  address metadata with the existing Relay health record when a DB write is
  due.
- Public heartbeat peer IPs derive `public_addr` as `<observed_ip>:9093`.
  Private/LAN addresses are intentionally not promoted to `public_addr`.
- Added focused heartbeat tests for persistence and public-address
  classification.

**Verification:**

```bash
cd controller
go test ./internal/relay/...
go build ./...
```

The Relay certificate EKU contract is now:

```text
ServerAuth + ClientAuth
```

Existing Relay certificates issued before this change contain only
`ServerAuth` and must be reprovisioned before heartbeat mTLS can succeed.

Remaining in this phase:

- Enforce authenticated single-use provisioning tokens.
- Add stale/offline health transitions and their tests.
- Add a complete live Controller-to-Relay heartbeat integration test.
