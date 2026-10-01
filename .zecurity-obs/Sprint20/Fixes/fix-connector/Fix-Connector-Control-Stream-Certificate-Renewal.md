---
type: fix
sprint: 20
fix: connector-control-stream
title: Connector control-stream drop during certificate renewal
status: implemented-pending-live-acceptance
component: connector + controller (control plane only)
related: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]]
source_rows: [U5, G9]
tags:
  - connector
  - controller
  - control-stream
  - cert-renewal
---

# Connector Control-Stream Certificate Renewal

> Separate from Client Fix 01. Fix 01 makes the client tolerate snapshot churn; this fix removes one
> source of that churn: the connector deliberately drops its controller control stream on every
> certificate renewal.
>
> Status: implemented (Option A, make-before-break + switch-and-drop); unit/integration tests green;
> **live acceptance not yet run**. See §Implementation at the end. Nothing committed.

## 1. Problem Statement

On every connector certificate renewal (every ~5 m in the Sprint 20 lab: `CONNECTOR_CERT_TTL=15m`,
`CONNECTOR_RENEWAL_WINDOW=10m`), the connector closes its controller control stream, sleeps about
1 s, and opens a new one. Nothing else on the connector goes down. But for that second the controller:

- marks the connector `disconnected`;
- republishes ACL and transport snapshots without it;
- then republishes them again with it.

The connector, its shield link and its client tunnel listener stayed healthy the whole time, so this
is a false topology disappearance. Any consumer that samples a snapshot during that second sees the
connector as gone:

- the client (this caused hold #1's reset, see Fix 01 / Q3);
- other connectors' policy caches;
- the shield's peer list.

## 2. Observed Live Evidence

Live runs 2026-10-01, connector `cf3da570` (inkyank .75), shield `92115415` (nika .38). Two renewal
cycles were sampled across all components (logs in `~/s20-run/logs/renew-trace*`, `dbsamp*.log`,
`probe2.log`, `fix01-hold2.log`, `fix01-hold1-evidence.txt`). These are **observed** values.

**Connector control plane** (cycle 1, IST):

| Step | Time | Log |
|---|---|---|
| ReEnroll received | 12:22:49.274 | `controller requested cert renewal` |
| Cert renewed, persisted, published | 12:22:49.320 | |
| Old stream closed by the connector | 12:22:49.320 | `control stream closed cleanly, reconnecting` |
| Reconnect starts | 12:22:50.322 (**+1.002 s**) | `starting mTLS SPIFFE preflight check` |
| New stream up | 12:22:50.338 | `Control stream established` |

- Cycle 2: close at 12:27:50.398, preflight at 12:27:51.400 (+1.002 s).
- Connector PID 12676 was unchanged throughout (started 10:42:24), NRestarts=0. **The process does not restart.**

**Connector data plane:**

- `:9092` switched to the new cert in place (`device tunnel (QUIC) switched to renewed certificate`
  at 49.322).
- The 100 ms `:9092` TLS probe never failed and served the new serial from 12:27:50.509.
- The shield-proxy controller channel was rebuilt in place at 49.329.
- The shield link was a single socket, `192.168.1.38:40904 ↔ 192.168.1.75:9091`, unchanged on both
  ends through both cycles. The shield (PID 20643) logged no reconnect and no failover.
- `shields.connector_id` stayed `cf3da570`.

**Controller state:**

- Logged `closed stream (EOF)` → `disconnected` at 12:22:49, then `connected` at 12:22:50.
- 100 ms DB sampler:

  | Cycle | `disconnected` at | `active` at | Gap |
  |---|---|---|---|
  | 1 | 12:22:49.427 | 12:22:50.426 | 0.999 s |
  | 2 | 12:27:50.425 | 12:27:51.501 | 1.076 s |

- Three ACL versions per renewal: v133 (`connectors=1` connected to push to), v134, v135.
- Controller log since 10:46, all three connectors:

  | Connector | ReEnroll sent | EOF disconnects | Gap = 1 s (1 s resolution) | Gap = 2 s |
  |---|---|---|---|---|
  | cf3da570 | 20 | 20 | 18 | 2 |
  | manoj | 19 | 19 | 18 | 1 |
  | udaya | 20 | 20 | 18 | 2 |

  The 2 s values are ~1 s gaps that crossed a second boundary.
- One unrelated 61–76 s outage for all three at 11:54:42 was the controller host's `enp2s0` link
  going down at 11:54:14 (network event, not renewal).

**Client snapshot state:**

- Hold #1 (11:22:34): the client's 60 s sync landed inside the gap and got ACL v56 / transport v48
  without `cf3da570`. Result: TUN restart, reset at req 589.
- The cycles sampled today fell between syncs. The client saw only the restored state
  (`effective config unchanged, keeping tunnel`), and hold #2 ran 270 × 200 on one socket across both.

## 3. Current Code Path

Connector:

1. `connector/src/main.rs:320` runs `control_stream::run_control_stream`. That is the single task
   owning the control stream; it holds `ack_rx` / `ctrl_rx` by `&mut`.
2. `run_control_stream` loop (`connector/src/control_stream.rs:73-107`):
   - `Ok(())` → log `closed cleanly, reconnecting`, `sleep(1s)` (`:92-95`);
   - `Err` → exponential backoff from `BACKOFF_INITIAL_SECS = 2` (`:33`, `:97-105`).
3. `run_once` (`:132-170`) does the connection setup:
   - takes `certs.current().store` (`:150`);
   - runs the SPIFFE preflight (`:153`);
   - `build_channel` (`:158`), which is `controller_client.rs:37-50`: a tonic `Channel` with a fixed
     `Identity::from_pem` taken at build time;
   - `client.control(...)` (`:166`), then sends the initial health report (`:178-197`).
4. `run_once` then runs its message loop (`:206-221`).
5. On `ReEnroll` (`handle_controller_msg`, `:357-368`):
   - calls `renewal::renew_cert`;
   - on `Ok(Some)` returns `Some(Ok(()))` with the comment *"Break the inner loop to reconnect with
     the renewed cert"*;
   - `run_once` returns `Ok`, which drops `out_tx` and `inbound`, so the controller sees EOF; then
     the 1 s sleep in step 2 runs.
6. Renewal itself (`renewal.rs:55-58`, `GrpcCertRenewer`) builds a **separate** channel with the
   **current** cert and calls the `RenewCert` RPC. `CertHolder::install_renewed`
   (`tls/cert_holder.rs:136`) publishes on a watch channel (`:107`). Subscribers react:
   - `quic_listener.rs:48,75` and the `HolderCertResolver` (`cert_holder.rs:194-197`) serve the new
     cert on `:9091` / `:9092`;
   - `agent_server.rs:153-189` rebuilds the shield-proxy channel.

Controller:

1. `Control` handler (`controller/internal/connector/control_stream.go:340-446`):
   - identity comes from the peer certificate (`:341-343`);
   - revocation check (`:352-360`);
   - activation UPDATE to `active`; `becameActive` is true only if the old status wasn't `active`.
     On true it calls `NotifyPolicyChange` and `NotifyTopologyChange` (`:362-392`);
   - `Registry.add(connectorID, client)` and `defer Registry.remove(connectorID)` (`:400-401`).
2. Close defer (`:409-446`): UPDATE to `disconnected` `WHERE status='active'`; if the status changed,
   it calls both notifiers.
3. Recv loop (`:476-489`): `io.EOF` → `closed stream (EOF)`, return nil → the defers run.
4. `ConnectorRegistry` (`:62-65`, `:163-179`) is a `map[connector_id]*connectorStreamClient`:
   - `add` overwrites;
   - `remove` deletes by key, with no ownership check.
5. Health (`:612-669`): the UPDATE sets `active` and notifies only when status or `lan_addr` changed;
   then `maybeSendReEnroll` (`:112-129`, throttle 10 min per stream, `:94`).
6. `RenewCert` (`enrollment.go:229-297`) updates `cert_serial` / `cert_not_after`. It does **not**
   notify.
7. Compilers include only `c.status='active'` connectors (`policy/store.go:449`,
   `transport/store.go:61`).

## 4. Root Cause

The drop is **application logic**, specifically break-before-make in the connector's control-stream
lifecycle:

- `control_stream.rs:357-368` ends the stream on purpose after renewal.
- `:92-95` adds a fixed 1 s sleep before reconnecting.
- The controller's close defer (`control_stream.go:409-446`) then turns that EOF into
  `disconnected` and two snapshot notifications.

TLS/gRPC/certificates are **not** the root cause:

- The only certificate constraint is that a tonic channel's client identity is fixed when it is
  built (`controller_client.rs:37-50`), so presenting the new cert needs a **new** channel. It does
  not require closing the old one first.
- The old certificate was still valid for ~10 minutes at the drop (renewed 06:52:49, old expiry
  07:02:48).
- The same connector identity is already used on several concurrent controller connections:
  - the RenewCert channel (`renewal.rs:55`);
  - the shield-proxy channel (`agent_server.rs:153-189`);
  - two simultaneous `:9090` sockets observed live (`:44422` + `:44446`).

## 5. Design Options (not ranked here)

### Option A — Make-before-break

**Code changes:**

- Connector `control_stream.rs`:
  - split the stream-opening steps of `run_once` (preflight, `build_channel`, `control()`, initial
    health report) into a helper;
  - on renewal, open the new stream from `certs.current()` while the old one is still open, then
    swap `out_tx` / `inbound` in place and drop the old ones;
  - return no `Ok(())` and do no sleep.
  - This stays inside the one task that owns `ack_rx` / `ctrl_rx`, so there are no concurrent
    readers.
- Controller `control_stream.go`, made ownership-aware:
  - `ConnectorRegistry` gets a compare-and-delete (`removeIfCurrent(id, client) bool`);
  - the close defer marks `disconnected` / notifies only if this stream was still the registered one.

**Concurrency:**

- For a short while two controller streams exist for one connector.
- The controller handler side is already independent per stream (separate goroutine, mailbox,
  writer).
- Without the guard, the old stream's deferred `Registry.remove(connectorID)` (`:401`) deletes the
  **new** client from the registry, and its close defer flips the DB to `disconnected` with the new
  stream live. That would be a regression.

**Failure behaviour:**

- If the new stream fails, keep the old one (its cert is still valid) and retry. After N failures,
  fall back to today's break-and-reconnect.
- If the old stream dies first, today's reconnect path takes over.

**Controller implications:**

- Activation on the new stream: `becameActive=false` (already `active`), so no notify.
  `Registry.add` overwrites with the new client. The old EOF is ignored by the guard.

**Certificate implications:**

- None beyond today's: the new channel uses the renewed cert, the old one keeps the old cert until
  dropped.

**Data plane:** unchanged (§10).

**Advantages:**

- Removes the false `disconnected` state and its snapshot versions at the source, for every
  consumer.
- The controller guard also closes a pre-existing race: a late error on an old stream after a fast
  reconnect today deletes the new registry entry and marks the connector disconnected.

**Disadvantages:**

- Changes in two components.
- The connector's `run_once` needs restructuring.

**Open questions:**

- Messages the controller enqueued on the old stream's mailbox before `Registry.add(new)` and that
  the connector hasn't read yet are lost when the old inbound is dropped. They are likely recovered:
  `pushPendingInstructions` on the new stream (`:479`), and an ACL push on a stale
  `acl_version` from the health report. **Not proven for every message type.**
- Whether a drain of the old inbound is needed.

### Option B — Controlled break/reconnect

**Code changes:**

- Connector only: return a distinct "renewed" result and skip the 1 s sleep (reconnect at once).
- Optionally the controller close defer could skip notifying… (that would be Option C).

**Concurrency:** none new (still one stream at a time).

**Failure behaviour:** same as today.

**Controller implications:**

- The EOF still marks `disconnected` and notifies.
- The activation path still notifies again.
- Each renewal still produces intermediate snapshot versions.

**Data plane:** unchanged.

**Advantages:** smallest change (one file).

**Disadvantages:**

- **Does not eliminate the intermediate snapshot.** It shrinks the window from ~1 s to about the
  reconnect time (~16–40 ms observed: preflight → established), lowering but not removing the chance
  a client, connector or shield samples it.
- Version churn per renewal is unchanged.

**Open question:** the actual reconnect latency spread under load.

### Option C — Controller-aware renewal transition

**Code changes:**

- Controller only. On EOF, if this stream's connector renewed recently, defer the
  `disconnected` UPDATE and notifications by a short bounded window. Evidence for "renewed recently"
  would be `RenewCert` succeeding during this stream's life, e.g. a serial change or a per-connector
  "renewing" marker set in `RenewCert` (`enrollment.go:229-297`).
- If a new stream for the same connector arrives within the window, cancel the pending disconnect.
- It also needs the same registry ownership guard as Option A for the overlap.

**Concurrency:**

- Per-connector pending-disconnect timers.
- Races between timer expiry and the new stream's activation must be serialised (mutex or DB-guarded
  UPDATE).

