---
title: PENDING-03 — Decouple Transport from ACL — Implementation Report
status: implemented
scope: Track B (ADR-015/016/017/018) Option A, Phases 1 & 3
branch: fixed-pendings
audience: team review / verification
---

# PENDING-03 — Decouple Transport from ACL: Implementation Report

> **Purpose:** a self-contained record for the team to verify what was built, the decision behind
> it, and how to reproduce the checks. Everything here is backed by code + tests on the
> `fixed-pendings` branch (commit index at the end).

---

## TL;DR

The client used to learn a connector's **relay** from fields embedded in the **ACL snapshot**, so
authorization and connectivity were coupled — every relay change forced an ACL recompile, and the
client only re-routed on its next ACL poll. We finished **Track B (Option A)**: relay routing now
lives in a first-class **`TransportSnapshot`** plane with its **own version**, polled by the client
independently of the ACL. A relay change bumps only the transport version — it never recompiles or
re-signs the ACL. The client also re-polls transport immediately on a relay failure.

**State:** implemented (Phases 1 & 3), hardened (7 findings), and tested (client 32/32).
**Deferred by design:** ADR-018 Phase 4 — removing the transitional ACL relay fields (breaking).

---

## 1. The Problem

Relay transport was delivered on two channels, only one decoupled from the ACL:

- **Connector side — already decoupled ✅:** the connector picks its relay from the controller-pushed
  `LabelledRelayList` (`ConnectorControlMessage.relay_list = 17`), an independent message with its
  own version.
- **Client side — coupled ⚠️:** the client learned a connector's relay from
  `ACLConnector.relay_addr` / `relay_spiffe_id` (transitional fields 4/5) **embedded in the ACL
  snapshot**. Consequences:
  - a relay migration forced an **ACL recompile** (authorization work for a connectivity change);
  - the client only re-routed on its **next ACL poll** (up to the refresh interval);
  - **auto-failover** (Placement Engine, ADR-016 Phase 3C) was awkward-to-impossible while the two
    planes shared a version.

**Decision needed:** finish Track B now, keep a hybrid, or defer?

---

## 2. Options We Considered

| Option | What it is | Pros | Cons |
|---|---|---|---|
| **A — Finish Track B** | Add `TransportSnapshot` (own version), client consumes it; later retire the ACL relay fields | Authorization & transport versioned independently; relay migration never recompiles the ACL; unlocks clean auto-failover | Real proto + client + controller work; needs a migration window (dual-write / ACL fallback) |
| **B — Keep the hybrid** | Leave relay coords in the ACL; just make the client rebuild transports on ACL-version change | Much smaller; fixes the felt bug | Planes stay coupled; every relay flap still recompiles ACLs; auto-failover stays awkward |
| **C — Defer** | Ship current state; revisit later | Zero effort now | Known-limited; blocks auto-failover |

---

## 3. Decision: **Option A** — and why it's the best

**Chosen: Option A.**

Rationale:

1. **It removes the root coupling, not just the symptom.** B only papers over the felt bug (slow
   re-route) while leaving authorization and connectivity on the same version. A separates them at
   the data-model level, so the two planes evolve independently forever after.
2. **It's the prerequisite for auto-failover.** The Placement Engine (ADR-016 Phase 3C) needs to
   re-home a connector to a healthy relay *without* touching authorization. That is only clean when
   transport has its own version/epoch — i.e. Option A. B would make every failover a workspace-wide
   ACL recompile.
3. **It stops relay churn from doing authorization work.** Under A, a relay flap bumps only the
   transport version; the (potentially large, signed) ACL snapshot is neither recompiled nor
   re-sent. That's less controller CPU and less client churn.
4. **The decoupling invariant is enforceable and testable.** Relay lifecycle fires
   `NotifyTopologyChange` (transport); policy changes fire `NotifyPolicyChange` (ACL) — never the
   other's plane. This invariant is now a checkable property (see Verification).

**Honest trade-off:** A is more work and needs a transition window. We keep the transitional ACL
relay fields **and** a client-side ACL fallback during the window (so nothing breaks if the transport
snapshot is briefly absent). Removing them is a **breaking** change, deliberately deferred as
**ADR-018 Phase 4** (see §7). So A was adopted in full for behavior, with the final field-removal
scheduled later — the standard safe migration path.

