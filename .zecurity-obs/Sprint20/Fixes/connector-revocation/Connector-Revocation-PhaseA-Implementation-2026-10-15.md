---
title: Connector Revocation — Phase A Implementation Record
date: 2026-10-15
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (revision 3)
branch: feat/sprint20-m1-phase2
base: 12e43ac (uncommitted working tree; nothing committed)
status: PHASE A IMPLEMENTED (protocol + controller + connector plumbing) — the intermediate is superseded: B1/B2 made the connector fail-closed (2026-10-21) and phases C–F added client/relay/shield enforcement (end-to-end live PASS 2026-10-23).
---

# Phase A — Controller ↔ Connector Authorization Protocol

Phase A sets up the protocol contract and the controller-side behaviour. The
connector **does not fail closed yet**: it still serves without an
acknowledgement, exactly as before. The serving gate, the state machine and
the deadline belong to Phases B1/B2. Don't use a Phase A build for live
revocation acceptance.

## 1. Protocol (`proto/connector/v1/connector.proto`)

Additive changes only. Field numbers were checked against the proto at `12e43ac`; reserved field 16 is untouched.

| Element | Field | Meaning |
|---|---|---|
| `ConnectorHealthReport.heartbeat_seq` | 9 (uint64) | ≥ 1 on every authorization-bearing report. Process-global, starts at 1, never reset by a reconnect or a renewal switch. 0 means the sender lacks the contract. |
| `ConnectorControlMessage.authorization_ack` | 19 → `AuthorizationAck{uint64 heartbeat_seq = 1}` | acknowledges that seq |
| `ConnectorControlMessage.connector_revoked` | 20 → `ConnectorRevoked{}` | terminal administrative revocation |
| `AgentPolicy.authorization_acks` | 2 (bool) | capability assertion and diagnostic only; never grants anything |

Generation uses `buf generate --exclude-path .worktrees`. The exclusion is needed because the untracked `.worktrees/s20-live` checkout contains a duplicate proto tree, which buf would otherwise include in the module. The tracked generated files (client, relay) regenerate byte-identical. The connector's generated Go code is gitignored (CI regenerates it). Rust code is generated in `build.rs`; only the connector compiles `connector.proto`. `buf breaking --against .git#ref=HEAD` passes.

**Structured marker (D10):** gRPC trailer `zecurity-connector-state`, with value `revoked` (REVOKED) or `unknown` (UNKNOWN_CONNECTOR). It is attached only after a **successful** authoritative lookup.

## 2. Controller

Files: `controller/internal/connector/authorization.go` (new), `control_stream.go`, `enrollment.go` (test-seam field), `graph/resolvers/connector.resolvers.go`.

**Ack gating (`handleConnectorHealth`).** `AuthorizationAck{seq}` is enqueued last, and only when all of these hold:
1. `seq ≥ 1`;
2. the revocation-guarded UPDATE returned a row;
3. the required ACL push for this report returned success, meaning it was enqueued ahead of the ack on the same FIFO stream (D9). If the version is already current, no push is required.

No ack is sent for any of the following:

| Case | Result |
|---|---|
| `seq = 0` | `FailedPrecondition`, no marker, no DB write; the stream closes |
| Guarded UPDATE `ErrNoRows` | authoritative follow-up SELECT (`id` + `trust_domain`): revoked → `PermissionDenied` + `revoked`; absent → `NotFound` + `unknown`; lookup failed, or the row is inconsistently still authorized → `Unavailable`, no marker |
| Other DB error on the UPDATE | the stream stays open; no ACL push, no ack; the next report retries (relay-placement handling is unchanged) |
| ACL push failed (compile error or full mailbox) | no ack |
| The stream already carries `ConnectorRevoked` | no ack (`authMu` serialises the ack with revocation) |

**Open (`Control`):** the three-way split.
- absent → `NotFound` + `unknown`
- revoked → `PermissionDenied` + `revoked`
- any query error → `Unavailable`, no marker. This fixes the old `Scan error → NotFound "connector not found: <db error>"`.

**Activation (`activateStream`):** `ErrNoRows` gets the same follow-up classification. A deleted row is no longer mislabelled "revoked". Other DB errors are still logged with the stream registered (unchanged); that confers no authorization, since only an ack does.

**Capability:** `AgentPolicy` is always produced, with `authorization_acks = true`. The shield window is sent as 0 ("keep default") when it isn't configured. Previously the whole message was suppressed when the window was ≤ 0. It is now the **first** message enqueued on every stream, before the initial Ping.

**`ConnectorRevoked` delivery:**
- `ConnectorRegistry.live` tracks **every** activated, not-yet-retired stream per connector, including a stream superseded by a make-before-break renewal that the connector may still be reading.
- `RevokeConnector` calls `SignalRevoked(id)` right after the revoking UPDATE commits, before the notifies.
- `SignalRevoked` holds `lifecycleMu`, which makes it atomic with activation (guarded UPDATE + registration). So every stream either receives `ConnectorRevoked`, or was activated after the commit and is refused with the `revoked` marker.
- The event goes into a one-slot `terminal` mailbox that the writer drains **before** `outbound`, so a full outbound queue can't drop it.
- After sending it, the writer stops. The handler (Recv now runs in its own goroutine) ends the stream with `PermissionDenied` + `revoked`.