**Failure behaviour:**

- If the connector really dies right after renewal, the disconnect shows up `window` seconds late.
- On controller restart the timers are lost. The disconnect watcher (heartbeat-based,
  `disconnect_watcher.go:49-53`) still catches stale connectors.

**Controller implications:** new timer state, plus a new "renewing" signal.

**Certificate implications:** none.

**Data plane:** unchanged.

**Advantages:** no connector change; works with today's connectors.

**Disadvantages:**

- It is a controller-side grace window: real disconnects are reported later in the renewal case.
  That is an ADR-017-type trade-off.
- More state to keep correct.

**Open questions:**

- The window length.
- How to identify "renewal EOF" reliably without a new protocol signal.

## 6. Recommended Design

**Option A** (connector make-before-break plus a controller stream-ownership guard), based on:

1. **Feasible:**
   - concurrent same-identity mTLS connections to the controller already happen and work
     (`renewal.rs:55`, `agent_server.rs:153-189`; two `:9090` sockets observed);
   - the controller authenticates each connection from its own peer cert (`control_stream.go:341-343`)
     and has no single-session rejection: `Registry.add` just overwrites (`:163-167`);
   - the activation path is idempotent for an already-`active` connector (`:362-379`, no notify when
     `becameActive=false`).
2. **Not safe on the connector alone:**
   - the controller's per-stream cleanup is keyed only by connector ID (`:400-401`, `:409-446`), so
     the old stream's EOF would remove the new stream from the registry and mark the connector
     `disconnected`;
   - the guard is therefore required, not optional.
3. **Removes the cause instead of narrowing or hiding it:**
   - Option B leaves the intermediate snapshot (only shorter);
   - Option C adds a grace window that delays real disconnects.
4. **The fallback keeps today's behaviour:** if the new stream can't be opened, keep the old stream
   and retry, and in the worst case do today's break-and-reconnect. Nothing becomes less safe than
   now.

Smallest safe alternative if Option A is rejected: Option B (connector only), knowing the
intermediate snapshot shrinks to tens of ms but is still published.

## 7. Failure Scenarios (with Option A)

| Scenario | Behaviour |
|---|---|
| Successful renewal | New stream opened with the new cert. Controller: already `active` → no notify; registry now points at the new client. Old stream dropped → EOF → guard sees it isn't current → no DB change, no notify. **No snapshot version from renewal** |
| New control connection fails | Old stream keeps running (old cert valid until expiry). Retry with backoff. Throttling is handled by DB `cert_not_after` already being renewed. If retries run out before the old cert expires: today's break-and-reconnect |
| Old connection dies first | Today's path: the old EOF is current → `disconnected` + notify; reconnect with the renewed cert from `certs.current()` (`:150`) |
| Connector crashes during renewal | Old TCP closes → controller EOF/error → `disconnected` (same as today). On restart the connector loads persisted state: `install_renewed` persists before publishing (`cert_holder.rs:136`; `renewal.rs:142` "persisted and published"). If the crash happens before persist: old cert, and ReEnroll repeats on the next stream |
| Controller restart | All streams drop; connectors reconnect via the outer loop (unchanged) |
| Network interruption | Unchanged. Stream error → backoff reconnect. Disconnect watcher (`disconnect_watcher.go:49-53`) for silent losses |
| Duplicate control streams | Supported by design during the overlap. The registry keeps the latest. Sends via `ClientsForWorkspace` / `get` reach only the current one |
| Stale old stream closes after the new one is active | Guard: `removeIfCurrent` returns false → no registry delete, no `disconnected` UPDATE, no notify. **Without the guard this is the regression case** |
| Old stream error detected late after a fast non-renewal reconnect | Already a race today (same keying). The guard fixes it too |

Remaining risk: between the guard's "am I current?" check and its UPDATE, a newer stream can
register. The UPDATE must be serialised with registry ownership (decision under the registry lock, or
a stream generation checked in the UPDATE). Otherwise a ≤15 s wrong `disconnected` state is possible
until the next health report. The health UPDATE re-sets `active` and notifies (`:612-669`).

## 8. ACL/Snapshot Impact

- **Should renewal change the ACL version?** On the inspected code, no.
  - `RenewCert` changes only `cert_serial` / `cert_not_after` and doesn't notify
    (`enrollment.go:265-283`).
  - The ACL and transport connector entries carry no serial (`policy/compiler.go:109-129`: id,
    tunnel address, SPIFFE, relay).
- **Why there's an intermediate snapshot today:** each renewal makes two real status transitions
  (`active` → `disconnected` → `active`). Each one calls both notifiers (`control_stream.go:379-392`,
  `:432-444`), and the compilers drop non-`active` connectors (`policy/store.go:449`,
  `transport/store.go:61`). Observed: 3 ACL versions per renewal.
