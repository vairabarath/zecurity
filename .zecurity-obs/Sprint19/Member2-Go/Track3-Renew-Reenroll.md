---
title: Track 3 — Renew / Re-enroll (RenewCert + daemon renewal scheduler)
pending: PENDING-13
depends_on:
  - feat/pending-13-device-revoke-handler (Track 1 + Track 2, merged via PR #79)
---

# Track 3 — RENEW / RE-ENROLL (fixes weekly device breakage)

## Goal
Client device certs have a **7-day TTL and zero renewal mechanism** — connectors,
shields, and relays all auto-renew; clients don't. Today the only way to get a
fresh cert is a full interactive browser re-login, so **every device breaks
weekly** (ADR-028, gap #1). Track 3 adds a `RenewCert` RPC + a daemon scheduler
so the device silently renews its own cert before expiry, using the same
private key it already holds — with a real cryptographic guarantee that a
renewal request actually comes from the device that holds that key, not just
someone holding a stolen session token.

## Key discoveries (why this is smaller than it looks)
1. **`ClientService` is exempt from the mTLS/SPIFFE interceptor** (service.go:3-4)
   — every RPC, including the new `RenewCert`, authenticates via the bearer
   `access_token` field, never via the device's own certificate. This means:
   - Unlike the connector's `RenewCert` (`internal/connector/enrollment.go:229`),
     which gets "proof you hold the current cert's key" for free from the mTLS
     handshake, the client gets no such guarantee from the channel itself. We
     have to build that guarantee explicitly (see D-A).
   - An **expired device cert never blocks a control-plane call** — only the
     *data-plane* tunnel handshakes to connectors/shields (which do use mTLS)
     break on expiry. So `RenewCert` has no hard deadline tied to the cert's
     own clock; it works anytime the access/refresh-token session is alive,
     cert expired or not. The "offline past expiry" fallback the ADR mentions
     therefore isn't a new code path — it falls out of the existing
     session-dead -> re-login -> `EnrollDevice` flow for free once the
     *session* (not the cert) has died.
2. **`pki.Service.SignClientCert`** (workspace.go:440) is stateless — given any
   `deviceID` + verified CSR it mints a cert. `EnrollDevice` (service.go:420)
   already does CSR-parse -> CSR-verify -> `SignClientCert` ->
   `updateClientDeviceCert`. `RenewCert`'s handler is almost the same sequence
   minus `insertClientDevice` (the row already exists) and minus
   `NotifyPolicyChange` (the SPIFFE ID doesn't change, so nothing ACL-relevant
   changed) — most of the plumbing is reused, not new.
3. **The client already builds CSRs with `rcgen`** (`login.rs:110-126`) and
   `rcgen::KeyPair::from_pem` (confirmed present in the vendored 0.13.2 source,
   `key_pair.rs:176`) can reconstruct the daemon's *existing* keypair from the
   `private_key_pem` already held in `state_store`/`RuntimeState`. So renewal
   never needs a new keypair — it builds a fresh CSR from the same key, exactly
   like the connector's `renewal.rs` already does.

## Scope
✅ `client_devices.public_key_fingerprint` — pinned once, at `EnrollDevice`
   time, from the CSR's public key. Never overwritten by renewal.
✅ `RenewCert` RPC: verify access_token + device ownership + not
   revoked/re-enroll-required, verify the CSR's fingerprint matches the pinned
   one, sign a fresh cert for the *same* device_id/spiffe_id.
✅ `deviceGate` derives `RENEW_SOON` from `cert_not_after` vs a renewal window
   (elapsed life ≥ ~60%, ADR-028 D4) — **derived, not stored**, extending the
   same "single source of truth" pattern Track 2 used for `revoked` (never a
   `status='renew_pending'` write anywhere).
✅ Daemon renewal scheduler (mirrors `run_refresh_scheduler`, daemon.rs:1513):
   wakes near the renewal window, builds a CSR from the existing key, calls
   `RenewCert`, persists the new cert. `DIRECTIVE_RENEW_SOON` on the ACL poll
   is a backstop nudge, not the primary trigger — the scheduler doesn't wait
   for a poll to notice.
✅ Legacy devices (enrolled before this migration, `public_key_fingerprint IS
   NULL`) are told to re-enroll on their first renewal attempt — **no
   trust-on-first-use**. See D-A for why.
✅ Uniform denial response from `RenewCert` regardless of *why* it was denied
   (not found / revoked / re-enroll-required / fingerprint mismatch / legacy
   no-fingerprint) — no oracle for an attacker holding a stolen access_token.
❌ No change to `clientCertTTL` (stays 7 days) — the ADR leaves the
   shorten-it-now-that-renewal-exists question open; not this track's call.
❌ No admin-facing surfacing of `renew_pending`/last-renewal — `status` (CLI)
   only, per D4's "surface a warning in status" scope.
❌ No change to the revoke/re-enroll paths from Track 1/2 — `RenewCert` reads
   `deviceGate`'s directive to refuse REVOKED/RE_ENROLL_REQUIRED devices, it
   doesn't touch how those states are set.

## Design decisions (locked)

### D-A — Fingerprint pinning, no TOFU (the whole point of this track)
`client_devices.public_key_fingerprint` is set **exactly once**, by
`EnrollDevice`, from the CSR's public key (SHA-256 of the DER-encoded SPKI).
`RenewCert` **compares** against it and never writes it. This is deliberately
asymmetric — if renewal could overwrite the pinned fingerprint, a stolen
access_token could "renew" once with an attacker-controlled key and pin itself
as canonical going forward, which is exactly the hijack this exists to prevent.

Devices enrolled *before* this migration have `public_key_fingerprint IS
NULL`. Decided: **no trust-on-first-use fallback.** A `NULL` fingerprint means
"must re-enroll," full stop — `RenewCert` denies it with the same generic
denial as every other rejection reason (D-D). Rationale: TOFU on first
renewal call is a race an attacker with only a stolen access_token can win
(first caller pins the "canonical" key), so it silently reopens the exact
hijack the fingerprint check exists to close, for the entire pre-migration
device population. Since this isn't a production rollout with a large
installed base to migrate gently, there's no reason to accept that window —
force the one-time re-login instead.

### D-B — `RENEW_SOON` is derived, not stored
Extends Track 2's D-C pattern (`revoked` is derived from `revoked_at`, never
duplicated into `status`). `deviceGate` computes, in priority order:
```
re_enroll_required status                              -> RE_ENROLL_REQUIRED
else revoked_at IS NOT NULL                             -> REVOKED
else cert_not_after - now() <= renewalWindow            -> RENEW_SOON
else                                                     -> NONE
```
`renewalWindow = clientCertTTL * 0.4` (≈2.8 days for the current 7-day TTL,
i.e. renewal becomes due once elapsed life crosses ~60% — ADR-028 D4). Named
constant, not a magic number. The `renew_pending` value already reserved in
`status`'s CHECK constraint (Track 2 migration) stays permanently unused —
this is a deliberate deviation from ADR-028 D5's literal text, consistent with
Track 2's own precedent of preferring derivation over dual-write.

### D-C — `RenewCert` authorization order
1. Verify `access_token` -> claims.
2. `deviceGate`-style ownership check (device belongs to claims' user +
   workspace) — reuse the existing helper's lookup, not a new query.
3. Reject if directive is `REVOKED` or `RE_ENROLL_REQUIRED` — those must go
   through `EnrollDevice`, never a silent renewal.
4. Parse + `CheckSignature()` the CSR (proves possession of *some* key).
5. Compare the CSR's fingerprint to the stored one. `NULL` stored -> deny
   (D-A). Mismatch -> deny. Match -> proceed.
6. `pkiSvc.SignClientCert` (reused, unmodified) -> `updateClientDeviceCertOnRenewal`
   (new — sets `cert_serial`/`cert_not_after` only; never touches
   `spiffe_id` or `public_key_fingerprint`).

### D-D — Uniform denial, detailed audit
Every rejection in D-C steps 2-5 returns the **same** `PermissionDenied`
message to the caller (e.g. "device not eligible for renewal") — no
distinguishing text for not-found vs revoked vs fingerprint-mismatch. The
*specific* reason is still written to `audit_logs` server-side
(`device.cert.renew_denied` with a `reason` field), so operators can debug it,
but the wire response gives an attacker no oracle.

### D-E — Same keypair, fresh CSR, every renewal
The daemon never generates a new keypair for renewal. It loads
`private_key_pem` from `RuntimeState`/`state_store`, reconstructs the
`rcgen::KeyPair` via `KeyPair::from_pem`, and builds a brand-new
self-signed CSR from that same key each time (mirrors
`connector/src/renewal.rs`, which does the equivalent for connectors). Reusing
the key is what makes the fingerprint check meaningful in the first place — a
fresh key on every renewal would defeat D-A entirely.

## Files

| File | Change |
|------|--------|
| `controller/migrations/0XX_client_device_pubkey_fingerprint.sql` | `ALTER TABLE client_devices ADD COLUMN public_key_fingerprint TEXT;` (nullable — legacy rows stay NULL, handled by D-A, not backfilled). |
| `proto/client/v1/client.proto` | `RenewCertRequest{ access_token, device_id, csr_pem }` / `RenewCertResponse{ certificate_pem, workspace_ca_pem, intermediate_ca_pem, cert_expires_at }`; `rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);` on `ClientService`. |
| `controller/gen/go/proto/client/v1/*.pb.go` + `client_grpc.pb.go` | Regenerated via `make generate-proto` — this one **does** need `client_grpc.pb.go` regen too, since it's a new RPC (Track 2 only added message fields). |
| `client/proto` (generated, tonic) | Regenerated via `cargo build`. |
| `controller/internal/client/store.go` | New `publicKeyFingerprint(pub crypto.PublicKey) (string, error)` helper (SHA-256 of `x509.MarshalPKIXPublicKey`). `updateClientDeviceCert` (used only by `EnrollDevice`) gains a `fingerprint` param and writes it. New `updateClientDeviceCertOnRenewal(ctx, db, deviceID, serial, notAfter string/time.Time)` — sets only `cert_serial`/`cert_not_after`. |
| `controller/internal/client/service.go` | `EnrollDevice`: compute fingerprint from the parsed CSR, pass to `updateClientDeviceCert`. New `RenewCert` handler per D-C. `deviceGate`: add the `RENEW_SOON` branch per D-B. |
| `controller/internal/audit/*` (or wherever `audit.Entry` actions are used) | New `device.cert.renewed`, `device.cert.renew_denied` (reason field), `device.cert.renewed_legacy_denied` if we want that distinguishable server-side — TBD naming at implementation time, not locked here. |
| `client/src/daemon.rs` | New `run_cert_renewal_scheduler(state, conf)` mirroring `run_refresh_scheduler` (daemon.rs:1513): sleeps until `cert_expires_at - renewalWindow`, builds a CSR from the existing key (`rcgen::KeyPair::from_pem`), calls `RenewCert`, persists via state_store, updates `RuntimeState`. Directive handling: `DIRECTIVE_RENEW_SOON` in `deviceGate`'s response nudges an immediate attempt (best-effort wake, not a hard trigger) rather than waiting for the next scheduler tick. |
| `client/src/state_store.rs` | New `save_renewed_cert(workspace_slug, certificate_pem, cert_expires_at)` — load -> mutate -> save, same idiom as `save_rotated_tokens`; updates only `certificate_pem`/`cert_expires_at`, leaves `private_key_pem` untouched. |
| `client/src/cmd/status.rs` | Surface a warning when renewal has failed and the cert is within its last ~24h (D4) — exact wording TBD at implementation time. |

## Migration number coordination
Next free number on this branch is `035`. Per the Track 2 note, `feat/sprint17-scim`
(unmerged as of this writing) already claims both `034` and `035`. Same
resolution as before: keep the `0XX_` placeholder, take whichever number is
actually free at integration/rebase time — not a call to make now.

## Step-by-step
1. Migration: add `public_key_fingerprint` column (nullable, no backfill).
2. Proto: add `RenewCertRequest`/`RenewCertResponse` + the `RenewCert` RPC.
   Regenerate **both** Go (`make generate-proto`, including the gRPC service
   stub this time) and Rust (`cargo build`).
3. `store.go`: `publicKeyFingerprint` helper; split `updateClientDeviceCert`
   (enrollment, writes fingerprint) from `updateClientDeviceCertOnRenewal`
   (renewal, never writes fingerprint).
4. `service.go`: wire the fingerprint into `EnrollDevice`; extend `deviceGate`
   with the `RENEW_SOON` window; implement the `RenewCert` handler per D-C/D-D.
5. `daemon.rs`: `run_cert_renewal_scheduler` + the CSR-from-existing-key
   helper (reusable by both the scheduler and, if useful, tests); wire
   `DIRECTIVE_RENEW_SOON` as a backstop nudge alongside it.
6. `state_store.rs`: `save_renewed_cert`.
7. `cmd/status.rs`: near-expiry-with-failed-renewal warning.
8. Build gate: `go build ./... && go vet ./...`; `cargo build`.
9. Tests (below).

## Tests
- Controller unit: `deviceGate`'s new `RENEW_SOON` branch across the time
  window (just inside / just outside / already past due) — same table-test
  shape as `TestDeviceGateDirectiveDerivation`.
- Controller integration: `RenewCert` happy path (fingerprint matches, new
  cert issued, `cert_serial` changes, `spiffe_id`/`public_key_fingerprint`
  unchanged); denied for `REVOKED`/`RE_ENROLL_REQUIRED`; denied for fingerprint
  mismatch; denied for `NULL` fingerprint (legacy, no TOFU); denial responses
  are textually identical across all four reasons (D-D) while audit rows
  differ.
- Client unit (Rust): building a CSR from an existing `private_key_pem`
  reproduces the same public key (round-trip check); scheduler wake-timing
  math against the renewal window, mirroring however `run_refresh_scheduler`
  is tested today (check for existing coverage before assuming none exists).

## Verify commands
```
cd controller && go build ./... && go vet ./...
PKI_TEST_DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform?sslmode=disable \
  go test ./internal/client/...
cd ../client && cargo build && cargo test -p zecurity-client daemon::renewal
```

## Acceptance criteria
- [x] `public_key_fingerprint` pinned at `EnrollDevice`, never overwritten by renewal.
      Verified: `TestRenewCertHappyPath` asserts the fingerprint is unchanged after renewal.
- [x] `RenewCert` issues a fresh cert only when the CSR's key matches the pinned fingerprint.
      Verified: `TestRenewCertHappyPath` (match) + `TestRenewCertDenials/fingerprint_mismatch`.
- [x] `RenewCert` denies (uniformly) REVOKED / RE_ENROLL_REQUIRED / fingerprint-mismatch / NULL-fingerprint devices — with distinct audit rows server-side.
      Verified: `TestRenewCertDenials` (6 subtests) — identical wire message, distinct `reason` per audit row.
- [x] `RENEW_SOON` reachable via `deviceGate`, derived from `cert_not_after`, no new stored status value.
      Verified: `TestDeviceGateDirectiveDerivation` (8 subtests, including revoked/re-enroll beating renew-soon).
- [x] Daemon renews automatically before expiry using the *same* keypair; a device left running never needs an interactive re-login again.
      Verified at the unit level: `renewal_sleep_secs` (wake-time math), `build_renewal_csr_reuses_the_same_key`
      (CSR round-trip proves the same key, not a fresh one), `run_cert_renewal_scheduler` wired into `run()`.
      No full scheduler-against-a-live-server test existed until the e2e pass below.
- [x] Session-dead (not cert-dead) is the actual trigger for falling back to interactive re-enroll — no separate "offline past expiry" code path needed.
      True by construction: `RenewCert`'s handler never checks `cert_not_after` — only ownership/revoked/
      re-enroll/fingerprint — so it can succeed even after the old cert has expired, as long as the session
      is alive. No dedicated regression test; this is an absence-of-a-check, not a feature to assert on.
- [x] `go build ./...` + `go vet ./...` + `cargo build` green; tests pass.

## E2E verification
`daemon::renewal_tests::renewal_e2e_against_real_tls_server` drives `attempt_cert_renewal` — the
per-attempt logic split out of `run_cert_renewal_scheduler`'s loop for exactly this reason, same
pattern as `sync_acl_now_with` being split from `sync_acl_now` — against a real in-process tonic
`ClientServiceServer`, over a real self-signed TLS handshake (the same trust model `connect_grpc` always
uses, never plaintext). Required flipping `build.rs`'s `build_server(false)` to `true`: this binary only
ever dials OUT as a client in production, so server codegen is dead weight there, generated purely so
this fake server can exist in tests. Only `renew_cert` does real work; the fake's other 8 RPC methods
return `Unimplemented` (not `unimplemented!()`) so an accidental call fails the test cleanly.

Confirms end-to-end: build the CSR from the existing key -> real TLS-secured gRPC round trip with the
actual `RenewCertRequest`/`RenewCertResponse` wire types -> parse the response -> update `RuntimeState`
-> `state_store::save_renewed_cert` actually persists to disk, verified by reloading it -> the private
key is untouched throughout. Stable across repeated runs (checked). This is the one gap the per-piece
unit tests (wake-time math, CSR-reuses-the-same-key, RENEW_SOON notify) didn't close on their own: proof
the pieces are wired together correctly, not just individually correct.

## Coordination
- Stacks on the merged Track 1 + Track 2 work (PR #79).
- No dependency on Sathiya's SCIM work.
- Revisits ADR-028's open question #1 (shorten the 7-day TTL now that renewal
  exists?) only if the team raises it — not decided here.
