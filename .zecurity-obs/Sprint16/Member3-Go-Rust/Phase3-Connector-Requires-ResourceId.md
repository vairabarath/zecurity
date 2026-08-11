---
type: phase
sprint: 16
stage: 1
phase: 3
title: Connector Requires `resource_id`
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase2-Client-Identity-Handshake
status: done
tags: [sprint16, connector, rust, identity, handshake, security, default-deny, gate1]
---

# Sprint 16 · Phase 3 — Connector **Requires** `resource_id`

> Goal: delete the legacy "resolve by the client's destination string" path. A handshake without a
> `resource_id` can no longer be authorized, so it is **denied**. This is the Stage 1 merge point
> (**GATE 1**).
> Depends on **Phase 2** being deployed to every client — this is a breaking change for old clients.

## Why this is a separate phase

Phase 1 was deliberately *tolerant*: it accepted both the identity path and the legacy destination
path so the client could roll out independently. Tolerance is a rollout mechanism, not an end state —
while the legacy branch exists, the confused-deputy surface Stage 1 was written to close is still
reachable by any client that simply omits `resource_id`. This phase removes the escape hatch.

## Tasks

### 3.1 — Remove the legacy fallback
`connector/src/device_tunnel.rs`

- [x] Delete the `None => acl.resolve_resource(&req.destination, ...)` arm. A missing or empty
      `resource_id` is now `reason=missing_resource_id` → deny.
- [x] Treat an **empty-string** `resource_id` the same as absent — otherwise `resource_id: ""`
      becomes a trivial bypass of the requirement:
      ```rust
      let asserted_resource_id = req.resource_id.as_deref().filter(|id| !id.is_empty());
      ```
- [x] `PolicyCache::resolve_resource` itself is **kept** (still used by the network-tuple path
      elsewhere and by tests); only the *handshake's* use of it is removed.

### 3.2 — Four distinct, countable denial reasons
- [x] `missing_resource_id` — no identity asserted
- [x] `unknown_resource` — id not in the snapshot, **or** port/protocol disagree with the entry
- [x] `unauthorized_spiffe` — the client's SPIFFE ID is not in `allowed_spiffe_ids`
- [x] `destination_mismatch` — the client's `destination` disagrees with the ACL's `address`
- [x] Each is logged on the `warn` line **and** carried into `emit_access_log` as
      `AccessLogFields.error`, so denials are countable from the audit trail, not only from stderr.

> `unknown_resource` intentionally covers both "no such id" and "wrong port/protocol". They are
> distinguishable in `resolve_by_resource_id` but are deliberately reported identically to the client:
> a caller must not be able to probe which resource ids exist by comparing error strings.

## Build gate

```bash
cd connector && cargo build
```

## 🚩 GATE 1 — E2E, Stage 1 merge point

**Status: authorization PASSED (2026-08-05); byte transfer PASSED (2026-08-06).**

Verified on a live stack (controller + connector + client, one LAN, no relay):

```
DEBUG received tunnel request  resource_id="32e69282-…"  auth_path="resource_id"
                              destination=192.168.1.164 port=5173 protocol=tcp
                              spiffe_id=spiffe://ws-yoge.zecurity.in/client/5b59fa0d-…
INFO  access allowed          resource_id=32e69282-…  route="connector"
INFO  tunnel_opened ok        resource_id=32e69282-…  dest=192.168.1.164:5173
```

The client asserts `resource_id`, the connector authorizes on identity and dials **its own ACL's
address**. Phases 1–3 confirmed working end-to-end.

- [x] IP resources still work end-to-end.
- [x] Byte transfer works — `HTTP 200 in 0.067s`, exactly **one** tunnel per connection. This was
      blocked for one day by a P0 data-plane stall that was **not caused by this sprint**; see
      [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]] (resolved 2026-08-06, client/connector
      co-location routing loop).
- [x] **Negative cases — DONE (2026-08-10, written during Phase 7).** All covered by the new
      `device_tunnel.rs` test harness (`tokio::io::duplex` + assertions on the emitted `ConnectorLog`):
      - [x] `missing_resource_id` — handshake with no id is denied
      - [x] `unknown_resource` — unknown id is denied, and **no dial is attempted**
      - [x] `destination_mismatch` — valid id, wrong destination, is denied
        > This one is load-bearing for Stage 2. Phase 7.0 **scoped** it rather than deleting it, and
        > `destination_mismatch_is_denied_for_pinned_resources` is the guard. Note Phase 7 also
        > corrected an overstatement about this check — see
        > [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]] § *Scope correction*.
      - [x] `unauthorized_spiffe` — valid id, wrong principal, is denied
      - [x] shield-routed resource with every shield offline **fails closed** (invariant #3) as
            `SHIELD_NOT_ATTACHED`, and is never resolved

## Verify (manual)

- [x] An old client (no `resource_id`) is denied rather than silently served.
- [x] `auth_path="resource_id"` appears on every accepted handshake — no `legacy_destination`.

## Post-Phase Fixes

### Fix: blocking audit-log send could stall the byte pump
**Issue:** `emit_access_log` used `control_tx.send().await` immediately before
`copy_bidirectional`. A full control mailbox would block the tunnel's byte pump.

**Root cause:** the audit path was on the data plane's critical path, contradicting the fail-fast
contract already documented on `connectorStreamClient::send`.

**Fix applied:** switched to `try_send`. Note this was **not** the cause of the data-plane stall
(disproved as hypothesis #4 in the bug doc) — it is a legitimate defect fixed on its own merit.

**Related files:** `connector/src/device_tunnel.rs`.

## Notes

- Making `resource_id` mandatory is the **only** breaking change in Stage 1. Sequence matters: Phase 2
  must be everywhere before this ships.
- Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.