- **How Option A stops it:** the status never leaves `active`. New-stream activation has
  `becameActive=false` (no notify). The guarded old EOF does no UPDATE and no notify.
- **Legitimate changes still bump:** a real `lan_addr` change on the new stream's first health
  report (`:612-669`), a real disconnect, a relay placement change, and policy edits are untouched.
- **Not proven:** whether a relay-attached connector's first relay-state report on the new stream
  re-notifies when nothing changed. The lab had no relays. Check this in acceptance.

## 9. Client Impact

The fix is meant to make a connector certificate renewal **not look like** a topology disappearance
to any snapshot consumer. Then the client never gets the snapshot that triggered hold #1's restart,
whatever its sync timing.

It does not change the client. It does **not** propose a client grace period. Fix 01 Phase 1 stays as
it is, and real disconnects, removals and revocations still reach the client as before.

## 10. Required Code Changes (not made)

| File | Function | Change |
|---|---|---|
| `connector/src/control_stream.rs` | `run_once` (`:132-221`), `handle_controller_msg` ReEnroll arm (`:357-368`), `run_control_stream` (`:73-107`) | Split out an open-stream helper; renewal swaps to a new stream before dropping the old; no `Ok(())` + 1 s sleep on success; fall back to today's path on repeated failure |
| `controller/internal/connector/control_stream.go` | `ConnectorRegistry.remove` (`:169-173`) → compare-and-delete; `Control` defers (`:400-446`) | Close defer marks `disconnected` / notifies only if this stream was current; ownership check serialised with the UPDATE |
| `controller/internal/connector/control_stream_test.go` (tests, later) | | Overlapping streams for one connector: old EOF after new add → no disconnect, registry keeps the new client |
| connector tests (later) | | Renewal with the stream swap; new-stream failure keeps the old stream |

**Expected unchanged:**

- `controller_client.rs`, `renewal.rs`, `tls/cert_holder.rs`, `agent_server.rs`, `quic_listener.rs`,
  `device_tunnel.rs`;
- all shield code;
- all client code;
- compilers, notifiers, migrations, proto.

## 11. Live Acceptance Plan

Same lab and TTLs (15 m / 10 m). Samplers as in §2: 100 ms DB, 200 ms sockets on connector + shield,
`:9092` probe, controller log, client journal.

1. **Normal renewal:**
   - connector logs the renewal plus a new-stream line, and **no** `closed cleanly, reconnecting`;
   - controller logs `connected` for the new stream and **no** `disconnected`;
   - the DB sampler shows **no** `disconnected` row;
   - socket sampler: a new `:9090` socket appears **before** the old one disappears;
   - connector PID unchanged.
2. **Repeated renewals:** ≥ 6 consecutive renewals for each of the 3 connectors (≥ 30 min). Zero
   renewal-caused `disconnected` lines; the counts in the §2 table go to 0 disconnects per ReEnroll.
3. **Long-lived client flow:** a keep-alive hold of ≥ 30 min (≥ 6 renewals of the shield's connector)
   with zero resets. The client logs no `snapshot changed, restarting VPN`, whatever its sync phase.
4. **Controller status:** `connectors.status` stays `active` throughout; `cert_serial` changes to the
   new serial each cycle; `last_heartbeat_at` keeps advancing.
5. **ACL/transport versions:**
   - no ACL or transport version bump attributable to a renewal (controller `acl push` lines only on
     real changes);
   - the client's `transport snapshot stored version` doesn't move across a renewal with no other
     change.
6. **Shield link continuity:**
   - the shield socket to `:9091` is unchanged (same port pair) across renewals;
   - no shield reconnect or failover;
   - `shields.connector_id` unchanged;
   - the `:9092` probe never fails and switches serial.
7. **New-stream failure:**
   - block the connector's *new* `:9090` connections only, after renewal starts (e.g. an nft rule
     added at ReEnroll for new SYNs only);
   - expect: the old stream stays, no `disconnected`, retries are logged; after unblocking, the swap
     completes;
   - and the fallback: block until retries run out → today's break/reconnect, one `disconnected`
     cycle.
8. **Connector crash during renewal:** `kill -9` the connector right after `certificate renewed,
   persisted and published`. Expect:
   - controller `disconnected` (real) with notify;
   - systemd restart loads the persisted renewed cert;
   - the new stream is `active`;
   - the connector serial matches the DB;
   - the shield reconnects (real outage, expected).

## 12. Scope Boundary

**In scope:**

- connector control-stream behaviour during certificate renewal;
- controller stream/session ownership handling needed to make stream replacement safe;
- preventing the false connector disappearance;
- preserving data-plane continuity.

**Out of scope:**

- the client Fix 01 implementation;
- client grace periods;
- Phase 2 hot-apply;
- resource routing redesign;
- shield failover redesign;
- ACL policy redesign;
- unrelated certificate/revocation work (e.g. Finding 34: revoked connector data plane).

## 13. Implementation Gate

`STATUS: INSPECTION COMPLETE — IMPLEMENTATION NOT STARTED`

Before implementation starts:

1. The user approves Option A, or picks another option, explicitly.
2. Decide how the controller guard is serialised: decision under the registry lock vs a stream
   generation checked in the UPDATE.
3. Decide the new-stream failure policy: retry count/backoff, and when to fall back to
   break-and-reconnect relative to the old cert's expiry.
4. Decide whether the connector drains the old inbound stream briefly before dropping it, or relies
   on replay (`pushPendingInstructions`, ACL-version push). Confirm which controller→connector
   message types can be enqueued during the overlap and how each one recovers.
5. Agree the test plan in §11, including how test 7 (new-stream-only block) is set up.
6. Confirm that relay-attached connectors don't re-notify on an unchanged relay-state report from
   the new stream (§8 open question), or include it in acceptance.
7. Commit discipline as usual: no commit until told.

Items 2, 3, 4 and 6 are resolved in the section below. Items 1, 5 and 7 remain user decisions.

## Pre-Implementation Design Resolution

Added 2026-10-01. Source inspection only; no code changed.

Evidence labels used below:

- **[src]** = verified from source.
- **[live]** = observed in the 2026-10-01 lab.
- **[open]** = not verified.

### 1. Control-message overlap analysis

**Controller → connector**

Every controller→connector message goes through `connectorStreamClient.send` into a per-stream
mailbox of 128 (`control_stream.go:72-76, 135-142`). One writer goroutine per stream drains it
(`:149-161`).

How a sender picks a stream:

- **Registry callers** pick the stream via `Registry.get` / `ClientsForWorkspace` / `BroadcastRelayList`
  (`:175-199, 245, 284, 307, 320-335`). These return the most recently **added** client.
- **Per-stream callers** use the stream they run on: the recv loop's `client`, for connect-time
  pushes, health replies and the reconciler.

When the controller handler returns, its context is cancelled. The writer then exits **without
flushing** what is left in the mailbox (`:150-153`). So whatever is still in the old stream's
mailbox at that moment is lost [src].

The overlap window runs from B's `Registry.add` (`:400`) to the end of A's handler.

- Messages sent through a registry lookup after `add(B)` go to B.
- Messages that can still be on A were either enqueued before `add(B)`, or taken from a
  `ClientsForWorkspace` snapshot captured before it. The code comment at `:184-187` documents this as
  benign.

