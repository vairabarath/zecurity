---
type: planning
status: planned
sprint: 13
owner: M3 (Yogesh)
tags:
  - sprint13
  - transport
  - acl-decoupling
  - relay
  - proto
  - pending-03
  - execution-path
  - solo-sprint
---

# Sprint 13 — Decouple Transport from ACL (Track B / PENDING-03 Option A)

> **Solo sprint — M3 (Yogesh).** This is the one workstream that is fully independent of the
> provider plane (Sprint 12). It touches proto + controller (Go) + client (Rust), and does **not**
> overlap `internal/provider`, relay provisioning, or the tenant/provider auth work.
>
> **Read this whole file first, then your first phase file.** The design is already approved — you
> are *executing* ADR-015 / ADR-017 / ADR-018, not designing. This document explains **what** and,
> more importantly, **why**.

---

## 1. Why this sprint exists (read this — it's the whole point)

### The problem in one sentence
Today the client learns *which relay to route a connector through* from the **ACL snapshot** — so a
relay change forces an **authorization recompile**, and the client only re-routes on its next ACL
poll. Transport and authorization are two different domains sharing one pipeline.

### What "coupled" means concretely, in our code
- `controller/internal/policy/compiler.go:118-119` stamps each connector's `RelayAddr` /
  `RelaySpiffeId` **into the ACL snapshot** (`ACLConnector` fields 4+5), and `:163` reads
  `GetActiveRelay()` for the snapshot-level relay (fields 6+9).
- `client/src/daemon.rs` `build_transports_by_resource` reads the relay for a connector **out of
  those ACL fields**.
- So: relay heartbeat / relay address change / connector re-home → `NotifyPolicyChange` →
  **the entire workspace's ACL snapshot recompiles and re-pushes** — even though no access rule
  changed. And the client can't re-route until it polls a *new ACL version*.

### Why it hurts (the two costs)
1. **Wasted, wrong-scoped invalidation.** A relay flap recompiles authorization for every connector
   in the workspace and re-fans ACL snapshots to all of them. Authorization invalidation is
   *workspace-scoped*; transport invalidation should be *topology-scoped* (only the connectors on
   the affected relay). In a 50-connector workspace, one relay failure should touch a handful of
   connectors, not all 50.
2. **Slow, coupled failover.** The client re-homes only when the connector's new relay shows up in a
   recompiled ACL snapshot it happens to poll. M3's own PENDING-03 Option-B fix (early ACL resync on
   transport failure, commit `29a766e`) cut the *felt* latency from ~60s to ~15s — but it is a
   **mitigation on top of the coupling, not the fix.** Every relay flap still recompiles ACLs, and
   authorization + transport versions still move together.

### What we're building instead (Option A / Track B)
Two **independent** control planes on the controller, correlated at the consumer by one shared join
key (`remote_network_id`, which **already exists** in `ACLEntry` field 9 and `ACLRemoteNetwork`
field 1):

```
             Controller
   ┌──────────────────────────────┐
   │  connector_relay_placement    │  ← produced by ADR-016 (Sprint 11), already shipped
   └───────────────┬──────────────┘
       ┌───────────┴───────────┐
       ▼                       ▼
  ACL Compiler          Transport Compiler   ← NEW (this sprint)
       │                       │
       ▼                       ▼
  ACL Snapshot         Transport Snapshot     ← NEW: relay coords live HERE, not in the ACL
  (authorization)      (connectivity)
```

Client runtime: **ACL Cache** answers *"is this authorized?"* → then **Transport Cache**, keyed by
`remote_network_id`, answers *"through which connector + relay?"*. Relay migration now propagates on
its own fast, topology-scoped channel with **no ACL recompile**.

### The value this adds to the product (why it's worth an L-effort sprint)
- **Seamless, low-latency relay failover** — the clean prerequisite for the relay-fleet uptime story
  we're selling. Relay migration no longer waits on an ACL recompile+poll.
- **Fleet operability without policy blast radius** — drain / rebalance / add a relay as a
  transport-plane action that never churns tenant policy. Complements the provider console (07b).
- **Controller scalability** — a relay flap fans out a small topology delta to affected connectors,
  not full ACL recompiles to the whole workspace.
- **Blast-radius isolation** — authorization and transport are independently versioned; a transport
  change can never perturb authorization state or bump the ACL version (same isolation philosophy as
  the tenant/provider split we just shipped in ADR-021).

---

## 2. Scope guardrails (what this sprint is NOT)

