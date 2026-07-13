---
type: phase
member: M3
sprint: 13
phase: 3
title: Client Transport Cache — consume GetTransportSnapshot, route via remote_network_id
status: planned
depends_on:
  - Sprint13/Member3-Go-Rust/Phase1-Transport-Proto-and-Compiler
tags: [rust, client, transport, daemon, acl-decoupling, pending-03]
---

# Phase 3 — Client Transport Cache (non-breaking)

> Executes **ADR-018 Phase 3**. Depends on Phase 1 (the `GetTransportSnapshot` RPC must exist).
> Can run in parallel with Phase 2 — the client only needs the RPC, not the propagation rewiring.

## Goal

Make the client resolve a connector's relay from a **separate Transport Cache** (fetched via
`GetTransportSnapshot`, keyed by `remote_network_id`) instead of reading it out of the ACL snapshot —
while keeping the ACL fields as a fallback so nothing breaks during rollout.

## Why (context)

This is the consumer side of the decoupling and the point where **Option A supersedes M3's own
Option-B mitigation** (PENDING-03 `29a766e`). Today `build_transports_by_resource` reads the relay
from `ACLConnector` fields 4+5, so the client re-routes only on a new *ACL* version. After this phase
the client re-routes on a new *transport* version — a channel that moves independently of policy.

## Files

| File | Change |
|------|--------|
| `client/src/daemon.rs` | add `transport_snapshot` to `SharedState`; add `fetch_and_store_transport` (mirror `fetch_and_store_acl`, ~line 1125); update `build_transports_by_resource` |
| `client/src/daemon_tests.rs` (or the existing client test file) | cache-hit / fallback / version-change tests (separate `_tests.rs` per repo convention) |

## Behaviour

1. **Fetch.** `fetch_and_store_transport` polls `GetTransportSnapshot` on the **same 60s TTL** as
   ACL, passing `known_version`; on `up_to_date == true`, keep the cached snapshot (skip deserialize).
   Store into `SharedState.transport_snapshot`. Model it on `fetch_and_store_acl` /
   `fetch_acl_snapshot_with_refresh` (token refresh reuse).
2. **Route.** In `build_transports_by_resource`: for each authorized ACL entry, take its
   `remote_network_id`, look it up in the Transport Cache to find the connector + relay coords, and
   build the transport from those.
3. **Fallback (required).** If the Transport Cache is empty or lacks that `remote_network_id`
   (old controller without the RPC, or convergence window), **fall back to `ACLConnector` fields
   4+5** exactly as today. This is what makes the rollout non-breaking — do not remove it.
4. **Early resync (retarget the PENDING-03 signal).** The `relay_resync: Arc<Notify>` you added in
   `29a766e` should now wake `fetch_and_store_transport` (transport re-poll) rather than the ACL
   scheduler — a data-plane relay failure should trigger an early *transport* resync. Keep all the
   good parts you already built: failure-vs-normal-close distinction, backoff-until-version-changes,
   30s cooldown. Just point them at the transport plane.

## Convergence note

ACL and Transport are independently versioned, so there's a brief window where a resource is
authorized but its transport entry hasn't arrived yet (or vice versa). Per ADR-015 this is transient
unavailability, **not** a security issue — never route without an authorizing ACL entry. If transport
is missing for an authorized entry, fall back to ACL fields (step 3), then to direct-only.

## Build check

```bash
cd client && cargo build
cd client && cargo test
```

## Verification (drive it)

With the controller from Phase 1/2: confirm the client routes via the Transport Cache when present,
and — by serving a snapshot with the transport entry stripped — that it cleanly falls back to the ACL
relay fields with no regression.

## Implementation checklist

- [ ] **C1** `SharedState.transport_snapshot` + `fetch_and_store_transport` (60s TTL, `known_version`)
- [ ] **C2** `build_transports_by_resource` resolves relay from Transport Cache on `remote_network_id`; ACL 4+5 fallback retained
- [ ] **C3** retarget the `relay_resync` signal to wake the transport re-poll
- [ ] **Build gate:** `cd client && cargo build`
- [ ] Tests in a separate `_tests.rs` file

## Post-Phase Fixes

_None yet._
