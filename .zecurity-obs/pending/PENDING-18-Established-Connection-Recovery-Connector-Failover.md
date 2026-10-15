---
type: adr
status: pending
implementation: deferred-not-implemented
id: PENDING-18
domain: data-plane
priority: unassigned
created: 2026-10-10
related:
  - Fix01-Client-Tunnel-Restart-On-Snapshot-Change
  - Phase2-Full-Hot-Apply
  - Phase1-AppliedConfig-Restart-Decision
tags: [pending, adr, data-plane, client, connector, failover, deferred, future-work, known-limitation, sprint-20, q3, fix-01]
---

# Pending ADR 18 — Future Implementation: Established Connection Recovery and Connector Failover

> **Status: DEFERRED / NOT IMPLEMENTED.** Future implementation. It is excluded from the Sprint 20
> scope and was deferred out of Sprint 20 / Fix 01 **Q3** on 2026-10-10. Nothing in this file is
> implemented, accepted or approved. Proposed behaviour and acceptance criteria below are
> **proposals** that need confirmation by whoever plans the work.
> No owner, sprint or date is assigned.

**Search terms:** pending implementation · deferred implementation · future work · known limitation ·
established connection recovery · established-flow recovery · connector failover · connector
migration · same-remote-network fallback · TCP session preservation · application reconnection ·
"established connection fails when connector becomes unavailable" · Q3 · Sprint 20 · Fix 01.

## 1. Known limitation (verified current behaviour)

The client already falls back to another connector when **opening a new connection**. Once a
connection is **established**, though, its flow is pinned to the stream it opened. If the connector
carrying that flow fails:
- the client does **not** pick another connector;
- it does **not** migrate or recover the flow;
- the application's TCP connection gets a reset or times out.

The client does signal an early transport resync. That refreshes the connector map for **new**
connections only; it does not migrate the established flow. The rest of the tunnel keeps running:
an individual flow failing is **not** a VPN restart.

Evidence. Checked against the source on 2026-10-10 (`feat/sprint20-m1-phase2` @ `7a37b5f`). Line
numbers drift; the function names are the stable reference.

| Fact | Where |
|---|---|
| A flow takes the resource's transport list once, when the connection is accepted. Later map swaps don't touch it | `client/src/net_stack.rs` `transports_for`, the accept path in `FlowTable` (~:647), and its doc comment |
| After the handshake succeeds, the relay task keeps only the selected stream | `net_stack.rs` `relay_tcp_to_quic`, the loop after `selected_stream` |
| A mid-session QUIC read or write error sets `relay_failed`, fires `resync.notify_one()`, and returns `RelayEnd::Failed`. The application socket gets an **RST**. There is no re-selection | `relay_tcp_to_quic`, the bidirectional loop; tests `relay_mid_session_failure_reports_failed_and_resyncs`, `relay_failure_resets_the_application_connection` |
| No component records which connector a flow uses | `client/src/transport.rs` `ClientTransport` has no `connector_id` (only `RelayContext` does); `net_stack.rs` `ActiveRelay` has no connector field (its `close_tag` is for the per-resource-key reset) |
| An existing flow survives a map swap that removes its connector, and keeps running until its own path fails | test `established_flow_survives_map_swap_that_removes_its_connector` (`net_stack.rs`) |
| A silent connector is detected through QUIC keep-alive (10 s, `client/src/tunnel_pool.rs`) and the idle timeout. The idle timeout isn't set in client or connector code, so it is **presumably** quinn's default (inferred, not verified) | `tunnel_pool.rs` `TransportConfig` |
| Early resync re-fetches only the transport snapshot (2 s backoff, 5 attempts, 30 s cooldown) and hot-applies it for new connections | `client/src/daemon.rs` resync branch of the ACL sync scheduler, `run_transport_recovery` |

Live observations, for context only (not acceptance of this item):
- Fix 01 Phase 2-A row C on 2026-10-05: when a held flow's connector was stopped, the hold died with a timeout ([[Phase2A-Live-Acceptance-2026-10-05]]).
- Fix 05 5-A row L4 on 2026-10-06: same result ([[Phase5A-Live-Acceptance-2026-10-06]]).

## 2. Existing behaviour that must be preserved

New-connection fallback exists and works; see `relay_tcp_to_quic` and `ClientTransport::open_authenticated_stream_probed`. It was introduced in Sprint 11.2 ("Multi-Connector Failover & Framed Tunnel Handshake", `.zecurity-obs/Sprint11.2/path.md`). It is preferred-first ordered fallback. It is **not** load balancing: there is no round-robin, random or load-aware selection.