| Message / type | Source | Delivery mechanism | Persisted? | Replayable? | Can be lost during overlap? | Consequence |
|---|---|---|---|---|---|---|
| `Ping` | `Control` connect (`:453-459`) | per-stream, once at connect | no | sent on every new stream | no: B sends its own | none |
| `RelayList` (LabelledRelayList) | connect push (`:463-469`); `BroadcastRelayList` (`main.go:287` → `:320-335`) | per-stream at connect + registry broadcast | relay pool is in DB (relay store) | **yes**: B's connect push rebuilds it from the store | a broadcast copy left in A's mailbox | none: B's connect copy is current; the connector's `watch` keeps the latest (`control_stream.rs:391-407`) |
| `ResourceSnapshots` | `pushPendingInstructions` per shield (`:546-558`); `PushSnapshotForShield` from resolvers (`resource.resolvers.go:88,106,133,196` → `:240-257`) | per-stream at connect + registry `get` | desired set + generation in DB (`resource.BuildShieldSnapshot`, `:207-233`) | **yes**: B rebuilds every shield's snapshot from DB **after** `add(B)` (`:479`) | yes (on A) | none: B's copy is built from DB after any earlier commit. The shield `generation <=` gate drops duplicates (`:201-206`; connector `agent_server.rs:707-717`) |
| `ResourceInstructions` (`apply` / `remove`) | resolvers → `PushInstruction` (`resource.resolvers.go:87,105,132` → `:262-302`); `pushPendingInstructions` re-sends `remove` only (`:570-600`) | registry `get` + per-stream at connect | **yes**: `pending_action` in DB, written before the push (`:259-261`) | **yes**: `apply` is covered by the snapshot, `remove` is re-sent (`:561-569`) | yes (on A) | none: B's replay reads DB after `add(B)`. A resolver that ran `get()` before `add(B)` committed to DB before calling `get()`, so B's replay includes it |
| `AclSnapshot` (proactive) | `ACLPusher.pushOnce` → `ClientsForWorkspace` (`acl_push.go:116-143`) | registry fan-out | compiled from DB, cached by epoch (`acl_push.go:120-125`) | **yes**: heartbeat path below | yes (on A, or a stale fan-out snapshot) | none: B's first health report carries the connector's `acl_version`; any mismatch → push (`:700-702, 755-788`) |
| `AclSnapshot` (heartbeat) | `handleConnectorHealth` → `pushACLSnapshot` (`:700, 755-788`) | per-stream reply to each health report | compiled from DB | **yes**: every 15 s (`HEALTH_INTERVAL_SECS`, `control_stream.rs:35`), version-gated | a reply to A's last health report | none: B's next report repeats the check |
| `ReEnroll` | `maybeSendReEnroll` (`:112-129`) | per-stream, throttled 10 min per stream (`:94`) | due-ness is from DB `cert_not_after` | **yes**: re-evaluated on every health report | yes | none. After `RenewCert`, DB `cert_not_after` is the new one (`enrollment.go:265-277`), so B isn't due. A duplicate on A is dropped by connector dedupe (`renewal.rs:144-152`, "already in progress" / "renewed moments ago") |
| `ResourceSnapshots` (drift resync) | reconciler (`reconcile.go:162-168`) | per-stream on the stream that received the state report | from DB | **yes**: drift is re-counted on B's next reports | yes | none: the next drift run re-pushes; B's connect snapshot already resyncs |
| `ScanCommand` | `TriggerScan` resolver → `PushScanCommand` (`discovery.resolvers.go:83` → `:306-312`) | registry `get` | **no** (request id only returned to the caller) | **no** | **yes**, if routed to A and left in its mailbox | **one-shot loss**: no scan runs, no `ScanReport`. The resolver already returned success. No security or state impact; the admin re-triggers. **Same exposure as today's** break-and-reconnect |

**Connector → controller**

The connector owns `ack_rx` / `ctrl_rx` in a single task (`control_stream.rs:206-279`, `main.rs:320-331`).
Whatever it has already pushed into A's `out_tx` is flushed when A is half-closed: dropping the
`mpsc` sender lets `ReceiverStream` yield what is buffered, then end. That holds **if the
controller's A handler is still reading** [src, tokio mpsc semantics]. The risk is A's handler
ending (EOF/cancel) before it processes items already sent.

| Message | Recovery if lost |
|---|---|
| `ResourceAcks` | `ResourceState` reports + reconciler (`reconcile.go`) |
| `RelayState` | heartbeat `relay_id` reconciles placement (`:670-698`) |
| `ConnectorLog` | lost (informational) |
| `ScanReport` from a scan already spawned with A's `out_tx` clone (`control_stream.rs:310-345`) | lost, same as today |

**Ordering hazard**

The connector's ACL cache is replace-only, with no version-monotonic check
(`connector/src/policy/mod.rs:40-46` `update_and_revoked`). If the connector applied an older
`AclSnapshot` from A **after** a newer one from B, its ACL would regress for up to one health interval
(15 s). If the older snapshot allowed more, a just-revoked grant could be briefly re-allowed.

All messages on A were enqueued before (or alongside) `add(B)`, so A's messages are never newer than
B's. **The hazard exists only if the connector interleaves reads of A and B.**

**Conclusion**

> **Superseded (2026-10-01):** the drain step is simplified to *switch-and-drop*. See
> §Certificate Rotation Architecture Comparison → 9. The ordering analysis below still holds.

Make-before-break is **safe with the current delivery architecture**, under one connector rule:

