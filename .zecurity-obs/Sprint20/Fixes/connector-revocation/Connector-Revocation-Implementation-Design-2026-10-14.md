---
title: Connector Revocation Enforcement — Implementation Design and Phased Plan
date: 2026-10-14
type: design (documentation only)
status: PROPOSED (revision 5, 2026-10-21) — Phase A done; Phase B1 IMPLEMENTED 2026-10-21 (`da5b12f`, unit-tested intermediate, not for live deployment before B2); Phase M done; Phase B2 DONE 2026-10-21 (unit + live L-1…L-8 PASS); other phases NOT AUTHORIZED
branch: feat/sprint20-m1-phase2
revision: 38e38c62044dd869e57bc299e7d3e1dbf4ad10a9 (revisions 1–2); 12e43ac (revision 3)
based_on:
  - Connector-Revocation-Enforcement-Feasibility-2026-10-14.md
  - Connector-Identity-Revocation-Architecture-Investigation-2026-10-13.md
  - Connector-Revocation-Remaining-Live-Tests-2026-10-13.md
  - Connector-Revocation-Live-Verification-2026-10-12.md
---

# Connector Revocation Enforcement — Implementation Design (2026-10-14, rev. 2026-10-15)

**Evidence and decision labels:**

| Label | Meaning |
|---|---|
| **CONFIRMED BY SOURCE** | read at `38e38c6` (path + symbol) |
| **CONFIRMED BY TEST** | asserted by a named existing test (not run here) |
| **HISTORICAL LIVE EVIDENCE** | from earlier live records |
| **INFERRED** | follows from source but nothing asserts it |
| **ACCEPTED** | decided by the user |
| **RECOMMENDED, AWAITING APPROVAL** | this document's recommendation; not approved |
| **OPEN** | undecided |
| **BLOCKED ON MEASUREMENT** | needs the §6.6 measurements before it can be decided |
| **BLOCKED ON SCHEMA APPROVAL** | needs a separately approved migration |
| **NOT APPLICABLE (UNRELEASED)** | moot because Zecurity has no released versions |

**Revision history:**

**Revision 1 (2026-10-15):**
- D1 accepted (one-interval freshness).

**Revision 2 (2026-10-15):**
- O1 accepted: the deadline is anchored to the send time of the acknowledged request.
- Development-only context (D8): no released versions exist, so no legacy compatibility is required.
- Legacy-serving fallbacks were removed.
- Fail-closed was redefined step by step.
- Decision statuses were re-classified.

**Revision 3 (2026-10-15):**
- O2 accepted (D10): a machine-readable gRPC status marker carries the authoritative connector state; human-readable message text is never used.
- O3 accepted (D11): an unknown/deleted connector identity never serves; it enters its own terminal non-serving state (`UnknownIdentity`), distinct from `Revoked`.
- §4.5 error classification, §5 state machine, §7, §13 Phase A, §14 tests (T19–T28), §16 and §17 updated accordingly. Phase A is unblocked **with respect to O2/O3 only**.
- New source facts verified at `12e43ac`: `activateStream` maps `ErrNoRows` to `PermissionDenied "connector is revoked"` (`control_stream.go:621-622`) and only logs other DB errors (`:624-625`); the connector certificate interceptor maps a workspace-CA load failure to `Unauthenticated` (`control_stream.go:1008-1011`, `spiffe.go:256-260`); the relay path already separates `Unavailable` (state unavailable) from `PermissionDenied` (revoked) (`:1018-1024`).

**Revision 4 (2026-10-20):**
- O9 accepted (D12): `Revoked`/`UnknownIdentity` are **not persisted** across restart, and the connector **idles** in a terminal state (stays alive with the data plane closed and the reconnect loop stopped) instead of exiting. Reason for idle: the Docker deployment uses `restart: unless-stopped` (`connector/docker-compose.yml:17`), which restarts after any exit, including exit 0; the systemd unit uses `Restart=on-failure` (`connector/systemd/zecurity-connector.service:24`). Exiting would cause a restart loop against the controller under Docker. Reason for no persistence: safety doesn't depend on it (§5.2), and a persisted flag would need clearing rules on re-enrollment (O10, undesigned).
- §5.2, §7, §16 and §17 updated. Phase B1 acceptance list: [B1 acceptance](Connector-Revocation-PhaseB1-Acceptance-2026-10-20.md).

**Revision 5 (2026-10-21):**
- Phase M measured B, the controller cost per request, renewal and reconnect latency ([results](Connector-Revocation-PhaseM-Results-2026-10-21.md)).
- Accepted from those results:
  - **O5 → D13:** backoff reset after an accepted ack; 1 s → 5 s cap, ±20 % jitter while not serving;
  - **O7 → D14:** P = 4.5 s, B budget 1 s;
  - **O8 → D15:** `Disabled` keeps connections but carries nothing; shield streams close after 30 s;
  - **O6 → D16:** full health report every P now; a lightweight authorization request is a later improvement.
- D17: the Phase A contract details (§4.2, §4.4, §4.6) are approved as built. D18: the renewal head-of-line fix is in B2's scope.
- The timing values are fixed at build time (`connector/.env` → `connector/build.rs` → `crate::authz_timing`); they are not runtime configuration.
- B2 live-lab substitutions: shield host **udaya**, relay **manojinkyank**, client with one resource or a `down`/`up` per resource change (§15).
- §6.3, §6.6, §7, §13, §16 and §17 updated.

Raw evidence isn't repeated. See:
- [Feasibility report](Connector-Revocation-Enforcement-Feasibility-2026-10-14.md) ("FR");
- [Identity investigation](Connector-Identity-Revocation-Architecture-Investigation-2026-10-13.md);
- [10-13 live record](Connector-Revocation-Remaining-Live-Tests-2026-10-13.md);
- [10-12 live record](Connector-Revocation-Live-Verification-2026-10-12.md).

---

## 1. Purpose and scope

This document turns the feasibility findings into a design and a phased plan that a reviewer can approve before any code is written. It covers:
- the controller ↔ connector authorization contract;
- the connector serving state machine;
- the deadline and scheduling semantics;
- data-plane fail-closed;
- client, relay and shield enforcement;
- the CRL/identity split;
- certificate-history decisions;
- phases, tests and development rollout.