**Which connectors are candidates.**
- Only connectors from the resource's own `remote_network_id`. They come from the transport snapshot, falling back to the ACL's list for that same remote network (`daemon.rs` `resolve_entry_coords`).
- The controller lists only `status='active'` connectors, per remote network, ordered by `last_heartbeat_at DESC` (`controller/internal/policy/store.go`, `controller/internal/transport/store.go`).

**Order.**
- The preferred connector goes first when it is in the list; the rest follow in snapshot order (`ordered_connectors_for_entry`, `ordered_transport_connectors_for_entry`).
- The preferred connector is the one the resource's shield is attached to (`controller/internal/policy/compiler.go`: `ShieldConnectorID`). It is empty for unprotected resources.

**Within one connector:** the direct path is tried first (2 s), then the relay (5 s). An authentication error on the direct path never falls back to the relay (`transport.rs`).

**Moving to the next connector:**
- the client tries the next connector after:
  - transport failures and timeouts;
  - authentication failures;
  - handshake I/O errors;
  - a handshake timeout (5 s, with the Fix 05-B silent-connection eviction);
  - malformed replies;
  - `SHIELD_NOT_ATTACHED`.
- it **stops immediately** on:
  - `access denied`;
  - `INTERNAL`;
  - certificate revocation;
  - missing `shield_id` or an unknown route type.
- an empty list, or a remote network with no connectors, fails closed (immediate RST).

Full matrix: the Sprint 20 Q3 investigation (2026-10-10), summarised in
[[Phase2-Full-Hot-Apply]] § Remaining work.

## 3. Problem / decision needed

How should the client recover **established** flows when the connector carrying them becomes
unavailable? Specifically:
- which level of recovery to offer (§4);
- for which kinds of connector loss (§6).

## 4. Recovery levels (design alternatives, needing technical investigation)

These are different capabilities. Don't silently swap one for another.

| Level | Meaning | Notes |
|---|---|---|
| R0, today | Let the original connection fail; keep the rest of the VPN running | Already the behaviour (RST or timeout, no restart) |
| R1 | Re-establish connectivity for **new** application connections through an alternative connector, quickly and predictably after the failure | Partly exists through new-connection fallback and early resync. Cost: the stale map's timeouts until the resync |
| R2 | Recover the **affected flows**: detect, classify and act on all flows owned by the failed connector, so the application reconnects on its own through an eligible connector | Needs flow → connector ownership (§7) |
| R3 | **Preserve the original TCP session** across connector failure (transparent migration) | **Must not be assumed possible.** The application's TCP endpoint is the client's smoltcp socket, but the resource-side TCP connection is owned by the connector (connector route) or the shield, so a new connector would open a new upstream connection. Byte-stream continuity and resynchronisation of in-flight data are unsolved. Needs its own feasibility study |

## 5. Future implementation objectives (to investigate; not assumptions)

The implementation owner must inspect the actual transport and network-stack design before choosing
an approach. They need to determine how to:
1. identify the connector associated with an established flow;
2. detect and classify the failure of that connector;
3. determine which alternative connectors are eligible for the affected resource;
4. restrict fallback to eligible connectors in the **same remote network**;
5. decide whether the flow can be preserved (R3) or whether the application must reconnect (R1/R2);
6. recover affected flows without needlessly restarting the whole VPN;
7. coordinate recovery with transport-map hot-apply (Phase 2-A) and configuration sync;
8. avoid repeatedly selecting an unavailable connector;
9. handle the case where no eligible alternative exists;
10. preserve policy enforcement, authorization, and resource-removal semantics (Phase 5-C client reset, F-3 connector revocation).

Do not assume the new-connection loop in `relay_tcp_to_quic` can be reused unchanged for
established-flow recovery.

## 6. Security and correctness requirements (constraints)

- No fallback outside the resource's remote network.
- No bypass of access denial or policy restrictions. A connector denial must not become a reason to try elsewhere.
- No continued access after applicable revocation.
- No continued access to permanently removed resources, including through stale configuration.
- No use of an alternative connector the device is not authorized for.
- No needless restart of the entire VPN.
- No unbounded retry loop against unavailable connectors.
- No recovery based on stale or invalid configuration.

