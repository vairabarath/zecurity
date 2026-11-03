---
title: Connector Revocation and Enforcement — Requirements vs. Existing Architecture (Feasibility)
date: 2026-10-14
type: source-investigation + feasibility (documentation only)
status: evidence-for-decision — IMPLEMENTATION NOT AUTHORIZED, ARCHITECTURE NOT APPROVED
branch: feat/sprint20-m1-phase2
reference_revision_requested: 9844a00
revision_investigated: 38e38c6 (HEAD) + pre-existing uncommitted working tree
related:
  - Connector-Revocation-Live-Verification-2026-10-12.md (historical, unchanged)
  - Connector-Revocation-Remaining-Live-Tests-2026-10-13.md (historical, unchanged)
  - Connector-Identity-Revocation-Architecture-Investigation-2026-10-13.md (unchanged)
  - Live-Acceptance-Run-Sheet.md Finding 34 (unchanged)
  - pending/PENDING-18-Established-Connection-Recovery-Connector-Failover.md (scope boundary, unchanged)
  - Fixes/fix 1/Phase2-Full-Hot-Apply.md § Q3 (scope boundary, unchanged)
---

# Connector Revocation and Enforcement — Feasibility (2026-10-14)

## 1. Purpose and investigation scope

This document records:
- the **accepted** connector-revocation security requirements (R1–R12);
- what the **current source** does for each of them, per component (controller, connector, client, relay, shield);
- whether each requirement can be met with **existing** interfaces, or needs code, state or protocol changes;
- what stays **unverified** and which **decisions** are still open.

It is a source investigation. Nothing was implemented, no test was run, no service was touched.

**Finding labels** (used on every claim):

| Label | Meaning |
|---|---|
| **CONFIRMED BY SOURCE** | read in the current checkout (path + function given) |
| **CONFIRMED BY TEST** | an automated test asserts it (test name given); the test was **not** run here |
| **HISTORICAL LIVE EVIDENCE** | observed in an earlier live run (10-12, 10-13, Finding 34); not re-run |
| **INFERRED** | follows from source but not asserted by a test or a live run |
| **PARTIAL** | some of the capability exists; the rest is missing |
| **MISSING** | not found in the current source |
| **UNVERIFIED** | source is insufficient; runtime evidence needed |
| **NOT FEASIBLE WITH CURRENT INTERFACES** | needs a new or changed interface/protocol/state (reason given) |
| **ACCEPTED REQUIREMENT** | one of R1–R12 (given, not derived) |
| **OPEN DECISION** | needs human approval |

Scope boundaries kept from earlier records:
- **PENDING-18** (established-connection recovery and connector failover) stays deferred. Where R6/R8 touch failover or session migration, this document reports feasibility only and does not expand PENDING-18.
- **Q3** (client handling of a transient connector-absent snapshot during renewal; `Fixes/fix 1/Phase2-Full-Hot-Apply.md` § Q3) stays open. This document does not decide it. It does record that a snapshot-absence rule can't tell revocation from a transient disconnect (§11).

## 2. Repository revision and working-tree state

Recorded before the investigation (2026-10-14 ~11:00 IST):

| Item | Value |
|---|---|
| Branch | `feat/sprint20-m1-phase2` (matches the expected branch) |
| HEAD | **`38e38c62044dd869e57bc299e7d3e1dbf4ad10a9`** (2026-10-12 18:40 +0530, "Merge pull request #117 … sprint21/k-sprint20-cleanup") |
| Requested reference | `9844a00` (2026-10-09). It is an **ancestor** of HEAD; HEAD is 16 commits later (`git log 9844a00..HEAD`) |
| Working tree | 26 pre-existing status lines: 11 modified, 15 untracked (snapshot saved to `$TMPDIR/status-before.txt`). The modified code files are `client/src/daemon.rs` and `client/src/daemon_tests.rs`: log-wording only (F-1, `Fix-Client-Restart-Log-Wording`), no behaviour change |
| Code investigated | the **checked-out working tree**. Line numbers in `client/src/daemon.rs` are working-tree numbers (they differ from HEAD by up to +5 after line ~5018) |

**Revision differences that matter here** (`git diff --stat 9844a00 HEAD`, code paths only):

| Commit area | Change | Effect on revocation findings |
|---|---|---|
| K2 (`2c3dda0`) | proto: `ConnectorControlMessage.agent_policy = 18`, message `AgentPolicy` (shield renewal window); controller sends it when a Control stream opens; connector `handle_controller_msg` arm | None on revocation. Shows the oneof is extended additively in practice |
| B2 (`fba6029`) | controller renewal trigger uses `min(stored cert_not_after, presented NotAfter)` (`control_stream.go` `renewalExpiry`) | None on revocation; renewal still runs only after the revocation-guarded health UPDATE |
| B4 (`960709a`) | CRL `thisUpdate` backdated 5 min (`pki/crypto.go` `crlWindow`, used at `pki/workspace.go:622`) | CRL `nextUpdate` is still now + 24 h. The 10-13 record's "thisUpdate = fetch time" described the pre-B4 build |
| S1 / K1 (relay) | relay revocation uses the leaf's real issuer; relay renewal scheduling | Relay CRL check remains handshake-time only (§8) |
| B1 (client) | restart a running tunnel onto a renewed device cert | None on connector revocation |

**Discrepancies with earlier records:**
1. The 10-13 identity investigation cites `StreamSPIFFEInterceptor` at `control_stream.go:913` and the CRL `nextUpdate` at `pki/workspace.go:626`. At HEAD they are at `control_stream.go:984` and `pki/workspace.go:622` (line drift from K2/B2/B4; behaviour unchanged).
2. The 10-13 live record leaves open (§13 Q5) why the revoked connector stopped sending `PeerConnectorList` to the shield. The current source explains it (§9.2): **CONFIRMED BY SOURCE**.
3. PENDING-18 lists the client moving to the next connector after "authentication failures". That is true for TLS/handshake failures. A connector **denial reply** other than `SHIELD_NOT_ATTACHED` ends selection with no fallback and no resync (`client/src/net_stack.rs:1167`). This isn't a contradiction, but it matters for any connector self-enforcement design (§7.3, §19).

## 3. Accepted security requirements

All twelve are **ACCEPTED REQUIREMENT**. They are verified against the source below; they don't imply the code meets them.

| ID | Requirement (summary of the accepted text) |
|---|---|
| R1 | Controller persistent revocation state is authoritative; reconnecting must not resume service |
| R2 | A connected connector that receives an authoritative revocation enters a terminal revoked state immediately, not a retryable network error |
| R3 | On reconnect after revocation, the controller rejects by authoritative identity/status; the connector stays unable to serve; reconnection never reactivates |
| R4 | Data plane only while controller authorization is fresh: failing to communicate successfully by the **next scheduled heartbeat deadline** → fail closed (stop new connections/streams, terminate existing sessions, close or disable Shield-facing data-plane connections, stop using stale ACL, stay disabled until authorization is resolved). Initial bound: **one heartbeat interval**, no extra grace. Authoritative revocation acts immediately, without waiting for the deadline |
| R5 | After an ordinary interruption, resume only after the controller confirms authorization. After administrative revocation, resume only via an explicitly authorized recovery process (not to be invented) |
| R6 | Client: stop selecting a revoked connector, terminate its flows, prevent stale pooled-connection reuse, keep eligible alternatives working. Map removal alone is not enforcement |
| R7 | Relay: no new connections through a revoked connector, and terminate affected established sessions |
| R8 | Shield: stop carrying traffic through a revoked connector (channel, sessions, stale peer lists, state); failover may be evaluated; failover ≠ session migration |
| R9 | An offline client retrieves current authoritative policy/revocation state before reusing stale connector configuration |
| R10 | An offline connector fails closed at its heartbeat deadline, obtains authorization before serving again, and stays disabled while revoked |
| R11 | Administrative revocation is primarily an identity-authorization decision (stable SPIFFE ID + controller state); the CRL stays for certificate-level revocation; serial CRLs don't revoke "all certs of a SPIFFE ID" |
| R12 | Normal make-before-break renewal keeps working and isn't mistaken for revocation |

## 4. Current connector-revocation lifecycle (as the source behaves today)

Step by step, all **CONFIRMED BY SOURCE** unless labelled otherwise:

1. **Admin revoke.** GraphQL `revokeConnector(id)`, `@hasRole(roles: [ADMIN])` (`controller/graph/connector.graphqls:33`). Resolver `RevokeConnector` (`controller/graph/resolvers/connector.resolvers.go:113-155`): one UPDATE sets `status='revoked'`, `revoked_at=NOW()`, `revocation_reason='revoked via admin API'`, tenant-scoped, only from `active|disconnected` with `revoked_at IS NULL`.
2. **Version bumps.** Same resolver: `PolicyNotifier.NotifyPolicyChange` (`:143-147`), then `TransportNotifier.NotifyTopologyChange` (`:148-152`).
   - `NotifyPolicyChange` (`controller/internal/policy/notifier.go:57`) bumps the in-memory version, invalidates the cache and fires the push hook. That hook is the connector ACL pusher (`internal/connector/acl_push.go` `PushWorkspace`/`pushOnce:116`, fan-out to `registry.ClientsForWorkspace` at `:131`).
   - `NotifyTopologyChange` (`controller/internal/transport/notifier.go:57`) only bumps and invalidates; clients pick it up by polling.
   - The resolver sends nothing addressed to the revoked connector itself.
