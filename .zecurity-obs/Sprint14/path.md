---
type: planning
status: in-progress
sprint: 14
tags:
  - sprint14
  - dependencies
  - execution-path
  - security
  - pki
  - revocation
  - relay
  - pending-02
---

# Sprint 14 — Relay Certificate Revocation (PENDING-02, Track 1)

> **Read this before writing a single line of code.**
> Source of truth: `.zecurity-obs/pending/PENDING-02-Certificate-Revocation-Enforcement.md`.
> Branch: `feat/pending-02-cert-revocation` (copy of `fixed-pendings`).
> Scope: **Track 1 only** — relay *certificate* revocation. Relay *peer* revocation (relay rejecting
> revoked connectors/clients) is **deferred to a separate ADR**.

## Sprint Goal

Close the highest-severity revocation gap: **today nothing checks whether a *relay* certificate has
been revoked** — the controller, connectors, and clients all trust a relay's cert for its full 30-day
life (`connector/spiffe.go:226`, `connector/src/relay_client.rs:310`, `client/src/relay_pool.rs:61`).

Make a revoked relay certificate rejected by **every party that authenticates a relay**:
- **Controller** (relay Provision + Heartbeat) — enforces from its **database** (source of truth).
- **Connector** and **Client** (dial the relay) — enforce via a **signed Intermediate-CA CRL** pulled
  from the controller and verified by a **hardened** CRL manager.

Revocation is a **provider** action, done **atomically**: revoke every unexpired relay serial → drop
the relay from `LabelledRelayList` → broadcast → audit.

```text
BEFORE                                          AFTER
------                                          -----
relay cert: verified chain + SPIFFE only,       relay cert: also checked for revocation at
  no revocation check anywhere → a stolen         controller (DB) + connector + client (signed
  relay key is trusted for 30 days                /relay.crl); revoked → handshake/heartbeat rejected
```

## Key Design Decisions

