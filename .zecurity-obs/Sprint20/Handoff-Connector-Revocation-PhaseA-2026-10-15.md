---
title: Handoff — Connector Revocation Phase A done → continue with Phase B1
date: 2026-10-15
branch: feat/sprint20-m1-phase2
commit: 4cb5a6a (pushed to origin/feat/sprint20-m1-phase2)
design: Fixes/connector-revocation/Connector-Revocation-Implementation-Design-2026-10-14.md (rev 3)
record: Fixes/connector-revocation/Connector-Revocation-PhaseA-Implementation-2026-10-15.md
status: PHASE A DONE (development intermediate) — connector NOT fail-closed; Phase B1 not started
---

# Handoff — Connector Revocation, continuing after Phase A

## Where things stand

- **Phase A** (controller–connector authorization protocol) is implemented, tested, **committed as `4cb5a6a` and pushed**. The final Phase A report was written before the commit, so its line "nothing committed, HEAD `12e43ac`" is out of date; this file replaces it.
- **The connector still serves without an acknowledgement.** A Phase A build is a development intermediate. **Do not use it for live revocation or security acceptance.**
- Nothing from Phase B, M, C, D, E or F has been started.

## What Phase A delivered (summary; full detail in the record)

**Protocol (additive):**
- `ConnectorHealthReport.heartbeat_seq = 9`
- `ConnectorControlMessage.authorization_ack = 19` → `AuthorizationAck{heartbeat_seq}`
- `ConnectorControlMessage.connector_revoked = 20` → `ConnectorRevoked{}`
- `AgentPolicy.authorization_acks = 2`

**Structured marker:** gRPC trailer `zecurity-connector-state` = `revoked` | `unknown`, attached only after a successful lookup.

**Ack gating (controller):** the ack is enqueued last, and only when `seq ≥ 1`, the revocation-guarded UPDATE returned a row, and the required ACL push succeeded (enqueued ahead of it, FIFO). Never on:
- `seq = 0`: `FailedPrecondition`, no marker, no DB write;
- a health-UPDATE DB error: the stream is kept, no ACL push, no ack, the next report retries;
- an ACL push failure or a full mailbox;
- a stream already carrying `ConnectorRevoked`.

**Classification** (at open, mid-stream after the UPDATE matches no row, and at activation):

| Situation | Result |
|---|---|
| Revoked row | `PermissionDenied` + `revoked` |
| Absent row | `NotFound` + `unknown` |
| Lookup failure, or the row is unexpectedly still authorized | `Unavailable`, no marker |

This fixes the old "DB error → `NotFound`" and the "deleted row labelled revoked" bugs.

**`ConnectorRevoked` delivery:**
- `RevokeConnector` → `SignalRevoked` → every live stream, including a stream superseded by a renewal.
- It is atomic with activation (it holds `lifecycleMu`).
- It uses a one-slot priority mailbox, so a full queue can't drop it.
- The stream then ends with the `revoked` marker.
- Remaining ordering: an ack enqueued before the revoke committed may precede the event; the terminal signal wins on the connector (§5.4).

**Connector (plumbing only):**
- sequence numbers are process-global, start at 1 and never reset;
- send time is stamped when tonic's encoder pulls the message (the transport boundary, conservative);
- a renewal replacement reuses the outstanding seq and its original send time;
- acks are validated (zero, duplicate or out-of-order, unknown or mismatched, stale ≥ H, after a terminal signal, no capability);
- statuses are classified on the marker only, never the text;
- every outcome is logged as a structured `authz_event`.

**Tests:**
- full controller `go test ./...` with the DB: 26 packages ok;
- `internal/connector`: 137 passes (only the pre-existing unconditional `TestEnroll_CSRSignatureInvalid` skips);
- connector `cargo test`: 161 lib tests, +20 new over the 141 baseline;
- cross-language Go grpc ↔ Rust tonic test passed;
- mutation checks: 11 controller and 11 connector mutants, all killed.

## Next continuation — Phase B1 (connector states + serving gate)

**Prerequisites before starting B1:**
1. An explicit B1 go-ahead with a pre-written acceptance list (design §17).
2. A decision on **O9**: persist `Revoked`/`UnknownIdentity` across restart, and exit vs idle. Safety doesn't depend on it.
3. Re-read design §4.4 (all ack rules, including the **deadline D** and **`lost_at`** rows deferred from Phase A), §5 (state machine), and §7 steps 1–2.

**B1 scope (design §13):**
- the supervisor task owning state, `sent`, `highest`, D and `lost_at`;
- the states `Starting` / `Serving` / `ServingDisconnected` / `Disabled` / `Recovering` / `Revoked` / `UnknownIdentity`;
- the funnel gate in `device_tunnel::handle_stream` (state + `now < D`), and refusing new QUIC connections and TLS sockets;
- stopping the reconnect loop in the terminal states;
- the capability assertion as a fail-closed check;
- tests T3–T8, T15, T16, T19–T28 (connector side).

**Build on these Phase A pieces:** `connector/src/authorization.rs` (`AckTracker`, `stamp_sends`, `observe_controller_msg`, `observe_status`, `TerminalSignal`). `AckTracker.latest_accepted_sent_at()` is the anchor for D = sent + H.

**Phase A acceptance items that only B1 can close:**
- the "never serves" half of T9/T14 (Phase A shows only that no ack is accepted without the contract);
- the deadline rows of the ack rules (D, `lost_at`).

**Later phases (unchanged blockers):**

| Phase | Needs |
|---|---|
| M (measurements) | a Phase A lab build |
| B2 (schedule P, deadline expiry, fail-closed steps 3–6, tokio `test-util`) | M results (O6/O7), plus O5 and O8 |
| C (client `ACLSnapshot.revoked_connectors`) | O11 |
| D (relay heartbeat fields) | O12 |
| E (shield peer-list pruning) | O13 |
| F (cert history / tombstones) | schema approval (O14) |

## Open items surfaced by Phase A

- **Relay-placement UPSERT:** it still runs after a health-UPDATE DB error (unchanged). Only the ACL push and the ack are skipped.
- **Interceptor:** a workspace-CA load failure is still `Unauthenticated`, unmarked (non-terminal; optional per §4.5).
- **Design doc Phase A row:** it lists the client revoked list and the relay heartbeat field. They stay in C and D; the design doc has not been updated to say so.
- **Test seam:** `EnrollmentHandler.lookupOverride` exists only to force lookup failures in tests (nil in production).

## Environment notes for resuming

- **Proto generation:** `buf generate --exclude-path .worktrees`. The untracked `.worktrees/s20-live` worktree holds duplicate protos. `controller/gen/go/proto/connector` is gitignored and was stale at `12e43ac`; regenerate before building.
- **Controller DB tests:** `ENROLLMENT_TEST_DATABASE_URL=$DATABASE_URL` (from `controller/.env`); Postgres container `ztna_postgres`.
- **Cross-language test:** `scripts/xlang-authz-markers.sh` builds `controller/internal/connector/xlangharness` (build tag `xlangharness`) and runs `connector/tests/xlang_authz_markers.rs` (`#[ignore]` in a plain `cargo test`).
- **staticcheck:** the local binary is unusable (built with Go 1.25, toolchain 1.26).
- **tokio test-util:** the connector has no `test-util` feature (no paused clock) until B2 adds it.
- **Build artifacts were cleaned** on 2026-10-15 (`client/target`, `connector/target`, worktree targets, `admin/dist`, `operator/dist`). The first Rust build will be a full rebuild.
- **Not in git, on purpose:** `.localdev/s20-relay1/` (relay private key + cert) and `.worktrees/s20-live/`.