3. **The revoked connector gets the new ACL.** It is still registered, so the ACL fan-out sends the recompiled snapshot to it too. Compilers include only `c.status = 'active'` connectors (`policy/store.go:449`, `transport/store.go:61`), so that ACL **omits the revoked connector itself** from its remote-network list. HISTORICAL LIVE EVIDENCE: 10-13 §5, A logged `ACL snapshot stored version=7` at 14:50:05.480, after `revoked_at`.
4. **Control stream closed at the next health report.** The connector sends `ConnectorHealthReport` every `HEALTH_INTERVAL_SECS = 15` (`connector/src/control_stream.rs:38`, `run_session` ticker `:426`). The controller's `handleConnectorHealth` (`control_stream.go:749`) runs a revocation-guarded UPDATE (`:754-778`, `status NOT IN ('revoked','deleted') AND revoked_at IS NULL`). No row → logs `revoked/deleted — closing control stream` (`:782`). `Control` then returns `status.Error(codes.PermissionDenied, "connector revoked")` (`:566-568`). `retireStream` (`:637`) only moves `active → disconnected`, so `revoked` stays.
5. **Reconnect refused.** `Control` (`:419`) checks `status`/`revoked_at` by `(id, trust_domain)` and returns `PermissionDenied "connector is revoked"` (`:433-441`). `activateStream` returns the same on its guarded UPDATE (`:604-629`). `RenewCert` refuses too (`enrollment.go:249-250`, plus a guarded serial UPDATE `:267-282`).
6. **Connector reaction.** The error surfaces as `Some(Err(e))` in `run_session` (`control_stream.rs:443`) or as an open failure (`open_stream:237-240`). Both return `Err` to `run_control_stream` (`:76-110`), which logs and retries with backoff 2 → 60 s **forever**. Listeners, ACL cache, sessions, relay attachment and shield streams are untouched.
7. **Clients.** They learn of it only at their next ACL/transport poll (60 s tick, `client/src/daemon.rs:38` `ACL_REFRESH_TTL_SECS`, scheduler `:4723`) or an early transport resync. The connector is **absent** from the new snapshots. New flows to its resources then fail closed or go to another listed connector. Established flows are untouched (§7, §10).
8. **CRL.** The next `/ca.crl` fetch lists the connector's **current** `cert_serial` only (`pki/workspace.go:530` `GenerateClientCRL`, connector query `:583-608`).
   - The relay is the only consumer that checks connector serials, and only at handshake (§8).
   - The connector checks client serials only (§6.6).
9. **Shield.** It receives nothing. The revoked connector stops sending `PeerConnectorList` (§9.2) and the shield ignores the silence. Its DB status flips to `disconnected` only through the shield disconnect watcher, because its status reaches the controller only via the connector (HISTORICAL LIVE EVIDENCE, 10-13 R2).

**HISTORICAL LIVE EVIDENCE** for the whole sequence:
- 10-12 (controller binary `controller-f5c`, built before `4781bc8`, i.e. not exactly HEAD);
- 10-13 (fresh build of `9844a00`);
- Finding 34 (2026-09-28).

In all three, existing direct and shield flows survived revoke, client sync and certificate `notAfter`. None of them ran on HEAD `38e38c6`. The diff above makes no revocation-relevant change, so the behaviour is **INFERRED** unchanged; it is not re-verified live.

## 5. Controller enforcement and revocation distribution (Investigation A)

| # | Question | Answer | Evidence |
|---|---|---|---|
| A1 | Does persistent state distinguish revoked from disconnected? | **Yes.** `status` ∈ {…, `disconnected`, `revoked`} plus `revoked_at` and `revocation_reason`. Every writer guards `revoked` (health UPDATE `:770-771`, activation `:615-616`, retire `:655` only from `active`, watcher `disconnect_watcher.go:51` only from `active`, renew `enrollment.go:271-272`) | CONFIRMED BY SOURCE; CONFIRMED BY TEST `TestControl_RejectsRevokedConnector`, `TestControl_RejectsRevokedAtLegacyStatus`, `TestGoodbye_RevokedStaysRevoked`, `TestRenewCert_RevokedStatusDenied`, `TestRenewCert_RevokedAtLegacyDenied`, `TestRenewCert_RaceRevokeReturnsNoCert`, `TestHandleConnectorHealth_RevokedClosesStream` (`internal/connector/revocation_test.go`) |
| A2 | Does the client receive an explicit revocation decision? | **No.** It only sees the connector disappear. `ACLConnector` (`proto/client/v1/client.proto:198`) and `TransportConnector` (`:266`) carry id, addr, `connector_spiffe`, relay fields, and **no status**. The only explicit server→client trust signal, `DeviceDirective` (`:190`), is about the client's **own device** | CONFIRMED BY SOURCE |
| A3 | Can existing sync convey "revoked identity" rather than "absent"? | **No.** Absence also results from disconnect (`retireStream`), the watcher and shutdown (Goodbye). The snapshots have no field that could carry the difference | CONFIRMED BY SOURCE → **NOT FEASIBLE WITH CURRENT INTERFACES** (needs an additive proto field/message or a new endpoint) |
| A4 | Order of notification and sync | (1) DB UPDATE commits; (2) policy version bump + ACL push to all registered connectors (the revoked one included); (3) transport version bump (no push); (4) mutation returns; (5) ≤ 15 s later the revoked connector's next health → stream closed; (6) clients converge at their own poll (0–60 s, phase-dependent); (7) CRL reflects the serial on the next fetch by each consumer (relay/connector 60 s + 0–15 s jitter) | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE: stream closed 3 s (10-13) and 12.6 s (10-12) after revoke; serial on CRL 5.35 s after `revoked_at` (10-13) |
| A5 | Can the controller tell a connected connector "terminal revocation" with an existing protocol? | **Partially.** gRPC status `PermissionDenied` with the message `"connector revoked"` (mid-stream, `:568`) or `"connector is revoked"` (open/activation, `:440`, `:622`). There is no structured reason field; `PermissionDenied` is also used for "wrong role" (`:427`) and "trust domain not accepted" (`:1005`). There is no in-band revocation message in the `ConnectorControlMessage` oneof (`proto/connector/v1/connector.proto:69`) | CONFIRMED BY SOURCE → PARTIAL (code + message string usable without proto change; a structured reason needs an additive proto change: OPEN DECISION) |
| A6 | Is a revoked connector rejected whatever serial it presents? | **Yes, for the controller.** The interceptor verifies the chain only (`spiffe.go:256` `verifyConnectorCertificate`). Handlers check status by `(connector id, trust_domain)` and never consult the serial | CONFIRMED BY SOURCE; CONFIRMED BY TEST (A1 tests, ID-based). No test explicitly presents an **older** certificate |
| A7 | What happens on reconnect after revocation? | `Control` → `PermissionDenied "connector is revoked"`; no activation, no registration, no ACL push, no notify | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE (backoff 4/8/16/32/60 s, 10-13) |
| A8 | Authorization by identity, serial, or both? | **Identity** (SPIFFE → `connectors.id` + `trust_domain`) plus chain validity. The serial isn't used on the controller's connector paths. (Relays, by contrast, are serial-checked in the interceptor, `:1017-1024`) | CONFIRMED BY SOURCE |
| A9 | Can an authenticated identity be mapped to workspace, record and state? | **Yes.** `parseSPIFFEID` (`spiffe.go:110`) → `SELECT status, tenant_id, revoked_at FROM connectors WHERE id=$1 AND trust_domain=$2` (`control_stream.go:433-435`) | CONFIRMED BY SOURCE |
| A10 | Does a hard delete leave a durable revocation record? | **No.** `DeleteConnector` (`connector.resolvers.go:158-178`) `DELETE`s `pending`/`revoked` rows. Afterwards: (a) the serial leaves the CRL (the query reads `connectors`); (b) controller RPCs return `NotFound "connector not found"`, not "revoked"; (c) no tombstone exists. This contradicts the comment in `controller/migrations/029_connector_revocation.sql:4-5` ("Rows are never deleted"; "the connector inner mTLS reject it") | CONFIRMED BY SOURCE |