---

## 4. What Was Built (how it's done)

### 4.1 Proto — new transport plane (`proto/client/v1/client.proto`)
- Messages: `TransportSnapshot`, `TransportRemoteNetwork`, `TransportConnector`.
- RPC: `GetTransportSnapshot(GetTransportSnapshotRequest) → GetTransportSnapshotResponse`.
- Poll semantics: request carries `known_version`; response returns `up_to_date = true` (and omits
  the body) when the client is already current.
- Defined in `client.v1` (not `connector.v1`) to avoid an import cycle.

### 4.2 Controller — transport plane (`controller/internal/transport/`)
- `store.go` — reads workspace connectors + their relay placement.
- `cache.go` — epoch-CAS `SnapshotCache` (ADR-013 pattern) so a compile raced by a change is never
  cached stale.
- `notifier.go` — `NotifyTopologyChange(workspace, affectedConnectors)`: bumps the **transport**
  version and invalidates the transport cache. **Never** touches the ACL plane.
- `compiler.go` — `CompileTransportSnapshot` / `GetOrCompile`.
- Handler: `GetTransportSnapshot` in `internal/client/service.go` — same device-ownership +
  revocation gate as `GetACLSnapshot`; returns `up_to_date` when `known_version` matches.

### 4.3 Controller — trigger decoupling
- Relay lifecycle (`internal/relay/heartbeat.go`, `expiry.go`, `provision.go`) and connector
  connectivity changes fire **`NotifyTopologyChange`** (transport version).
- Policy/ACL changes continue to fire **`NotifyPolicyChange`** (ACL version).
- Connector relay-attachment changes fire **both** (so the transitional ACL fields stay correct
  during the migration window).

### 4.4 Client — consume the transport plane (`client/src/`)
- `runtime.rs` — `transport_snapshot` + `transport_last_sync_at` in `RuntimeState`.
- `daemon.rs` — `fetch_transport_snapshot` (poll w/ `known_version`), `fetch_and_store_transport`,
  and `resolve_entry_coords`: **transport-preferred, ACL-fallback** routing. The 60s scheduler
  refreshes both planes; a relay failure signals an **early transport re-poll** (`run_transport_recovery`).

### 4.5 Delivery model (Q1 decision)
The **client is the sole consumer** of the transport snapshot (it polls). Connectors do **not**
consume it, so the earlier connector-push path was removed (commit `90803e4`) and the unused
connector proto field retired (`#5`).

---

## 5. Hardening (findings addressed after the core landed)

Two rounds of review hardened the client transport path. All are on `fixed-pendings`.

