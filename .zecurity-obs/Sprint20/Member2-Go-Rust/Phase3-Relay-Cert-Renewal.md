---
type: phase
member: M2
person: Barath
sprint: 20
phase: 3
execution: F
title: Relay In-Band Certificate Renewal (D-19)
status: in-progress   # F-1 (controller) done; F-2 (relay Rust runtime) next
depends_on: [2]
tags:
  - go
  - rust
  - relay
  - pki
  - proto
  - renewal
  - provider-dashboard
---

# Phase 3 (F) — Relay In-Band Certificate Renewal (D-19)

> Depends on Phase 2 (C): renewal applies the same stored-allowlist SAN rule.
> Implements **D-19** exactly: in-band `RelayService.RenewCert` over the existing mTLS channel, same
> key, CSR proof of possession, scheduled by the relay before expiry, recorded in `relay_certificates`.
> **D-20** applies: an already-expired or lost-key relay is replaced, not recovered.

## Problem (verified)

- `proto/relay/v1/relay.proto` has only `Provision` and `Heartbeat`.
- The relay loads its cert once (`relay/src/main.rs:43-54`) and skips provisioning whenever cert files exist (`provision.rs:26-57`).
- `RELAY_CERT_TTL` defaults to 30 d (`main.go:212`). After that, heartbeats and every peer handshake fail.
- `Store.RecordIssuedCert` (`store.go:155`) exists with no callers.
- `relay.Service` embeds `relaypb.UnimplementedRelayServiceServer` (`provision.go:21`), so adding the RPC is build-safe before the handler exists.

## Goal

A relay renews its certificate automatically, well before expiry, with the **same private key**.
The new serial is tracked in `relay_certificates`, and revoking the relay revokes every serial it
ever held, including one issued concurrently with the revoke.

## Files

| File | Change |
|------|--------|
| `proto/relay/v1/relay.proto` | **Additive**: `rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);` + two new messages |
| `controller/internal/relay/renew.go` (new) | `Service.RenewCert` handler |
| `controller/internal/relay/heartbeat.go` | Factor the relay-identity checks (role, trust domain, canonical ID, single URI SAN) into a shared helper used by `Heartbeat` and `RenewCert` |
| `controller/internal/relay/store.go` | `RecordRenewedCert` (transactional); reuse/retire `RecordIssuedCert` |
| `controller/internal/relay/renew_test.go` (new), `store_renew_integration_test.go` (new) | Tests |
| `relay/src/renewal.rs` (new) | Scheduler + RPC call + atomic file write |
| `relay/src/csr.rs` | Build a CSR from the **existing** key (today `generate_relay_csr` generates a new key) |
| `relay/src/tls.rs`, `relay/src/listener.rs` | Swappable server certificate via a rustls `ResolvesServerCert` holding an `ArcSwap<CertifiedKey>` (or equivalent) |
| `relay/src/heartbeat.rs` | Take the identity from the shared holder so the next (re)connect uses the renewed cert; reconnect promptly after a swap |
| `relay/src/main.rs` | Construct the shared holder; spawn the renewal task |

## Proto (additive only)

```proto
service RelayService {
  rpc Provision(ProvisionRequest) returns (ProvisionResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
  // Renews the Relay's own certificate over mTLS. Same key pair (CSR proof of
  // possession); SANs must stay within the relay's registered allowlist.
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);
}

message RenewCertRequest {
  bytes csr_der = 1;            // PKCS#10 CSR signed by the relay's CURRENT private key
}

message RenewCertResponse {
  bytes certificate_pem      = 1;
  bytes intermediate_ca_pem  = 2;
  int64 cert_not_after_unix  = 3;
  int64 cert_not_before_unix = 4;
}
```

New messages, new field numbers. No existing field is touched. Run `buf generate` from the repo root.

## Controller handler (`Service.RenewCert`)

The call is **not** added to the interceptor skip list: `UnarySPIFFEInterceptor` already verifies
the relay chain to the Intermediate and consults `RelayRevocationChecker`.

1. Relay identity (shared helper with `Heartbeat`): role `relay`, trust domain `zecurity.in`, canonical UUID, leaf with exactly one URI SAN = `RelaySPIFFEID(id)`. On failure → `Unauthenticated` / `PermissionDenied`.
2. Load the row (`LoadRelayByID`). `status` must be `active` or `inactive`. `pending`, `revoked` or `deleted` → `FailedPrecondition`.
3. **Presented serial must be known and unrevoked** in `relay_certificates` (defense in depth over the 60 s revocation-checker refresh). Otherwise → `PermissionDenied`.
   *Edge case:* a relay provisioned before migration 027 has no `relay_certificates` row and is refused here. Its cert is ≤ 30 d old by construction, so it reaches expiry and is replaced (D-20). Check the dev/staging DBs for such rows before shipping, and record the finding in Post-Phase Fixes.