Additional controller facts relevant to design:
- **Ambiguous NotFound** (CONFIRMED BY SOURCE): `Control` maps **any** `Scan` error, including a transient DB error, to `codes.NotFound "connector not found: …"` (`control_stream.go:436-437`). A connector that treated `NotFound` as "deleted ⇒ terminal" would self-revoke on a controller DB hiccup. Any terminal-state design must account for this (§19, §22).
- **Health-update DB error doesn't close the stream** (`:785-786`). Only "no row" (revoked or deleted) does.
- **No authorization recovery workflow for revoked connectors** (CONFIRMED BY SOURCE): the schema exposes only `generateConnectorToken`, `revokeConnector`, `deleteConnector` (`connector.graphqls:30-36`); no un-revoke mutation exists (repo grep for `unrevoke|reactivat|restoreConnector|reinstate` finds only SCIM/user paths). Enrollment requires `status='pending'` (`enrollment.go:109`). **The only way to resume service after revocation today is a new connector identity (new UUID).** R5's "explicitly authorized recovery process" doesn't exist; none is proposed here.
- **Controller liveness detection** (CONFIRMED BY SOURCE): `RunDisconnectWatcher` ticks every `CONNECTOR_HEARTBEAT_INTERVAL` (default **30 s**; despite the name, this is the watcher period, not the connector's 15 s cadence) and marks `active` rows `disconnected` when `last_heartbeat_at < now − CONNECTOR_DISCONNECT_THRESHOLD` (default **90 s**) (`cmd/server/main.go:194-195`; `disconnect_watcher.go:14,47-53`). No gRPC keepalive is configured on the controller server (repo grep: no `keepalive` in `controller/cmd` or `controller/internal`).

## 6. Connector self-enforcement and heartbeat deadline (Investigation B)

### 6.1 Startup order: what is exposed before authorization (A. STARTUP GATE)

`connector/src/main.rs` `main` (CONFIRMED BY SOURCE):

| Order | Step | Line | Listening before controller authorization? |
|---|---|---|---|
| 1 | load or enroll state; `CertHolder::load` | `:64-92` | — |
| 2 | `controller_client::build_channel(...).connect().await?` for the ShieldRegistry proxy | `:100-102` | Needs the controller TLS endpoint reachable, or **the process exits**. Not an authorization check: the interceptor runs per RPC, and a revoked connector completes this TLS connect |
| 3 | Shield-facing gRPC server `:9091` | `:131-140` | **Yes.** Accepts shields (SPIFFE match, `agent_server.rs` `verify_shield_identity:487`) |
| 4 | Workspace CRL + relay CRL initial fetch, then refresh 60 s + 0–15 s jitter | `:184-206` | — (an HTTP GET of `/ca.crl`, which needs no auth: `ca_endpoint.go:41`) |
| 5 | Device tunnel TLS listener `:9092/tcp` | `:216-232` | **Yes** |
| 6 | Device tunnel QUIC listener `:9092/udp` | `:234-260` | **Yes** |
| 7 | Relay selector (attaches to relays from the controller-pushed list) | `:268-314` | Gated in practice: the relay list arrives only on an accepted Control stream |
| 8 | `run_control_stream` (first Control stream open) | `:320-332` | — |

Can these listeners **serve** before authorization?
- `PolicyCache` starts as `None` (`policy/mod.rs:22-26`) and is held only in memory; it isn't persisted.
- `handle_stream` resolves every request against the cache (`device_tunnel.rs:187`). `resolve_resource`/`is_allowed` return deny when there is no snapshot (`policy/mod.rs:87-91`, `:132`).
- So until the first `AclSnapshot` arrives, **every new device stream is denied** (`access denied`, `no_acl_match`), on all three entry paths (QUIC, TLS, relay-routed via `relay_handler.rs:187`). **CONFIRMED BY SOURCE.**
- The first ACL is pushed only after the controller accepts a Control stream (initial health → `pushACLSnapshot`, `control_stream.go:837`, `:892`). That stream is refused for a revoked connector.

Consequences:
- A **restarted revoked** connector serves no device traffic. That is **INFERRED** from source and consistent with HISTORICAL LIVE EVIDENCE: in Finding 34 access ended only when the process restarted (`connector rejected authentication — failing closed`).
- That gate is a **side effect** of an empty cache, not an explicit authorization state. A shield can still attach to `:9091`, and the listeners are open.
- An **authoritative authorization result before exposing the data plane** is obtainable from existing behaviour: the controller sends its first message (an initial `Ping`, `control_stream.go:511-520`) only after the revocation check and `activateStream`. So `client.control(...).await` resolving successfully (`connector/src/control_stream.rs:237-240`) means the controller accepted this identity at that moment. **CONFIRMED BY SOURCE** on the controller side. The connector doesn't currently use it as a gate.

**Status: PARTIAL.** New device streams are denied until a first ACL arrives. Listeners are open; there is no explicit serving state; the shield `:9091` accepts shields regardless.

### 6.2 Control-plane loss detection (B. CONTROL-PLANE LOSS)

| Item | Current source | Label |
|---|---|---|
| Health cadence | `HEALTH_INTERVAL_SECS = 15`, `tokio::time::interval` created per session, first tick consumed (`control_stream.rs:426-428`); initial health sent at open (`establish_stream:187-204`). This build's cadence is **15 s** | CONFIRMED BY SOURCE |
| Transport | one persistent bidirectional gRPC stream (`ConnectorService.Control`), a fresh tonic `Channel` per stream (`open_stream:211`) | CONFIRMED BY SOURCE |
| What "sending a heartbeat" means | `send_msg` = `mpsc::Sender::send` into a 64-slot buffer (`:115-122`, `:235`). Success means **enqueued locally**, not delivered, not processed | CONFIRMED BY SOURCE |
| Heartbeat acknowledgement | **none.** The controller answers health only with an ACL push when `acl_version` is stale (`control_stream.go:837`) and with `ReEnroll` when renewal is due. When the ACL is current and no renewal is due, the controller sends **nothing** per heartbeat. `Ping` is sent once per stream at open (`:514-520`) | CONFIRMED BY SOURCE |
| gRPC/HTTP2 keepalive | none configured on the connector channel (`controller_client.rs` `build_channel:37-50`) or the controller server | CONFIRMED BY SOURCE |
| Last-success timestamp | **none** in the connector | MISSING |
| Deadline anchor | none exists | MISSING |
| Timer reset on success | none (the health ticker is a fixed-period interval; nothing records success) | MISSING |
| Loss detection today | only when the stream yields `Err`/`None` (`:441-447`) or a `send_msg` fails (channel closed). With a silent network path, detection depends on TCP/HTTP2 failure: **UNVERIFIED** timing (no keepalive; the 64-slot buffer can absorb ~16 minutes of 15 s health reports alone before `send` blocks; ack/log traffic shortens that) | CONFIRMED BY SOURCE (mechanism) / UNVERIFIED (time) |
| Backoff beyond any deadline | `run_control_stream` sleeps 2, 4 … 60 s between attempts (`:36-37`, `:100-108`), indefinitely | CONFIRMED BY SOURCE |
| Automatic stop of serving | **none.** Nothing in the control-stream path touches listeners, `PolicyCache`, `SessionRegistry`, relay attachment or shield streams | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE (10-13 R3-V: 114 new sessions accepted after the control plane was refused) |
| Stale ACL lifetime | **unbounded.** `PolicyCache` has no expiry, version check or clear (`policy/mod.rs:9-75`). The CRL's 24 h `nextUpdate` bounds only the **client-serial** check: after it, `CrlManager::check` → `Unavailable` (`crl.rs:47-58`) → `handle_stream` denies new streams (`device_tunnel.rs:143-147,166`) | CONFIRMED BY SOURCE |

### 6.3 Heartbeat-bound enforcement (C)

| Required capability | Present? | Where / what is missing |
|---|---|---|
| Reliable "successful controller communication" timestamp | **MISSING**, and today's protocol gives nothing to timestamp. A local enqueue proves nothing; no per-heartbeat reply exists (§6.2) | Needs the controller to send a per-heartbeat confirmation. Options: give the existing `Ping` message new semantics (a controller behaviour change with no `.proto` change), or add a message (additive proto change). Either way the controller must send it **only after** the revocation-guarded UPDATE succeeds (`handleConnectorHealth:754-784`) for it to mean "authorization still current". **OPEN DECISION** |
| Deadline timer independent of the retry loop | **MISSING.** The only timers are the per-session health ticker and the backoff sleeps inside `run_control_stream`; both stop or restart with the session | Needs a long-lived task or state outside `run_control_stream` |
| Serving/authorized state distinct from connection state | **MISSING.** No flag exists; serving is implicit in "listeners up + ACL cached" | Needs a shared state read by every entry point |
| Safe disable of all data-plane entry points | **PARTIAL.** Every entry path funnels through `device_tunnel::handle_stream`: QUIC (`quic_listener.rs:143-160`), TLS (`device_tunnel.rs:119`), relay-routed (`relay_handler.rs:187`). Each request re-reads `PolicyCache` (`:187`, `:256`). A cache clear or an "unauthorized" flag checked there would deny all new streams **without** closing listeners. There is no `PolicyCache::clear` (the field is private, `policy/mod.rs:9-11`). The QUIC `Endpoint` handle isn't retained beyond `listen` (`quic_listener.rs:74-75`), so refusing new QUIC **connections** would need the handle kept | INFERRED (small connector-internal change, no protocol change) |
| Close already-established data-plane sessions | **PARTIAL.** Every admitted session, both connector and shield routes, registers a `CancellationToken` (`device_tunnel.rs:250`; `session_registry.rs` `register:65`). Both copy loops `select!` on it (`device_tunnel.rs:352-363`, `:480-490`). Only `cancel_all(key)` for one `(client SPIFFE, resource)` key exists (`session_registry.rs:103`); there is no "cancel every session". QUIC connections themselves are not tracked (`quic_listener.rs:96-160` spawns and forgets) | INFERRED (registry-wide cancel is a small addition over a DashMap). Closing the QUIC **connections** isn't needed to stop data flow once sessions are cancelled and new streams are denied, but stale pooled client connections stay open until idle timeout: **OPEN DECISION** whether that is acceptable |
| Prevent caches reopening access after the deadline | **MISSING.** A late-arriving `AclSnapshot` on a still-open stream would repopulate the cache (`handle_controller_msg` ACL arm, `control_stream.rs:635-648`); there is no authorization-generation check | Needs the serving state to gate ACL application or admission |

**Conclusion (feasibility Q1): not implementable with the existing timers, state and listener controls alone.** The data-plane "off switches" are mostly present in connector-internal form: per-request ACL gating and per-session cancellation tokens. What is missing:
1. a positive per-heartbeat authorization confirmation from the controller;
2. a deadline timer anchored to that confirmation and independent of the reconnect loop;
3. an explicit serving state, plus registry-wide cancel and shield-stream handling.

Item 1 can't be built from the connector alone, because the current controller sends nothing per healthy heartbeat. **NOT FEASIBLE WITH CURRENT INTERFACES** (current controller behaviour); whether to reuse `Ping` or add a message is an OPEN DECISION.

### 6.4 Terminal revocation (D)

| Capability | Current | Label |
|---|---|---|
| Inspect gRPC status codes | **No.** Errors are flattened to `anyhow` strings (`control_stream.rs:443` `anyhow!("control stream recv error: {}", e)`; `open_stream:240` `.context(...)`). The `tonic::Status` is still reachable before flattening, so distinguishing needs only connector code, no proto change | CONFIRMED BY SOURCE / INFERRED (feasible) |
| Treat `PermissionDenied "connector revoked"` differently | **No.** Same path as any error: log + backoff + retry (`:100-108`) | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE |
| Enter a terminal revoked state | **MISSING** | — |
| Stop QUIC listeners | **MISSING** (no endpoint handle kept, §6.3) | CONFIRMED BY SOURCE |
| Close existing sessions | **PARTIAL** (tokens exist; no registry-wide cancel) | §6.3 |
| Close shield-facing channels | **MISSING.** `ShieldRegistry` keeps per-shield senders (`agent_server.rs:740-750`); the per-shield task ends only when the shield's stream ends (`:905-915`). No API closes a shield stream from the connector | CONFIRMED BY SOURCE |
| Invalidate ACL cache | **MISSING** (no clear) | CONFIRMED BY SOURCE |
| Prevent the reconnect loop restoring service | Today nothing needs restoring because nothing is stopped. After a stop, the existing loop would reconnect and receive a fresh ACL on any accepted stream; with an explicit state that is safe **only** if re-enable requires the authorization confirmation (§6.5) | INFERRED |
| Stay disabled across process restart | **De facto yes, by accident**: a restart starts with `PolicyCache = None`, and the controller refuses the stream, so no ACL ever arrives (§6.1). Nothing is persisted that says "revoked"; a restarted revoked connector keeps its listeners and accepts shields | INFERRED (source) + HISTORICAL LIVE EVIDENCE (Finding 34) |

**Conclusion (feasibility Q2): a terminal revoked state can be driven by the existing control-plane error** (`PermissionDenied` + message), with connector-only changes. Caveats:
- the signal is a status code plus a free-text message, shared with non-revocation `PermissionDenied` causes (§5 A5);
- `NotFound` is ambiguous, because the controller returns it for DB errors too (§5);
- detection of a mid-stream revocation waits for the next health report (≤ 15 s + processing), because the resolver pushes nothing to the revoked connector.

Whether to rely on the message string, add a structured reason, or push an explicit revocation message on the live stream at revoke time is an **OPEN DECISION**.

### 6.5 Reconnection classification (E)

What the connector can tell apart today, from existing signals (all errors take one path today):

| Condition | Observable signal in current source | Distinguishable? |
|---|---|---|
| Temporary network interruption | transport/connect error at `build_channel` / `control()` (`open_stream:229-240`) | yes (by error kind; not done today) |
| Controller unavailable | same as above; or SPIFFE preflight failure (`:224-226`) | not reliably from a network interruption |
| Authorization state unavailable (controller DB error) | `NotFound "connector not found: <db err>"` at open (`control_stream.go:436-437`); mid-stream health DB errors are swallowed (`:785-786`) | **No**: it collides with "deleted" |
| Explicitly revoked | `PermissionDenied "connector is revoked"` (open) / `"connector revoked"` (mid-stream) | yes (code + message) |
| Certificate renewal failure | `renew()` → `false`, logged, stream kept (`control_stream.rs:292-309`); an expired cert fails the open with `Unauthenticated` from the interceptor (INFERRED from `verifyConnectorCertificate`) | partially |

Service gating on fresh authorization after reconnect: **not gated** today. Nothing was stopped, so nothing is re-enabled. The accepted Control stream (§6.1) plus its first `AclSnapshot` is the existing point where authorization is confirmed at reconnect. **Feasibility Q3:** startup and reconnect gating are **feasible** using that existing point plus connector-internal state; the gate itself is MISSING.

### 6.6 Own CRL checking (F)

- **The connector does not check its own serial** (CONFIRMED BY SOURCE). The current CRL consumers in the connector are:
  - `device_tunnel.rs:166`: the **client device** serial, once per stream;
  - `relay_client.rs:304` and `monitor_relay_revocation:466-478`: the **relay** serial, at connect and then every 1 s on the live relay connection.
- What a self-check would and would not do:

| Problem | Would a self-serial CRL check fix it? |
|---|---|
| Identity-level revocation | **Only for the current serial.** The CRL lists only `connectors.cert_serial` (§11.3). It can't express "all certificates of this SPIFFE ID" (R11) |
| Certificate-history gap | **No.** Earlier serials are never listed (HISTORICAL LIVE EVIDENCE, 10-13 R4) |
| Established-session termination | **Only if** it is wired to the same stop/cancel machinery that §6.3 finds missing |
| Detection latency | 60–75 s refresh, worse than the ≤ 15 s control-plane signal; useful only as a second, controller-independent channel while the control stream is down. It needs the controller's HTTP endpoint, so it doesn't help a fully isolated connector |

**Conclusion:** a CRL self-check is **not sufficient alone** (INFERRED). At most it is a secondary signal.

## 7. Client enforcement (Investigation C)

### 7.1 Answers

| # | Question | Answer | Label / evidence |
|---|---|---|---|
| 1 | Workspace CRL for connector certs? | **No.** The client fetches only `/relay.crl` (`daemon.rs:624-633`; `client/src/crl.rs`) | CONFIRMED BY SOURCE |
| 2 | Explicit revocation reason? | **No** (§5 A2) | CONFIRMED BY SOURCE |
| 3 | Revoked vs disconnected? | **No.** Both are absence | CONFIRMED BY SOURCE |
| 4 | Direct path SPIFFE check | **Prefix** `spiffe://<td>/connector/` (`tunnel_pool.rs:341`, `PrefixSpiffeVerifier:491-535`); any workspace connector passes | CONFIRMED BY SOURCE; no dedicated test found |
| 5 | Relay path | **Exact** `connector_spiffe` from the snapshot (`relay_pool.rs:134` → `ExactSpiffeVerifier`, `tunnel_pool.rs:72`); the relay hop has its own CRL monitor (`relay_pool.rs:253-272`, 1 s) | CONFIRMED BY SOURCE; CONFIRMED BY TEST `exact_spiffe_verifier_rejects_wrong_spiffe`, `relay_connection_rejects_revoked_serial` |
| 6 | Map a flow to its connector? | **No.** `ClientTransport` has no connector id (`transport.rs:81-87`; only `RelayContext` has one, `:77`); `ActiveRelay` has none (`net_stack.rs:232-245`) | CONFIRMED BY SOURCE |
| 7 | Identify and terminate all flows using a revoked connector? | **No** (follows from 6). Per-resource-key close exists (`net_stack.rs:555` `remove_listeners_and_close`), but it is keyed by resource, not connector | CONFIRMED BY SOURCE |
| 8 | Does removing a connector from the map close its QUIC connections and flows? | **No.** A flow keeps only its authenticated stream; the map swap leaves it running | CONFIRMED BY TEST `established_flow_survives_map_swap_that_removes_its_connector` (`net_stack.rs:1485`); HISTORICAL LIVE EVIDENCE (10-12 HOLD-D, 19 min after revoke) |
| 9 | Can stale pools carry traffic after revocation? | **Until the next map rebuild, yes.** `get_or_connect` reuses any pooled connection with `close_reason().is_none()` (`tunnel_pool.rs:374-400`), so no new handshake runs and certificate expiry is never re-checked. A rebuilt map makes fresh pools per connector, so a removed connector's pool is no longer reachable for **new** flows | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE (Finding 34; 10-13 R3-V, likely pre-revoke connection: INFERRED) |
| 10 | Offline during revocation | A running client keeps its cached snapshots indefinitely (`daemon.rs:5065`, `:5078` "keeping cached snapshot"; `:4722` comment "fail-open on staleness") and keeps selecting the revoked connector | CONFIRMED BY SOURCE (mechanism); time bound: none |
| 11 | Sync before reusing old state on reconnect? | **Daemon restart: yes.** Snapshots live only in memory (`runtime.rs:134,140`, initialised `None` at `:235-237`) and `up_preflight` fails closed without an ACL snapshot (`daemon.rs:765`; test `up_preflight_without_acl_snapshot_fails_closed`). **Network recovery while running: no.** There is no "reconnected" event; the cached map is used until the next 60 s tick or an early resync | CONFIRMED BY SOURCE |
| 12 | Controller unavailable during sync | Keeps the cached snapshot (running); refuses `up` (fresh daemon, never synced) | CONFIRMED BY SOURCE |
| 13 | Achievable with existing snapshot and flow interfaces | Not selecting an absent connector for **new** flows (EXISTS); fresh pools on rebuild (EXISTS); per-resource-key close (EXISTS, wrong key) | — |
| 14 | Needs new identity state, flow ownership, events or termination | An explicit revoked-connector signal (proto); flow → connector ownership; per-connector flow close command in the smoltcp loop; a staleness bound (policy decision) | MISSING / NOT FEASIBLE WITH CURRENT INTERFACES (signal) |

### 7.2 New-connection rejection vs established-flow termination (client)

| Window | New connection | Established flow |
|---|---|---|
| Revoked, before client sync | **accepted through the revoked connector** (if the connector still accepts, §6) | continues |
| After client sync | not routed to it (absent); falls through the other listed connectors or fails closed (`net_stack.rs:675-677`) | **continues** (no ownership, no close) |
| Client offline | as before sync, indefinitely | continues |

### 7.3 Interaction with connector self-enforcement (INFERRED, design-relevant)

`relay_tcp_to_quic` (`net_stack.rs:1064`) tries the next connector only on transport/auth/handshake failure or the reply `SHIELD_NOT_ATTACHED` (`:1157-1164`). **Any other denial reply returns immediately** (`:1166-1167`, `tunnel denied: …`), and when every failure was a denial it fails closed **without** signalling resync (`:1176-1186`).

If a self-disabled connector answers new streams with an in-band denial, the client:
- will not fall back to an eligible alternative connector for that flow;
- will not resync early.

Recovery waits for the controller to mark the connector `disconnected` (watcher, ≤ 90 s + 30 s; §5) **and** the client's next tick (≤ 60 s). If the connector refuses at the transport level instead, the client falls back and resyncs.

So the *way* a connector refuses decides whether R6's "preserve operation through eligible alternatives" holds: **OPEN DECISION** (§22).

## 8. Relay enforcement (Investigation D)

**Runtime caveat:** relay revocation has **never been exercised live**. It was BLOCKED in 10-12 and 10-13, because LAN-only relays get no `public_addr`. Every relay statement below is source-only.

| # | Question | Answer | Label |
|---|---|---|---|
| 1 | Consumes workspace CRL? | **Yes**, per workspace, background refresh 60 s + 15 s jitter (`relay/src/main.rs:69`, `crl.rs` `spawn_refresh:114`) plus fetch-on-miss (`crl.rs` `check:53-80`) | CONFIRMED BY SOURCE; CONFIRMED BY TEST `revoked_denied_fresh_allowed`, `expired_crl_fails_closed` |
| 2 | Checks connector serial on new handshake? | **Yes**, after the mTLS handshake: `tls::peer_revocation` → `crl.check(workspace, leaf_serial, ca)` → close on `Revoked`/`Unavailable` (`listener.rs:156-173`) | CONFIRMED BY SOURCE; CONFIRMED BY TEST `rejects_revoked`, `fails_closed_on_unavailable`, `normal_chain_is_checked_against_its_own_workspace_crl` |
| 3 | Gets authoritative status beyond the CRL? | **No** | CONFIRMED BY SOURCE |
| 4 | Maps SPIFFE ID → connector record/status? | Maps to its **own registry** entry only: `Register` must match the authenticated entity id (`session.rs:123-128`); `RelayState` keys `ConnectorEntry{connection,…}` by `connector_id` (`state.rs:7-9,24`). There is no DB or status | CONFIRMED BY SOURCE |
| 5 | Refreshes CRL while running? | **Yes** (1) | CONFIRMED BY SOURCE |
| 6 | Rechecks established sessions on CRL change? | **No.** No monitor over accepted peers (repo grep: no `monitor`/`recheck` in `relay/src` outside tests) | CONFIRMED BY SOURCE |
| 7 | Can it terminate every session of a revoked connector? | **Not today.** A connector's registered `Connection` is held in `ConnectorEntry` (`state.rs:9`), so closing it is **mechanically possible**. Client-side bridged streams through that connector would then fail. No trigger exists | INFERRED (capability) / MISSING (trigger) |
| 8 | Records authenticated identity against sessions? | **Yes for connectors** (registry keyed by the authenticated `connector_id`); the serial isn't stored, but is re-derivable from `Connection::peer_identity` | CONFIRMED BY SOURCE / INFERRED |
| 9 | Loses access to updated CRL | keeps last good until `nextUpdate`, then `Unavailable` → reject **new** peers; established sessions untouched | CONFIRMED BY SOURCE |
| 10 | What 24 h `nextUpdate` guarantees | that a **new** handshake is rejected if the relay holds no CRL fresher than 24 h; it says nothing about established sessions or earlier serials | CONFIRMED BY SOURCE |
| 11 | Implementable with existing mechanisms | new-handshake rejection of the **current** serial (exists); closing a registered connector connection (mechanism exists, trigger missing); a periodic re-check of registered connectors against the CRL modelled on the client/connector relay-monitor precedent (`relay_pool.rs:253`, `relay_client.rs:466`) | INFERRED |
| 12 | Missing | identity-level status (needs a new distribution channel to relays: **NOT FEASIBLE WITH CURRENT INTERFACES**); serial history (schema, §11.3); established-session re-check; any binding of client-bridged sessions to a connector beyond the registry | MISSING |

## 9. Shield enforcement (Investigation E)

### 9.1 Answers

| # | Question | Answer | Label |
|---|---|---|---|
| 1 | Receives authoritative revocation? | **No.** Its only inbound control channel is its connector's `:9091` stream; the controller has no path to it except via a connector | CONFIRMED BY SOURCE |
| 2 | Consumes CRL? | **No** (`grep -rni 'crl\|revok' shield/src` → no matches) | CONFIRMED BY SOURCE |
| 3 | Exact SPIFFE + enrolled association? | **Yes**: `spiffe://<td>/connector/<state.connectors[i].connector_id>` (`shield/src/tls.rs` `verify_connector_spiffe:51-90`), chain checked by webpki at handshake (`:138-170`) | CONFIRMED BY SOURCE; no shield TLS tests found |
| 4 | Can it determine its connector was revoked? | **No** | CONFIRMED BY SOURCE |
| 5 | Peer list distinguishes revoked/disconnected? | **No.** `PeerConnector{connector_id, connector_addr}` only; an empty list is ignored (`control_stream.rs` `apply_peer_connector_list:397-403`); the list is persisted in `state.json` (`:439`) | CONFIRMED BY SOURCE |
| 6 | Does closing the connector's controller stream close the shield channel? | **No.** They are independent; the shield channel is its own gRPC stream to the connector's `:9091` | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE (10-13 R2: link stayed up to 15:06) |
| 7 | Can the shield terminate all traffic through a revoked connector? | Not by itself. Shield-routed sessions are multiplexed **inside** the shield's control stream (`TunnelOpen`/`TunnelData`/`TunnelClose`, `control_stream.rs:358-376`; per-`run_once` `tunnel_hub`, `:154`), so ending that stream ends all of them. The shield never ends it on revocation | CONFIRMED BY SOURCE (mechanism) / INFERRED (effect of ending the stream) |
| 8 | Established protected connections | continue | HISTORICAL LIVE EVIDENCE (10-13 R2: 702 × 200 incl. 85 after the revoked cert expired) |
| 9 | Attach to a healthy alternative? | Only on connection **failure**: `build_client` tries `state.connectors` in order and rotates the list to the survivor (`run_once:126-142`); `reconnect_loop:99-123` retries with 5 → 60 s backoff (`:28-29`) | CONFIRMED BY SOURCE; HISTORICAL LIVE EVIDENCE (no failover on revocation, 10-13 R2) |
| 10 | Does re-attachment restore reachability / preserve sessions? | New sessions through the new connector: INFERRED yes, once the client selects it (the alternative answers `SHIELD_NOT_ATTACHED` until the shield attaches to it: `agent_tunnel.rs:107-119`; 10-13 R2: 379 such replies from B). Existing sessions: **not preserved**; `TunnelOpen` is per session and the client flow is pinned (PENDING-18) | INFERRED / CONFIRMED BY SOURCE |
| 11 | Stale revocation/peer info | unbounded; persisted across restart; no expiry | CONFIRMED BY SOURCE |
| 12 | Existing vs new | see matrix §14 | — |

### 9.2 Why the revoked connector stopped sending `PeerConnectorList` (resolves 10-13 §13 Q5)

**CONFIRMED BY SOURCE**, and consistent with HISTORICAL LIVE EVIDENCE: the last list was at 14:49:57 and the post-revoke ACL v7 arrived at 14:50:05.480.
1. The connector builds the list from its own ACL: `build_peer_connector_list` → `policy_cache.peers_of_connector(self.connector_id)` (`agent_server.rs:206-223`).
2. `peers_of_connector` returns empty when the connector isn't listed in any remote network (`policy/mod.rs:113-129`).
3. The post-revoke ACL omits the revoked connector (compilers filter `status='active'`, §4 step 3).
4. An empty list makes `build_peer_connector_list` return `None`, so nothing is sent (`:218-220`). The shield ignores empty lists anyway.

So the shield's peer list is **frozen** at the pre-revocation content.

The same mechanism gives the connector an **implicit** hint that it is no longer active: its own id is missing from its ACL. That is not authoritative, because the disconnect watcher can mark a still-streaming connector `disconnected` (heartbeat age > 90 s), and that ACL would omit it too (INFERRED). It must not be used as a revocation signal on its own.

### 9.3 Failover vs migration (kept separate)

- **Failover** (the shield re-attaching to another connector) exists only for connection failure. Triggering it on revocation would need either a shield-side signal (NOT FEASIBLE WITH CURRENT INTERFACES: no channel) or the revoked connector closing the shield stream (connector-internal; MISSING, §6.4). Whether a revoked connector *should* push its shields away is an **OPEN DECISION**.
- **Session migration**: NOT FEASIBLE WITH CURRENT INTERFACES (per-session `TunnelOpen` to one connector; client TCP flow pinned to one stream). It is PENDING-18 scope, deferred.

## 10. Existing-session ownership and termination

| Path | Who owns the session | Identity recorded | Termination hook that exists | Triggered by connector revocation today? |
|---|---|---|---|---|
| Client app flow (smoltcp) | client `FlowTable`/`ActiveRelay` (`net_stack.rs:232`) | resource key only | per-resource-key abort (`remove_listeners_and_close:555`) | **No** |
| Client → connector QUIC connection | client `TunnelPool` per connector (fresh per map build) | none (prefix verifier) | implicit close when the last ref drops; relay-hop monitor only for relays | **No** |
| Connector device session (direct / TLS / relay-routed, connector or shield route) | connector `SessionRegistry` (`session_registry.rs:51-52`) keyed `(client SPIFFE, resource)` with a `CancellationToken` | client identity | `cancel_all(key)` (`:103`) used by ACL diff (`control_stream.rs:638-646`) | **No** (keyed by client, never invoked for connector revocation) |
| Connector ↔ shield stream (carries all shield sessions) | connector `ShieldRegistry`/`AgentTunnelHub`; shield `run_once` `tunnel_hub` | shield id; connector id (shield side, exact) | ends on stream close (both sides) | **No** |
| Relay ↔ connector registration | relay `RelayState` (`state.rs`) | authenticated `connector_id` | `ConnectorEntry.connection` closable | **No** |
| Connector ↔ relay (as client) | connector `relay_client` | relay serial | `monitor_relay_revocation` (relay revocation only) | n/a |
| Controller control stream | controller registry | connector id | closed at next health | **Yes** (control plane only) |

The **only** existing identity-level revocation-with-teardown is for **client devices**:
- ACL `allowed_spiffe_ids` → `update_and_revoked` → `SessionRegistry::cancel_all` (CONFIRMED BY TEST `access_removal_still_revokes_pair`, `tuple_change_revokes_all_previously_allowed_spiffes`);
- the client-side `DeviceDirective::REVOKED` path (CONFIRMED BY TEST `revoked_wipes_credentials_stops_and_records_state`, `daemon.rs:6202`).

Both are precedents; neither applies to connector identities.

## 11. ACL, topology, CRL and SPIFFE identity relationships (Investigation F)

### 11.1 ACL
- Per-resource `ACLEntry` with `allowed_spiffe_ids` (client identities), route type, `shield_id`, `remote_network_id`, `preferred_connector_id`. Connectors appear only in `ACLRemoteNetwork.connectors` (`client.proto:198-235`).
- Revocation changes ACL content **only** by removing the connector from `remote_networks` (and possibly changing `preferred_connector_id` if the shield binding changes; it doesn't on revoke). The entries' `allowed_spiffe_ids` are unchanged.
- Versioned by an in-memory counter (resets on controller restart); cached at the controller (invalidate on notify) and in memory on client and connector. **No expiry** anywhere.
- Client-side effect of a change: `classify_delta` → TransportOnly hot-apply / Structural restart (Fix 01 2-A/5-C); new flows only.

### 11.2 Topology (transport snapshot)
- Active connectors per RN, ordered `last_heartbeat_at DESC` (`transport/store.go:61-62`), with exact `connector_spiffe`. No status field.
- Absence ≡ {disconnected, watcher-marked, Goodbye, revoked, deleted}. No push; poll only.

### 11.3 CRL
- Workspace CRL generated per request (`GenerateClientCRL`, `pki/workspace.go:530`): revoked devices + `connectors WHERE revoked_at IS NOT NULL AND cert_serial IS NOT NULL` (`:583-608`). `thisUpdate = now − 5 min`, `nextUpdate = now + 24 h` (`:622-627`, `crlWindow`).
- Serial storage: one column. Renewal overwrites it (`enrollment.go:267`). No history table for connectors (the relay precedent is `relay_certificates`, migration 027).
- Consumers: connector (client serials, 60 s + jitter, keep-last-good, 24 h → deny new streams); relay (connector/client serials at handshake); client (relay CRL only); shield (none).
- Enforcement applies to **new** handshakes and streams only; no consumer re-checks established **connector** sessions.
- CONFIRMED BY TEST: no test covers the connector branch of `GenerateClientCRL` (`crl_window_test.go:71` sets up the table but inserts no revoked connector; `device_trust_handler_test.go` covers devices).

### 11.4 SPIFFE identity
- Construction: `spiffe://<workspace trust_domain>/connector/<connectors.id>` (`controller/internal/appmeta/identity.go:85`; connector `appmeta::connector_spiffe_id`). Stable across renewal **by construction**: `RenewConnectorCert` re-derives the URI (`pki/workspace.go:219`). No connector test asserts SPIFFE equality across renewal (per the 10-13 identity investigation; not re-tested here).
- Unique: PK UUID + unique `trust_domain`. Reuse after deletion: prevented only by UUID randomness (no tombstone, §5 A10).
- Exact identity checks per component:

| Component | Check |
|---|---|
| controller | exact, via DB |
| client relay path | exact |
| shield | exact |
| client direct path | **prefix** |
| relay | role + chain + serial, register bound to the authenticated id |

- **Same identity with an earlier, still-valid certificate:**
  - the controller rejects it if the identity is revoked (ID-based);
  - the relay accepts it if its serial is not on the CRL (earlier serials never are);
  - client and shield accept it (no revocation check).

  That last point is INFERRED from source; R4 in 10-13 confirmed the absence from the CRL live.

### 11.5 Can the architecture represent both kinds of revocation?

| Kind | Representation today | Carrier today | Gap |
|---|---|---|---|
| Administratively revoked **identity** | DB `connectors.status/revoked_at` (authoritative) | controller only (gRPC status); absence in snapshots | no component outside the controller receives it; no snapshot field; hard delete erases it |
| Individual revoked **certificate** | the CRL entry for the current serial | `/ca.crl` (relay enforces at handshake) | no serial history; not consumed by client (connector certs) or shield |

Adding a SPIFFE ID **into** a standard X.509 CRL is **not** a valid representation: RFC 5280 CRL entries identify certificates by serial number under the issuer. Identity-level state therefore needs its own carrier (INFERRED; consistent with R11).

**Candidate carriers** (no choice made: OPEN DECISION):
- identity state via a new field or message in the existing ACL/transport snapshots, or an explicit control-plane message (connector), alongside the CRL for certificates;
- relays and shields would need a new channel, because they receive neither snapshot today (relays get only the CRL; shields get only their connector's messages).

**Feasibility Q8:** keeping the CRL for certificate-level revocation while identity state governs administrative revocation is **feasible as a model**. The controller already separates the two (ID-based RPC checks, serial-based CRL). Only the controller *enforces* identity state today.

## 12. Offline and stale-authorization behaviour (Investigation I)

A = authoritative revocation received; B = not received, can still communicate; C = isolated (cached state only); D = reconnects after isolation. All entries are CONFIRMED BY SOURCE unless labelled.

| Component | A | B | C | D |
|---|---|---|---|---|
| **Connector** (current) | gets `PermissionDenied` at the next health; keeps serving | keeps serving; the controller closes it at the next health | serves indefinitely on the cached ACL; new device streams denied only after the 24 h CRL `nextUpdate`; a restart → no ACL → denies all new streams (de facto) | reconnects; if not revoked, gets an ACL; if revoked, refused but keeps serving whatever it had |
| Connector (R4/R10 intent) | terminal stop immediately | serve while fresh | stop at the heartbeat deadline | re-enable only after authorization is confirmed |
| **Client** | never receives one | converges at the next poll (absence); established flows continue | keeps the cached map indefinitely (no bound) | uses the cached map until the next tick/resync; a restarted daemon must sync before `up` |
| **Relay** | never receives one (only a CRL serial) | rejects a **new** handshake with the current serial after its CRL refresh | keeps the last good CRL ≤ 24 h, then rejects all new peers | resumes refresh |
| **Shield** | never | never (the peer list freezes, §9.2) | keeps `state.json` peers indefinitely | reconnects to the head connector; would re-attach to a revoked connector whose process is still up (INFERRED: the connector's `:9091` doesn't check its own status) |

**Policy for components other than the connector: OPEN DECISION.** One-heartbeat connector enforcement would **not** give a one-heartbeat guarantee for clients, relays or shields:
- they have no heartbeat-bound authorization of connectors;
- client snapshots fail open on staleness;
- the relay CRL bound is 24 h;
- the shield has no bound.

No numeric timeouts are proposed here.

What connector self-enforcement does cover for the others (INFERRED): if the connector itself stops admitting streams and cancels its registered sessions, client flows and shield sessions **through that connector** stop carrying application data however stale the client, relay or shield state is. Every data path to a resource passes through the connector's `handle_stream`; relay-routed streams arrive via `relay_handler.rs:187`. This holds only while the connector process is honest, i.e. not compromised. Protecting against a compromised or modified connector binary needs enforcement elsewhere (relay, client, shield), which the current interfaces can't express (§11.5).

## 13. Certificate renewal and identity lifecycle (Investigation H, renewal aspects)

- **Make-before-break (EXISTS)**:
  - connector `run_session` switch-and-drop (`control_stream.rs:394-505`);
  - controller `lifecycleMu` + `retireStream` current-pointer check (`control_stream.go:604-668`);
  - QUIC config swap keeps existing connections (CONFIRMED BY TEST `quic_swap_serves_new_cert_and_preserves_existing_connections`);
  - the shield-proxy channel is rebuilt by a `CertHolder` subscriber (`main.rs:121-129`);
  - CONFIRMED BY TEST `renewal_opens_new_stream_first_then_switches_and_drops_old` and related.
  - Earlier live runs (U5, F3) showed renewals without ACL/transport bumps (historical).
- **Renewal is itself an authorization checkpoint:** the replacement stream's `Control` re-runs the revocation check (`:433-441`). A renewal racing a revoke gets no certificate (`TestRenewCert_RaceRevokeReturnsNoCert`).
- **Separation from revocation:** a renewal never produces `PermissionDenied`. Its failure modes are `renew() → false` (stream kept) or an old-cert-expiry fallback reconnect (`:502-505`). A revocation detector keyed on `PermissionDenied` + message would not misfire on renewal (INFERRED).
- **Interaction with a heartbeat deadline (OPEN DECISION, §6.3/§22):**
  - during a pending switch the old stream keeps sending health, so a per-heartbeat confirmation would keep flowing on it;
  - at the switch, the new stream's acceptance is itself a confirmation (initial `Ping`).
  - With confirmations on both streams, the deadline need not see a gap (INFERRED). Whether confirmations from an old, superseded stream still count after the controller treats it as non-current (`superseded stream closed`) must be decided.
- **Key reuse:** renewal reuses the key (`renewal.rs:94-97`); the controller doesn't enforce same-key for connectors (10-13 identity investigation). This is relevant only to certificate-history risk, not to identity revocation.

## 14. Component-by-component feasibility matrix (Investigation G)

Columns: Requirement · Component · Current behaviour · Required behaviour · **Status** · Existing functions/interfaces · Required category of change · Relevant tests · Remaining uncertainties.

| Req | Component | Current behaviour | Required behaviour | Status | Existing functions / interfaces | Category of change | Relevant tests | Uncertainties |
|---|---|---|---|---|---|---|---|---|
| R1 Authoritative controller revocation | Controller | DB `revoked`/`revoked_at`; all writers guard it; ID-based checks on every RPC | same | **EXISTS** | `RevokeConnector`; `Control:433-441`; `activateStream`; `handleConnectorHealth`; `RenewCert:249-282`; watcher; Goodbye | none for the state itself; hard-delete semantics (see Identity reuse) | revocation_test.go (8 tests listed §5) | `NotFound` conflates deleted with DB error |
| R6/R9 Revocation notification to clients | Controller → Client | absence from ACL/transport after poll | explicit authoritative revoked-connector state | **NOT FEASIBLE WITH CURRENT INTERFACES** (no field/message); absence-based avoidance **EXISTS** | `ACLConnector`, `TransportConnector`, `DeviceDirective` (device-only precedent) | additive proto + controller compiler + client consumer | client snapshot/map tests; none for connector revocation | poll latency 0–60 s; no push |
| R2 Revocation notification to connector | Controller → Connector | `PermissionDenied "connector revoked"` at next health (≤ 15 s); `"connector is revoked"` on open | authoritative, immediate, unambiguous | **PARTIAL** | `handleConnectorHealth:779-783`, `Control:566-568` | connector: interpret status; optional controller push at revoke time; optional structured reason (OPEN DECISION) | `TestHandleConnectorHealth_RevokedClosesStream` | message-string reliance; no push at revoke time |
| R2/R3 Connector terminal revoked state | Connector | logs + backoff forever; keeps serving | terminal; data plane stopped; reconnect can't restore | **MISSING** | `run_control_stream:76-110`; `run_session:443`; `open_stream:237-240` | connector state machine + stop/cancel hooks | none | persistence across restart (OPEN DECISION) |
| R4/R5/R10 Connector startup authorization gate | Connector | listeners open first; no ACL ⇒ new streams denied (side effect); shields accepted on `:9091` | data plane exposed only after authorization confirmed | **PARTIAL** | `PolicyCache` default-deny; Control open = controller acceptance (initial Ping after `activateStream`) | explicit serving state; optional deferral of listener/shield acceptance | none for gating | exiting at `main.rs:100` when the controller is unreachable is a separate availability behaviour |
| R4/R10 Heartbeat-deadline enforcement | Connector (+ Controller) | no timestamp, no ack, no deadline, no stop | fail closed at the next scheduled heartbeat deadline | **MISSING**; per-heartbeat confirmation **NOT FEASIBLE WITH CURRENT INTERFACES** (controller sends nothing per healthy heartbeat) | health ticker (15 s), `Ping`/`Pong` message types | controller: confirmation after the guarded UPDATE; connector: deadline task + serving state | none | exact deadline semantics (§6.3/§22); RTT/scheduler jitter |
| R4 Closing active connector data-plane sessions | Connector | sessions cancelled only by per-client ACL diff | cancel all on deadline/revocation | **PARTIAL** | `SessionRegistry` tokens on every session (`device_tunnel.rs:250`); `cancel_all(key)` | registry-wide cancel; trigger | `acl_diff_session_tests` (per-key cancel) | pooled QUIC connections stay open (OPEN DECISION) |
| R4/R8 Closing connector-to-Shield channels | Connector | never closed by the connector | close or disable shield data path | **MISSING** | `ShieldRegistry`/`AgentTunnelHub` per-shield senders | connector API to end shield streams or refuse `TunnelOpen` | none | closing triggers shield failover attempts (desired?) |
| R6 Client direct-path revocation | Client | prefix SPIFFE; no CRL; absence after sync | no new flows via a revoked connector | **PARTIAL** (new flows after sync) | `PrefixSpiffeVerifier`; map rebuild | explicit signal (proto) and/or exact identity binding (OPEN DECISION) | map-swap tests | pre-sync window; offline unbounded |
| R6 Client relay-path revocation | Client | exact SPIFFE; relay CRL for the relay hop only | same as direct | **PARTIAL** | `ExactSpiffeVerifier`; relay monitor | as above | `exact_spiffe_verifier_rejects_wrong_spiffe` | relay path never live-tested |
| R6 Client established-flow termination | Client | flows survive map swap | terminate flows via a revoked connector | **MISSING** | per-resource-key abort (wrong key) | flow → connector ownership + in-loop per-connector close + trigger | `established_flow_survives_map_swap_that_removes_its_connector` (asserts the opposite, by design) | must not fire on transient absence (Q3) |
| R11 Exact SPIFFE identity validation | All | exact: controller, client relay path, shield; prefix: client direct; relay role-only | exact everywhere a connector is selected | **PARTIAL** | verifiers listed §11.4 | client direct path verifier change (OPEN DECISION) | relay-path verifier test; none for prefix | — |
| R7 Relay new-handshake enforcement | Relay | CRL serial at accept (current serial only) | reject a revoked connector's new connections | **PARTIAL** | `listener.rs:156-173`; `crl.rs` | identity state to relays (new channel: NOT FEASIBLE WITH CURRENT INTERFACES) or serial history (schema) | `rejects_revoked`, `fails_closed_on_unavailable`, relay `crl.rs` tests | **UNVERIFIED live** (BLOCKED twice) |
| R7 Relay established-session termination | Relay | none | terminate affected sessions | **MISSING** | `ConnectorEntry.connection` (closable) | periodic re-check + close (precedent: relay-revocation monitors) | none | live behaviour unknown |
| R8 Shield revocation enforcement | Shield | no signal, no CRL | stop carrying traffic via a revoked connector | **NOT FEASIBLE WITH CURRENT INTERFACES** (no channel; peer list lacks state) | `PeerConnectorList` (frozen by empty-list rule) | new signal path, or rely on connector self-enforcement (OPEN DECISION) | none | — |
| R8 Shield proxy-channel termination | Connector/Shield | stream persists | end shield-path sessions | **MISSING** (connector-side cancel of shield-route sessions is PARTIAL via `SessionRegistry`) | session tokens on shield-route sessions | registry-wide cancel; optional stream close | none | — |
| R8 Shield alternative attachment / failover | Shield | failover only on connection failure; not on revocation | evaluate failover on revocation | **PARTIAL** | `build_client`/rotate; `reconnect_loop` | trigger (connector closes stream, or a new shield signal) | none | migration NOT FEASIBLE (PENDING-18) |
| R9 Offline client synchronization | Client | restart: must sync first; running: cached map, no bound, no reconnect event | sync before reusing stale connector state | **PARTIAL** | `up_preflight` fail-closed; 60 s tick; early resync | staleness bound / reconnect-triggered sync (OPEN DECISION) | `up_preflight_without_acl_snapshot_fails_closed` | — |
| R10 Offline connector behaviour | Connector | serves indefinitely; CRL 24 h affects client serials only | stop at deadline; stay stopped until authorized | **MISSING** | see heartbeat row | see heartbeat row | none | — |
| R4/R9 Stale CRL and ACL handling | All | CRL ≤ 24 h → deny new; ACL/peer list: no bound | bounded per agreed policy | **PARTIAL** | `CrlManager::check` `Unavailable`; relay same | ACL/peer-list staleness rules (OPEN DECISION) | `expired_cached_crl_is_not_valid_or_usable`, `expired_crl_fails_closed` | — |
| R11 Certificate-level revocation and serial history | Controller/CRL | latest serial only | all unexpired serials of a revoked connector | **MISSING** | relay precedent (`relay_certificates`, migration 027) | schema + CRL query (migration: needs approval) | none for the connector CRL branch | — |
| R11 Identity reuse prevention | Controller | UUID PK; hard delete, no tombstone | durable non-reuse and retained revocation | **PARTIAL** | PK, `enrollment.go:109` pending-only | delete semantics / tombstone (OPEN DECISION; may need schema) | `TestEnroll_ConnectorNotPending` | — |
| R12 Normal renewal without interruption | Connector/Controller | make-before-break, no version bumps | unchanged, not mistaken for revocation | **EXISTS** | `run_session` switch-and-drop; `lifecycleMu` | none; must be preserved by any new deadline | renewal_switch_tests; `TestControlLifecycle_*` (per 10-13 investigation) | deadline interaction (§13) |
| R5 Authorized reactivation after revocation | Controller | no un-revoke; new identity only | explicit authorized recovery | **MISSING** (not invented here) | — | OPEN DECISION | — | — |

## 15. Existing tests and historical live evidence (Investigation J)

### 15.1 Automated tests (read, **not run**)

| Area | Tests found | Establishes | Doesn't establish |
|---|---|---|---|
| Controller revocation / ID enforcement | `revocation_test.go`: `TestControl_RejectsRevokedConnector`, `…RevokedAtLegacyStatus`, `TestGoodbye_RevokedStaysRevoked`, `TestGoodbye_ActiveGoesDisconnectedAndNotifies`, `TestRenewCert_RevokedStatusDenied`, `…RevokedAtLegacyDenied`, `…RaceRevokeReturnsNoCert`, `TestHandleConnectorHealth_RevokedClosesStream` | identity-level control-plane rejection; status stickiness | explicit older-certificate rejection; behaviour of a revoked connector |
| Connector control stream / renewal | `renewal_switch_tests` (`control_stream.rs`, e.g. `renewal_opens_new_stream_first_then_switches_and_drops_old`, `agent_policy_sets_the_shield_renewal_window`) | make-before-break; message handling | any reaction to `PermissionDenied` |
| Connector ACL session teardown | `acl_diff_session_tests` (`control_stream.rs`); `policy/mod.rs` `access_removal_still_revokes_pair`, `revokes_on_*_change_same_resource_id`, `first_snapshot_revokes_nothing` | per-client-identity session cancel | connector-identity teardown |
| CRL managers | connector `crl.rs` (`expired_cached_crl_is_not_valid_or_usable`, `keeps_last_good_crl_after_failed_validation`, B4 window tests); relay `crl.rs` (`revoked_denied_fresh_allowed`, `expired_crl_fails_closed`); client `crl.rs` window tests | parsing, validity, fail-closed on new peers | established-session recheck; connector-serial generation |
| CRL generation | `crl_window_test.go` (`TestGenerateClientCRL_BackdatesThisUpdate`), `device_trust_handler_test.go` | window; device rows | **connector rows on the workspace CRL: none** |
| Relay handshake | `listener.rs` `rejects_revoked`, `fails_closed_on_unavailable`, `normal_chain_is_checked_against_its_own_workspace_crl`; `register_must_match_authenticated_certificate` (per 10-13) | new-peer rejection; identity binding | established sessions |
| Client selection / map swap | `established_flow_survives_map_swap_that_removes_its_connector` (`net_stack.rs:1485`); restart-decision tests | flows survive map changes (by design) | revocation-triggered close |
| Client established-flow lifecycle | Fix 05-A `FlowTable` tests; `relay_mid_session_failure_reports_failed_and_resyncs` | close semantics on failure | per-connector close |
| Client relay path | `exact_spiffe_verifier_rejects_wrong_spiffe`, `relay_connection_rejects_revoked_serial`, `relay_connection_fails_closed_without_crl` | exact identity; relay-hop revocation | connector revocation |
| Shield TLS / peer list / proxy channel | K2 debounce tests in `shield/src/control_stream.rs`; **no** TLS SPIFFE or revocation tests found | — | everything revocation-related |
| Device revocation with teardown (precedent) | `revoked_wipes_credentials_stops_and_records_state` (`daemon.rs:6202`) | client-device directive teardown | — |
| Stale authorization during control-plane outage | **none** | — | — |
| Exact connector SPIFFE on the direct path | **none** (prefix verifier untested) | — | — |

### 15.2 Historical live evidence (not re-run; no new live test was executed)

| Record | Build | What it showed | Limits |
|---|---|---|---|
| Finding 34 (2026-09-28) | pre-2-A | a revoked connector kept serving existing tunnels until its process restarted, including after cert expiry; restart ended access | older client/connector builds |
| 10-12 | controller `controller-f5c` (before `4781bc8`), connector source = `2a44ab2` | existing direct and shield flows continued through revoke, client sync and cert expiry; new flows accepted until client sync; control stream closed/refused; relay BLOCKED | controller not exactly HEAD |
| 10-13 | fresh `9844a00` | R2: no shield failover, protected session survived the revoked cert's expiry; R3-V: 115 new sessions accepted before client sync (114 after control refusal), fresh handshakes **not** established; R4: serial-history gap; R1 relay BLOCKED | not HEAD `38e38c6` |

**Not established by any evidence:**
- one-heartbeat shutdown (no such behaviour exists to test);
- a fresh QUIC handshake to a revoked connector;
- relay behaviour;
- shield failover on connector process loss.

## 16. Requirements possible with existing interfaces

"Existing interfaces" here means **no protocol, schema or cross-component API change**. Code changes inside one component are still needed unless marked EXISTS.

| Capability | Basis | Status |
|---|---|---|
| R1/R3 controller authority and reconnect refusal | `Control`, `activateStream`, `RenewCert`, health guard | **EXISTS** |
| R12 make-before-break renewal | switch-and-drop + `lifecycleMu` | **EXISTS** |
| R2 terminal revocation driven by `PermissionDenied` | connector-internal status inspection | feasible, connector code only (signal is a status code + message) |
| R4 stop new streams on all entry paths | per-request `PolicyCache` gating in `handle_stream` | feasible, connector code only |
| R4/R8 cancel all connector-side sessions (connector and shield routes) | `SessionRegistry` tokens | feasible, connector code only |
| R5/R10 reconnect/startup gate on authorization | Control stream acceptance (initial Ping after `activateStream`) + first ACL | feasible, connector code only |
| R6 not selecting an absent connector for new flows | snapshot absence + map rebuild | **EXISTS** |
| R7 closing a registered connector connection at the relay | `ConnectorEntry.connection` | mechanism exists; trigger is relay code only (CRL-based, current serial only) |
| R11 CRL kept for certificate-level revocation, identity state at the controller | `GenerateClientCRL` vs ID-based checks | **EXISTS** as a model (controller-only enforcement) |

## 17. Requirements needing code or protocol changes

| Requirement | Why existing interfaces don't suffice | Smallest category of change |
|---|---|---|
| R4/R10 one-heartbeat deadline | no per-heartbeat authorization confirmation from the controller; no connector timestamp/deadline/serving state | controller behaviour (confirm after the guarded UPDATE; either reuse `Ping` semantics or add a message: OPEN DECISION) + connector deadline task + serving state |
| R2 immediate (not ≤ 15 s) revocation delivery | the resolver sends nothing to the connector's stream | controller: close the live stream or send a message at revoke time (registry lookup exists: `Registry.get`) |
| R2 structured terminal reason | free-text status only | additive proto (optional; OPEN DECISION) |
| R6/R9 explicit revocation to clients | no status in `ACLConnector`/`TransportConnector` | additive proto + compiler + client consumer |
| R6 established-flow termination at the client | no flow → connector ownership; no per-connector close | client data-plane changes (`ClientTransport`/`ActiveRelay` ownership + in-loop command) |
| R6/R11 exact identity on the client direct path | prefix verifier | client verifier change |
| R7 identity-level enforcement at the relay | relays receive only the CRL | new distribution channel to relays (NOT FEASIBLE WITH CURRENT INTERFACES) |
| R7 established-session termination at the relay | no re-check | relay monitor over registered connectors |
| R8 shield awareness | no channel; peer list stateless | new signal path, **or** connector-side shield-stream close (connector code) |
| R11 serial history | one `cert_serial` column | schema + CRL query (migration; needs approval) |
| R11 identity non-reuse / retained revocation after delete | hard delete | delete semantics or tombstone (may need schema) |
| R5 authorized recovery | none exists | product/process decision first |

## 18. Requirements that remain unverified

| Item | What is missing |
|---|---|
| Connector loss-detection time on a silent network path | runtime test; no keepalive, so it is TCP/HTTP2-dependent |
| Whether a revoked connector accepts a **fresh** QUIC handshake (vs a pooled connection) | a per-connector probe tool (missing capability, 10-13 §13 Q1) |
| Relay new-handshake rejection and established-session behaviour | a relay reachable by connectors (public `public_addr` or an approved test build) |
| Shield failover on connector **process loss** | a live run (not done) |
| Behaviour on HEAD `38e38c6` specifically | live runs used `9844a00` or older; the diff is INFERRED irrelevant |
| SPIFFE equality across connector renewal by test | no test |
| Watcher/stream-close interplay producing "self absent from own ACL" while streaming | INFERRED only |
| The client's choice on in-band connector denial vs transport refusal at scale | source-derived (§7.3); no live test |

## 19. Security and compatibility risks

1. **False terminal revocation**: treating `NotFound` as terminal would turn a controller DB error into permanent self-disablement (`control_stream.go:436-437`). `PermissionDenied` is shared with role and trust-domain failures.
2. **Availability flapping under a strict one-interval deadline**: any anchor that includes network RTT and scheduler jitter in a single interval can trip during ordinary operation. The accepted requirement is not changed here; the anchor semantics need an explicit decision (§22).
3. **Client fallback suppression**: if a disabled connector refuses with an in-band denial, clients don't try alternatives or resync (`net_stack.rs:1166-1186`). Recovery then waits for the watcher (≤ ~120 s) plus the client tick (≤ 60 s).
4. **Controller/connector timeout disagreement**: the connector would stop at one interval (15 s), while the controller marks it `disconnected` only after 90 s (+ up to 30 s watcher period). During that gap clients keep selecting a connector that refuses (combines with risk 3).
5. **Shield blast radius**: ending a shield stream ends **all** shield-routed sessions and triggers shield reconnect/failover. It also affects users who are not revoked, which is intended under revocation and disruptive under a transient outage.
6. **Renewal interaction**: a deadline must not count the make-before-break switch as lost authorization (§13).
7. **Restart semantics**: the de-facto "restart ⇒ no ACL ⇒ deny" depends on the ACL never being persisted. Persisting the ACL in future would silently remove that safety.
8. **Compromised-connector threat**: connector self-enforcement assumes an honest binary. Relay/client/shield-side identity enforcement is the only defence against a hostile revoked connector, and is unavailable today.
9. **Hard delete** removes CRL coverage and the identity record while old certificates remain chain-valid until `notAfter`.
10. **Prefix verifier**: a client direct-path handshake accepts any workspace connector certificate. Binding to the selected connector isn't enforced at TLS.
11. **Compatibility**: older connectors ignore unknown oneof fields (proto3), and a controller that sends per-heartbeat confirmations to old connectors is harmless. A **new** connector against an **old** controller would never get confirmations and would fail closed at every deadline. Rollout order matters: OPEN DECISION.

## 20. Suggested implementation dependencies (no implementation)

Ordered where the source forces an order; otherwise flagged.

1. **Decisions first** (§22): deadline anchor semantics; terminal-signal source; shield policy; client staleness policy; delete/tombstone policy. Everything below depends on them.
2. **Controller revocation-state distribution**:
   - a per-heartbeat authorization confirmation (needed by 4);
   - an optional immediate push or close at revoke (improves 3);
   - optional explicit client-visible revoked state (needed by 7).

   The confirmation must precede connector deadline work: without it the connector can't tell "fresh" from "enqueued" (§6.2). **Source-justified order.**
3. **Connector terminal revoked state**, from the existing status code. It can proceed independently of 2 (uses existing errors), but shares the serving-state and stop machinery with 4–5.
4. **Connector authorization gate and heartbeat deadline**: serving state + deadline task + re-enable on confirmation. Depends on 2 (confirmation) and on 5.
5. **Listener and session termination** in the connector: per-request gate (reject new streams), registry-wide cancel, shield-stream handling. Needed by both 3 and 4, so build it first among the connector items. **Source-justified order (5 before 3/4).**
6. **Client-visible enforcement**: the explicit signal (depends on 2) → flow → connector ownership → per-connector flow close. Independent of connector work. Its order relative to Q3 is undecided.
7. **Relay revocation monitoring**: an established re-check against the CRL. Independent; it delivers identity-level coverage only with a new relay channel (undecided).
8. **Shield attachment revocation**: either through 5 (the connector closes the shield streams) or a new shield signal. Undecided.
9. **CRL and identity-state consistency**: serial history (schema) and delete/tombstone semantics. Independent of 2–8; needs migration approval.
10. **Identity reuse prevention**: tied to 9.
11. **Automated regression and integration tests** (§21), alongside each item. Prefer test-first for 3–5.

## 21. Acceptance-test requirements (to prove the requirements after a future implementation)

Not run here. Labels give the minimum evidence each needs.

| # | Test | Proves | Level |
|---|---|---|---|
| T1 | Revoke a connected connector: it reaches terminal state on `PermissionDenied`, with no reconnect and no service across restart | R2, R3 | unit (fake stream) + live |
| T2 | Revoke → existing direct, shield and relay sessions on that connector end within the bound; new streams are refused at the connector | R2, R4, R7, R8 | unit (`acl_diff_session_tests`-style harness) + live (holds per path) |
| T3 | Control-plane cut (`connector-control-block.sh` style), not revoked: the data plane stops at the defined deadline; resumes only after confirmation | R4, R5, R10 | unit (paused clock) + live |
| T4 | Startup with the controller reachable but the connector revoked: no listener serves; shields are not served | R3, R10 | unit + live |
| T5 | Renewal under load with the deadline active: no fail-closed, no session loss | R12 | unit + live long hold (≥ 3 renewals) |
| T6 | Controller DB error during open/health: no terminal state | R2 (negative) | unit |
| T7 | Client receives explicit revoked state: no selection, existing flows on that connector closed, flows on other connectors untouched, alternatives used | R6 | unit (map + `FlowTable`) + live |
| T8 | Client offline across a revoke, then back: sync before any new flow uses the revoked connector | R9 | live (cgroup client-block harness) |
| T9 | Relay: a revoked connector's new handshake is rejected; its established sessions are terminated after a CRL/status change | R7 | unit + **live (needs a relay lab)** |
| T10 | Shield: no traffic via a revoked connector; failover to an eligible alternative where policy allows; existing sessions **not** expected to migrate | R8 | live (two connectors) |
| T11 | Earlier still-valid serial of a revoked connector is rejected wherever serials are checked | R11 | unit (CRL generation incl. connector branch) + live |
| T12 | Delete after revoke: the identity can't be reused and revocation coverage is retained (per the decided policy) | R11 | unit (DB) |
| T13 | Direct path rejects a different connector's valid certificate | R6/R11 | unit (verifier) |
| T14 | Mixed versions (new connector / old controller and the reverse) behave as decided | compatibility | integration |

## 22. Open decisions requiring human approval

1. **Deadline anchor semantics for R4** (no change to the accepted one-interval bound, no numeric grace invented). Candidates, none chosen:
   - (a) deadline = last confirmed heartbeat + one interval;
   - (b) at each scheduled heartbeat time, the previous heartbeat must have been confirmed;
   - (c) each heartbeat must be confirmed within a response bound inside the interval.

   Each has a different worst-case authorization staleness and false-trip behaviour.
2. **Confirmation carrier**: reuse the existing `Ping` with "authorized" semantics (controller-only change) vs a new message (additive proto).
3. **Terminal signal**: rely on `PermissionDenied` + message; add a structured reason; and/or have the controller push or close at revoke time. And how to treat `NotFound`.
4. **What "fail closed" stops**: reject new streams only, or also close listeners and endpoints; cancel sessions; close pooled QUIC connections; close shield streams (which triggers shield failover); detach from relays.
5. **How a disabled connector refuses**: in-band denial vs transport-level refusal (affects client fallback, §7.3).
6. **Terminal-state persistence** across process restart (today: de facto via the absent ACL).
7. **Client policy**: an explicit revoked signal; established-flow termination; a staleness bound when the controller is unreachable; sync-on-reconnect. How it interacts with the open Q3 (transient absence).
8. **Relay policy**: identity-state channel vs CRL-only; established-session re-check.
9. **Shield policy**: rely on connector self-enforcement only, or add a shield-side signal; failover on revocation.
10. **Serial history** for connectors (schema change) and **delete semantics** (tombstone vs hard delete; the migration 029 comment conflicts with the code).
11. **Authorized recovery** after revocation (R5), or confirm "new identity only".
12. **Client direct-path verifier**: prefix → exact binding.
13. **Rollout order** across controller/connector versions.

## 23. Explicit statement that implementation is not authorized

This was a source investigation and documentation task. **No implementation, architecture choice, protocol change, schema or migration change, configuration change, test change, test execution, live test, service restart, or database change was made or authorized.** No existing document was modified, and nothing was committed. The only file created is this report.

Validation performed after writing (see the final session report): `git status` compared against the pre-task snapshot; sha256 of the three earlier reports in this directory compared before and after; `git diff --check`. Line references were re-verified against the checked-out tree after drafting.
