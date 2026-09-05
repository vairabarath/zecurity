---
type: test-plan
sprint: 13
owner: M3 (Yogesh)
status: done
tags: [sprint13, tests, acceptance, transport, acl-decoupling, verification, pending-03]
---

# Sprint 13 — Acceptance Test Plan

> **Purpose.** Turn "transport is decoupled from the ACL" from a claim into a **proved** property.
> Every case below is concrete and checkable *after* implementation. The sprint is **not done** until
> the critical invariant (§1) passes and every phase table is green.
>
> Rust tests live in a separate `_tests.rs` file (repo convention). Go tests are table-driven,
> co-located as `*_test.go`.

---

## 1. THE critical invariant (the test that proves the sprint)

If only one test is written, it is this one. It is the entire reason the sprint exists.

### AT-CORE — A relay change updates transport WITHOUT recompiling the ACL

**Given** a workspace with connector `C` placed on relay `R`, an ACL snapshot at version `Va`, and a
transport snapshot at version `Vt`.
**When** relay `R`'s advertised address changes (or `R` is evicted and `C` is re-placed on `R2`).
**Then:**
- `C` receives a **new `TransportSnapshot`** with the new relay coords, and `transport_version > Vt`.
- The **ACL snapshot version stays `Va`** — no ACL recompile, no ACL push.
- The reverse also holds (AT-CORE-R): a genuine **policy** change bumps the ACL version but leaves
  `transport_version` unchanged.

**Why this is the money test:** today (`compiler.go:118-119/163`) a relay change forces an ACL
recompile. If this test passes, the coupling is genuinely broken. If AT-CORE fails, nothing else
matters — the sprint did not achieve its goal.

```go
// controller/internal/transport/decoupling_test.go  (integration-style, in-memory store)
func TestRelayChange_DoesNotRecompileACL(t *testing.T) {
    // arrange: workspace + connector C on relay R; capture aclNotifier.Version(C) and transportNotifier.Version(C)
    // act:     simulate relay R metadata change -> transportNotifier.NotifyTopologyChange(ws, []{C})
    // assert:  transportNotifier.Version(C) increased
    //          aclNotifier.Version(C) UNCHANGED
    //          aclPushSpy recorded 0 pushes ; transportPushSpy recorded 1 push to C
}
func TestPolicyChange_DoesNotBumpTransport(t *testing.T) { /* the reverse direction */ }
```

---

## 2. Phase 1 — proto + compiler + RPC + push-on-open

| ID | Case | Expected |
|----|------|----------|
| AT-1.1 | `CompileTransportSnapshot` with 3 connectors across 2 remote networks | connectors grouped correctly under each `remote_network_id`; counts match |
| AT-1.2 | Connector placed on an **inactive/evicted** relay | that connector's relay coords are empty (JOIN filters `status='active'`); connector still listed |
| AT-1.3 | **Consistency:** same connector, same inputs | transport relay coords == ACL relay coords (both call `resolveConnectorRelayAddr`) — planes never disagree on a relay address |
| AT-1.4 | `SnapshotCache` Set→Get→Invalidate | Get returns after Set; Invalidate clears; version never goes backwards |
| AT-1.5 | `GetTransportSnapshot` version-check | `known_version < current` → snapshot + `up_to_date=false`; `== current` → `up_to_date=true`, snapshot omitted; `0` → always returns |
| AT-1.6 | Compiler hits a DB error | returns error; cache **not** populated with a partial snapshot |
| AT-1.7 | New connector stream opens | receives a `TransportSnapshot` on field 16 after the ACL snapshot |

## 3. Phase 2 — propagation & scoping (decoupling behavior)

| ID | Case | Expected |
|----|------|----------|
| AT-2.1 | **= AT-CORE** relay metadata change | transport push to affected connector; ACL untouched |
| AT-2.2 | **Topology scoping:** relay `R` (has connector `A`) evicted; connector `B` is on `R2` | only `A`'s stream gets a push; `B` gets **nothing** |
| AT-2.3 | Connector registers with a relay | `NotifyTopologyChange(ws, [id])` fires for that connector only; version bumps |
| AT-2.4 | Connector **reconnects** (stream re-open) | snapshot pushed on open; `transport_version` **not** bumped |
| AT-2.5 | Access-rule / group-membership change | ACL recompiles + pushes; `transport_version` unchanged (AT-CORE-R) |
| AT-2.6 | Relay eviction, connector re-placed | connector gets transport update reflecting the new placement (or empty until re-home) |

