---
type: adr
status: pending
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

> **Status: PENDING — for team discussion.** Design already exists in ADR-015/017/018; this is a
> *sequencing & scope* decision, not a greenfield design. On adoption, this may just become an
> execution plan under the existing ADRs rather than a new ADR number.

## Context / Current State

Relay transport is delivered on **two channels** today, only one decoupled from the ACL:
- **Connector side (decoupled ✅):** the connector picks its relay from the controller-pushed
  `LabelledRelayList` (`ConnectorControlMessage.relay_list = 17`), an independent message with its
  own version. Static `RELAY_ADDR`/`RELAY_SPIFFE_ID` config has already been **removed** from the
  connector (`config.rs` has none; `relay_selector.rs` drives everything;
  `maintain_registration` is dead code). *Note: the Relay-Roadmap still lists "remove static
  RELAY_ADDR" as future Track-B work — the code is ahead of the roadmap here.*
- **Client side (still coupled ⚠️):** the client learns a connector's relay from
  `ACLConnector.relay_addr`/`relay_spiffe_id` (transitional fields 4/5) **embedded in the ACL
  snapshot** (`client/src/daemon.rs` `build_transports_by_resource`). So a relay change forces an
  ACL recompile, and the client only re-routes on its next ACL poll + transport rebuild.
- **`TransportSnapshot` does not exist yet** (no code in connector or client).

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