**Open: distinguish the kinds of connector loss.** Don't assume one recovery behaviour fits all:
- transient control-plane interruption, including certificate renewal (since `6362d1f`, renewal no longer bumps the snapshot version);
- a configured connector that is temporarily unreachable (it drops out of the `active` list);
- a connector intentionally removed from configuration (today this looks the same to the client as being unreachable);
- a revoked connector or identity (today this shows up as a TLS, CRL or authentication failure, or a revocation denial).

This is the still-open part of Q3. No grace period, timeout or close policy is approved.

## 7. Known dependencies / where to look

Source, as of the 2026-10-10 investigation:
- `client/src/net_stack.rs`: `relay_tcp_to_quic`, `transports_for`, `FlowTable` accept path, `ActiveRelay`, `TransportMap`.
- `client/src/transport.rs`: `ClientTransport` (no `connector_id`), `RelayContext`, `open_authenticated_stream_probed`, `report_handshake_stall`, the direct-path cooldown.
- `client/src/tunnel_pool.rs` (pooled QUIC connections, keep-alive, `evict_if_silent`) and `client/src/relay_pool.rs`.
- `client/src/daemon.rs`:
  - `ordered_connectors_for_entry`, `resolve_entry_coords`, `build_transports_by_resource_with_crl`, `build_transport_from_coords`;
  - the transport-only hot-apply path;
  - the early resync and `run_transport_recovery`.
- Controller connector and transport configuration: `controller/internal/policy/{compiler.go,store.go}`, `controller/internal/transport/store.go`.
- Connector: `connector/src/device_tunnel.rs` (`handle_stream`; denial and `SHIELD_NOT_ATTACHED` replies). Shield attachment is a single `shields.connector_id`, so for a protected resource only the shield's current connector can succeed. That is inferred from the schema and not traced in full.

Existing tests:
- connector removal and hot-apply: `connector_removed_hot_applies_and_new_flows_cannot_select_it`, `connector_returns_hot_applies_with_preferred_first`, `preferred_connector_change_hot_applies` (`daemon.rs`);
- established flows: `established_flow_survives_map_swap_that_removes_its_connector`;
- mid-session failure: `relay_mid_session_failure_reports_failed_and_resyncs`;
- fallback on timeout: `partial_loss_evicts_silent_connector_and_falls_through_to_healthy_one` (`net_stack.rs`).

**Untested today** (unit level):
- `SHIELD_NOT_ATTACHED` → next connector;
- `access denied` / `INTERNAL` → stop;
- authentication failure → next connector.

Prerequisite already identified in [[Phase2-Full-Hot-Apply]] § Remaining work (items 1–3):
- `ClientTransport` needs a `connector_id`;
- flows need to record which connector they use;
- the client needs a per-connector close and selection mechanism.

## 8. Proposed acceptance criteria (PROPOSED, not approved)

1. When an established flow's connector becomes temporarily unavailable, the client detects the failure and follows the agreed recovery policy.
2. If another eligible connector exists in the same remote network, the client can use it to recover connectivity according to the chosen recovery level (§4).
3. If the original TCP session cannot be preserved, the implementation defines clearly whether and how application connectivity is re-established.
4. The client does not restart the whole VPN needlessly.
5. When no eligible connector is available, the client fails safely and does not enter an uncontrolled retry loop.
6. Connector fallback never bypasses access denial or revocation.
7. Permanently removed resources cannot be recovered through stale configuration.
8. Connector selection stays restricted to the resource's remote network.
9. Recovery works correctly while connector topology and transport maps change.
10. Automated tests cover:
    - successful fallback;
    - failed fallback;
    - unavailable alternatives;
    - configuration changes;
    - the relevant security cases.
11. Tests keep new-connection fallback separate from recovery of an already-established flow.
12. A suitable live acceptance test passes before the item is marked complete.

## 9. Relationship to Q3, Fix 01 and other validation

- Deferring established-flow recovery out of Q3 does **not** mean the capability exists. It also doesn't invalidate the existing new-connection fallback.
- **Q3 stays open** for the connector-loss classification and lifecycle policy (§6). See [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]] → Decisions → Q3.
- **Fix 01** addresses needless tunnel restarts caused by snapshot or configuration changes. Validating Fix 01 is separate from this item, and this item is **not** part of Fix 01's scope.
- A successful connector-renewal hold does **not** prove that an established flow can recover when its connector actually fails. Example: the 2026-10-06 hold behind the G9 / AT-G.6 re-grade, in [[Phase1-AppliedConfig-Restart-Decision]] § G9 / AT-G.6 re-grade.
- Strict U5 acceptance and the Fix 01 post-slice regression test are separate validation activities. Both are still outstanding.