**Out of scope:**
- implementation of any kind;
- PENDING-18 (established-connection recovery and migration);
- Fix 01 Q3 (the client's transient-absence policy). Revocation enforcement here uses an **explicit** revoked signal, never snapshot absence, so Q3 is unaffected.

**Revision and working tree:**
- Revisions 1–2: branch `feat/sprint20-m1-phase2`, HEAD `38e38c6`.
- **Revision 3:** HEAD `12e43ac` (same branch, = `origin/fixed-pendings` after PR #119). `git diff 38e38c6 12e43ac -- controller connector proto` is empty, so every controller/connector/proto citation below is unchanged; line numbers were re-checked in the restored working tree. The `client/`, `connector/` and `relay/` deletions described next were restored from the index on 2026-10-15; the working tree now has only untracked `.localdev/`, `.worktrees/` and `.zecurity-obs/Sprint20/live-lab-scripts/`.
- **Working-tree note (historical, recorded at the start of revision 2).** The checked-out tree no longer contains the `client/`, `connector/` and `relay/` directories: `git status` shows 106 tracked files deleted (36 client, 50 connector, 20 relay). A new untracked `.worktrees/` holds a worktree `s20-live` at `46ab87d`. Neither change was made by this design work.
- The earlier uncommitted F-1 edits to `client/src/daemon.rs` / `daemon_tests.rs` are no longer present in the working tree. No commit or worktree checked contains them.
- Every source reference in this revision was therefore verified against **`HEAD` (`git show 38e38c6:<path>`)**, not the working tree. `client/src/daemon.rs` line numbers below are HEAD numbers.

### 1.1 Development-only context (ACCEPTED, D8)

Zecurity is unreleased, with no deployed connector, client, relay or shield versions to stay compatible with. Consequences for this design:
1. There is **no legacy serving mode**. A connector that can't establish the authorization contract with the controller never serves data-plane traffic.
2. A missing `AuthorizationAck`, a missing capability, an unsupported protocol version or an incompatible controller **never** counts as authorization.
3. Every component that participates in serving must implement its part of the contract. Mixed versions are not a supported configuration; they must fail closed, and tests must show it (T14).
4. Rollout is a **coordinated rebuild** of the affected components (§15). §15 states which components really need protocol or code changes.
5. Protocol versioning or capability fields may remain **only** as fail-closed safety assertions and diagnostics, never to permit insecure operation.
6. Security checks are not removed because the product is unreleased.

## 2. Accepted requirements

| Ref | Requirement |
|---|---|
| Q1 | A connector stops serving if it can't obtain a fresh authorization by the deadline. **ACCEPTED D1** (§6.1): freshness ≤ one interval H, anchored to the send time of the latest acknowledged request; no grace |
| Q2 | An explicit authoritative revocation disables the connector immediately (terminal) |
| Q3r | A revoked connector doesn't resume because it reconnects |
| Q4 | Client, relay and shield enforce connector revocation for new and established connections |
| Q5 | Make-before-break renewal continues without unnecessary disruption |
| Q6 | Administrative identity revocation and certificate-level CRL revocation stay distinct but compatible |

These are R1–R12 of FR §3, condensed. The FR wording stays authoritative.

## 3. Current architecture (CONFIRMED BY SOURCE at `38e38c6` unless labelled)

| Area | Fact | Where |
|---|---|---|
| Heartbeat cadence | `HEALTH_INTERVAL_SECS = 15`, a per-session `tokio::time::interval` (default `MissedTickBehavior::Burst`, tokio 1.51.1, `interval.rs:366-369`), first tick consumed; the initial health is sent at stream open | `connector/src/control_stream.rs:38`, `run_session:426-428`, `establish_stream:187-204` |
| "Sending" a heartbeat | an `mpsc` enqueue (64 slots), no delivery proof | `send_msg:115-122`, `open_stream:235` |
| Ping/Pong | `shield.v1.Ping/Pong{timestamp_unix}` (`proto/shield/v1/shield.proto:190-196`). The controller sends **one** Ping per stream at open, after `activateStream` (`control_stream.go:511-520`). Pong is only logged (`:582-583`) | — |
| Healthy heartbeat | `handleConnectorHealth` (`control_stream.go:749`) runs a guarded UPDATE (`:754-778`). `ErrNoRows` → `Control` returns `PermissionDenied "connector revoked"` (`:566-568`); other DB errors are only logged (`:785-786`). Then notify, ReEnroll (`:804-806`), relay placement, `pushACLSnapshot` (`:837`, version-gated `:910-913`). **Nothing is sent when the ACL is current and no renewal is due** | — |
| Ordering | one mailbox + one writer goroutine per stream (`runWriter:222-234`) → FIFO; the writer exits on ctx cancel **without flushing** | — |
| Revoked at open | `Control:433-441` → `PermissionDenied "connector is revoked"`; **any** `Scan` error, including a DB/infrastructure failure, → `NotFound "connector not found: …"` (`:436-437`). This conflates an infrastructure failure with an authoritatively absent row | — |
| Activation | `activateStream` (`control_stream.go:604-628`) runs a second guarded UPDATE. `ErrNoRows` → `PermissionDenied "connector is revoked"` (`:621-622`), even when the row was **deleted** rather than revoked; any other DB error is only logged and the stream is still registered (`:624-627`) | — |
| Cert interceptor | `verifyConnectorCertificate` (`spiffe.go:256`); a workspace-CA load failure (`:258-259`) or chain failure → `Unauthenticated` (`control_stream.go:1008-1011`). Relay precedent in the same interceptor: `Unavailable` when revocation state is unavailable, `PermissionDenied` when revoked (`:1018-1024`) | — |
| Connector error handling | `connector/src/control_stream.rs` never inspects a gRPC code, message or metadata; every failure is flattened into `anyhow` and retried (`:443`, `open_stream:240`). No text matching exists today, and none may be introduced | — |
| Revoke mutation | `RevokeConnector` = UPDATE + two notifies (`graph/resolvers/connector.resolvers.go:113-155`); nothing is sent to the stream. Resolvers hold `ConnectorRegistry` (`resolver.go:26`); `get` is unexported (`control_stream.go:256`) | — |
| `AgentPolicy` | field 18, enqueued at every open (`control_stream.go:522-523`), **but** `agentPolicyMessage` returns nil when the shield renewal window ≤ 0 (`:184-188`) | — |
| Connector retry | `run_control_stream:76-110`, backoff 2 → 60 s forever; errors flattened to `anyhow` (`:443`, `open_stream:240`) | — |
| Proto room | `ConnectorControlMessage` oneof uses 1–15, 17, 18 (16 reserved) → 19+ free. `ConnectorHealthReport` 1–8 → 9+. `ACLSnapshot` 1–4, 6, 9, 10 (5/7/8 reserved) → 11+. `PeerConnectorList` 1 → 2+. Relay `HeartbeatRequest` 1–7, `HeartbeatResponse` 1–2 | `proto/connector/v1/connector.proto:69-110,150-164`; `proto/client/v1/client.proto:206-235`; `proto/shield/v1/shield.proto:137-140`; `proto/relay/v1/relay.proto:64-77` |
| Error tooling | tonic 0.14 `Status::code()/details()/metadata()` exist (`tonic-0.14.5/src/status.rs:512,522,527`); the connector has **no** `tonic-types` dependency (`connector/Cargo.toml:11-13`). The controller has `genproto/googleapis/rpc` only as an indirect dependency (`controller/go.mod:100`) | — |
| Data-plane entry | QUIC `:9092` (`quic_listener.rs:58`, endpoint not retained), TLS `:9092` (`device_tunnel.rs:61`, call `:119`), relay-routed (`relay_handler.rs:156,187`): **all** reach `device_tunnel::handle_stream:151` | — |
| Sessions | every admitted session (connector and shield routes) registers a `CancellationToken` (`device_tunnel.rs:250`; `session_registry.rs:51-70`); copy paths `select!` on it (`device_tunnel.rs:352-363,480-490`); only per-key `cancel_all` exists (`session_registry.rs:103`) | — |
| Shield channels | `ShieldRegistry::control` (`agent_server.rs:728`) registers senders + `tunnel_hub` (`:740-750`); teardown only when the shield's stream ends (`:905-915`); shield sessions multiplexed in it (`agent_tunnel.rs:107`) | — |
| Peer list | `build_peer_connector_list` (`agent_server.rs:206-223`) from `peers_of_connector(self)`; empty → not sent; the shield ignores empty lists and persists the list (`shield/src/control_stream.rs:397-440`) | — |
| Shield failover | only on connection failure (`shield/src/control_stream.rs` `run_once:126-142`; `reconnect_loop:99-123`, 5 → 60 s) | — |
| Client flows | `relay_tcp_to_quic` (`client/src/net_stack.rs:1064`): open failures and handshake I/O errors → `continue`; `SHIELD_NOT_ATTACHED` → `continue`; any other `ok:false` → return (`:1166-1167`). Transport-failure Io kinds (`:159-173`); quinn `Reset` → ConnectionReset (`quinn-0.11.9/src/recv_stream.rs:576`) | — |
| Client ownership | `ClientTransport` has no connector id (`transport.rs:81-87`); `ConnCoords` has `connector_id`/`connector_spiffe` (`daemon.rs:5524`); `build_transport_from_coords` (`daemon.rs:5773`) builds per-connector pools (prefix verifier on direct); `ActiveRelay.close_tag` only (`net_stack.rs:232-251`); `ResourceCmd` channel (`net_stack.rs:733`) | — |
| Relay | `RelayState` → `ConnectorEntry{registration_id, connection, spiffe_id, trust_domain}` (`relay/src/state.rs:7-60`); client lookups `open_bi` on the connector's registered connection (`session.rs:224-266`); CRL checked only at accept (`listener.rs:156-173`); unary heartbeat, 30 s default (`controller/internal/relay/heartbeat.go:24`) | — |
| Controller liveness | watcher 30 s / threshold 90 s (`cmd/server/main.go:194-195`) | — |
| Cert history | connectors: one overwritten `cert_serial` (`enrollment.go:267`); relays: `relay_certificates` (`migrations/027_relay_certificates.sql:8,19`); latest migration 037 | — |
| Status values | `connectors.status ∈ {pending, active, disconnected, revoked}` (`migrations/002_connector_schema.sql:90`); deletion removes the row | — |
| Recovery | no un-revoke; enrollment requires `pending` (`enrollment.go:109`) | — |
| Update timers | install scripts install `zecurity-connector-update.timer` (`scripts/connector-local-install.sh:111`), which pulls release binaries (lab history: it has overridden branch builds) | — |

## 4. Heartbeat authorization contract

### 4.1 Where the controller can attest authorization (CONFIRMED BY SOURCE)

- **The guarded statement.** The guarded UPDATE in `handleConnectorHealth` (`:754-778`) is a per-heartbeat authoritative check: it updates only `WHERE … status NOT IN ('revoked','deleted') AND revoked_at IS NULL`.
- **What `ErrNoRows` (`:779`) covers.** It conflates "row missing" (deleted/unknown) with "guard refused" (revoked); the statement can't tell them apart. Any other error is a DB/internal failure. The same conflation exists in `activateStream` (`:621-622`).
- **The `'deleted'` literal never matches**: `connectors.status` has no such value.
- **Ordering.** FIFO per stream: a message enqueued after `pushACLSnapshot` (`:837`) is delivered after that ACL.

### 4.2 Contract (D2 ACCEPTED direction; field details ACCEPTED D17)

**Messages (additive; all numbers free at `38e38c6`):**
- `ConnectorHealthReport.heartbeat_seq = 9` (uint64; ≥ 1 for every authorization-bearing report);
- `ConnectorControlMessage.authorization_ack = 19` → `AuthorizationAck{uint64 heartbeat_seq}`;
- `ConnectorControlMessage.connector_revoked = 20` → `ConnectorRevoked{}`;
- `AgentPolicy.authorization_acks = 2` (bool): see §4.6 for whether to keep it.

**Rejected:** reusing `Ping`. It can't be correlated with a request, and it overloads an existing message.

### 4.3 Controller rules

1. Send `AuthorizationAck{seq}` **only** when all three hold:
   - the guarded UPDATE returned a row (the connector is authorized now);
   - `heartbeat_seq ≥ 1`;
   - any required ACL push for this heartbeat has been enqueued.

   The ack is enqueued last for that heartbeat. Note that "ACL push completes" means **enqueued ahead of the ack** on the same FIFO stream; the controller has no delivery receipt (INFERRED).
2. Never ack on `ErrNoRows`, on any DB error, on `heartbeat_seq = 0`, or for non-health messages. A DB error in `activateStream` (today only logged, `:624-625`) therefore never yields serving: no ack can follow until a health report's guarded UPDATE succeeds.
3. A health report with `heartbeat_seq = 0` comes from a connector without the contract. The controller logs it and doesn't ack (RECOMMENDED: also reject the stream with an unmarked `FailedPrecondition`, so a stale build in a dev lab is obvious).
4. There is no open-time ack. The connector's initial health on every open is acked like any other report.

### 4.4 Acknowledgement acceptance (connector)

The connector records `sent[seq] = Instant` when it **enqueues** the report (monotonic `tokio::time::Instant`; enqueue precedes the wire send, so the anchor is conservative). Seqs are process-global, start at 1 and never reset per stream.

Let `D` be the current deadline (undefined in `Starting` and in the terminal states), and `lost_at` the instant authorization was last lost (process start or entry to `Disabled`; **not** reset on `Disabled → Recovering`, per §5.2 — confirmed by the user 2026-10-21).

Ack k is **accepted** only if **all** of these hold:

| Condition | Rejects |
|---|---|
| state ∉ {`Revoked`, `UnknownIdentity`} | anything after a terminal state |
| k ≥ 1 and k ∈ `sent` | zero, unknown, forged or already pruned |
| k > highest accepted seq | duplicates, out-of-order older acks |
| `now < sent[k] + H` | acks whose request is already older than H (they would confer nothing) |
| if the state is `Serving`/`ServingDisconnected`: `now < D` | a late ack arriving after the deadline expired |
| if the state is `Starting`/`Disabled`/`Recovering`: `sent[k] ≥ lost_at` | acks for requests sent before authorization was lost (late acks can't restore it) |

On acceptance: `D = sent[k] + H` (monotonic; it can only increase because k > highest accepted), `highest = k`, and drop `sent` entries ≤ k. Every rejection is logged with its reason.

### 4.5 Terminal signals and error classification (D10 = O2 ACCEPTED, D11 = O3 ACCEPTED)

**Security rule (ACCEPTED D10).** Authoritative connector states are carried by a **structured, machine-readable gRPC status marker**. A gRPC status code alone can't distinguish every outcome (today `PermissionDenied` covers revoked, wrong role and deleted-at-activation; `NotFound` covers absent rows *and* DB failures, §3). The marker carries the authoritative reason. **Human-readable message text is never used to decide that a connector is revoked or unknown**, on either side.

**Mechanism (ACCEPTED, from the revision-2 recommendation):** trailer metadata on the error status, key `zecurity-connector-state`, values `revoked` (REVOKED) and `unknown` (UNKNOWN_CONNECTOR).
- Compatible with the source: Go sets stream trailers before returning the status; tonic 0.14 exposes `Status::metadata()` (§3), so no new dependency is needed. `google.rpc.ErrorInfo` details are no longer proposed.
- The final key/value spelling and casing are a **protocol implementation detail** fixed in Phase A; they are not an open question about whether a structured marker is required.
- Trailer propagation on a trailers-only error response is INFERRED and must be proven by T8 before the connector relies on it. If it can't be proven, Phase A must carry the same marker by another structured channel; text matching is never the fallback.
- The marker is only attached after a **successful** authoritative lookup. Unmarked statuses are never terminal, whatever their code.

**Two delivery cases:**
- **A. Connected connector.** `RevokeConnector` enqueues `ConnectorRevoked{}` on the live registry stream after the UPDATE commits (needs an exported registry accessor). Delivery is about one RTT (INFERRED). The connector enters `Revoked` immediately, without waiting for D. A connected connector whose row is **deleted** learns it at its next authorization request: the guarded UPDATE returns `ErrNoRows`, the follow-up SELECT finds no row, and the stream ends with the `unknown` marker. (Whether `DeleteConnector` should also close the live stream proactively is a Phase A implementation detail; O3 doesn't require it, and D1 bounds service either way.)
- **B. Disconnected connector.** No live event can be delivered. Authorization expires at D (D1) and the connector fails closed. When it reconnects, `Control`'s lookup returns the authoritative result: `revoked` or `unknown` marker.

**Outcome classes:**

| Class | Controller determination | Controller response (proposed) | Connector result |
|---|---|---|---|
| **A. REVOKED** | a successful lookup shows `status='revoked'` or `revoked_at` set. Mid-stream: health `ErrNoRows` followed by a successful SELECT that finds the revoked row | `PermissionDenied` + marker `revoked`; or `ConnectorRevoked{}` on a live stream | **`Revoked`** (terminal administrative revocation), immediately; fail-closed §7; reconnection can't restore serving |
| **B. UNKNOWN_CONNECTOR** | a successful lookup returns no row for the authenticated identity (`id` + `trust_domain`). Mid-stream: health `ErrNoRows` followed by a successful SELECT with no row. Activation: `activateStream` `ErrNoRows` followed by the same follow-up classification | `NotFound` + marker `unknown` | **`UnknownIdentity`** (terminal non-serving state for this process); fail-closed §7; distinct from `Revoked`; recovery only through an explicitly authorized re-enrollment or replacement (§5.1) |
| **C. Temporary / internal failure** | the lookup or follow-up SELECT **failed** (DB/pool/controller error), so the state is undetermined | `Unavailable`, **no marker** | authorization unavailable → no ack; retries allowed; serving ends at D (`Disabled`/`Recovering`) |
| **D. Transport failure** | no reliable response (reset, timeout, controller unreachable, tonic-generated status) | — | nothing inferred: no ack, no marker; D1 expiry applies; fail closed at D |
| **E. Successful authorization** | guarded UPDATE returned a row; required ACL push enqueued first (D9) | `AuthorizationAck{seq}` | only a valid, timely ack (§4.4) enables or refreshes serving |

**Other unmarked statuses (all recoverable, never terminal):**

| Condition | Response | Connector |
|---|---|---|
| Wrong role / trust domain (`:427`, `:1005`) | unchanged `PermissionDenied`, no marker | recoverable (misconfiguration); never serves without an ack |
| Expired certificate or failed chain verification | interceptor `Unauthenticated`, no marker | recoverable; closed until a fresh ack, which needs a valid cert; never confused with revocation |
| Workspace-CA load failure in the interceptor (`spiffe.go:258-259`) | `Unauthenticated` today, no marker. INFERRED: it would be more accurate as `Unavailable`, but because it carries no marker it is already non-terminal, so changing it is optional for O2/O3 | recoverable |
| `heartbeat_seq = 0` (contract missing) | logged, no ack (optional unmarked `FailedPrecondition`, §4.3) | n/a for a contract build |
| Unsupported protocol (no capability, no ack) | no ack ever arrives | stays closed; never serves (§4.6) |
| Make-before-break renewal | no error; acks continue on either stream (§5.4) | continuity preserved |

**Required error-handling corrections for Phase A (documented, not implemented):**
1. `Control:436-437`: split the `Scan` result three ways. `pgx.ErrNoRows` → `NotFound` + `unknown`. A row with revoked state → `PermissionDenied` + `revoked` (`:439-441` today, marker added). **Any other error → `Unavailable`, no marker.** A DB failure must never become `NotFound` or carry a terminal marker.
2. `handleConnectorHealth` `ErrNoRows` (`:779`) and the `Control` close path (`:566-568`): the guarded UPDATE can't tell deleted from revoked, so run an authoritative follow-up SELECT and map it to class A, B or C. If the follow-up fails → `Unavailable`, no marker.
3. `activateStream` `ErrNoRows` (`:621-622`): same follow-up classification; today it returns `PermissionDenied "connector is revoked"` even for a deleted row. Other DB errors (`:624-625`) need no marker; serving is impossible without an ack anyway (§4.3 rule 2).
4. Connector: classify only on `ConnectorRevoked`, the trailer marker and the absence of an ack. Do not inspect message text; an unmarked status of any code is class C or D.

**Rules:** only `ConnectorRevoked` or the `revoked` marker enters `Revoked`; only the `unknown` marker enters `UnknownIdentity`; an unmarked `PermissionDenied`/`NotFound` is never terminal; **a DB failure is never converted into an unknown or revoked result.**

### 4.6 Capability flag: still useful? (ACCEPTED D17: keep it as a fail-closed assertion)

**Without the flag**, a connector paired with a controller lacking the contract never receives acks. It stays `Starting`/`Disabled`, which is already fail-closed. So the flag is **not needed for security**.

**Recommendation: keep it as a fail-closed assertion and diagnostic.** An ack-capable controller **always** sends `AgentPolicy{authorization_acks: true}` at open: change `agentPolicyMessage` so the shield-window check no longer suppresses the whole message. FIFO puts it before the first ack.

**Connector behaviour with the flag:**
- **Flag absent or false** when the first non-policy message arrives on a stream: log `controller does not support the authorization contract`, stay closed and keep retrying.
- **Flag true:** normal operation.

The flag never grants anything; only accepted acks do.

**Simplification option:** drop the flag and rely on "no ack → closed". This loses only the explicit diagnostic.

**Compatibility claims:** none beyond the test-proven matrix in T14, which covers:
- new + new: enforced;
- any mixed pairing: no serving.

## 5. Connector serving state (D4 ACCEPTED)

### 5.1 States

No existing type represents serving state (CONFIRMED BY SOURCE); this is a new model.

| State | Meaning | Data plane | Kind |
|---|---|---|---|
| `Starting` (authorization pending) | no accepted ack since process start; every process starts here | closed | recoverable |
| `Serving` | `now < D`, control stream up | open | — |
| `ServingDisconnected` | stream down or reconnecting, `now < D` | open **only** until D; it can't outlive D (§6.3) | — |
| `Disabled` | `now ≥ D` without an accepted ack, or authorization unavailable (class C/D, §4.5), certificate expiry or renewal failure | closed (§7) | recoverable |
| `Recovering` | `Disabled` with a (re)established stream, awaiting an accepted ack for a request sent at or after `lost_at` | closed | recoverable |
| `Revoked` | `ConnectorRevoked` or the `revoked` marker (class A) | closed permanently for this identity | **terminal** (administrative revocation) |
| `UnknownIdentity` | the `unknown` marker (class B): the controller authoritatively has no record of this identity | closed for the rest of this process | **terminal** for the process (unknown/deleted identity); **not** a revocation |

- No transition leaves `Revoked` or `UnknownIdentity`.
- **Recovery after `Revoked`** is **OPEN (O10)**; today the only path is a new connector identity, and any future operation must first change the controller's authoritative state.
- **Recovery after `UnknownIdentity` (ACCEPTED D11):** only an explicitly authorized re-enrollment or replacement that establishes a valid connector identity and authoritative authorization. Routine reconnection, transport success or a still-valid certificate never reactivates it. The workflow itself is not designed here. Today, enrollment requires a `pending` row (`enrollment.go:109`), so a deleted connector needs a new connector record and token.

**Kept distinct (none of these conflate):**

| Situation | State |
|---|---|
| Administrative revocation | `Revoked` |
| Unknown/deleted identity | `UnknownIdentity` |
| DB failure at the controller (`Unavailable`) | `Disabled`/`Recovering` after D |
| Controller unavailable / transport error | `ServingDisconnected` until D, then `Disabled` |
| Certificate expiry / renewal failure | `ServingDisconnected` or `Disabled`; never terminal |

### 5.2 Transitions

| From | Event | To |
|---|---|---|
| Starting / Recovering / Disabled | ack accepted (§4.4) | Serving |
| Serving | stream ends / write fails / unmarked error | ServingDisconnected |
| ServingDisconnected | ack accepted (before D) | Serving |
| Serving / ServingDisconnected | `now ≥ D` | Disabled (`lost_at = D`) |
| Disabled | stream accepted (first controller message received) | Recovering (`lost_at` unchanged) |
| Recovering | stream ends / unmarked error | Disabled |
| any non-terminal | `ConnectorRevoked` / `revoked` marker | Revoked (immediately; fail-closed §7) |
| any non-terminal | `unknown` marker | UnknownIdentity (immediately; fail-closed §7) |
| Revoked | anything | Revoked (reconnect loop stops) |
| UnknownIdentity | anything, including a marker of another kind | UnknownIdentity (reconnect loop stops) |
| process restart | — | Starting (in every case) |

**Restart (ACCEPTED D11).** Terminal states are in-memory only and are **not persisted** (ACCEPTED D12 = O9). In a terminal state the process idles: it stays alive, the data plane stays closed and the reconnect loop is stopped; it does not exit. Protection **does not depend on that persistence**: every restart begins in `Starting`, and only the controller's authoritative response can open the data plane.
- A valid connector becomes `Serving` only after a fresh accepted ack.
- A revoked identity is rejected again by the controller (`revoked` marker) and returns to `Revoked`.
- An unknown identity gets the `unknown` marker again and returns to `UnknownIdentity`; it never serves in between.
- A DB failure remains a recoverable authorization failure (`Unavailable`), never a terminal state.

### 5.3 Guarantees

| # | Guarantee | Mechanism |
|---|---|---|
| 1 | No serving before initial authorization (also after restart) | start in `Starting`; admission requires `Serving`/`ServingDisconnected` **and** `now < D` (§6.3) |
| 2 | Only a valid, timely ack enables or refreshes; a missing or invalid ack never does | §4.4 |
| 3 | A reconnect doesn't authorize | stream events only move `Disabled → Recovering` |
| 4 | `revoked` → `Revoked` immediately, without waiting for D | §4.5 class A |
| 5 | `unknown` prevents serving and routine reactivation | §4.5 class B; `UnknownIdentity` is absorbing for the process; controller repeats `unknown` after restart |
| 6 | `Unavailable` doesn't mean revoked or deleted | class C is unmarked → recoverable states only |
| 7 | A revoked connector can't resume via reconnect | `Revoked` is absorbing; the controller refuses anyway (`Control:439-441`) |
| 8 | Expiry disables and invalidates stale authorization | `now ≥ D` → `Disabled`; acks for pre-expiry requests rejected (`sent[k] ≥ lost_at`) |
| 9 | Terminal ≠ temporary, and revoked ≠ unknown | separate states; only the explicit, matching signal reaches each terminal state |
| 10 | Reversing revocation needs an explicit recovery process | none defined (O10) |
| 11 | Renewal can't reopen service, and isn't mistaken for revocation | renewal emits no ack and no marker; only the controller's ack opens service |
| 12 | Events are serialized | single supervisor (§5.4) |
| 13 | No state bypasses D1; no extra grace | every open state is additionally bounded by `now < D` at admission; D1 unchanged |
| 14 | Existing sessions end on `Disabled`, `Revoked` and `UnknownIdentity` | §7 |

### 5.4 Serialization and races

- **Single supervisor.** One authorization supervisor task owns the state, `sent`, `highest`, `D`, `lost_at`, the request schedule and the expiry timer. Its inputs arrive over one `mpsc`:
  - `ReportEnqueued{seq, at}`;
  - `Ack{seq}`;
  - `Revoked`;
  - `UnknownIdentity`;
  - `StreamUp` / `StreamDown`;
  - `Capability{bool}`.

  The control-stream task stays the single owner of streams (CONFIRMED BY SOURCE `run_session`) and only forwards events.
- **Published state.** The supervisor publishes `{state, D}` through a `watch` / atomic snapshot; readers never mutate it.
- **Ordering:**
  - a terminal signal (`Revoked` or `UnknownIdentity`) before or after an `Ack` → the terminal state wins (rule 1 in §4.4); the first terminal signal processed is kept;
  - an expiry and an `Ack` racing → both are judged on `now` at processing against D;
  - a late ack is rejected.
- **Renewal (make-before-break):**
  - acks on the old stream count until the switch;
  - the switch (`run_session:480-487`) drops the old stream unread, so an in-flight ack can be lost;
  - the replacement stream's initial report **reuses the outstanding seq and its original `sent` time**, so either stream's ack satisfies it without extending anything.
  - If the switch takes longer than the margin, D expires and the connector disables: correct under D1, recorded as a measurement item (renewal switch latency, §6.6).

## 6. Deadline and scheduling

### 6.1 ACCEPTED D1 (with O1 accepted: send-time anchor)

> Authorization freshness ≤ **H** (one heartbeat interval, currently 15 s). The deadline is `D = sent[k] + H`, where k is the latest accepted acknowledgement's request. No grace interval. H is not changed by this design. The earlier interpretation allowing almost two intervals is **rejected**.

**Sequence:**
1. The connector enqueues an authorization-bearing health report with seq k and records `sent[k]` on the monotonic clock.
2. The controller runs the authoritative guarded check.
3. On success it enqueues any required ACL push, then `AuthorizationAck{k}`.
4. The ack identifies request k.
5. The connector accepts it only under §4.4: valid, outstanding, newer, and arriving before the applicable deadline.
6. `D = sent[k] + H`, from the send time, not the receipt time.
7. Duplicate, out-of-order, unknown, zero, revoked-state or otherwise invalid acks don't refresh anything.
8. `now ≥ D` → fail closed (§7).
9. Explicit revocation → `Revoked` immediately, without waiting for D; an authoritative `unknown` result → `UnknownIdentity` immediately. A DB or transport failure does neither: it only withholds the ack, and D1 expiry applies.
10. A late ack (arriving at or after D, or for a request sent before authorization was lost) can't restore authorization.

### 6.2 Today's schedule can't meet D1 (CONFIRMED BY SOURCE)

- Reports go out every H on a per-session ticker (`run_session:426-428`) that restarts on every stream.
- With `D = sent[k] + H`, the ack for request k+1 must be processed before `sent[k] + H`. With period H, that requires zero latency, so **every** cycle would miss.
- The controller acks nothing today.

### 6.3 Required scheduling (design; values ACCEPTED: D13, D14, D16)

- **Quantities:**
  - `H`: maximum freshness (accepted);
  - `B`: measured response budget (enqueue → ack processed);
  - `P`: request period, with **`P < H − B`**.

  **B is not selected; no number is proposed.**
- **Schedule ownership.** The supervisor owns the schedule, independent of individual streams, reconnects and renewal switches. The per-session health ticker no longer carries authorization.
- **Sending.** On each tick, a report is enqueued on the current stream. On a new or replacement stream it is enqueued immediately (reusing the outstanding seq during a renewal switch). Pipelining is allowed; each ack is judged on its own.
- **A send never moves D.** A failed or delayed response never moves D.
- **Stalls.** Use `MissedTickBehavior::Delay` (not tokio's default `Burst`). `sent[k]` is the actual enqueue instant, so a catch-up send after a stall carries its true, late timestamp. No outdated request can refresh authorization beyond its own `sent + H`.
- **Admission check independent of the scheduler.** The data-plane funnel (§7 step 2) compares `Instant::now()` with the published D on every new connection or stream, so **new traffic stops at D even if the supervisor task is starved** (INFERRED; test T2).
- **What the runtime guarantees and what it doesn't:**
  - **Guaranteed:** no new stream is admitted at or after D; no ack processed at or after D is accepted.
  - **Not guaranteed:** that already-established sessions are cancelled exactly at D. Cancellation runs when the supervisor's timer task is polled, so a starved runtime delays it by the starvation time. A late send can only cause an earlier fail-closed, never a later one.
  - This residual risk is stated, not hidden.
- **Reconnect backoff** doesn't affect D. Today's 60 s cap, which never resets after a successful reconnect, delays recovery from `Disabled` by up to 60 s (measured in Phase M R3). **ACCEPTED (D13):** reset after an accepted ack; while not serving 1 s → ×2 → 5 s cap, ±20 % jitter.
- **If measurements show B can't fit within H**, fail-closed stays mandatory. H isn't extended and no grace is added. The outcome is either a change elsewhere (e.g. faster controller path, O6) or reduced availability, recorded explicitly.

### 6.4 Trade-offs (recorded)

| Trade-off | Detail |
|---|---|
| A. Controller load | P < H − B means more reports per connector than today's one per 15 s. Each full report runs the guarded UPDATE, notifications, the ReEnroll check, the relay-placement upsert and the ACL compare (`handleConnectorHealth:749-840`). Measured about 3 ms per report, 2.3 ms of it DB (Phase M). **D16:** keep the full report at P; a lighter authorization-only request is a later improvement |
| B. Scheduler stalls | the starvation limit above; new-traffic refusal is stall-proof, session cancellation is not |
| C. Budget doesn't fit | fail closed stays mandatory; no extension |

### 6.5 Ordering of ack vs ACL push (ACCEPTED)

The ack is sent only after any required ACL push for that heartbeat (per the task). So ACL compile and push latency are part of B, and a cache-miss compile (`GetOrCompile`) lies on the ack path. The earlier "ack before ACL" idea is withdrawn.

### 6.6 Measurements required before the schedule is implemented (DONE 2026-10-21: [Phase M results](Connector-Revocation-PhaseM-Results-2026-10-21.md); WAN emulation R4 not run)

| Measure | Where |
|---|---|
| Ack round trip, enqueue → ack processed: p50/p99/max, lab and WAN-emulated | connector instrumentation |
| Controller handler latency for `handleConnectorHealth`, end to end | controller |
| Guarded DB UPDATE latency, separately, under load | controller |
| ACL compile (cache miss/hit) and push enqueue latency | controller |
| Mailbox queueing delay (128-slot outbound) under ACL fan-out and shield traffic | controller |
| Renewal stream-switch latency (`open_stream` preflight + channel + `control()` + initial ack) | connector |
| Reconnection latency after a transient TCP reset | connector |
| Runtime scheduling lag under data-plane load | connector (timer drift / tokio metrics) |
| Controller DB load at the candidate P per connector × expected connector count | controller + DB |

B, P and the O6 choice are recorded (D14, D16) with these results. The measured B was p99 ≤ 79 ms and max 83 ms on the LAN. Its tail is the renewal awaited inline in the connector's Control-stream loop (61–80 ms), plus controller DB spikes (≤ 41 ms).

## 7. Fail-closed (D5 ACCEPTED): what `Disabled`, `Revoked` and `UnknownIdentity` stop

Closing the control stream closes nothing on the data plane (CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE 10-13 R3-V). `Disabled` is recoverable; `Revoked` and `UnknownIdentity` are terminal. All three run the steps below; the differences are marked ("terminal" = both terminal states).

| Step | Existing mechanism (reusable) | Missing (source change) | Concurrency / lifecycle risk | Regression test |
|---|---|---|---|---|
| 1. Publish the closed state atomically | none | a supervisor-owned `watch`/atomic `{state, D}` | readers must see state and D together (one snapshot) | T2, T9 |
| 2. Refuse new client connections and streams at the transport level | single funnel `device_tunnel::handle_stream:151` for QUIC, TLS and relay-routed; `quinn::Incoming::refuse()` (`incoming.rs:44`) | funnel check of the state + `now < D`. Refuse by **stream reset / no `TunnelResponse`** (the client treats ConnectionReset as a transport failure → next connector + resync). Never send `ok:false`, which blocks fallback (`net_stack.rs:1166-1167`). Refuse new QUIC connections; drop new TLS sockets | a stream accepted just before step 1 and registered after step 3 → step 3 must re-check after registration (mirror the existing post-register `is_allowed` re-check, `device_tunnel.rs:252-258`) | T9, T2 |
| 3. Cancel all connector-routed and shield-routed sessions | `SessionRegistry` tokens on every session; both copy paths `select!` on them | `cancel_every_session()` (only per-key `cancel_all` exists) | the race above; cancellation timing depends on scheduling (§6.3) | T9 |
| 4. Close or disable shield-facing data-plane channels | `ShieldRegistry` per-shield senders; the shield's existing failover on connection loss | `close_all()` + refuse `control()` while closed. **Terminal states: close.** **`Disabled`: keep the streams open, carrying nothing, and close them after 30 s in `Disabled` (ACCEPTED D15)**; either way no shield traffic flows, because shield sessions start only from step-2-gated streams and existing ones are cancelled in step 3 | closing triggers shield failover to a peer (intended in the terminal states; possibly disruptive for short `Disabled` periods) | T13 |
| 5. Stop stale ACL/cache state reopening access | `PolicyCache` (memory only; starts `None`) | admission keyed on state + D, not cache presence; keep applying ACLs while closed (so re-enable starts with the ACL delivered just before the ack, FIFO); `PolicyCache::clear()` in the terminal states | an ACL arriving while closed must not open anything (it can't: admission ignores it) | T6, T7 |
| 6. Stop retries and listeners re-enabling serving | `run_control_stream` reconnect loop; listener accept loops | they only **read** the state; only the supervisor transitions; the reconnect loop stops in either terminal state; restart starts in `Starting` | none if writes are confined to the supervisor | T6, T7 |

**Other items:**
- Relay attachment: relay-routed streams are already covered by steps 2–3. **`Disabled` stays attached** (ACCEPTED D15).
- Existing client QUIC connections **stay open in `Disabled`**; new streams are reset and sessions cancelled (ACCEPTED D15). The terminal states close them.
- `Revoked`/`UnknownIdentity` are not persisted across restart, and the process idles rather than exits (ACCEPTED D12 = O9). Safety doesn't depend on persistence: a restarted process starts in `Starting` and the controller repeats the authoritative `revoked`/`unknown` result (§5.2).

**Failure handling:** a step that fails is logged and doesn't revert step 1. Fail-closed never depends on later steps succeeding.

## 8. Client enforcement (D6 ACCEPTED direction; details RECOMMENDED, AWAITING APPROVAL)

**Interface:** `ACLSnapshot.revoked_connectors = 11` → `repeated RevokedConnector{connector_id, connector_spiffe}`, compiled from `connectors WHERE tenant_id=$1 AND (status='revoked' OR revoked_at IS NOT NULL)`.
- The ACL is the policy plane.
- It is already pushed to connectors, which §10 reuses.
- `NotifyPolicyChange` already fires on revoke.

| Behaviour | Design | Existing pieces |
|---|---|---|
| Receive authoritative state | 60 s poll + early resync | `run_acl_sync_scheduler` (`daemon.rs:4723`) |
| Reject new connections | exclude listed ids in coords resolution, even if a stale transport snapshot lists them | coords resolution in `daemon.rs` |
| Associate flows | carry `ConnCoords.connector_id` into `ClientTransport`; record the selected id on `ActiveRelay` | `ConnCoords` (`daemon.rs:5524`) |
| Terminate flows | `ResourceCmd::CloseConnectorFlows{ids, ack}` in the smoltcp loop (abort → RST; Fix 05-A contract) | `ResourceCmd` (`net_stack.rs:733`), `remove_listeners_and_close` (`:555`) |
| Stale pools | the rebuild drops the revoked connector's pools; optionally `close()` its pooled connection | `build_transport_from_coords` (`daemon.rs:5773`) |
| Exact identity on the direct path | `ExactSpiffeVerifier` with `ConnCoords.connector_spiffe` instead of the prefix verifier | `tunnel_pool.rs:72` |
| Reconnect sync | apply the list on the first successful sync, before the next flow is accepted | — |
| Absence ≠ revocation | only the explicit list triggers termination; absence keeps today's behaviour (Q3 untouched) | — |
| Offline staleness bound | **ACCEPTED (D19)**: 1 h; implemented in Phase C | — |

Mixed versions aren't supported (D8): the client and controller ship together. An empty list from a contract-capable controller means "none revoked".

## 9. Relay enforcement (D6 ACCEPTED direction; details RECOMMENDED, AWAITING APPROVAL)

**Runtime status: IMPLEMENTED AND VERIFIED LIVE (Phase D, 2026-10-23)**: a revocation-ignoring connector's registration was dropped 25.9 s after revocation. The periodic CRL re-check bullet below was not implemented; the heartbeat answer covers established sessions. See `Connector-Revocation-PhaseD-Implementation-2026-10-23.md`.

- **Identity channel:** the existing unary heartbeat.
  - Request field 8: `registered_connector_spiffe_ids`.
  - Response field 3: `revoked_connector_spiffe_ids` (only the subset the relay holds).
- **New connections:** refuse `Register` for a listed identity (after the authenticated binding, `session.rs:123-128`); keep the CRL check at accept.
- **Established sessions:** `ConnectorEntry.connection.close()` + `remove_connector`. Bridged client streams are `open_bi` on that connection, so they end too (INFERRED).
- **CRL path:** a periodic re-check of registered connectors' serials, following the existing 1 s relay-revocation monitors (`client/src/relay_pool.rs:253`, `connector/src/relay_client.rs:466`).
- **Stale information:** **ACCEPTED (D20)**: sticky set, keep serving, no new timeout.
- **Unrelated connectors:** untouched (per-entry close).

## 10. Shield enforcement (D6 ACCEPTED direction; details RECOMMENDED, AWAITING APPROVAL)

The shield's only channel is its connector (CONFIRMED BY SOURCE). With an **honest** connector, enforcement needs **no shield code change**: the connector's §7 step 4 closes the streams, which cancels the shield sessions multiplexed in them and triggers the shield's existing failover.

| Situation | Behaviour |
|---|---|
| Current connector revoked | the connector closes shield streams and refuses new ones (§7) |
| Channel closed | existing failover to the next peer (`run_once:126-142`) |
| Stale peer list | RECOMMENDED: `PeerConnectorList.revoked_connector_ids = 2` from any connector's ACL `revoked_connectors`; the shield prunes; lists are sent even when the sender's own id is absent. That is a shield proto + code change. Prune rule: **OPEN (O13)** |
| Alternative eligible connector | attaches via existing failover; new protected flows then work through it |
| Existing protected application session | terminated; **not migrated** (PENDING-18) |
| Controller/connector unavailable | the connector's `Disabled` handling (O8) |
| Hostile revoked connector | can't be enforced at the shield with current interfaces (FR §19.8); **OPEN (O13)** |

Revocation enforcement, failover and migration are separate deliverables with separate tests.

## 11. CRL and SPIFFE identity responsibilities (D7 ACCEPTED)

| Concern | Carrier | Enforced by |
|---|---|---|
| Administrative **identity** revocation (unit: the SPIFFE ID, i.e. `connectors.id`) | controller DB → no ack + `revoked` marker / `ConnectorRevoked` (connector; an absent row yields the separate `unknown` marker, which is not a revocation); `ACLSnapshot.revoked_connectors` (clients; connectors → shields); relay heartbeat response (relays) | controller (existing), connector, client, relay, shield (via connector) |
| Individual **certificate** revocation (unit: the serial) | workspace CRL `/ca.crl` | relay (accept + proposed re-check), connector (client serials, existing) |

- A standard X.509 CRL lists certificate serials under an issuer. **It is not a mechanism for revoking a SPIFFE ID**, and no SPIFFE ID is placed in it.
- Identity revocation covers **every** certificate of the identity (current, renewed or earlier), wherever the enforcing component checks identity state. The two checks are independent: either one denies.

## 12. Certificate-history and identity-lifecycle decisions (each BLOCKED ON SCHEMA APPROVAL unless noted)

| Decision | Options / facts | Status |
|---|---|---|
| Per-certificate connector history | precedent: `relay_certificates` (027). A `connector_certificates` table would need migration 038+ | BLOCKED ON SCHEMA APPROVAL (recommended as defence in depth) |
| CRL coverage of every still-valid historical certificate | needs the history table; identity revocation already covers them where identity state reaches | BLOCKED ON SCHEMA APPROVAL |
| Revoked serials surviving deletion | today a hard delete removes the CRL entry (`DeleteConnector`); migration 029's "Rows are never deleted" comment contradicts the code | BLOCKED ON SCHEMA APPROVAL (or a code-only "forbid delete" policy, OPEN) |
| Tombstone / durable lifecycle record | `status` has no `deleted` value; a tombstone needs a schema change | BLOCKED ON SCHEMA APPROVAL |
| Identity reuse prevention | UUID randomness only today | depends on the tombstone decision |
| Same-key renewal | not enforced for connectors (relays and devices do enforce it) | OPEN; not required by Q1–Q6 |

No schema or database change is authorized by this document.

## 13. Phased implementation plan

```
M0 (dev-stack baseline: coordinated build/install procedure, §15)
A  (controller contract, revoked/unknown markers, error classification, revoked list compile, relay heartbeat field) ─┬─> B1 (supervisor, states, funnel gate, terminal handling)
                                                                                                        ├─> C (client)
                                                                                                        └─> D (relay)
M  (timing measurements on an A build) ─> B2 (schedule P, deadline expiry, fail-closed steps 3–6)
B2 + C ─> E (shield peer-list pruning; core shield enforcement comes from B2 with no shield change)
F  (cert history / tombstone) — BLOCKED ON SCHEMA APPROVAL
G  (tests) — inside every phase; live rows after B2, C, D, E
```

| Phase | Prerequisites | Files / interfaces (HEAD paths) | Conceptual changes | Tests (§14) | Acceptance | Blocking decisions |
|---|---|---|---|---|---|---|
| **A** Controller contract | §17 gate | `proto/connector/v1/connector.proto`; `controller/internal/connector/control_stream.go` (`handleConnectorHealth`, `Control`, `activateStream`, `agentPolicyMessage`, exported registry accessor); `graph/resolvers/connector.resolvers.go`; policy compiler/store (revoked list); `proto/client/v1/client.proto` | ack rules §4.3; `ConnectorRevoked` push; `revoked`/`unknown` trailer markers (D10); the four §4.5 corrections (`Scan` error → `Unavailable`; follow-up classification in health and activation); `AgentPolicy` always sent; revoked-connector list compiled | T5, T8, T19–T22, T14 | new controller suite tests; existing suite green | O2, O3 **resolved** (D10, D11); remaining gate items in §17 |
| **M** Measurements | A in the lab | instrumentation only | §6.6 | T17 | B, P, O6 recorded | — |
| **B1** Connector states + gate | A | `connector/src/control_stream.rs`, new supervisor module, `device_tunnel.rs`, `quic_listener.rs`, `main.rs` | §4.4, §5 (incl. `UnknownIdentity`), §7 steps 1–2, terminal handling, capability assertion | T3–T8, T15, T16, T19–T28 (connector side) | unit tests | O9 resolved (D12); B1-Q1 = (a) |
| **B2** Deadline + fail-closed | B1, M | supervisor schedule; `session_registry.rs`; `agent_server.rs`; `policy/mod.rs`; tokio `test-util` dev-dependency (`full` excludes it) | §6.3, §7 steps 3–6 | T1, T2, T9, T10, T17 | paused-clock + live | none: O5, O6, O7, O8 resolved (D13–D16) |
| **C** Client | A | `client/src/daemon.rs`, `transport.rs`, `net_stack.rs`, `tunnel_pool.rs` | §8 | T11 | unit + live | O11 |
| **D** Relay | A (controller relay heartbeat) | `proto/relay/v1/relay.proto`, `controller/internal/relay/heartbeat.go`, `relay/src/heartbeat.rs`, `state.rs`, `session.rs`, `listener.rs` | §9 | T12 | unit + **live relay lab (BLOCKED today)** | O12 |
| **E** Shield pruning | B2, C | `proto/shield/v1/shield.proto`, `connector/src/agent_server.rs`, `shield/src/control_stream.rs` | §10 | T13 | live (two connectors) | O13 |
| **F** History / lifecycle | schema approval | migrations 038+, `enrollment.go`, `pki/workspace.go`, `DeleteConnector` | §12 | CRL history / delete tests | — | O14 |
| **G** Live acceptance | each phase | live-lab scripts | §14 | — | OBSERVED_* records | relay lab |

## 14. Test and acceptance strategy

**No test listed here has been written or executed.** Coverage of the O2/O3 requirements: revoked marker T5; unknown marker T19; distinguishable T20; DB failure T21; no `NotFound` from DB error T22; transport failure T23; unknown + reconnect T24; unknown + restart T25; authorized start T26; revoked + reconnect T7; D1 on failures T27; renewal T28.

| # | Requirement | Level |
|---|---|---|
| T1 | Healthy acks arrive within the deadline over sustained operation (0 `Disabled`) | paused clock + live ≥ 1 h with latency logged |
| T2 | Missing ack → new traffic refused exactly at `D = sent[k] + H`, sessions cancelled at expiry (assert the instant) | paused clock + live control-plane block |
| T3 | A send alone never moves D | unit |
| T4 | Duplicate, out-of-order, unknown, zero, stale (`now ≥ sent+H`), post-expiry, pre-`lost_at` and mismatched acks are all rejected (one case per §4.4 row) | unit |
| T5 | `ConnectorRevoked` (connected) and the `revoked` marker (reconnect) → `Revoked` immediately, without waiting for D; sessions cancelled | unit + live |
| T6 | Reconnect after a temporary interruption doesn't serve until an accepted ack | unit + live |
| T7 | A revoked connector never reactivates by reconnecting or restarting | unit + live |
| T8 | Unmarked `NotFound` / unmarked `PermissionDenied` / other unmarked codes → recoverable, never terminal; the trailer marker survives a trailers-only response (tonic client against the Go server) | controller + connector unit, integration |
| T9 | Fail-closed cancels all connector-routed sessions and refuses new streams on QUIC, TLS and relay paths at the transport level; the client classifies the refusal as a transport failure | unit (`handle_stream` harness, as `acl_diff_session_tests`) + live holds |
| T10 | Make-before-break renewal under load stays uninterrupted; outstanding-seq reuse covers a lost in-flight ack | unit + live (≥ 3 renewals) |
| T11 | Client closes existing flows on a revoked connector, keeps others, rejects new ones, exact direct verifier | unit (`FlowTable`) + live |
| T12 | Relay closes established sessions of a revoked connector; refuses Register | unit + live (**relay lab required**) |
| T13 | Shield-facing sessions are terminated and the shield stops serving through the revoked connector; failover tested separately; no migration asserted | unit (connector `close_all`) + live (two connectors) |
| T14 | Protocol incompatibility prevents serving: new connector + controller without the contract, and the reverse, never serve | integration |
| T15 | Startup doesn't serve before the first accepted ack (all entry paths, shield server) | unit |
| T16 | `Revoked` and `Disabled` are distinguished (separate states, logs and recovery paths) | unit |
| T17 | Authorization scheduling and controller load measured against B at the candidate P (the §6.6 table) | lab measurement run |
| T18 | Full development-stack version consistency before release: every component built from one commit, binary shas recorded, update timers disabled, T14 green | release checklist + live |
| T19 | The `unknown` marker → `UnknownIdentity` (non-serving, terminal for the process); sessions cancelled | unit + live (delete a disposable connector) |
| T20 | `Revoked` and `UnknownIdentity` stay distinguishable: controller emits `revoked` vs `unknown` for a revoked vs absent row (at open, mid-stream after a health `ErrNoRows`, and at activation); connector reaches the matching state and logs it | controller unit + connector unit |
| T21 | A DB lookup failure (open, health follow-up, activation follow-up) returns `Unavailable` with **no** marker; the connector stays recoverable | controller unit (failing pool) + connector unit |
| T22 | `NotFound` is never produced from a DB error (regression for `Control:436-437`) and a marker is never attached without a successful lookup | controller unit |
| T23 | Transport failure (reset, timeout, controller down) never produces `Revoked` or `UnknownIdentity` | connector unit + live control-plane block |
| T24 | A connector in `UnknownIdentity` doesn't resume serving after a plain transport reconnect | connector unit |
| T25 | Restarting an unknown connector doesn't expose any data-plane entry path before an accepted ack (it returns to `UnknownIdentity`) | connector unit + live |
| T26 | A legitimately authorized connector starts, receives an accepted ack and becomes `Serving` | unit + live |
| T27 | D1 still applies to authorization failures: under `Unavailable` or transport failure, serving ends at exactly `D = sent[k] + H` with no extra interval | paused clock |
| T28 | Normal make-before-break renewal produces no marker and no terminal state (with T10) | unit + live (≥ 3 renewals) |

Mutation checks (disable each enforcement line; the matching test must fail) follow existing team practice. Live rows are graded OBSERVED_* with fresh disposable connectors.

## 15. Development rollout (D8: coordinated, no legacy support)

**Which components need changes:**

| Component | Protocol change? | Code change? | Needed for |
|---|---|---|---|
| Controller | yes (connector proto, client ACL field, relay heartbeat fields) | yes | everything |
| Connector | yes (consumes new messages; sends seq; peer-list field for E) | yes | Q1, Q2, Q3r, shield enforcement |
| Client | yes (`ACLSnapshot.revoked_connectors`) | yes | client enforcement |
| Relay | yes for identity enforcement (heartbeat fields); no for CRL re-check only | yes | relay enforcement |
| Shield | **no** for core enforcement (the connector closes streams; failover exists); yes only for peer-list pruning (E) | only for E | stale peer lists |

**Development procedure** (before any live acceptance row):
1. Build all affected binaries from one commit; record their shas.
2. Stop and disable the connector/shield `*-update.timer` units (they pull release binaries, §3) and verify the on-disk shas after every install.
3. Reinstall the controller, connectors, relays, shields and clients from that build.
4. **Fresh enrollment** of every connector and shield used for revocation tests (disposable identities, as in 10-12/10-13). Existing certificates stay valid protocol-wise. Fresh enrollment ensures no state from older builds and keeps revoked identities separate. Revoked DB rows from earlier runs stay in place (CRL coverage).
5. No DB reset is required by phases A–E. Phase F needs its migration first.
6. Run T14 and T18 before any acceptance row.

Rollback = redeploy the previous coordinated build. Mixed pairings fail closed by design (T14).

**B2 live lab (user decision, 2026-10-21):**

| Role | Host | Notes |
|---|---|---|
| Controller | this PC (192.168.1.164) | private binary path; other agents must not restart it during runs |
| Connector | inkyank (192.168.1.75) | update timer disabled |
| **Shield** | **udaya (192.168.1.37)**, replacing nika | a shield has to be installed and enrolled there (sudo on udaya); T13 and the D15 30 s shield-close check run here |
| **Relay** | **manojinkyank (192.168.1.49)** | relay-path rows of T9/T12 run here instead of being unit-only |
| Client | this PC | one resource, or `zecurity-client down` / `up` after each resource change |

**Why one resource or a `down`/`up` cycle:** the client's resource hot-apply (`167ffa5`) admits only one new resource IP. The cap is `smoltcp::config::IFACE_MAX_ADDR_COUNT` = 2 minus 1 reserved address (Phase M results, "Lab finding"). This is a client issue, outside B2.

Access to the lab hosts is through the user's Orca SSH terminals. sudo on each host needs the user's password.

## 16. Decisions

### Accepted

| ID | Decision |
|---|---|
| D1 | Freshness ≤ one interval H (≈ 15 s, unchanged); `D = sent[k] + H` of the latest accepted ack; no grace; the < 2H reading is rejected |
| D2 | Additive acknowledgement protocol (not Ping reuse) |
| D3 | `ConnectorRevoked` on active streams + a machine-readable rejection on reconnect; explicit revocation is terminal |
| D4 | The §5 state model and guarantees |
| D5 | Fail closed stops new traffic **and** terminates existing connector-owned sessions |
| D6 | Client/relay/shield directions (§8–10); PENDING-18 stays deferred |
| D7 | Identity revocation and CRL serial revocation stay distinct; a SPIFFE ID isn't a CRL entry |
| D8 | Unreleased: no legacy compatibility; nothing without the contract may serve; coordinated dev rollout |
| D9 | The ack is sent only after any required ACL push (§6.5) |
| D10 (= O2) | A structured, machine-readable gRPC status marker (trailer metadata, §4.5) distinguishes REVOKED and UNKNOWN_CONNECTOR from ordinary errors; message text is never used for security decisions; markers only after a successful lookup |
| D11 (= O3) | An unknown/deleted identity never serves; it enters `UnknownIdentity`, a terminal non-serving state for the process, distinct from `Revoked`; recovery only through explicitly authorized re-enrollment or replacement; DB failures stay recoverable |
| D12 (= O9) | Terminal states are not persisted across restart; in `Revoked`/`UnknownIdentity` the connector idles (process alive, data plane closed, reconnect loop stopped, terminal state logged periodically) rather than exiting, because `restart: unless-stopped` restarts any exit |
| D13 (= O5) | Control-stream reconnect backoff resets after an accepted ack; while not serving 1 s → ×2 → 5 s cap, ±20 % jitter; fixed at build time (`connector/.env` → `build.rs` → `authz_timing.rs`) |
| D14 (= O7) | P = 4.5 s with H = 15 s fixed; B budgeted at 1 s from Phase M; fixed at build time, allowed 500–14000 ms |
| D15 (= O8) | `Disabled` keeps shield/relay/client connections but carries nothing; shield streams closed after 30 s in `Disabled`; fixed at build time |
| D16 (= O6) | The authorization request every P is the full health report (no protocol or controller change). A lightweight request is a later improvement |
| D17 | The Phase A contract details are approved as built: field numbers (§4.2), ack acceptance rules (§4.4, with `lost_at` per §5.2) and the `AgentPolicy.authorization_acks` capability flag kept as a fail-closed assertion (§4.6) |
| D18 | B2 includes the renewal head-of-line fix: a certificate renewal must not delay processing of `AuthorizationAck` or `ConnectorRevoked` on the current Control stream (Phase M: all 61–80 ms ack tails) |
| D19 (= O11) | Client: revoked connectors listed until revoked_at + connector cert TTL; the client stops using connectors after 1 h without a successful ACL sync |
| D20 (= O12) | Relay: a stale revoked list is kept; the relay keeps serving; no new timeout |
| D21 (= O13) | Shield: prune field approved; prune immediately from any connector's list, never re-add while listed, memory only; hostile-connector limit accepted and documented |

### Former open items O1–O14 (re-classified)

| ID | Question | Status |
|---|---|---|
| O1 | Deadline anchor | **ACCEPTED**: send time of the acknowledged request (§6.1) |
| O2 | Terminal marker form | **ACCEPTED (D10)**: structured trailer marker (§4.5); exact key/value spelling is a Phase A protocol implementation detail |
| O3 | Deleted/unknown connector: terminal or another lifecycle class | **ACCEPTED (D11)**: separate terminal non-serving state `UnknownIdentity` (§5.1); recovery workflow not designed here |
| O4 | Legacy mode for a new connector with an old controller | **NOT APPLICABLE (UNRELEASED)**: removed. The remaining question, whether to keep `AgentPolicy.authorization_acks` as a fail-closed assertion, is **RECOMMENDED, AWAITING APPROVAL** (keep, §4.6) |
| O5 | Reconnect backoff cap in `Disabled`/`Recovering` | **ACCEPTED (D13, 2026-10-21)**: reset the backoff after a session receives an accepted ack; while not serving start at 1 s, ×2, cap 5 s, ±20 % jitter (`CONTROL_RECONNECT_BASE_MS` / `_MAX_MS` / `_JITTER_PCT`) |
| O6 | Full health report vs lightweight authorization request at period P | **ACCEPTED (D16, 2026-10-21)**: (a) full health report every P now. (b) a lightweight request (guarded UPDATE only, about 25–30 % cheaper per Phase M) is a **later improvement**, not part of B2 |
| O7 | Response budget B and period P (H fixed; ack-after-ACL accepted as D9) | **ACCEPTED (D14, 2026-10-21)**: measured B p99 ≤ 79 ms / max 83 ms (Phase M); B budgeted at 1 s; **P = 4.5 s** (`AUTHZ_REQUEST_PERIOD_MS`, range 500–14000), tolerating two consecutive lost requests |
| O8 | On `Disabled`: close vs disable shield streams; detach relays; close client QUIC connections | **ACCEPTED (D15, 2026-10-21)**: keep shield streams, relay attachment and client QUIC connections open but carry nothing (new streams refused, sessions cancelled); close the shield streams after 30 s in `Disabled` (`DISABLED_SHIELD_CLOSE_SECS`). To be checked live once a shield host is available |
| O9 | Persist `Revoked`/`UnknownIdentity` across restart; exit vs idle | **ACCEPTED (D12)**: no persistence; idle, don't exit |
| O10 | Recovery after permanent revocation | **OPEN** (today: new identity only) |
| O11 | Retention of `revoked_connectors`; client offline staleness bound | **ACCEPTED (D19, 2026-10-22)**: a revoked connector stays listed until its last certificate can no longer be valid (revoked_at + connector cert TTL); the client stops using connectors after **1 h** without a successful ACL sync |
| O12 | Relay behaviour when its revoked list is stale | **ACCEPTED (D20, 2026-10-22)**: keep the last known list, keep serving, no new timeout |
| O13 | Shield prune rule; hostile-connector limit | **ACCEPTED (D21, 2026-10-22)**: `PeerConnectorList.revoked_connector_ids` approved; a revoked id from any connector prunes it immediately and it is never re-added while listed (in memory only); the hostile-connector limit is accepted and documented (bounded by connector certificate lifetime) |
| O14 | Cert history, historical-serial CRL coverage, serials after deletion, tombstones, reuse prevention | **BLOCKED ON SCHEMA APPROVAL** (each separately, §12) |

Release requirement (not a decision): the new development stack's protocol behaviour must be validated by T14/T18 before release. Support for old production connectors is not required.

## 17. Explicit approval gate

**Source implementation remains NOT AUTHORIZED.**

| Gate item | Status |
|---|---|
| Final heartbeat acknowledgement contract | ACCEPTED: direction (D2, D9); field numbers, acceptance rules (§4.4) and capability flag (§4.6) (D17); marker (D10 = O2) |
| Exact deadline and scheduling semantics | ACCEPTED (D1, O1, D14 = O7, D16 = O6) |
| Terminal vs recoverable states | ACCEPTED (D4, D11 = O3, D12 = O9) |
| Complete fail-closed definition | ACCEPTED (D5, §7, D15 = O8) |
| Enforcement ownership: client, relay, shield | directions ACCEPTED (D6); O11–O13 OPEN |
| Rollout sequence | ACCEPTED (D8, §15) |
| Schema changes needing approval | O14 BLOCKED ON SCHEMA APPROVAL; none authorized |
| Recovery after permanent revocation | O10 OPEN |

Each phase needs its own explicit go-ahead with a pre-written acceptance list:
- **A**: no longer blocked by O2/O3 (accepted as D10/D11). It still needs an explicit implementation go-ahead and approval of the remaining contract details (field numbers, §4.4 acceptance rules, §4.6 capability flag). Other phases' blockers are unchanged.
- **M** needs an A lab build.
- **B1**: O9 resolved (D12). [B1 acceptance list](Connector-Revocation-PhaseB1-Acceptance-2026-10-20.md) signed off 2026-10-20 with B1-Q1 = (a): B1 is a unit-tested intermediate, not deployed live until B2. **B1 implemented 2026-10-21** (`da5b12f`): [B1 record](Connector-Revocation-PhaseB1-Implementation-2026-10-21.md). Not for live deployment before B2.
- **B2**: Phase M done ([results](Connector-Revocation-PhaseM-Results-2026-10-21.md)); O5, O7 and O8 accepted (D13–D15; build-time values: `connector/.env` → `connector/build.rs` → `connector/src/authz_timing.rs`; documented in `connector/.env.example`; not runtime config). O6 accepted (D16, full report); Phase A contract details approved (D17); renewal head-of-line fix in scope (D18). [B2 acceptance list](Connector-Revocation-PhaseB2-Acceptance-2026-10-21.md) signed off; **B2 unit level implemented 2026-10-21** ([record](Connector-Revocation-PhaseB2-Implementation-2026-10-21.md)); **live L-1…L-8 PASS 2026-10-21** (udaya shield, manojinkyank relay). Live acceptance uses udaya (shield) and manojinkyank (relay), see §15.
- **C**, **D** and **E** need O11, O12 and O13 respectively.
- **F** needs schema approval (O14).

This revision changed only this document. No source, test, protocol, configuration, migration or runtime state was modified, and nothing was committed.