| Decision                    | Detail                                                                                                                                                                                                                               |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Scope = Track 1 only        | Relay *certificate* revocation. Relay-checks-its-*peers* (workspace CRLs at the relay) is a **separate future ADR**.                                                                                                                 |
| Two-CA reality              | Relay certs are signed by the **Intermediate CA** → **one platform CRL** (`/relay.crl`). Workspace CRLs (`/ca.crl`) are **untouched** this sprint.                                                                                   |
| Controller = DB check       | The controller is the CRL's source of truth; it enforces via a DB-backed in-memory checker, **not** by parsing its own CRL.                                                                                                          |
| Off-controller = signed CRL | Connector + client pull `/relay.crl` (Intermediate-CA-signed) and verify it.                                                                                                                                                         |
| Renewal-safe model          | New `relay_certificates` history table; revoke marks **every unexpired serial** for the relay (relays don't renew today, but this future-proofs it). `relays.cert_serial` stays as the "current" pointer.                            |
| Hardened CRL manager        | Verify **signature + issuer + thisUpdate + nextUpdate**; **deny once past `nextUpdate`**; keep-last-good on transient fetch failure; **cold-boot fail-closed**. Fixes the pre-existing unverified-CRL bug in `connector/src/crl.rs`. |
| Refresh interval            | **60s + jitter** (down from today's 300s).                                                                                                                                                                                           |
| Revoke API                  | `POST /provider/relays/{id}/revoke` (revoke without delete); `DELETE /provider/relays/{id}` = **revoke-then-remove**. **Never physically delete** the relay row or its `relay_certificates` rows — the CRL needs them until expiry.  |
| Atomic revoke               | revoke serials → mark relay non-active → `provider_audit_logs` (`relay.revoke`) → `broadcastRelayList`, in one flow.                                                                                                                 |
| No proto changes            | Controller enforces at the existing SPIFFE interceptor; verifiers pull a new HTTP endpoint. Cleanly additive.                                                                                                                        |

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Go (Controller) | `relay_certificates` migration + store, provider revoke transaction, `GenerateRelayCRL` + `/relay.crl`, controller revocation enforcement in `verifyRelayCertificate` |
| **M2** | Rust (Data plane) | Hardened CRL manager (verify sig/issuer/dates), connector + client consumption of `/relay.crl` |

## Critical Rule: Conflict Zones

| File | Who | Rule |
|------|-----|------|
| `controller/cmd/server/main.go` | **M1 only** | routes (`/relay.crl`, `POST …/revoke`) + revocation-checker wiring |
| `controller/internal/relay/store.go` | **M1 only** | history-table + revoke methods |
| `controller/internal/relay/admin_handler.go` | **M1 only** | revoke handler + DELETE change |
| `controller/internal/pki/*` | **M1 only** | `GenerateRelayCRL` |
| `controller/internal/connector/spiffe.go` | **M1 only** | `verifyRelayCertificate` revocation check |
| `controller/migrations/027_relay_certificates.sql` | **M1 only** | next free number is 027 |
| `connector/src/crl.rs`, `connector/src/relay_client.rs`, `client/src/relay_pool.rs` | **M2 only** | hardened manager + consumption |

No Go/Rust file overlaps → M1 and M2 do not touch the same files.

## Dependency Graph

```text
M1-A Relay cert-history + revoke data model (Day 1, independent)
   ↓
   ├── M1-B Provider revoke transaction (POST /revoke, DELETE, audit, broadcast)
   ├── M1-C Relay CRL generation + /relay.crl endpoint
   └── M1-D Controller revocation enforcement (verifyRelayCertificate, DB/cache)

M2-A Hardened CRL manager (Day 1, independent)
   ↓
M2-B Connector + client consume /relay.crl        (needs M1-C endpoint + M2-A manager)
```

> Day-1 parallel starts: **M1-A** (data model) and **M2-A** (hardened CRL manager) have no dependencies.

## Execution Path

### Phase A — M1: Relay Cert-History + Revocation Data Model
> See [[Sprint14/Member1-Go/Phase1-Relay-Cert-History-Data-Model]]. Depends on nothing — Day 1.
- [x] **M1-A1** `controller/migrations/027_relay_certificates.sql` — `relay_certificates` history table.
- [x] **M1-A2** `internal/relay/store.go` — `RecordIssuedCert`, `RevokeAllForRelay(id, reason)`, `ListRevokedRelaySerials()` (unexpired only).
- [x] **M1-A3** `store.go` — `MarkProvisioned` also records the issued cert (same transaction); `BuildLabelledRelayList` excludes revoked relays.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase B — M1: Provider Revoke Transaction
> See [[Sprint14/Member1-Go/Phase2-Provider-Revoke-Transaction]]. Depends on Phase A.
- [x] **M1-B1** `internal/provider/authz.go` — `ActionRelayRevoke="relay.revoke"` + `CanRevokeRelay`.
- [x] **M1-B2** `internal/relay/admin_handler.go` — `Revoke` handler: certificate revocation + non-active status + `InsertAuditTx("relay.revoke")` commit atomically; convergence fan-out runs after commit.
- [x] **M1-B3** `admin_handler.go` — `Delete` = atomic revoke-then-remove + audit; **row preserved**, never hard-delete.
- [x] **M1-B4** `cmd/server/main.go` — route `POST /provider/relays/{id}/revoke` behind `requireProvider`; inject broadcaster.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase C — M1: Relay CRL Generation + Endpoint
> See [[Sprint14/Member1-Go/Phase3-Relay-CRL-Generation-Endpoint]]. Depends on Phase A.
- [x] **M1-C1** `internal/pki/service.go` — add `GenerateRelayCRL` to the interface.
- [x] **M1-C2** new `internal/pki/relay_crl.go` — Intermediate-CA-signed CRL from `ListRevokedRelaySerials()`; `nextUpdate` is 10 minutes.
- [x] **M1-C3** `internal/relay/crl_handler.go` + `main.go` — `RelayCRLHandler` + `GET /relay.crl`.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase D — M1: Controller Revocation Enforcement
> See [[Sprint14/Member1-Go/Phase4-Controller-Revocation-Enforcement]]. Depends on Phase A (+ B for the refresh-on-revoke hook).
- [x] **M1-D1** `internal/connector/spiffe.go` — a DB-backed `RevocationChecker` (in-memory cache, refreshed on revoke + periodically); consult it for authenticated relay RPCs; **fail closed** if state unavailable.
- [x] **M1-D2** `cmd/server/main.go` — construct + wire the checker; refresh it **after commit** through the revoke convergence hook.
- [x] **Build gate:** `cd controller && go build ./...`

### Phase E — M2: Hardened CRL Manager
> See [[Sprint14/Member2-Rust/Phase1-Hardened-CRL-Manager]]. Depends on nothing — Day 1.
- [x] **M2-E1** hardened CRL manager: verify **signature + issuer + thisUpdate + nextUpdate**; **deny past nextUpdate**; keep-last-good on transient failure; cold-boot fail-closed.
- [x] **M2-E2** refactor `connector/src/crl.rs` onto it (fixes the existing unverified-CRL bug); 60s + jitter refresh.
- [x] **Build gate:** `cd connector && cargo build`

### Phase F — M2: Connector + Client Consume /relay.crl
> See [[Sprint14/Member2-Rust/Phase2-Connector-Client-Relay-CRL]]. Depends on Phase C (endpoint) + Phase E (manager).
- [x] **M2-F1** `connector/src/relay_client.rs` — reject a revoked relay cert (relay-CRL check via the hardened manager) when dialing the relay.
- [x] **M2-F2** `client/src/relay_pool.rs` — same on the client side, including live cached-connection eviction.
- [x] **Build gate:** `cd connector && cargo build && cd ../client && cargo build`
- [x] **M2-F3 E2E gate:** Controller revoke + stale `LabelledRelayList` connector/client rejection.

## Final Build Gates
```bash
cd controller && go build ./... && go test ./internal/...
cd connector && cargo build
cd client && cargo build
cd relay && cargo build
```

## Acceptance Criteria
- [x] `relay_certificates` history table exists; revoke marks **every unexpired serial** for the relay.
- [x] `POST /provider/relays/{id}/revoke` (provider-auth) revokes + removes-from-pool + audits (`relay.revoke`) **atomically**, then broadcasts after commit; relay row **preserved**.
- [x] `DELETE /provider/relays/{id}` = revoke-then-remove; **no path physically deletes** a relay or its cert-history rows. *(Atomicity with audit remains covered by the unchecked criterion below.)*
- [x] `GenerateRelayCRL` emits an **Intermediate-CA-signed** CRL of exactly the revoked unexpired relay serials; verifies against the Intermediate CA; `GET /relay.crl` returns parseable `application/pkix-crl`.
- [x] Hardened CRL manager verifies **signature + issuer + thisUpdate + nextUpdate**, **denies once past nextUpdate**, keeps last-good on transient failure, and is **fail-closed on cold-boot**.
- [x] Controller rejects a revoked relay at **Heartbeat + authenticated relay RPCs** via a cache (no per-RPC DB query); **fails closed** when state is unavailable. (Provision is skipped by the SPIFFE interceptor, so it is covered by the DB status guard instead.)
- [x] Re-provisioning of a revoked relay is denied by a **Provision-time DB status guard** (`MarkProvisioned` guarded; revoke serializes against Provision) — a revoke cannot be silently undone.
- [x] Connector **and** client reject a revoked relay cert — including from a cached `LabelledRelayList`.
- [x] Revocation **evicts already-established** relay connections (connector session + client `RelayPool` cache), not just future handshakes.
- [x] A revoked relay is removed from `LabelledRelayList` and a broadcast fires immediately.
- [x] The revoke transaction is **atomic** (revocation rows + relay status + audit in one DB tx); cache refresh + broadcast happen **after commit**.
- [x] Relay serials use one **canonical normalized form** (`SerialNumber.Text(16)`) across DB, CRL, and checker; invalid serials rejected.
- [x] Refresh interval is **60s + jitter**.

## Deferred (out of scope this sprint)
- **Relay peer revocation** (relay rejecting revoked connectors/clients via workspace CRLs) → **separate ADR**.
- Relay cert renewal / short-TTL rotation.
- OCSP; any platform-wide revocation manifest.
- Promoting `PENDING-02` → `ADR-0NN` (revisit after the sprint lands).

## Notes for AI Agents
1. Read this `path.md`, then your first unchecked phase whose `depends_on` are satisfied.
2. **No proto changes** are expected this sprint.
3. Respect the M1(Go)/M2(Rust) split — the file sets do not overlap.
4. Every revocation path must **fail closed**; never un-revoke on a transient fetch error.