4. Parse CSR, `CheckSignature()`. The CSR public key must **equal** the presented leaf's public key (D-19: same key). Otherwise → `PermissionDenied`.
5. SANs: CSR DNS/IP SANs ⊆ stored `dns_allowlist` / `ip_allowlist` (Phase C rule); URI SAN checked by `SignRelayCert`.
6. `pki.SignRelayCert(ctx, relayID, csr, storedDNS, storedIPs, s.certTTL)` (unchanged function).
7. `store.RecordRenewedCert(...)`; see below. On `ErrRelayRevoked` / not found → `PermissionDenied` and **do not return the cert**.
8. Respond with cert PEM, Intermediate PEM, not_before / not_after.
9. Log `relay cert renewed relay=<id> old_serial=<> new_serial=<> not_after=<>`.

### `RecordRenewedCert` (one transaction; closes the renew-vs-revoke race)

```sql
BEGIN;
SELECT status FROM relays WHERE id = $1 FOR UPDATE;            -- same lock RevokeRelay takes
-- abort with ErrRelayRevoked unless status IN ('active','inactive')
INSERT INTO relay_certificates (relay_id, serial, not_after) VALUES ($1, $2, $3);
UPDATE relays SET cert_serial = $2, cert_not_after = $3, updated_at = NOW() WHERE id = $1;
COMMIT;
```

`RevokeRelay` (`store.go:231-281`) locks the same row `FOR UPDATE`, then revokes every unrevoked
`relay_certificates` row. With both sides locking the row, a renewal committed first is revoked by
the later revoke, and a renewal attempted after revoke aborts.

## Relay side (Rust)

### Scheduler (`renewal.rs`)