| Not in scope | Why |
|--------------|-----|
| **A Placement Engine / distributed singleton** | Superseded. ADR-016 (Sprint 11) already does connector→relay assignment via `LabelledRelayList` self-selection + make-before-break migration. Sprint 13 only *publishes the resulting topology* to clients. Do **not** build a relay-assignment engine. |
| **ADR-018 Phase 4 — removing ACL relay fields** | Breaking + point-of-no-return (reserving proto field numbers). Requires ~4 weeks of fleet compat after the client ships. **Deferred to a later coordinated cleanup sprint.** This sprint keeps the ACL relay fields populated as a fallback. |
| **Connector config changes** | `RELAY_ADDR` was already removed in Sprint 11. Nothing to do. |
| **Proto field renumbering** | Never. Field 16 on `ConnectorControlMessage` is already reserved for `TransportSnapshot`. Add, don't renumber. |

> **The safety property of this sprint:** every change is **additive and rollback-safe**. New clients
> that hit an old controller (no `GetTransportSnapshot`) fall back to ACL fields 4+5. Old clients
> ignore the new plane entirely. Nothing breaks until the deferred Phase 4.

---

## 3. Source of truth

| Doc | Role |
|-----|------|
| `Decisions/ADR-015-Transport-Control-Plane.md` | Target architecture (the two-plane design). |
| `Decisions/ADR-017-Transport-Propagation.md` | Propagation pipeline: Notifier, triggers, compiler SQL, RPC shape, convergence SLA. **Has an implementation checklist — follow it.** |
| `Decisions/ADR-018-Migration-Strategy.md` | Phased rollout + exact proto reserved statements. **We do Phases 1 & 3 only; Phase 4 is deferred.** |
| `pending/PENDING-03-Decouple-Transport-From-ACL.md` | The decision record (Option A chosen). Reconciled at sprint end (§7). |
| [[Sprint13/Acceptance-Test-Plan]] | **The verification gate.** Concrete, checkable test cases per phase + the critical invariant. The sprint is not done until this checklist is green. |

---

## 4. Team assignment

| Member | Role | Area |
|--------|------|------|
| **M3 (Yogesh)** | Go + Rust | proto (`connector.proto`, `client.proto`) · `controller/internal/transport/*` · `relay/{heartbeat,expiry}.go` rewire · connector control-stream push · `client/src/daemon.rs` transport cache |

Solo sprint. No conflict-zone table needed — no other member touches these files this sprint.
(If the 2-month timeline slips, the controller Go work in Phases A/B is the natural place for M2 to
pair; the client work in Phase C is M3-only.)

---

## 5. Execution path

```text
Phase A — Transport proto + Transport Compiler + GetTransportSnapshot RPC + push-on-open   (Day 1)
  ↓
Phase B — TransportNotifier + trigger rewiring (relay heartbeat/expiry/registration) + topology-scoped push
  ↓
Phase C — Client Transport Cache: fetch_and_store_transport + join on remote_network_id
```

### Phase A — Transport plane data path *(non-breaking; ADR-018 Phase 1)*
> See [[Sprint13/Member3-Go-Rust/Phase1-Transport-Proto-and-Compiler]].

- [ ] **A1** `proto/connector/v1/connector.proto` — add `TransportSnapshot` / `TransportRemoteNetwork` / `TransportConnector`; wire `transport_snapshot = 16` into `ConnectorControlMessage` (the slot is already reserved).
- [ ] **A2** `proto/client/v1/client.proto` — add `GetTransportSnapshot` RPC + `GetTransportSnapshotRequest` / `GetTransportSnapshotResponse` (mirror `GetACLSnapshot`; `known_version` + `up_to_date`).
- [ ] **A3** `buf generate` (repo root) — regenerate Go stubs; `cd controller && go generate ./graph/...` if resolver stubs needed.
- [ ] **A4** `controller/internal/transport/` — new package: `Store`, `SnapshotCache` (keyed by workspaceID), `CompileTransportSnapshot` (the JOIN is in ADR-017; reuse the `connector_relay_placement JOIN relays` shape, workspace-scoped).
- [ ] **A5** `GetTransportSnapshot` handler/resolver — same version-check pattern as `GetACLSnapshot`.
- [ ] **A6** connector control-stream: push current `TransportSnapshot` on stream open, alongside the ACL snapshot.
- [ ] **Build gate:** `cd controller && go build ./...`
- [ ] **Tests:** `SnapshotCache`, `CompileTransportSnapshot`.

### Phase B — Transport propagation *(non-breaking; ADR-017)*
> See [[Sprint13/Member3-Go-Rust/Phase2-Transport-Propagation]]. Depends on Phase A.

