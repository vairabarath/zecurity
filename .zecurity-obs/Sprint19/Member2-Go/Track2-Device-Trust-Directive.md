---
title: Track 2 — The Trust Signal (server→client device directive)
pending: PENDING-13
depends_on:
  - feat/identity-device-trust-contract (MERGED to fixed-pendings)
  - feat/pending-13-device-revoke-handler (Track 1, rebased on fixed-pendings)
---

# Track 2 — THE TRUST SIGNAL (server→client device directive)

## Goal
Give the client device a structured, pull-polled **device directive** so it learns
— every ~60s on its existing ACL poll — whether it has been **revoked** or must
**re-enroll**, and acts accordingly (wipe on-disk key, stop tunnels, surface the
right message). This closes the loop Track 1 opened: Track 1 revokes the cert
(server-side CRL + ACL gate enforce it), but today the *device itself* never learns
it — it just gets a bare `PermissionDenied` and keeps polling with a live key on
disk, forever. Track 2 turns that opaque error into an actionable signal.

## Key discovery (why this is small)
The 60s channel already exists. `Service.GetACLSnapshot` (internal/client/service.go:477)
already:
- receives `device_id` + verifies the access token,
- looks up `client_devices WHERE id=$device_id AND user_id=$claims.UserID`,
- gates on `revoked_at` (line 502-504) — a revoked device currently gets a bare
  `PermissionDenied: "device has been revoked"`.

`GetTransportSnapshot` (service.go:544) has the **identical** gate (line 567-569).
So the server↔device conversation happens every 60s and the server already computes
"is this device revoked?". The gap is purely: the answer ships as an error, not a
structured directive the client can act on. The Go server side is therefore tiny;
the effort is proto regen + Rust daemon reaction.

## Scope
✅ Add `device_directive` to `GetACLSnapshotResponse` (and a shared gate helper so
   `GetTransportSnapshot` returns a clean signal too, no log spam).
✅ `status` column on `client_devices` carrying ONLY {active, re_enroll_required,
   renew_pending} — revoked is DERIVED from `revoked_at`, never duplicated.
✅ Track 1's `ReEnrollHandler` upgraded to set `status='re_enroll_required'`
   (makes the honest-minimal handler real).
✅ Resolver populates the directive from status/revoked_at.
✅ Daemon reads the directive and acts (REVOKED / RE_ENROLL_REQUIRED / RENEW_SOON / NONE).
✅ Stamp `last_seen_at` (throttled) in the same handler — fills the dead column.
❌ No RenewCert RPC / renewal scheduler (that's Track 3 — but the RENEW_SOON enum
   value is reserved now so Track 3 only reacts).