**Residual (documented, inherent):** an ack enqueued before the revoke committed may precede `ConnectorRevoked` on the wire. The connector then sees ack → revoked, and the terminal signal wins (§5.4). No ack is ever enqueued after `ConnectorRevoked` on a stream.

## 3. Connector (protocol plumbing only)

Files: `connector/src/authorization.rs` (new), `control_stream.rs`, `lib.rs`.

**`AckTracker`:** owned by `run_control_stream`'s outer loop, shared across all streams.
- `next_seq()` is used for periodic ticks and for the initial report of a new, non-renewal stream.
- `seq_for_replacement()` re-sends the outstanding seq on a renewal replacement stream.

**Send time:**
- `stamp_sends` wraps each stream's outbound and records `sent[seq]` when tonic's encoder **pulls** the message.
- hyper only polls the request body once h2 has granted send capacity, and the frame then goes straight to h2 `send_data`. Messages still in the mailbox are not "sent".
- The first send wins: a re-send on a replacement stream never moves the time later.
- The h2 connection task → socket → kernel gap can't be observed; the stamp comes before it, so it is conservative.

**Ack acceptance** (rows implementable without the Phase B state machine): a terminal signal not yet seen; the stream asserted the capability; `seq ≥ 1`; `seq` > highest accepted (rejects duplicates and out-of-order); `seq` ∈ `sent` (rejects unknown, mismatched, forged or never-sent seqs); `now < sent[seq] + H`. Every rejection is logged with its reason.

**Terminal signals:** these are recognised and recorded (first one wins), with structured `authz_event` logs:
- `ConnectorRevoked`
- the `revoked` / `unknown` markers on a Control() refusal at open (`open_refused`) and on a mid-stream termination

Classification uses the marker only; message text is never read. Any unmarked status is `Unmarked(code)`, which is recoverable.

## 4. Tests

- **Controller** (`authorization_test.go`, DB-backed throwaway databases): T1/T7 (ack strictly after the ACL push; withheld on ACL failure or a full mailbox), T2/T3/T20 (revoked vs unknown at open, mid-stream and activation), T4/T21/T22 (DB failure → Unavailable unmarked, at open, in the health follow-up and in the activation follow-up; health-UPDATE DB error → no push, no ack), seq=0, T8 (capability first, also when no window is configured), T5 (live delivery; renewal overlap reaches the superseded and the new stream; revoke vs activation, deterministic and randomised; revoke vs heartbeats; full mailbox), T10 (renewal overlap acks both streams, no marker). Existing tests were updated for the new signatures and for the intended AgentPolicy behaviour change.
- **Connector:** 14 unit tests in `authorization.rs` (one per acceptance rule, stamping at pull, classification of marker vs text) and 6 session tests in `control_stream.rs` (renewal reuses seq + send time; capability per stream; revoked on the old stream while the replacement is pending; revoked on the new stream after the switch; replacement refused with the revoked marker; unmarked statuses never terminal; unknown ≠ revoked).
- **Cross-language** (`scripts/xlang-authz-markers.sh`): the production grpc-go `Control` handler (`xlangharness`, build tag `xlangharness`, throwaway DB) against the production tonic client and classifier. It proves end to end that the trailer marker survives a trailers-only refusal (revoked, unknown) and a mid-stream termination (live revoke, deletion), that seq=0 and DB failure arrive unmarked, that AgentPolicy arrives first, that an ack is accepted by the production tracker, and that `ConnectorRevoked` is delivered live. The harness substitutes plaintext h2c plus header-injected SPIFFE identity for mTLS. In a plain `cargo test` it shows as *ignored*, never as passed.
- **Mutation checks:** 11 controller and 11 connector enforcement lines were disabled one at a time; every mutant was killed by an assertion failure, with no build failures.

## 5. Not implemented in Phase A (deferred, unchanged)

- Connector serving state machine, the `Starting` gate and the data-plane funnel check (**B1**). Today the connector still serves without an ack.
- The deadline D, the `lost_at` rule, stopping the reconnect loop in the terminal states, and terminal persistence (O9) (**B1/B2**).
- The request period P, the response budget B and the measurements (**M/B2**).
- Fail-closed steps 3–6 (cancel-all, shield channels, cache clear) (**B2**).
- `ACLSnapshot.revoked_connectors` (**C**); relay heartbeat fields (**D**); shield peer-list pruning (**E**); certificate history and tombstones (**F**).
- **T9/T14 "never serves":** only the protocol half is shown (no ack is accepted without the contract). The "never serves" half needs B1.

## 6. Open items surfaced by Phase A

- **The relay-placement UPSERT still runs after a health-UPDATE DB error** (unchanged behaviour). Only the ACL push and the ack are skipped.
- **The design doc's Phase A row lists the client revoked list and the relay heartbeat field.** Per the Phase A instruction they stay in C/D. The design doc itself was not edited.
- **The Workspace-CA load failure in the interceptor is still `Unauthenticated`** (unmarked, so non-terminal; optional per §4.5).