**Round 1 (findings #1–#8):**
- `#1` IPv6-safe connector tunnel address (`net.JoinHostPort`).
- `#2` connector revocation invalidates **both** planes + resists resurrection + closes the stream.
- `#3` documented single-controller limitation of in-memory transport versions.
- `#4` run transport recovery **off** the ACL scheduler thread (so revocation polling never stalls).
- `#5` retired the unused connector `TransportSnapshot` proto field.
- `#7` documented workspace-wide transport disclosure (accepted, consistent with the ACL).
- `#8` only **network** failures trigger a transport resync; auth fails closed.

**Round 2 (this branch's concurrency/classification set + efficiency):**
- **transport_sync_lock** — serialize the client's whole *read-version → fetch → store* so a
  concurrent 60s tick and relay-recovery task can't overwrite a newer snapshot with an older one.
- **tunnel_restart_lock** — serialize tunnel `down→up` across recovery / tick / IPC; re-check after
  acquiring so a queued caller no-ops instead of corrupting the live TUN session.
- **recovery cooldown from completion** — cooldown is measured from when recovery *finishes* (not
  starts), so a long recovery can't immediately re-burst against a dead relay.
- **handshake error classification** — typed `FramedJsonError` splits transport I/O (→ resync) from
  protocol failures (malformed JSON / oversized frame → no resync). Broad `Self::Io(_)`, verified
  against the quinn 0.11.9 error mapping.
- **version-aware `GetACLSnapshot`** — added `known_version`/`up_to_date` (mirrors the transport
  RPC) so an unchanged ACL is no longer re-sent on every poll. Safe because every ACL-content change
  bumps the policy version.

---

## 6. How to Verify (reproduce the checks)

### 6.1 Build all components (proto is shared)
```bash
buf generate                                       # regen Go stubs (Rust regenerates on cargo build)
cd controller && go build ./...
cd connector && cargo build
cargo build --manifest-path shield/Cargo.toml
cd client && cargo build
```

### 6.2 Tests
```bash
# Controller (needs a local Postgres for the integration tests)
cd controller && PKI_TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/postgres \
  go test ./internal/policy/... ./internal/transport/... ./internal/connector/... ./internal/relay/...

# Client
cd client && cargo test          # expect: 32 passed
```

### 6.3 Behaviours worth spot-checking
- **Decoupling invariant:** grep confirms only `NotifyPolicyChange` invalidates the *policy* cache,
  and only `NotifyTopologyChange` invalidates the *transport* cache — neither crosses planes.
- **Routing preference:** client tests `resolve_prefers_transport_plane_when_rn_present` and
  `resolve_falls_back_to_acl_when_transport_lacks_rn` prove transport-preferred / ACL-fallback.
- **Serialization guard (Fix 1):** the test `transport_sync_serializes_concurrent_fetches` was
  verified to **FAIL** (`seen = [0, 0]`) when `transport_sync_lock` is removed and **pass** with it —
  so it genuinely guards the fix, not a tautology.
- **Cooldown / single-flight (Fix 3):** `may_start_transport_recovery` unit tests.
- **Handshake classification (Fix 4):** `malformed_… / disconnected_… / oversized_handshake_…` tests.
- **ACL up_to_date:** `acl_up_to_date_keeps_cached_and_reports_unchanged`.

---

## 7. Deferred / Out of Scope

- **ADR-018 Phase 4 (deferred, code):** the transitional `ACLConnector.relay_addr` (4) /
  `relay_spiffe_id` (5) fields still ship in the ACL, and the client keeps an ACL fallback. Retiring
  them (reserve the field numbers, drop the fallback) is a **breaking** change scheduled later.
- **Multi-controller / persistent version epochs (out of scope):** transport (and ACL) versions are
  process-local and in-memory. Multi-replica / restart-safe versioning is **PENDING-12 (Controller
  HA)** and must cover both notifiers together.

## 8. Still-open doc close-out (not code)
To formally close PENDING-03: flip **ADR-015/017/018** to Accepted, mark **PENDING-03 ✅** in
`pending/README.md`, add a **Session Log** entry. (Not done yet.)

---

## 9. Commit Index (branch `fixed-pendings`)

**Core implementation (Sprint 13):**
| Commit | Description |
|---|---|
| `f14e35d` | Phase A — transport plane data path |
| `51c5a0c` | Phase B — transport propagation + trigger decoupling |
| `90803e4` | client-poll delivery, drop connector push (Q1 = client is the consumer) |
| `0536043` | Q3 — refresh transport plane on connector connectivity changes |
| `73ab176` | Phase C — client transport cache, route via `remote_network_id` |

**Round 1 hardening (findings #1–#8):**
| Commit | Description |
|---|---|
| `59110e0` | #4 — run transport recovery off the ACL scheduler thread |
| `43f024c` | #1 — IPv6-safe connector tunnel address |
| `5bf840d` | #2 — connector revocation invalidates both planes, closes stream |
| `ef7f72c` | #3 — document single-controller version limitation |
| `a6ab25d` | #5 — retire unused connector `TransportSnapshot` field |
| `3c937bd` | #7 — document workspace-wide transport disclosure (accepted) |
| `bb11131` | #8 — only network failures trigger resync; auth fails closed |

**Round 2 hardening + tests + docs:**
| Commit | Description |
|---|---|
| `a43fbc4` | transport_sync_lock, tunnel_restart_lock, cooldown-from-completion, handshake classification, version-aware ACL |
| `50b7fe5` | regression tests (up_to_date branches + serialization guard) |
| `4fe8055` | PENDING-03 doc marked implemented + resolution recorded |