❌ No client-side cert renewal logic.
❌ No change to the CRL / revoke enforcement path (Track 1's durable path stands).

## Design decisions (locked)

### D-A — Directive transport: piggyback on GetACLSnapshot (single channel)
Put the directive ONLY on `GetACLSnapshot`. Once the ACL poll shuts the daemon down
(or disables it), transport is moot. `GetTransportSnapshot` reuses the SAME gate
helper but returns the directive value too (so it doesn't spew `PermissionDenied`
post-revoke) — however the *authoritative* reaction happens in the ACL poll path.

### D-B — Revoked response: directive-in-response, not error
Replace the `PermissionDenied` return at service.go:502-504 (and the transport twin
at 567-569) with a structured response:
- REVOKED / RE_ENROLL_REQUIRED → return `directive` set, `snapshot` **omitted
  (empty, NOT `up_to_date=true`)**. A directive-ignoring client therefore also
  drops its cached ACL — defense-in-depth fail-closed (the device loses access
  even if it ignores the directive). gRPC status OK. No ACL payload regardless
  (preserves the existing control-plane gate, no leak).
This keeps the control-plane gate (revoked devices get no ACL data) while giving a
clean, distinguishable signal. Keeping the bare error would force the daemon to
match error *strings* — fragile.

### D-C — status column does NOT carry "revoked"
`status ∈ {active, re_enroll_required, renew_pending}`. `revoked` is DERIVED:
`revoked_at IS NOT NULL`. Only the re-enroll handler writes `status` (to
`re_enroll_required`); the revoke paths keep writing ONLY `revoked_at` exactly as
today (Track 1 handler, admin, self-service). One writer of "revoked-ness" → zero
drift. The migration also gets simpler: `status` defaults `active`, NO backfill of
`revoked` (it's derived).

Directive derivation (single place, in the gate helper):
```
if status == 're_enroll_required'                          -> RE_ENROLL_REQUIRED
else if revoked_at IS NOT NULL                             -> REVOKED
else if status == 'renew_pending'                          -> RENEW_SOON   // Track 3 populates
else                                                       -> NONE
```

### D-D — REVOKED vs RE_ENROLL messaging MUST differ (locked)
- REVOKED → device is offboarded (user suspended/deleted upstream). Message:
  **"Access revoked — contact your admin."** NO re-login prompt (re-login will just
  fail). Wipe on-disk key, STOP tunnels, then **exit** the daemon, persisting a
  marker `device_state=revoked` to disk so the CLI (`zecurity-client status`)
  can surface "revoked: contact admin" on demand. A credential-less daemon polling
  forever is waste + confusing; re-enrollment is a fresh enroll anyway.
- RE_ENROLL_REQUIRED → device is recoverable (user reactivated). Message:
  **"Sign in again to re-register this device."** Wipe dead cert, STOP tunnels,
  keep daemon **alive-but-disabled** showing the re-login prompt (in-place recovery
  is possible). This is the one directive where stay-running is the better UX.
- RENEW_SOON → (Track 3 reaction) nudge renewal; daemon stays NONE until then.
- NONE → proceed normally.

### D-E — last_seen_at: throttled write
In the gate helper, after the lookup, stamp last_seen only if stale:
`UPDATE client_devices SET last_seen_at = NOW() WHERE id=$1 AND (last_seen_at IS NULL OR last_seen_at < NOW() - INTERVAL '5 minutes')`.
Idempotent-guard style (cf. Track 1) — avoids 60s-per-device write amplification
across the fleet.

## Files

| File | Change |
|------|--------|
| `controller/migrations/0XX_device_status.sql` | ADD COLUMN `client_devices.status` text NOT NULL DEFAULT 'active'; CHECK (status IN ('active','re_enroll_required','renew_pending')); comment that `revoked` is derived from `revoked_at`. |
| `proto/client/v1/client.proto` | Add `enum DeviceDirective { DIRECTIVE_NONE=0; DIRECTIVE_REVOKED=1; DIRECTIVE_RE_ENROLL_REQUIRED=2; DIRECTIVE_RENEW_SOON=3; }`. Add `device_directive` + `directive_reason` to `GetACLSnapshotResponse` (and `GetTransportSnapshotResponse`). |
| `controller/gen/go/proto/client/v1/client.pb.go` + `client_grpc.pb.go` | **Regenerated** via `make generate-proto` (buf). The `clientv1` Go package is generated into `controller/gen/go/...`, NOT checked in under `proto/`. Without this regen the resolver cannot set `DeviceDirective`. |
| `client/proto/client/v1/client.rs` (generated) | Regenerated via `cargo build` (client/build.rs uses `tonic_build` on `proto/client/v1/client.proto`). |
| `controller/internal/client/service.go` | Factor a `deviceGate(ctx, pool, deviceID, claims) (status string, revoked bool, directive DeviceDirective, err error)` helper used by BOTH `GetACLSnapshot` and `GetTransportSnapshot`. On revoked/re-enroll: return the directive + empty snapshot (no `up_to_date`), gRPC OK. Stamp `last_seen_at` (throttled) on success path. |
| `controller/internal/client/device_trust_handler.go` | `ReEnrollHandler`: set `status='re_enroll_required'` on the user's devices (in addition to the audit it already writes). Revoke path unchanged (writes only `revoked_at`). |
| `client/src/daemon.rs` | `fetch_acl_snapshot_with_refresh` must return the directive (not just `Option<AclSnapshot>`). Act on it: REVOKED → wipe key + stop tunnels + persist `device_state=revoked` + exit; RE_ENROLL_REQUIRED → wipe cert + stop tunnels + stay-disabled with re-login prompt; NONE → proceed. Fail-closed: a poll *error* keeps existing behavior; the directive can only make the client more restrictive. |
| `client/src/state_store.rs` | Add `device_state: String` (`#[serde(default)]`) to `StoredDevice` so the REVOKED marker persists on disk. |
| `client/src/ipc.rs` | Add `device_state` / `revoked_reason` to the `Status` IPC response so the CLI can read it. |
| `client/src/cmd/status.rs` | Print "Status: Revoked — contact your admin" (or re-enroll prompt) when `device_state` is set, instead of the normal running status. |

## Step-by-step
1. Migration `0XX_device_status.sql` — add `status` column (default active, CHECK).
   No backfill of revoked (derived).
2. Proto: add `DeviceDirective` enum + fields on both responses.
   - Go stubs: `cd controller && make generate-proto` (buf → `controller/gen/go/proto/client/v1/client.pb.go`).
   - Rust stubs: `cd client && cargo build` (build.rs `tonic_build` on `proto/client/v1/client.proto`).
   Both regen flows are required — the resolver sets the enum only after the Go
   `clientv1` package is regenerated.
3. `service.go`: extract `deviceGate` helper; both snapshot handlers call it;
   revoked/re-enroll returns directive + **empty snapshot (no `up_to_date`)**;
   throttle-stamp `last_seen_at`.
4. `device_trust_handler.go`: `ReEnrollHandler` sets `status='re_enroll_required'`.
5. `daemon.rs`: change `fetch_acl_snapshot_with_refresh` return type to carry the
   directive; implement REVOKED (wipe+exit+marker) and RE_ENROLL_REQUIRED
   (wipe+disabled+prompt) reactions. Also extend `state_store.rs` (`StoredDevice`
   marker), `ipc.rs` (Status response), and `cmd/status.rs` (print) so the
   REVOKED state is visible via `zecurity-client status`. Unit-test the directive
   mapping.
6. Build gate: `cd controller && go build ./... && go vet ./...`; `cd client && cargo build`.
7. Tests (below).

## Migration number (coordinate at integration)
The controller has no in-repo migration runner; migrations apply externally by
filename sort, and duplicate numeric prefixes already exist (two `031_*` files).
The next free number is **034** — which collides with Sathiya's SCIM migration
`034_scim_directory_sync.sql` on `feat/sprint17-scim`. Track 2 keeps the
`0XX_device_status.sql` placeholder and takes `034` only if the SCIM migration
lands under a different number; otherwise `035`. Resolve at integration/rebase
time.

## Tests
- Controller unit: `deviceGate` returns correct directive for each `{status, revoked_at}`
  combination (re_enroll first, then revoked-derived, then renew_pending, then none).
- Controller integration: revoked device calling `GetACLSnapshot` gets
  `directive=REVOKED`, empty snapshot (no `up_to_date`), gRPC OK (NOT PermissionDenied);
  re-enroll-event device gets `RE_ENROLL_REQUIRED`; active device gets `NONE` + snapshot.
- `last_seen_at` throttle: call twice rapidly → second call does not advance the
  timestamp beyond the 5min window.
- Client unit (Rust): map `REVOKED`→wipe+exit path invoked; `RE_ENROLL_REQUIRED`→
  wipe+disabled path; `NONE`→no-op. Mock the fetch to return each directive.

## Verify commands
```
cd controller && go build ./... && go vet ./...
PKI_TEST_DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform?sslmode=disable \
  go test ./internal/client/...
cd ../client && cargo build && cargo test -p zecurity-client daemon::directive
```

## Acceptance criteria
- [ ] `device_directive` reachable on `GetACLSnapshot`; REVOKED/RE_ENROLL returned as
      OK responses with no ACL payload (control-plane gate preserved).
- [ ] `status` column exists, carries only {active, re_enroll_required, renew_pending};
      `revoked` derived from `revoked_at` (no dual-write).
- [ ] Track 1 `ReEnrollHandler` sets `status='re_enroll_required'`.
- [ ] Daemon acts: REVOKED → wipe key + stop tunnels + persist marker + exit;
      RE_ENROLL_REQUIRED → wipe cert + stop tunnels + disabled w/ re-login prompt.
- [ ] `last_seen_at` stamped (throttled) each poll.
- [ ] `GetTransportSnapshot` returns clean directive (no PermissionDenied spam).
- [ ] `RENEW_SOON` enum reserved for Track 3.
- [ ] `go build ./...` + `go vet ./...` + `cargo build` green; tests pass.

## Coordination
- Track 2 stacks on `feat/pending-13-device-revoke-handler` (upgrades its
  `ReEnrollHandler`). Same pattern as the contract branch.
- Track 3 (RenewCert + renewal scheduler) rides on the `RENEW_SOON` channel defined
  here — design the directive enum to carry it now, implement the reaction later.
- No dependency on Sathiya's SCIM Phase 6 for Track 2 itself (the re-enroll event
  already flows via the outbox once his producer lands; until then the directive is
  exercised via tests + admin revoke).
