---
type: phase
member: M3
sprint: 13
phase: 2
title: Transport Propagation — TransportNotifier + trigger rewiring
status: done
depends_on:
  - Sprint13/Member3-Go-Rust/Phase1-Transport-Proto-and-Compiler
tags: [go, transport, propagation, notifier, relay, acl-decoupling, pending-03]
---

# Phase 2 — Transport propagation (non-breaking)

> Executes **ADR-017** (the propagation pipeline). Depends on Phase 1 (the snapshot + cache exist).
> This is the phase that actually **breaks the coupling**: after it, topology events drive transport
> propagation instead of ACL recompiles.

## Goal

Make topology changes flow through a dedicated, **topology-scoped** transport pipeline that shares no
state with ACL propagation. A relay flap should push a fresh `TransportSnapshot` to *only the
affected connectors* and must **not** call `NotifyPolicyChange` / recompile the ACL.

## Why (context)

Today every topology event (`relay/heartbeat.go`, `relay/expiry.go`, connector registration) calls
`NotifyPolicyChange`, which recompiles the **workspace** ACL snapshot and re-pushes it to **all**
connectors — wasteful and wrong-scoped. ADR-017's insight: authorization invalidation is
workspace-scoped; transport invalidation is topology-scoped (only connectors on the changed relay).
This phase introduces the parallel `TransportNotifier` and moves the transport-only triggers onto it.

## Files

| File | Change |
|------|--------|
| `controller/internal/transport/notifier.go` | `TransportNotifier` — connector-scoped versions, workspace-scoped cache invalidation, `NotifyTopologyChange(ctx, workspaceID, affectedConnectorIDs)` + `RegisterPushHook` (mirror `policy.Notifier`) |
| `controller/internal/connector/transport_push.go` | topology-scoped proactive push — mirror `acl_push.go`, but push the compiled workspace snapshot to **only the affected connector streams** |
| `controller/internal/relay/heartbeat.go` | relay online / metadata (IP/addr) change → `NotifyTopologyChange(workspaceID, connectorsOnThisRelay)` instead of `NotifyPolicyChange` |
| `controller/internal/relay/expiry.go` | relay eviction → `NotifyTopologyChange(...)` for connectors placed on the evicted relay |
| `controller/internal/connector/control_stream.go` | connector registers / self-selects new relay (after `ConnectorRelayState` updates `connector_relay_placement`) → `NotifyTopologyChange(workspaceID, [connectorID])`. Reconnect (stream re-open) just re-pushes current snapshot — **no version bump.** |
| `controller/cmd/server/main.go` | construct `TransportNotifier`, register its push hook, inject into relay + connector handlers |

## Trigger table (from ADR-017 — implement exactly this scoping)

| Event | Source | Affected scope | Call |
|-------|--------|----------------|------|
| Relay online (first heartbeat) | `relay/heartbeat.go` | connectors placed on this relay | `NotifyTopologyChange(ws, connectorIDs)` |
| Relay metadata change (IP/addr/port) | `relay/heartbeat.go` | connectors placed on this relay | `NotifyTopologyChange(ws, connectorIDs)` |
| Relay eviction (heartbeat expiry) | `relay/expiry.go` | connectors placed on that relay | `NotifyTopologyChange(ws, connectorIDs)` |
| Connector registers with relay | `connector/control_stream.go` | that connector only | `NotifyTopologyChange(ws, [id])` |
| Connector reconnects (stream re-open) | `connector/control_stream.go` | that connector only | push current snapshot on open — **no version bump** |
| Connector self-selects new relay (ADR-016) | after `ConnectorRelayState` → `connector_relay_placement` | that connector only | `NotifyTopologyChange(ws, [id])` |

> **Key discipline:** `NotifyPolicyChange` must remain wired for *genuine* policy events (access
> rules, group membership, device revocation) only. If, after this phase, a relay heartbeat still
> triggers an ACL recompile, the decoupling is not done.

## Convergence observable (light-touch)

If cheap, have connectors report `transport_version` on heartbeat and expose
`transport_convergence{workspace=X}` = fraction of connected connectors at the current compiled
version (ADR-017 §Convergence SLA). If it balloons scope, ship metrics-only and note it — the
dashboard is deferred to PENDING-10.

## Build check

```bash
cd controller && go build ./...
cd controller && go test ./internal/transport/... ./internal/relay/... ./internal/connector/...
```

## Verification (do this before calling the phase done)

Drive it, don't just unit-test: with a client+connector attached, change a relay's advertised
address (or evict it) and confirm (a) the affected connector receives a new `TransportSnapshot`,
(b) the ACL snapshot version does **not** change, (c) unaffected connectors get nothing.

## Implementation checklist

- [x] **B1 (architecture-corrected)** `TransportNotifier` with workspace-scoped versions/cache invalidation and an affected-connector advisory parameter
- [x] **B2 (architecture-corrected)** client polling plus failure-triggered re-poll replaces connector proactive push
- [x] **B3** rewire heartbeat/expiry/placement triggers to `NotifyTopologyChange`; transport-only placement changes do not notify policy
- [ ] **B4** (optional) `transport_version` convergence observable
  <!-- NOT IMPLEMENTED: marked optional; no transport_version piggyback in heartbeat -->
- [x] **Build gate:** `cd controller && go build ./...`
- [x] **Tests:** AT-CORE/AT-CORE-R notifier isolation, monotonic versions, and exact heartbeat/expiry/placement connector scoping

> B4 remains an optional observability follow-up; it does not block the implemented propagation phase.

## Post-Phase Fixes

### Fix: Client-poll propagation replaces connector-scoped push

**Issue:** The original design proposed pushing client routing topology through connector streams
and maintaining connector-scoped transport versions. That path had no consumer at the connector.

**Fix applied:** Commit `90803e4` removed `transport_push.go` and its push hook. Topology events now
invalidate the workspace transport cache and bump an independent workspace transport version; the
client polls that snapshot and performs an early re-poll after relay failure. The
`affectedConnectorIDs` argument is retained for topology context but is not a delivery target.

**Related behavior:** Relay heartbeat/expiry and connector placement changes call
`NotifyTopologyChange`. Policy and transport versions remain separate. The version state is
process-local and therefore assumes a single controller instance, as documented by the later
single-controller clarification (`ef7f72c`).

**Verification:** Executable tests now prove AT-CORE/AT-CORE-R version and cache isolation at the
notifier boundary, exercise the real relay-heartbeat → transport-notifier path, and verify exact
workspace/connector scoping for relay heartbeat, relay expiry, and connector placement changes.
The runtime client failover/convergence E2E remains a sprint-level acceptance gate.
