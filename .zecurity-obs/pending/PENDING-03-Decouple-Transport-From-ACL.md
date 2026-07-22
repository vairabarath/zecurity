---
type: adr
status: implemented
id: PENDING-03
domain: relay
priority: P1
created: 2026-07-03
related:
  - ADR-015-Transport-Control-Plane
  - ADR-016-Controller-Labelled-Connector-Probed-Relay
  - ADR-017-Transport-Propagation
  - ADR-018-Migration-Strategy
tags: [pending, adr, relay, transport, acl]
---

# Pending ADR 03 — Decouple Transport from ACL (finish Track B)

> **Status: IMPLEMENTED (Option A — ADR-018 Phases 1 & 3).** The sequencing decision was made —
> Option A ("finish Track B") — and executed in Sprint 13, with follow-up hardening completed on the
> `fixed-pendings` branch (see **Resolution** below). What remains is doc close-out (flip
> ADR-015/017/018 to Accepted, mark ✅ in `pending/README.md`, Session Log) and the deliberately
> deferred **ADR-018 Phase 4** — retiring the transitional `ACLConnector` relay fields (a breaking
> change). The Options / Recommendation / Open Questions below are retained as the original decision
> record.

## Resolution — Implemented (Sprint 13, Option A)

**Decision:** Option A. The client's relay routing was moved off the ACL snapshot into a first-class
`TransportSnapshot` plane with its own version, so relay changes propagate independently of the ACL.

**Implemented (code):**
- `TransportSnapshot` / `TransportRemoteNetwork` / `TransportConnector` messages + `GetTransportSnapshot`
  RPC (poll with `known_version` → `up_to_date`) in `proto/client/v1/client.proto`.
- Controller transport plane: `internal/transport/` (store, epoch-CAS cache, notifier, compiler) and
  the `GetTransportSnapshot` handler in `internal/client/service.go`. Relay lifecycle fires
  `NotifyTopologyChange` (transport version) independently of `NotifyPolicyChange` (ACL version) —
  the Track B decoupling invariant.
- Client consumes it: `fetch_transport_snapshot` + `resolve_entry_coords` (transport-preferred,
  ACL-fallback) in `client/src/daemon.rs`; a relay failure triggers an early transport re-poll.

**Follow-up hardening (`fixed-pendings`):**
- `transport_sync_lock` — serialize the client's transport fetch/store (no stale-version overwrite).
- `tunnel_restart_lock` — serialize tunnel down→up across recovery / 60s tick / IPC.
- recovery cooldown measured from *completion* (no runaway polling against a dead relay).
- typed `FramedJsonError` handshake classification — only transport I/O triggers a resync, not
  malformed/oversized replies.
- version-aware `GetACLSnapshot` (`known_version`/`up_to_date`), mirroring the transport RPC, so an
  unchanged ACL is no longer re-sent on every poll.
- Regression tests incl. a serialization guard proven to fail without the lock. Client 32/32 pass;
  all components build. Commits `a43fbc4` (fixes) + `50b7fe5` (tests), atop the earlier per-finding
  commits on the branch.

**Still deferred (by design) — ADR-018 Phase 4:** the transitional `ACLConnector.relay_addr` (4) /
`relay_spiffe_id` (5) fields remain in the ACL and the client keeps an ACL fallback. Retiring them
(reserve the field numbers, drop the fallback) is a breaking change scheduled as a later step.

**Out of scope (tracked elsewhere):** multi-controller / persistent transport-version epochs →
PENDING-12 (Controller HA); both notifiers must be made replica-safe together.

## Context / Original State (pre-Sprint 13)

> Superseded by the Resolution above — kept for history. When this ADR was written, relay transport
> was delivered on **two channels**, only one decoupled from the ACL:

- **Connector side (decoupled ✅):** the connector picks its relay from the controller-pushed
  `LabelledRelayList` (`ConnectorControlMessage.relay_list = 17`), an independent message with its
  own version. Static `RELAY_ADDR`/`RELAY_SPIFFE_ID` config has already been **removed** from the
  connector (`config.rs` has none; `relay_selector.rs` drives everything;
  `maintain_registration` is dead code).
- **Client side (was coupled ⚠️ — now decoupled, see Resolution):** the client learned a connector's
  relay from `ACLConnector.relay_addr`/`relay_spiffe_id` (transitional fields 4/5) **embedded in the
  ACL snapshot**, so a relay change forced an ACL recompile and the client only re-routed on its next
  ACL poll.
- **`TransportSnapshot` did not exist yet** at the time of writing — it now does (see Resolution).

## Problem — Decision Needed

Do we finish Track B now (move the **client's** relay routing off the ACL snapshot into a
first-class `TransportSnapshot`), defer it, or stop at the current "good enough" hybrid?

## Options

### Option A — Finish Track B per ADR-015/017
Add `TransportSnapshot` (own version/epoch), have the client consume it, then reserve
`ACLConnector` fields 4/5 and remove relay coords from the ACL snapshot (fields 6, 9).
- **Pros:** authorization and transport version independently; relay migration no longer triggers
  ACL recompiles; enables clean Placement-Engine auto-failover. **Cons:** real proto + client +
  controller work; migration window with dual-write.

### Option B — Keep the hybrid, just fix propagation
Leave relay coords in the ACL snapshot but fix the client to rebuild transports on ACL-version
change (see Relay review F3) so migrations actually propagate without `down`/`up`.
- **Pros:** much smaller; fixes the felt bug. **Cons:** authorization and transport stay coupled;
  every relay flap still recompiles ACLs; Placement Engine stays awkward.

### Option C — Defer
Ship current state; revisit when auto-failover (Placement Engine) is prioritized.

## Recommendation (non-binding)
If automatic relay failover is on the near roadmap → Option A (it's the prerequisite). If not →
Option B now (cheap correctness win) and schedule A later. Either way, **fix client transport
rebuild-on-ACL-change** (F3) regardless.

## Open Questions
- Is auto-failover (Placement Engine, ADR-016 Phase 3C) actually prioritized? That decides A vs B.
- Client transport poll interval / TTL and its relationship to relay drain timeout (ADR-017 cites 120s).

## Rough Effort / Priority
**A: L · B: S–M**, P1. Sequencing depends on the Placement-Engine decision.