> **Drain-then-switch.** After B is established:
> 1. route all *outbound* traffic (`ack_rx`, `ctrl_rx`, health, Pong) to B's `out_tx`;
> 2. half-close A (drop A's `out_tx`);
> 3. keep reading **only A**, processing messages normally (with B's `out_tx` as the reply channel),
>    until A's inbound ends;
> 4. only then start reading B.
>
> Bound the drain by `HEALTH_INTERVAL_SECS` (15 s, existing constant). If A hasn't ended by then,
> drop it.

- B's inbound is held by HTTP/2 flow control meanwhile; nothing is lost.
- If a full window stalls the controller's B writer, `send()` fails fast (`:135-142`). That message
  is recovered by the mechanisms in the table.
- A ends promptly: half-close → controller `Recv` sees EOF (`:483-486`) → handler returns → stream
  completes.

**Residual, accepted (not a regression):**

- the one-shot `ScanCommand` / `ScanReport` loss;
- the informational `ConnectorLog` loss.

Both exist today on every reconnect. Smallest fix if wanted later: persist scan requests, which is a
separate change.

### 2. Controller ownership model

**Compare-and-delete alone is insufficient [src].**

Today B's handler runs the activation UPDATE (`:362-392`) **before** `Registry.add` (`:400`). A's
defers run status first, then `Registry.remove`: the status defer is registered later at `:409`, so
under LIFO it runs before the `remove` deferred at `:401`.

This interleaving defeats a plain pointer compare-and-delete:

```
B: UPDATE status='active'      (no-op, becameActive=false — no notify)
A: removeIfCurrent(A) → true   (B not yet added)
B: Registry.add(B)
A: UPDATE status='disconnected' + notify
→ connector 'disconnected' in DB, absent from snapshots, while B is live
```

That lasts until B's next health report (≤ 15 s), which re-sets `active` and notifies (`:612-669`).

**A stream generation token alone is also insufficient.** The race is between two *transitions*
(registry + DB), not just two registry writes. A token stored only in memory leaves the same window
around the DB UPDATE. A DB-side token would need a new column, i.e. a migration.

**Chosen: pointer identity + one lifecycle mutex.**

- **Identity token:** the `*connectorStreamClient` pointer, which is unique per stream (`:394-399`).
  No new field is needed.
- **Serialisation:** a new `sync.Mutex` (e.g. `ConnectorRegistry.lifecycleMu`, separate from
  `r.mu`) held across each stream's *registry change + status UPDATE*.
  - Open (B): build client → `lock` → activation UPDATE (`becameActive`) → `Registry.add(B)` →
    `unlock` → notify if `becameActive`.
  - Close (A): `lock` → if `r.clients[id] == A` { delete; disconnect UPDATE (`becameDisconnected`) }
    else { no-op } → `unlock` → notify if `becameDisconnected`.
- **Why not `r.mu`:** holding the registry `RWMutex` across a DB round-trip would block every
  `get` / `ClientsForWorkspace` / broadcast reader (ACL fan-out, resolvers) for the DB latency.
  The separate mutex blocks only connect/disconnect transitions, which are rare and not on the hot
  path.
- **Why notify after unlock is fine:** the notifiers only bump a counter and invalidate; compile
  happens on demand from DB state (see the Fix 01 investigation). Whatever order A's and B's notifies
  happen in, the next compile reflects the final DB state.

**Both orders are now correct:**

| Order | Result |
|---|---|
| B then A | B is active and registered; A sees it isn't current → nothing |
| A then B (old died first, case C) | A marks `disconnected` + notify; B finds `becameActive=true` + notify. A real, short gap, reported truthfully |

**Other writers of `connectors.status`, unchanged and compatible:**

- **disconnect watcher** (`disconnect_watcher.go:49-53`): heartbeat-age based. Both streams send
  heartbeats, so it's unaffected.
- **Goodbye** (`goodbye.go:29-33`): connector-initiated.
- **health UPDATE** (`:617-646`): re-asserts `active` and is revocation-guarded.
- **revocation:** the guarded UPDATEs keep `revoked` sticky. A's close defer matches
  `WHERE status='active'` only (`:424`).

This also fixes the existing race where a late error on a stale stream after a fast reconnect removes
the new registry entry.

### 3. Renewal failure behaviour

Confirmed constraints:

- After a renewal the controller accepts **both** certs at new handshakes. Connector verification
  checks only the chain against the workspace CA (`spiffe.go:256-281`), not `cert_serial`. The old
  cert stays usable until it expires.
- An established TLS stream isn't re-validated mid-stream. Same class of behaviour as Finding 34;
  [src] for the gRPC server path, not live-tested past expiry.

**A — renewal OK, new stream OK.**

1. `renew_cert` → `Ok(Some)` (`control_stream.rs:357-365`).
2. Open B with `certs.current()` (`:150-170`: preflight, channel, `control()`, initial health).
3. Drain-then-switch (§1).
4. Controller: B activation `becameActive=false` (no notify); `add(B)`; A's close is a no-op under
   the lock.

Result:

- DB `active` throughout;
- no notify;
- no ACL/transport version from the renewal;
- shield and data plane untouched [src; matches §2 live evidence for the data plane].

**B — renewal OK, new stream fails.**

- A stays **authoritative and usable**: same task, same `out_tx` / `inbound`, still authenticated.
- Retry opening B on the **existing** schedule: `BACKOFF_INITIAL_SECS = 2` doubling to
  `BACKOFF_MAX_SECS = 60` (`:33-34, 103-104`). Keep serving A between attempts.
- **No new timeout values.** Stop make-before-break and fall back to today's break-and-reconnect
  (`Ok(())` → reconnect with `certs.current()`) when either:
  1. A ends by itself → normal outer loop (`:73-107`), which already reconnects with the renewed
     cert; or
  2. the *old* certificate's `not_after` is reached, so the connector stops relying on an expired
     identity even though the stream would survive.
- Both triggers come from existing state and need no new timeout constant. The old cert's `not_after`
  needs to be captured before `install_renewed`, e.g. from `certs.current()` before renewal.
- No ReEnroll storm meanwhile: DB `cert_not_after` is already the new one, so `renewalDue` is false
  (`:99-104`).
- **Controller unreachable:** the B attempts fail; A most likely fails too → normal backoff path.
  The controller marks `disconnected` on A's end, which is a real outage.

**C — A dies before B is established.**

- Controller: A is current → `disconnected` + notify (real).
- Connector: if the B attempt succeeds, it becomes the stream (`becameActive=true` + notify).
  Otherwise → outer loop.
- The gap is genuine and reported. Same as today, just shorter.

**D — connector crash during renewal.**

| Crash point | Recovery |
|---|---|
| Before `RenewCert` returns | Nothing changed. Restart uses the old cert; ReEnroll is re-sent when due |
| After `RenewCert` committed in DB (`enrollment.go:265-277`) but before the connector persists | Disk has the old (valid, not revoked) cert; the controller accepts it (chain only). DB `cert_not_after` is the new value, so the next ReEnroll comes only at `new_not_after − window` |
| After persist (`cert_holder.rs:154-167` writes before `send_replace` at `:169`) | Restart loads the renewed cert (`main.rs:90`) |

In every case: a real `disconnected` from the TCP close, systemd restart, reconnect with `active` +
notify. The shield reconnects too (real outage).

**Pre-existing hazard (not caused or changed by this fix; record as a follow-up, Finding-17-like).**
In the "committed in DB, not persisted" row, the old cert expires at
`renewal_time + window`. The next ReEnroll comes at `renewal_time + TTL − window`.

- That is safe only if `TTL − window < window`. The lab is fine: 15 m / 10 m.
- The **defaults are not**: `CONNECTOR_CERT_TTL=7d`, `CONNECTOR_RENEWAL_WINDOW=48h` (`main.go:191,197`).
  The old cert would expire 3 days before the next ReEnroll.

### 4. Relay behaviour

**[src] verified:**

- Relay placement is keyed by `connector_id` only (`connector_relay_placement`; `relay/store.go:401-426`
  upsert, `:431-443` delete). No stream or session identity.
- `UpsertPlacement` reports `changed` only when `relay_id` differs (`store.go:419`). An unchanged
  heartbeat or `RelayState` (`connected`/`switched`) from B → **no notify** (`control_stream.go:674-686,
  722-738`).
- `DeletePlacement` happens only on an empty heartbeat `relay_id` or `disconnected` (`:687-697, 739-749`).
- The stream close defer does **not** touch placement (`:409-446`). Make-before-break doesn't change
  placement handling.
- The connector's relay attachment runs in its own task (`main.rs:303-313` `relay_selector::run`) with
  its own cert-holder subscription (`relay_handler.rs:223,427`; `relay_selector.rs:86`). It is
  independent of the control stream. `RelayState` reaches the controller via `ctrl_tx` → `ctrl_rx`,
  which is owned by the control-stream task and goes to B after the swap.
- On every new stream the controller pushes a fresh `RelayList` (`:463-469`). The connector's
  `watch::send` always marks changed, so the selector runs `handle_list_change` → re-probe, and may
  migrate (`relay_selector.rs:279-326`). **This already happens on today's reconnect**, once per
  renewal. Make-before-break keeps exactly one connect-push per new stream: same frequency, not new.
  A resulting real migration produces a legitimate `RelayState switched` → placement changed →
  transport notify.

**Live-tested:** nothing relay-related. The lab has no relays (`connector_relay_placement` empty for
all three connectors on 2026-10-01).

**[open]:**

1. Whether the per-renewal re-probe triggered by the connect-push ever causes a needless relay
   migration. This is pre-existing and orthogonal, but would show up as transport version bumps.
2. Whether a `RelayState` lost at A's end (it is recovered by the next heartbeat) causes a visible
   ≤ 15 s placement lag.

Acceptance additions for a relay-attached lab:

- R-1: with a relay attached, renew → `connector_relay_placement` unchanged, no `notify topology after
  relay …` log lines, the relay registration (`Connector registered with Relay`) not repeated.
- R-2: count transport versions per renewal; expect 0 unless a migration is logged.

### Assumption checks

| # | Assumption | Result |
|---|---|---|
| 1 | Connector can hold the old controller channel while building the new one | **True [src+live]**: channel/stream are `run_once` locals (`:158-170`); independent channels already coexist (`renewal.rs:55`, `agent_server.rs:153-189`); two `:9090` sockets observed |
| 2 | New channel uses the renewed cert | **True [src]**: `install_renewed` `send_replace` (`cert_holder.rs:169`) before `run_once` reads `certs.current()` (`:150`) |
| 3 | Controller accepts the new stream before the old closes | **True [src]**: no single-session check; `add` overwrites (`:163-167`). **Not live-tested** for two `Control` streams (only other RPCs coexisted) |
| 4 | Activation on an already-`active` connector doesn't notify | **True [src]**: `becameActive = current.status IS DISTINCT FROM 'active'` (`:372, 379`); first health `connectorChanged` is false when status and `lan_addr` match (`:637-638, 647`) |
| 5 | Old-stream cleanup can be made ownership-aware | **True, with conditions [src]**: pointer compare **plus** the lifecycle mutex and the open-path reorder (§2). A bare compare-and-delete is unsafe |
| 6 | Shield/data plane independent of the control-stream swap | **True [src+live]**: `ShieldRegistry` / `tunnel_hub` / listeners live outside `run_once` and are passed by reference; the shield socket was unchanged across renewals |
| 7 | No client changes required | **True** |
| 8 | No migration or protocol change required | **True [src]**: the chosen ownership model is in-memory (pointer + mutex); no new message types; drain-then-switch uses existing streams |

### 5. Implementation readiness

All four critical questions are resolved from source:

1. Overlap delivery: safe with drain-then-switch; the only losses are pre-existing one-shot messages.
2. Ownership: pointer identity + lifecycle mutex + open-path reorder.
3. Failure behaviour: existing backoff; fallback when the old stream ends or the old cert's
   `not_after` is reached.
4. Relay: no new notifications, from source.

Non-blocking residuals (pre-existing, recorded, not regressions):

- `ScanCommand` / `ScanReport` / `ConnectorLog` loss on stream end;
- the TTL-vs-window crash-window hazard (§3 D);
- relay behaviour not live-tested (R-1 / R-2 to be run in a relay lab).

**Recommended changes (for implementation, not made):**

- **Connector:** `connector/src/control_stream.rs` only.
  - Extract an `open_stream(certs) -> (out_tx, inbound)` helper from `run_once` `:150-197`.
  - The ReEnroll arm (`:357-368`) returns a "renewed" signal instead of `Some(Ok(()))`.
  - The `run_once` loop handles it with drain-then-switch, the retry schedule
    (`BACKOFF_INITIAL_SECS` / `BACKOFF_MAX_SECS`), and fallback on A's end or old-cert `not_after`.
  - On a stream switch, `ack_rx` / `ctrl_rx` / health / Pong use the current `out_tx`.
  - No 1 s sleep on the renewal path. The `Ok(())` sleep at `:92-95` stays for genuine clean closes.
- **Controller:** `controller/internal/connector/control_stream.go` only.
  - Add `lifecycleMu` to `ConnectorRegistry`.
  - Replace `remove(id)` with `removeIfCurrent(id, c) bool`.
  - In `Control`, build the client before activation; run activation UPDATE + `add` under
    `lifecycleMu`; run the close defer's ownership check + disconnect UPDATE (+ remove) under
    `lifecycleMu`; notify after unlock.

**Tests required after implementation:**

- Controller unit tests (`control_stream_test.go`):
  - overlapping A/B, A closes after B → no `disconnected`, registry = B, no notify;
  - A closes before B registers → `disconnected` + notify, then B → `active` + notify;
  - the interleaving in §2 driven deterministically (hooks or a channel barrier) → never ends
    `disconnected` with B live;
  - revoked connector: both streams end, status stays `revoked`.
- Connector unit tests:
  - ReEnroll → new stream opened before the old is dropped;
  - messages are read from A until it ends, then from B (ordering);
  - new-stream failure keeps A and retries on the backoff schedule;
  - fallback when A ends;
  - fallback at old-cert `not_after`.
- Live: §11 tests 1–8, plus R-1 / R-2 in a relay lab, plus:
  - **O-1**: two `Control` streams for one connector coexist on the controller for the drain window
    (two `:9090` sockets in the connector sampler, a single `connected` without `disconnected` in the
    controller log).
  - **O-2**: a resource Protect/Unprotect issued by an admin during a renewal still reaches the shield
    (DB `applied_at` advances; the shield applies it).

**Remaining user decisions** (gate items 1, 5, 7):

- approve Option A with this resolution;
- agree the tests above;
- commit discipline.

`STATUS: DESIGN RESOLVED — READY FOR IMPLEMENTATION`

This is pending the user's explicit approval (gate item 1). Implementation has not started.

## Certificate Rotation Architecture Comparison

Added 2026-10-01. Question: *why can't the main control stream rotate its certificate the way the
Shield-proxy channel already does?*

Labels:

- **[src]** = source-verified.
- **[live]** = observed 2026-10-01.
- **[inf]** = inferred.
- **[open]** = unresolved.

### 1. Shield-proxy certificate lifecycle

1. **Startup [src].** `main.rs:100-102`: `controller_client::build_channel(&cfg, &cert_store)`
   builds a tonic `Channel` with a fixed `Identity::from_pem` (`controller_client.rs:37-50`). It is
   stored in `ShieldRegistry.controller_channel: Arc<RwLock<Channel>>` (`agent_server.rs:92, 124`).
2. **Subscriber [src].** `main.rs:122-129`: `spawn_controller_channel_refresh(certs, build_channel)`.
   The task (`agent_server.rs:153-189`) waits on `certs.subscribe().changed()`.
3. **Renewal [src].** `CertHolder::install_renewed` persists the cert, then calls `send_replace`
   (`cert_holder.rs:154-169`), which wakes the subscriber.
4. **New identity [src].** The subscriber calls `build_channel` with the **new** material. That is a
   **new `Channel`** (new TLS session, new identity), built while the old one still exists.
5. **Swap [src].** `replace_controller_channel` → `*self.controller_channel.write() = channel`
   (`:140-143`). This is an atomic pointer swap under a `parking_lot::RwLock`. On failure the old
   channel is kept and the build is retried every 5 s, or sooner if a newer cert lands (`:174-183`).
6. **Old connection [src + inf].**
   - Nothing closes it explicitly. It is dropped when the last clone goes.
   - Callers clone the channel per call (`controller_channel()` `:135-137`, used once at `:864`).
   - So an in-flight `renew_cert` RPC finishes on the old channel, and the next one uses the new.
7. **Controller registration [src].** **None.**
   - The only use is the unary `ShieldService.RenewCert` proxy (`:855-869`).
   - The controller keeps no per-connection state for this channel; the identity is read per call.
8. **Final state [live].** At 12:22:49.33, `Shield-proxy controller channel rebuilt`. The socket
   sampler shows `:44446 → :44778` replaced the same second, with no other effect.

### 2. Main control-stream certificate lifecycle

1. `run_once` reads `certs.current()` once (`control_stream.rs:150`).
2. It runs the SPIFFE preflight (`:153`) and `build_channel` with that fixed identity (`:158`).
3. It opens `client.control(outbound)`, a **bidirectional streaming RPC** (`:164-170`;
   `connector.proto:20`), and sends the initial health report (`:178-197`).
4. On ReEnroll: `renew_cert` → `install_renewed`, which publishes; then return `Some(Ok(()))` (`:357-368`).
5. `run_once` returns.
   - Locals `out_tx`, `inbound`, `client` and `channel` drop.
   - The stream half-closes and the controller sees EOF (`control_stream.go:483-486`).
6. Outer loop: `Ok(())` → `sleep(1s)` (`:92-95`) → `run_once` again with the new
   `certs.current()`, giving a new channel with the new identity.

**[live]** This matches the observed sequence: close at 49.320, preflight at 50.322, established at 50.338.

### 3. Side-by-side comparison

| Property | Main Controller Control Stream | Shield-Proxy Channel |
|---|---|---|
| TLS identity creation | `Identity::from_pem` at `build_channel` (`controller_client.rs:37-50`) | same function (`main.rs:100, 127`) |
| Certificate source | `certs.current()` read once per `run_once` (`control_stream.rs:150`) | `CertMaterial` from the `CertHolder` watch (`agent_server.rs:163-167`) |
| Channel creation | once per `run_once` (`:158`) | at startup + on every publish (`:168`) |
| Certificate replacement | rebuild the whole stream after `run_once` exits (`:357-368`, `:92-95`) | build a new `Channel`, then atomic swap under `RwLock` (`:140-143, 170`) |
| Can old connection remain alive? | today no: dropped before the new one is built. Structurally yes: locals in one task [src] | yes: old channel lives until the last clone drops [src] |
| Can new connection be created first? | not today. Possible: nothing forbids a second `build_channel` while `channel` is alive (`:158-170`) [src] | yes, always: build first, swap after [src] |
| Bidirectional stream? | **yes**: `Control(stream) returns (stream)` | **no**: unary `RenewCert` only (`:855-869`) |
| Controller→connector messages | 9 types (Pre-Implementation §1 table) | none: the reply belongs to the one call |
| Message ordering requirements | **yes**: ACL cache is replace-only (`policy/mod.rs:40-46`); messages must not interleave from two streams | none: each call is independent |
| Reconnect/replay mechanism | controller replays on stream open (`control_stream.go:453-479`) + heartbeat ACL push (`:700, 755-788`) | none needed: the caller retries the unary call |
| Connection ownership | **controller-side**: `ConnectorRegistry[connector_id] → *connectorStreamClient` (`:62-65, 400-401`) plus DB `status` via the close defer (`:409-446`) | none on the controller |
| Concurrent connections possible? | controller accepts (no single-session check, `add` overwrites `:163-167`) [src]; not live-tested for two `Control` streams [open] | yes [live]: separate `:9090` socket alongside the control stream |
| Existing implementation behaviour | break → sleep 1 s → reconnect; controller `disconnected` → `active`, 3 ACL versions [live] | swap with no gap; no controller state change [live] |

### 4. Is true in-place TLS identity rotation possible? (Option A)

**No [src].**

- **tonic 0.14.5:** `ClientTlsConfig.identity` is turned into a fixed client cert via
  `builder.with_client_auth_cert(client_cert, client_key)` (`tonic-0.14.5/src/transport/channel/service/tls.rs:86-93`).
  It installs no dynamic resolver.
- **rustls 0.23:** a client cert is presented only during the handshake. In the established
  (traffic) state the client accepts only `NewSessionTicket` and `KeyUpdate`. Any other handshake
  message is rejected as inappropriate (`rustls-0.23.40/src/client/tls13.rs`, `ExpectTraffic::handle`,
  `[NewSessionTicket, KeyUpdate]`). There is no post-handshake client auth and no renegotiation. So an
  **established** TLS connection can't change its identity.
- **Controller side:** identity is taken from the TLS peer cert once per stream
  (`control_stream.go:341-343`, interceptor `:860-889`).

A dynamic client cert resolver (as used elsewhere: `client/src/tunnel_pool.rs:354`) plus tonic
`connect_with_connector` (as the shield uses: `shield/src/tls.rs:295`) would make *new* TLS sessions
on the same `Channel` pick up the new cert. But:

- tonic only reconnects when the connection breaks;
- the control stream is pinned to its HTTP/2 connection for its whole life.

So it would still need a new stream on a new connection.

**The Shield-proxy code does not rotate in place either.** It builds a new `Channel` and swaps the
handle. That is make-before-break **channel replacement**, not identity rotation.

### 5. Is make-before-break possible? (Option B)

**Yes, the same pattern [src]:**

1. build a new `Channel` from the published `CertMaterial`;
2. open the new connection while the old is alive;
3. swap.

It works for the control stream for the same reasons it works for the proxy:

- `build_channel` is independent per call;
- the identity is valid at both ends;
- the controller doesn't reject a second same-identity connection.

**What doesn't carry over is the swap itself.** The proxy swaps a *handle* that callers re-read per
call, and dropping the old handle loses nothing. The control stream's "handle" is a long-lived
duplex conversation:

- its inbound side delivers state (ACL, snapshots, instructions);
- its outbound side carries acks, health and logs;
- the controller keys registry and DB status to it.

So a swap needs:

- **connector side:** stop reading the old inbound **before** reading the new one;
- **controller side:** the old stream's end must not undo the new stream's registration or status.

### 6. Is there a reusable abstraction? (Option C)

**Partly [src].** These are reusable:

- `CertHolder::subscribe()` / `current()` (`cert_holder.rs:102-108`): the "new identity available"
  signal and source;
- `controller_client::build_channel` (`controller_client.rs:37-50`): channel construction.

These are **not** reusable:

- `spawn_controller_channel_refresh` (`agent_server.rs:153-189`):
  - it is specific to `ShieldRegistry`: it hard-wires `replace_controller_channel` and the
    `Arc<RwLock<Channel>>` slot;
  - it runs in a **separate task**, while the control stream must swap inside the task that owns
    `ack_rx` / `ctrl_rx` / the reader. A separate task swapping the stream would create concurrent
    readers and writers.
- There is no reusable stream-ownership or atomic-stream-swap abstraction anywhere in the connector.

### 7. Message ordering: does the proxy mechanism preserve the control-stream guarantees?

**No [src].** The proxy mechanism has no ordering concept because it carries no ordered state.

Re-verified for the control stream:

- **ACL cache replace-only, no version check:** `PolicyCache::update_and_revoked` stores the
  incoming snapshot and computes revocations against the previous one, with no `version` comparison
  (`connector/src/policy/mod.rs:40-46`).
- **ACL versions are not monotonic across controller restarts:** in-memory
  `map[string]*atomic.Uint64` (`controller/internal/policy/notifier.go:16, 38, 64`). So a
  version-based "reject older" rule in the connector would wrongly reject fresh snapshots after a
  controller restart. **That alternative is rejected.**
- **`ScanCommand` is one-shot:** `PushScanCommand` → registry `get` → `send`, not persisted
  (`control_stream.go:306-312`, `discovery.resolvers.go:83`).
- **Other stateful messages have recovery paths:** Pre-Implementation §1 table, re-checked.

Applying the proxy's "swap the handle and let old in-flight work finish on the old one" literally
would mean continuing to read the old stream while the new one is read. That is exactly the
interleaving that can apply an older ACL after a newer one.

### 8. Controller ownership: is `lifecycleMu` still required?

**Yes [src].** The proxy needs nothing on the controller because its calls create no state.

The control stream does create state:

- `Registry.add(connectorID)` on open, `defer Registry.remove(connectorID)` keyed only by ID
  (`control_stream.go:400-401`, `:169-173`);
- the close defer sets `disconnected` + notify (`:409-446`).

With two streams, the old stream's exit removes the new stream's registry entry and flips the DB, and
the activation-before-add ordering (`:362-392` before `:400`) creates the window shown in
Pre-Implementation §2.

So using the proxy's channel pattern on the connector **does not remove** the need for:

- pointer-identity ownership;
- `lifecycleMu` around (registry change + status UPDATE);
- the open-path reorder.

### 9. Failure behaviour comparison

| # | Scenario | Shield-proxy today [src] | Main control stream with the proxy pattern + control-stream additions |
|---|---|---|---|
| 1 | Renewal succeeds | subscriber wakes and rebuilds | ReEnroll arm signals "renewed" |
| 2 | New connection succeeds | swap; old dropped when idle | switch reader + writer to B, drop A; controller A-close is a no-op (`lifecycleMu`) |
| 3 | New connection fails | keep old, retry every 5 s or on the next publish (`:174-183`) | keep A authoritative, retry on the existing 2 → 60 s backoff (`:33-34`); fall back to break/reconnect when A ends or the old cert's `not_after` passes |
| 4 | Controller unavailable | old channel calls fail; rebuild retries | A fails → normal outer-loop backoff (today's behaviour, real `disconnected`) |
| 5 | Old connection fails first | tonic reconnects the old channel with the **old** identity until the swap [inf] | A ends → outer loop reconnects with `certs.current()` (renewed) |
| 6 | Crash during renewal | restart builds a channel from the persisted cert (`main.rs:90, 100`) | same; real disconnect/reconnect (Pre-Implementation §3 D) |
| 7 | Renewed but new connection can't authenticate | build fails → keep old, retry; it never recovers if the new cert is bad until the old expires | same as #3; at old-cert expiry, fall back to break/reconnect, which then also fails → backoff (correct fail-closed) |

**Conclusion:** the proxy's failure **policy** (keep old, retry, swap on success) is directly
applicable. Its 5 s retry is a local constant. The control stream should use its own existing
`BACKOFF_*` constants rather than introduce the proxy's.

### 10. Re-evaluation of the proposed design and final recommendation

**Central answer: Conclusion 2.** The main control stream **can** use the same underlying
connection-replacement pattern as the Shield-proxy:

- `CertHolder` publish;
- `build_channel` with the new identity;
- open the new connection first;
- swap.

It **cannot** reuse the proxy's mechanism unchanged, because:

1. **The proxy swaps a stateless unary-call handle; the control stream is a stateful duplex
   conversation.** The connector must never read two control streams in parallel, because the ACL
   cache is replace-only and versions aren't monotonic across controller restarts (§7).
2. **The controller keys registry and connector status to the stream** (§8). The proxy has no
   controller-side state.
3. **The swap must happen inside the control-stream task**, not in a separate refresher task (§6),
   because that task exclusively owns `ack_rx` / `ctrl_rx` and the reader.

**Design changes:**

- **Simplify the connector part.** Drop the drain step: **switch-and-drop** replaces
  drain-then-switch:

  > ReEnroll renewed → open B (`open_stream` with `certs.current()`, which sends B's initial health
  > report) → from then on read and write **only B** (ack/ctrl/health/Pong go to B's `out_tx`) → drop A
  > entirely.

  Why the drain isn't needed:
  - Each message that can be stranded on A has a recovery path (Pre-Implementation §1 table).
  - B's **initial** health report triggers `pushACLSnapshot` immediately on a version mismatch
    (`control_stream.go:700, 755-788`). So a dropped ACL push on A is replaced within one round-trip.
    Revocation latency is about one RTT, not ≤ 15 s.
  - Never reading A after the switch removes the ordering hazard by construction.
  - The message-loss exposure is the **same as today's** break-and-reconnect: today's path also drops
    A's mailbox (`control_stream.go:150-153`). The only differences are no 1 s gap and no
    `disconnected` state.
  - Less code than the drain: no bounded drain loop, no two-phase reader.
- **Keep the controller part unchanged:** pointer identity + `lifecycleMu` + open-path reorder (§8).

**Final production files (eventual):**

- `connector/src/control_stream.rs`:
  - extract an `open_stream` helper (`:150-197`);
  - the ReEnroll arm (`:357-368`) returns a "renewed" signal;
  - the `run_once` loop does switch-and-drop, keeps A on failure with existing `BACKOFF_*` retries,
    and falls back on A's end or the old cert's `not_after`;
  - no renewal-path sleep.
- `controller/internal/connector/control_stream.go`:
  - `lifecycleMu`;
  - `removeIfCurrent`;
  - activation UPDATE + `add` under the lock;
  - close ownership check + disconnect UPDATE under the lock;
  - notify after unlock.
- **Not changed:**
  - `agent_server.rs` (proxy refresher stays as is), `controller_client.rs`, `cert_holder.rs`,
    `renewal.rs`;
  - shield, client, compilers, notifiers, proto, migrations.

**Tests (after implementation):**

- **Controller unit tests (`control_stream_test.go`):**
  - A closes after B → registry = B, no `disconnected`, no notify;
  - A closes before B registers → real `disconnected` + notify, then `active` + notify;
  - forced interleaving (activation UPDATE of B ↔ close of A) never ends `disconnected` with B live;
  - a revoked connector stays `revoked`.
- **Connector unit tests:**
  - B is opened before A is dropped;
  - after the switch, no message from A is applied (feed A a stale ACL after the switch → ignored);
  - B's initial health is sent;
  - B failure keeps A and retries on `BACKOFF_*`;
  - fallback on A's end;
  - fallback at the old cert's `not_after`;
  - the existing `controller_channel_refresh_tests` stay green.
- **Live:**
  - §11 tests 1–8;
  - O-1: two `Control` streams briefly coexist; a single `connected`, no `disconnected`;
  - O-2: resource Protect/Unprotect during a renewal reaches the shield;
  - **O-3: an ACL change (revoke a group grant) issued during a renewal is enforced by the connector
    within one round-trip of B opening**;
  - R-1 / R-2 in a relay lab.

**Unresolved (non-blocking):**

- two concurrent `Control` streams on the controller have not been live-tested (O-1);
- relay behaviour is not live-tested (R-1/R-2);
- `ScanCommand` / `ScanReport` / `ConnectorLog` loss on stream switch, same as today;
- the TTL-vs-window crash hazard at defaults, which is pre-existing (Pre-Implementation §3 D);
- §9 #5: whether tonic's internal reconnect of the old proxy channel matters is [inf], not verified.
  It is irrelevant to the control stream.

The architecture is confirmed from source:

- in-place rotation is impossible (tonic fixed identity + rustls has no post-handshake client auth);
- channel-replacement make-before-break is the proven pattern (proxy, live);
- the control-stream-specific additions (single-reader switch-and-drop, controller ownership guard)
  are each traced to concrete code.

The open items are verification tests, not design questions. This needs the user's approval of
Option A with switch-and-drop (gate item 1).

`STATUS: ARCHITECTURE CONFIRMED — READY FOR IMPLEMENTATION`

## Implementation

Added 2026-10-01. Implements the approved design: **Option A, make-before-break + switch-and-drop**
(§Certificate Rotation Architecture Comparison → 10). No architectural decision changed.

`STATUS: IMPLEMENTED — UNIT/INTEGRATION TESTS GREEN — LIVE ACCEPTANCE NOT RUN — NOT COMMITTED`

### Production files changed (exactly two)

| File | Change |
|---|---|
| `controller/internal/connector/control_stream.go` | `ConnectorRegistry.lifecycleMu` (separate from the registry `RWMutex`); `removeIfCurrent(id, c) bool` (pointer identity) replaces the ID-only `remove` (deleted — no callers left); `Control` builds the client **before** activation; `activateStream` = lock → revocation-guarded activation UPDATE → `Registry.add` → unlock; `retireStream` = lock → `removeIfCurrent` → (only if current) disconnect UPDATE `WHERE status='active'` → unlock; notifications run in `Control` **after** the lock is released. A superseded stream's close logs `superseded stream closed (newer stream is authoritative)` and changes nothing |
| `connector/src/control_stream.rs` | `open_stream` helper (current cert from `CertHolder` → SPIFFE preflight → new `build_channel` → `control()` → initial health report via `establish_stream`) returning a `ControlStream {out_tx, inbound, cert_not_after_unix, channel}`; the message loop moved to `run_session`, still the single owner of the stream, `ack_rx`, `log_rx`; the ReEnroll arm now returns `MsgAction::ReEnroll` and the renewal runs in the loop; on a renewed cert the loop opens the replacement **while serving the old stream**, then switches (`mem::replace`) and drops the old stream unread; open failure keeps the old stream and retries on `BACKOFF_INITIAL_SECS` doubling to `BACKOFF_MAX_SECS`; fallback (`return Ok(())` → existing outer-loop reconnect) when the old stream ends, or when the old stream's cert reaches `not_after` while a replacement is still pending. The `StreamOps` trait is the network seam (`LiveStreamOps` in production) |

The renewal path has **no sleep**. The outer loop's 1 s sleep after a clean close and the `BACKOFF_*`
error backoff are unchanged and still apply to genuine disconnects/failures and to the two fallbacks.
No new timeout/backoff constants were added.

### Tests added

Controller — `controller/internal/connector/control_stream_lifecycle_test.go` (new, DB-backed via the
existing `setupRevocationTestDB` harness; drives the real `Control` handler with held fake streams):

- `TestControlLifecycle_OldClosesAfterNewActive` — B registers while A is open; A's EOF leaves
  registry = B, status `active`, 0 extra notifies; B's own close is a real disconnect + notify.
- `TestControlLifecycle_OldClosesBeforeNewActive` — A closes first: real `disconnected` + notify,
  then B: `active` + notify.
- `TestControlLifecycle_RetireBlockedDuringActivation` — deterministic version of the §2 race: B's
  activation holds `lifecycleMu` across UPDATE + add; A's retire blocks, then finds it is not current;
  status never leaves `active`, B never removed.
- `TestControlLifecycle_ActivateRetireBothOrders` — both orders of `activateStream`/`retireStream`.
- `TestControlLifecycle_RevokedStaysRevoked` — revoked during overlap: both closes leave `revoked`,
  0 notifies, registry empty; activation of a revoked connector → `PermissionDenied`, not registered.

Test-only edits for the removed `remove`: `control_stream_test.go` (2 call sites),
`acl_push_test.go` (1 call site) → `removeIfCurrent` with the same client.

Connector — `renewal_switch_tests` module in `connector/src/control_stream.rs` (fakes only the
network at `StreamOps`; session loop, message handling and `establish_stream` are production code):

| Requested test | Test |
|---|---|
| 1 new opens before old dropped; 2 renewal switches; 3 old not read after switch; 5 initial health on new stream; renewal-path no-sleep | `renewal_opens_new_stream_first_then_switches_and_drops_old` |
| 4 stale ACL on old stream can't overwrite | `stale_acl_on_old_stream_cannot_overwrite_after_switch` |
| 6 new-stream failure keeps old; 7 backoff preserved (≈2 s, ≈4 s gaps) | `new_stream_failure_keeps_old_stream_and_retries_with_backoff` |
| 8 fallback: old stream ends | `fallback_when_old_stream_ends_before_replacement` |
| 8 fallback: old cert `not_after` | `fallback_when_old_certificate_expires_before_replacement`, `expiry_fallback_only_while_replacement_pending` |
| duplicate/failed renewal opens nothing | `duplicate_or_failed_renewal_opens_nothing` |
| 7 non-renewal ends unchanged (Ok → 1 s path, Err → backoff path) | `non_renewal_endings_unchanged` |

### Tests executed (2026-10-01, local; DB = local `ztna_postgres`)

| Command | Result |
|---|---|
| `gofmt -l` on the two touched Go files | clean |
| `go build ./...`, `go vet ./internal/connector/` | ok |
| `go test -race -count=1 ./internal/connector/` (with `ENROLLMENT_TEST_DATABASE_URL`) | ok (whole package, incl. revocation/goodbye/health/ACL-push tests) |
| `go test -race -run TestControlLifecycle -count=20 ./internal/connector/` | ok (100 lifecycle runs, no race reports) |
| `rustfmt --check src/control_stream.rs` | clean |
| `cargo build`, `cargo clippy --all-targets` | ok; no new warnings in `control_stream.rs` (the pre-existing `too_many_arguments` on `run_control_stream` remains) |
| `cargo test` (connector) | 123 lib passed (was 115; +8 new), 4 integration passed, 1 ignored (pre-existing) |
| `controller_channel_refresh_tests::shield_proxy_channel_rebuilds_on_publish` | ok |
| connector `renewal_switch_tests` ×5 repeated runs | 8/8 each run |

Delta: controller +5 test functions (1 new file); connector +8 tests.

### Deviations from the approved design

None in the architecture. Implementation details worth recording:

1. **The controller part was already in the working tree** at the start of this task (pre-staged,
   uncommitted). It was reviewed against §2 and kept; this task deleted the now-dead `remove`,
   updated its 3 test call sites and added the tests.
2. **A second renewal while a replacement is pending does not cancel the in-flight open.** Cancelling
   could drop a stream the controller has already registered, and that close would be authoritative
   (a real `disconnected`). The pending replacement reads `certs.current()` when each attempt starts,
   so retries pick up the newest cert.
3. **The old-cert expiry fallback is armed only while a replacement is pending.** With no renewal in
   flight the established stream is left alone, as before (no new behaviour, no new timers).
4. **"Usable" = the initial health report was enqueued on the new stream** (same bar as the pre-fix
   `run_once`). The controller's first message (Ping) isn't awaited; waiting for it would add an RTT
   and no safety the controller ownership guard doesn't already give.
5. The new stream's `Channel` is held inside `ControlStream`, so it lives exactly as long as its stream
   (before, it was a `run_once` local).

### Remaining risks (unchanged from the design, still to be closed live)

- O-1 (two concurrent `Control` streams on the controller) has only been tested in-process, not live.
- Fallback paths (old stream ends / old-cert expiry with an open in flight) drop the in-flight
  replacement; if the controller had registered it, its close is a real `disconnected` — same as
  today's break-and-reconnect, and only on the fallback path.
- One-shot `ScanCommand`/`ScanReport`/`ConnectorLog` loss at the switch (same as today).
- Pre-existing TTL-vs-window crash hazard at defaults (§3 D) — not addressed (out of scope).
- Relay R-1/R-2 untested (no relay lab).

### Scope confirmations

- **Client Fix 01 not modified.** The `client/src/*` and `Live-Acceptance-Run-Sheet.md` working-tree
  changes predate this task (mtime 2026-09-30 15:30) and were not touched.
- No shield, proto, migration, compiler, notifier, `CertHolder`, `renewal.rs`, `agent_server.rs` or
  `controller_client.rs` changes.
- **No live acceptance performed.** Next task: §11 tests 1–8, O-1, O-2, O-3, R-1/R-2 if available.
- **Nothing committed.**