- Renew when remaining lifetime ≤ **2/5** of total lifetime (same ratio as the client's `RENEWAL_WINDOW_SECS`, `client/src/daemon.rs:1629`), plus a one-time random jitter. For 30 d certs this is ~12 d before expiry.
- On failure: retry with capped backoff (e.g. 1 min → 30 min). Log at `warn`, and at `error` once < 24 h remain.
- If the cert is **already expired**: don't call `RenewCert`. Log an `error` that the relay must be replaced (D-20) and keep running (the listener will fail handshakes, which is the existing behaviour).
- Steps: build CSR from the existing key (same SPIFFE URI + configured `RELAY_DNS_SANS`/`RELAY_IP_SANS`) → `RenewCert` over the current mTLS identity → verify the response (leaf parses; URI SAN = own SPIFFE ID; public key equals own key; Intermediate fingerprint equals the pinned one) → **atomically** write `relay.crt` (write temp file, fsync, rename) → swap into the holder.

### Hot swap

- **QUIC listener:** `build_server_config` uses a resolver backed by the shared holder instead of a fixed `with_single_cert`. New handshakes get the new cert; existing connections continue.
- **Heartbeat:** `run()` / `run_connected()` read the identity from the holder on each (re)connect. After a swap, end the current channel so the next connect uses the new identity. The existing reconnect delay is fine.
- The key file is never rewritten.

## Invariants

- **I1** Same key: the renewed cert's public key equals the previous cert's public key.
- **I2** Every issued relay serial is recorded in `relay_certificates`; `relays.cert_serial` mirrors the newest.
- **I3** Revoking a relay revokes all of its serials, including one issued concurrently (row lock on both sides).
- **I4** A revoked, deleted or pending relay can never renew.
- **I5** Renewed SANs ⊆ stored allowlist.
- **I6** The old cert is **not** revoked on renewal; it expires naturally, so in-flight sessions and make-before-break are unaffected.
- **I7** No token re-issue path is added (D-20). No new CA (D-22).
- **I8** Proto change is additive; no field renumbering.

## Tests

Go (`renew_test.go`, fakes):
- Happy path: active relay, same-key CSR → cert returned; `RecordRenewedCert` called with the new serial.
- CSR signed by a different key → `PermissionDenied`.
- CSR SAN outside stored allowlist → rejected.
- Row `revoked` / `deleted` / `pending` → `FailedPrecondition`.
- Presented serial unknown or revoked in `relay_certificates` → `PermissionDenied`.
- Wrong role / trust domain / URI SAN → rejected (shared helper, same cases as `heartbeat_test.go`).

Go (`store_renew_integration_test.go`, same DB env as `store_revoke_integration_test.go`):
- `RecordRenewedCert` inserts the row and updates `relays.cert_*`.
- Race: run `RevokeRelay` and `RecordRenewedCert` concurrently many times. Afterwards **no** unrevoked `relay_certificates` row exists for the relay.
- `RecordRenewedCert` on a revoked relay → `ErrRelayRevoked`, nothing inserted.
- `ListRevokedRelaySerials` includes the renewed serial after revoke (so `/relay.crl` lists it).

Rust (`cargo test` in `relay/`):
- Schedule computation (renew-at from not_before/not_after, jitter bounds, already-expired branch).
- CSR from existing key has the same public key.
- Atomic write leaves either the old or the new file, never a partial one (simulate failure before rename).
- Resolver swap: build a server config, swap the holder, and a new handshake sees the new serial.
- Response validation rejects mismatched SPIFFE ID / key / Intermediate fingerprint.

## Acceptance Criteria

- [ ] Dev stack with `RELAY_CERT_TTL=15m`: the relay renews by itself around the 9-minute mark, the `relays.cert_serial` changes, `relay_certificates` has two rows, and the relay keeps serving connectors and clients past the first cert's `NotAfter` **without a restart**.
- [ ] Revoke that relay → both serials revoked; `/relay.crl` lists both; connectors drop it (existing CRL monitor).
- [x] `buf generate` diff for `relay.proto` is additive only (verified in F-1: no removed or changed lines).

## Build Check

```bash
buf generate                                   # repo root
cd controller && go build ./...
cd controller && go test ./internal/relay/... ./internal/connector/...
cd relay && cargo build && cargo test
cd connector && cargo build                    # proto consumer; must still build
```

## Implementation Checklist

**F-1 — controller half (done)**

- [x] **M2-F1** Proto: additive `RenewCert` + `RenewCertRequest{csr_der}` / `RenewCertResponse{certificate_pem, intermediate_ca_pem, cert_not_after_unix, cert_not_before_unix}`; `buf generate`
- [x] **M2-F2** `renew.go`: handler (identity via shared `authenticatedRelay`, expiry → replace (D-20), same key (D-19), status active/inactive, presented serial known + unrevoked, Phase C `newRelaySANAllowlist` + `precheckRelayCSR` reused, signer gets stored allowlists)
- [x] **M2-F3** `store.go`: transactional `RecordRenewedCert` (`FOR UPDATE` on the relay row, the same lock `RevokeRelay` takes) + `RelayCertStatus`; typed errors `ErrRelayNotRenewable`, `ErrPresentedCertInvalid`
- [x] **M2-F6 (Go)** Tests:
  - `renew_test.go` (12): happy path, inactive, different key, expired, pending/revoked/deleted, unknown/revoked presented cert, Phase C rules, wrong identity, idempotent retry, revoked cached renewal not replayed, concurrent attempt aborted, revoked mid-renewal returns no cert
  - `store_renew_integration_test.go`: record, retry supersede (exactly 2 unrevoked certs), renewal chain, refused states, renew-vs-revoke race (25 iterations; both lock orderings observed across 3 runs; never a live cert for a revoked relay; renewed serial on the revoked list)
- [x] **F-1 build gate:** `buf generate`, `go build ./...`, `go vet`, `go test ./internal/relay/... ./internal/pki/...` (0 skips with `PKI_TEST_DATABASE_URL`), `go test ./...`, relay `cargo build` with the additive proto

**F-2 — relay Rust runtime (next)**

- [ ] **M2-F4** `relay/src/renewal.rs`: scheduler, CSR from existing key, atomic write, response validation
- [ ] **M2-F5** Relay hot swap: listener resolver + heartbeat identity
- [ ] **M2-F6 (Rust)** Tests: scheduler, atomic write, resolver swap, response validation
- [ ] **Full build gate:** `cd controller && go build ./... && cd ../relay && cargo build && cargo test`
- [ ] **Live acceptance — NOT YET RUN** (see Acceptance Criteria)

## F-1 → F-2 Contract (binding for the relay runtime)

The controller's retry guarantees depend on the relay behaving as follows:

1. **Never keep presenting the old certificate after persisting the renewed one.** Once the renewed certificate is atomically written to disk, every consumer must switch to it: the QUIC listener, the heartbeat channel, and subsequent `RenewCert` calls.
2. **Why:** `RecordRenewedCert` revokes any unrevoked successor issued *after* the presented certificate (reason `superseded by renewal retry`). That is only safe because a relay still presenting certificate *P* has, by contract, not put P's successor into use.
3. **Retries are cheap and safe.** Retrying `RenewCert` while still presenting *P*, e.g. after a lost response, returns the **same** certificate for up to 1 h (Valkey result cache keyed by relay + presented serial). After that, the retry supersedes the unused successor and issues a new one. `codes.Aborted` means an identical attempt is in flight: back off and retry.
4. **Same key only (D-19).** The CSR must be signed by the relay's existing private key. The key file is never rewritten.
5. **Expired means replace (D-20).** If the certificate has already expired, don't call `RenewCert`. The relay must be replaced.

## Implementation Notes (F-1)

- The idempotency cache and lock are optimizations. If Valkey is unavailable the lock fails open, and the database rule (at most one live successor) still holds.
- The signed certificate is discarded, never returned or cached, if the locked write refuses it (relay revoked or deleted between the pre-checks and the write).
- There is no server-side "too early" renewal window. Scheduling is the relay's job (D-19); every renewal is recorded and revocable.

## Post-Phase Fixes

_None yet._