- [ ] **B1** `controller/internal/transport/notifier.go` — `TransportNotifier` parallel to `policy.Notifier`; connector-scoped versions, workspace-scoped cache invalidation, `NotifyTopologyChange(workspaceID, affectedConnectorIDs)`.
- [ ] **B2** `controller/internal/connector/transport_push.go` — topology-scoped proactive push (mirror `acl_push.go`, but push only to affected connector streams).
- [ ] **B3** Rewire triggers to `NotifyTopologyChange` (per ADR-017 trigger table): `relay/heartbeat.go` (relay online / metadata change), `relay/expiry.go` (eviction), connector registration / self-select-new-relay. **Leave `NotifyPolicyChange` for genuine policy events only.**
- [ ] **B4** (observable) include `transport_version` handling so convergence can be measured (heartbeat piggyback if cheap; else defer to metrics-only).
- [ ] **Build gate:** `cd controller && go build ./...`
- [ ] **Tests:** `TransportNotifier` (correct connectors notified, version bump semantics); topology-change → correct snapshot version.

### Phase C — Client Transport Cache *(non-breaking; ADR-018 Phase 3)*
> See [[Sprint13/Member3-Go-Rust/Phase3-Client-Transport-Cache]]. Depends on Phase A (RPC exists).

- [ ] **C1** `client/src/daemon.rs` — add `transport_snapshot` to `SharedState`; add `fetch_and_store_transport` mirroring `fetch_and_store_acl` (same 60s TTL, `known_version` → `up_to_date`).
- [ ] **C2** `build_transports_by_resource` — resolve the relay from the Transport Cache keyed by `remote_network_id`; **fall back to `ACLConnector` fields 4+5 if the cache is empty** (old controller / convergence window).
- [ ] **C3** Wire the existing PENDING-03 `relay_resync` signal to also wake `fetch_and_store_transport` — so a data-plane relay failure now triggers an early *transport* resync, not an ACL resync. (This is where Option-A supersedes M3's own Option-B mitigation — keep the signal, retarget it.)
- [ ] **Build gate:** `cd client && cargo build`
- [ ] **Tests:** transport-cache hit routes via cache; empty cache falls back to ACL fields; version-change triggers rebuild. (Rust tests in a separate `_tests.rs` file per repo convention.)

---

## 6. Final build gates

```bash
buf generate                              # from repo root, after proto edits
cd controller && go build ./...
cd controller && go test ./internal/transport/... ./internal/policy/... ./internal/connector/...
cd client && cargo build && cargo test
```

## 7. Acceptance criteria

> Full checkable test matrix in [[Sprint13/Acceptance-Test-Plan]]. The gate below is the headline.

- [ ] **AT-CORE (the invariant that proves the sprint):** a relay address/placement change pushes a new `TransportSnapshot` to affected connectors **without** recompiling or bumping the ACL snapshot version — and, in reverse, a policy change never bumps `transport_version`.
- [ ] `TransportSnapshot` is compiled per workspace and served both proactively (connector stream, field 16) and by poll (`GetTransportSnapshot`).
- [ ] A relay metadata/placement change pushes a new `TransportSnapshot` to **only the affected connectors** — and does **not** recompile or re-push the ACL snapshot.
- [ ] The client routes a connector's relay from the Transport Cache; with an empty cache it falls back to `ACLConnector` 4+5 with no regression.
- [ ] Relay failover propagates to the client via the transport plane without an ACL version bump.
- [ ] All four components build; existing tests still pass (ACL relay fields remain populated).

## 8. Deferred (explicitly NOT this sprint)

- **ADR-018 Phase 4** — reserve `ACLConnector` 4+5 / `ACLSnapshot` 6+9, delete `GetActiveRelay()` from the compiler, drop the client fallback. Breaking; needs the 4-week fleet compat window after Phase C ships. Schedule as a later coordinated deploy.
- Convergence metrics dashboard (feeds PENDING-10).

## 9. On completion (the decision-record step)

1. **Reconcile, don't re-mint.** The design already lives in ADR-015/017/018 (`status: Proposed`).
   On verified completion, flip those three to `Accepted` (or `Implemented`) with an "Implemented in
   Sprint 13" note — do **not** create a brand-new ADR number for PENDING-03.
2. **Resolve PENDING-03** — mark it accepted (Option A) and point it at ADR-015/017/018, mirroring how
   PENDING-01/07a were promoted to ADR-020/021. Note that Phase 4 removal remains outstanding.
3. Record any fixes in each phase file's **Post-Phase Fixes** section and append a **Session Log** entry.

## Notes for AI agents working on this sprint

1. Read this `path.md` fully, then ADR-017's implementation checklist, then your phase file.
2. **Never renumber proto fields.** Add `transport_snapshot = 16`; it is already reserved.
3. Everything this sprint is **additive** — keep the ACL relay fields populated and the client
   fallback intact. Removal is the deferred Phase 4.
4. Run `buf generate` after every proto edit before building Go.