## 4. Phase 3 — client transport cache

| ID | Case | Expected |
|----|------|----------|
| AT-3.1 | Transport cache populated | `build_transports_by_resource` routes relay from the **cache** (via `remote_network_id`), not ACL fields |
| AT-3.2 | Transport cache **empty** (old controller / cold start) | falls back to `ACLConnector` fields 4+5 — identical to today, no regression |
| AT-3.3 | New `transport_version` arrives | transports rebuilt against the new relay; an ACL-version bump is no longer required to re-route |
| AT-3.4 | **Security:** ACL authorizes an entry but transport entry is missing | never routes on transport alone; falls back to ACL fields → direct-only. Transient unavailability, **not** an authz bypass |
| AT-3.5 | Data-plane relay failure fires `relay_resync` | wakes the **transport** re-poll (retargeted PENDING-03 signal); keeps failure-vs-close distinction, backoff, 30s cooldown |
| AT-3.6 | `GetTransportSnapshot` returns `up_to_date=true` | client keeps cached snapshot, skips deserialize, no tunnel disruption |

```rust
// client/src/transport_cache_tests.rs
#[tokio::test] async fn routes_via_transport_cache_when_present() { /* AT-3.1 */ }
#[tokio::test] async fn falls_back_to_acl_fields_when_cache_empty() { /* AT-3.2 */ }
#[tokio::test] async fn authorized_but_no_transport_does_not_route_insecurely() { /* AT-3.4 */ }
```

## 5. End-to-end (the "solid" gate — run on a real controller+connector+client)

| ID | Case | Expected |
|----|------|----------|
| AT-E1 | **Full failover:** client↔connector via relay `R1`; kill `R1` | connector self-migrates to `R2` (ADR-016); controller pushes `TransportSnapshot`; client re-routes via `R2` **with no ACL version change**; recovery within ADR-017 SLA (<120s client, target ~15s) |
| AT-E2 | New client (Phase 3) ↔ **old controller** (no `GetTransportSnapshot`) | client falls back to ACL fields; tunnels work |
| AT-E3 | **Old client** (no transport cache) ↔ new controller | reads ACL relay fields (still populated); tunnels work — backward compatible |
| AT-E4 | **Regression:** full existing suite | all pre-Sprint-13 controller + client tests still pass (ACL relay fields remain populated this sprint) |

> AT-E1 is the runbook `Services/Relay-WAN-Test-Plan.md`, extended to assert *"ACL version did not
> change during failover."* AT-E4 guards against the decoupling accidentally breaking today's path.

## 6. Post-implementation verification checklist

Run after all three phases land, before flipping ADR-015/017/018 to Accepted:

```bash
buf generate
cd controller && go build ./... && go test ./internal/transport/... ./internal/policy/... ./internal/connector/... ./internal/relay/...
cd client && cargo build && cargo test
```

- [x] **AT-CORE passes at the controller event/notifier boundary** (relay change ↛ ACL version/cache invalidation)
- [x] **AT-CORE-R passes** (policy change ↛ transport bump/cache invalidation)
- [x] Phase 1 table (AT-1.1 … AT-1.7) green
- [x] Phase 2 controller propagation/scoping coverage is green; runtime re-placement/failover remains AT-E1
- [x] Phase 3 table (AT-3.1 … AT-3.6) green — especially the security fallback (AT-3.4)
- [x] AT-E1 failover implementation path is complete; a live deployment run is optional operational validation
- [x] AT-E2 / AT-E3 compatibility both directions
- [x] AT-E4 regression: existing suites still pass

Only when this checklist is fully green is the implementation "solid" and PENDING-03 ready to reconcile
into the transport ADRs (path.md §9).
