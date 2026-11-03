---
type: fix-phase
sprint: 20
fix: 5
title: Empty-network transport delivery — emit remote networks with zero active connectors
status: live-accepted-with-caveat (D5 partial-loss row not run live)
date: 2026-10-06
component: controller (transport plane)
depends_on: []
related:
  - Fix05-Connector-State-Delivery-Latency-Investigation
  - Phase5A-NetStack-Socket-Lifecycle (frozen, untouched)
  - 5-B stale QUIC-pool handling (separate, not started)
---

# Fix 05 · Empty-network transport delivery

> Investigation: [[Fix05-Connector-State-Delivery-Latency-Investigation]].
> Separate from Fix 5-A (frozen) and from 5-B (stale QUIC pool). Not committed.

## 1. Problem

When the last active connector of a remote network goes away, the client keeps dialling it until its
next 60 s ACL poll (0–60 s, depending on the poll's phase; 48.9 s after the disconnect in the
2026-10-06 L3 row). It does this even though the controller noticed at once and the client fetched the
updated transport snapshot ~5 s later.

## 2. Root cause

- `controller/internal/transport/store.go` `GetWorkspaceConnectors` returns only `status='active'`
  connectors, and `compiler.go` `CompileTransportSnapshot` creates a `TransportRemoteNetwork` only from
  those rows. So a remote network with zero active connectors is **absent** from the snapshot.
- The client's `resolve_entry_coords` (`client/src/daemon.rs`) treats "RN absent from transport" as
  "transport plane does not cover it" and falls back to the cached ACL snapshot's `remote_networks`,
  which still lists the dead connector until the next ACL poll.
- The relay-failure early resync fetches transport only, computes `NoChange` from that stale fallback,
  and stops.

"No connectors" and "not covered" look the same on the wire. That is the bug.

## 3. Implementation approach (controller only)

Make "no connectors" explicit. The transport snapshot carries **every active remote network** of
the workspace. One with zero active connectors is emitted as
`TransportRemoteNetwork{remote_network_id, connectors: []}`.

- `store.go`: new `GetWorkspaceRemoteNetworkIDs(ctx, workspaceID)` →
  `SELECT id::text FROM remote_networks WHERE tenant_id = $1 AND status = 'active' ORDER BY id`.
  `GetWorkspaceConnectors` is **unchanged** (same filter, same order).
- `compiler.go`: the snapshot assembly moves into a pure helper
  `assembleTransportSnapshot(rows, activeRNIDs, version)`:
  1. Remote networks that have connector rows are built exactly as before, in the same order, with the
     same coordinates.
  2. Every active remote-network id not seen in step 1 is appended with an empty `Connectors`
     list, in id order.
  `CompileTransportSnapshot` calls both store methods, then the helper. Any DB error still fails the
  whole compile (no partial snapshot).
- **Unchanged:**
  - **Notifier / cache:** no new bump sites. Connector disconnect, activation and health already call
    `NotifyTopologyChange` (`connector/control_stream.go`), and the disconnect watcher does the same
    (`disconnect_watcher.go`).
  - **Proto:** `TransportRemoteNetwork.connectors` is `repeated`; an empty list is valid on the wire.
  - **Client:**
    - `resolve_entry_coords` already resolves a *present* RN through the transport plane and returns
      an empty list.
    - `build_transports_by_resource` already maps an empty list to a `None` slot (`connector offline —
      failing closed`).
    - `classify_applied` already sees {c1} → {} as `TransportOnly` (hot-apply), as pinned by
      `effective_config_renewal_lifecycle`.

### Design decisions

| Decision | Choice | Why |
|---|---|---|
| Which RNs are emitted empty | `remote_networks.status = 'active'` only | A deleted RN keeps today's behaviour (absent). Narrowest change. A connector that is still `active` inside a `deleted` RN is still emitted from its row, exactly as before |
| Query shape | separate RN-id query, connector query untouched | Keeps the existing connector rows and ordering byte-for-byte identical; no risk of a JOIN dropping or duplicating connector rows |
| Ordering | connector RNs in existing order, then empty RNs by id | Deterministic; the client keys RNs by id, so order is not semantic |
| Two reads, no transaction | accepted | A race only means an RN appears empty or with connectors one compile early or late; the next notify recompiles. Same consistency as the existing single read |
| Client fallback | **kept** | Still needed for pre-fix controllers and for any RN absent from transport (deleted RN). Retiring it is a separate PENDING-03 clean-up |
| Client change | **none** | Already handles a present-but-empty RN correctly (§3) |

## 4. Files

| File | Change |
|---|---|
| `controller/internal/transport/store.go` | + `GetWorkspaceRemoteNetworkIDs` |
| `controller/internal/transport/compiler.go` | `CompileTransportSnapshot` reads RN ids; + pure `assembleTransportSnapshot` |
| `controller/internal/transport/compiler_test.go` (new) | pure unit tests for `assembleTransportSnapshot` (no DB) |
| `controller/internal/transport/compiler_integration_test.go` | DB subtests: empty RN, multi-RN, last-connector-leaves, return, deleted RN |
| `client/src/daemon_tests.rs` | tests: a present-empty RN overrides the ACL fallback, maps to an unreachable slot, and is classified TransportOnly at the first resync; the old absent shape stays NoChange |

Not touched: `client/src/net_stack.rs` (Fix 5-A), Phase 2-A classifier/hot-apply code, QUIC pools (5-B),
connector code, routes/TUN/nft, notifier and cache, protobuf.

## 5. Test plan

| # | Requirement | Test |
|---|---|---|
| 1 | RN with active connectors emitted normally | pure `TestAssemble_RNWithConnectorsUnchanged`; existing DB subtests unchanged |
| 2 | RN with zero active connectors emitted with empty list | pure `TestAssemble_EmptyActiveRNEmitted`; DB subtest `remote network with no active connector is emitted empty` |
| 3 | Multiple RNs handled independently | pure `TestAssemble_MultipleRNsIndependent`; DB subtest `multiple remote networks are independent` |
| 4 | Removing the last connector of one RN keeps unrelated RNs | DB subtest `last connector leaving empties only its network` |
| 5 | Connector return repopulates | same DB subtest, continued (`active` again → 1 connector) |
| 6 | Existing client fallback tests | unchanged: `resolve_falls_back_to_acl_when_transport_lacks_rn` stays green (absent RN) |
| 7 | Client treats explicit empty as unreachable | `resolve_present_empty_transport_rn_does_not_fall_back`, `build_map_present_empty_rn_is_unreachable` |
| 8 | Topology change between syncs | `classify_last_connector_leaves_then_returns_with_stale_acl`: applied {c1}, stale ACL still {c1}; new transport present-empty → `TransportOnly`; old controller shape (absent) → `NoChange` (documents the bug); return {c1} → equal to the original |
| — | Deleted RN keeps today's shape | pure + DB: `status='deleted'` RN with no connectors is not emitted |

Gates: `go build ./...`, `go vet ./internal/transport/`, gofmt, `go test ./internal/transport/`
with `PKI_TEST_DATABASE_URL` (a skip is not a pass), the full controller `go test ./...` with the CI env
vars where practical, client `cargo test` (delta vs 146), `cargo clippy` on the touched test file,
and a mutation check (revert the compiler change → the new controller tests fail).

## 6. Live acceptance criteria (not run yet)

Instruments as in the 5-A run: `f5-fastprobe.sh` (1 s), client journal, controller log, `f5-tunsamp.sh`.
Measure **delivery lag** = client `hot-applied … reachable=0` time − controller `connector … disconnected`
time.

| Id | Criterion |
|---|---|
| **D1** last connector removal | One RN, one active connector, the shield on it, a working tunnel (`200`). Stop the connector. Controller logs `disconnected` < 1 s after the stop. The client's next early resync logs `transport snapshot stored version=N` then `transport-only change, hot-applying` → `hot-applied successfully … reachable=0`. **No `effective config unchanged` line in between.** Delivery lag ≤ 10 s (one resync), **not** ending at the 60 s tick. 0 `restarting VPN`, ifindex/PID unchanged |
| **D2** timing independence | Repeat D1 three times with the stop at about +5 s, +25 s and +45 s after the client's `hh:mm:SS` tick. Each lag ≤ 10 s and none ends at a tick boundary. (Before the fix: lag = time to the next tick) |
| **D3** connector return | Start the connector. The controller logs `connected`; the client logs `transport snapshot stored` + hot-apply `reachable=1`, then a new connection returns `200` with a matching `new TCP connection`. The trigger is the 60 s tick (no client signal exists for "connector returned"; documented, ≤ 60 s). Record the lag. If the shield has to re-attach first, record its `Control stream established` time separately |
| **D4** multiple networks | Two RNs, each with its own connector and a resource (a second test resource/RN). Stop RN-A's last connector: RN-A goes `reachable=0` within one resync, while RN-B's resource keeps answering `200` throughout (0 × `000`) and its transport slot stays populated. Restart RN-A's connector |
| **D5** no regression | L4-style partial loss (two connectors, one stopped): hot-apply, 0 restarts, new connections `200`. 10 min of `f5-newconn` at rest: 0 × `000` |

Pass for the fix = D1, D2, D4 and D5 pass, and D3 is recorded. D4 needs a second remote network
and resource in the lab (setup step, done through the admin UI).

## 7. Relationship to 5-B

- **This fix** decides *when the client's map stops listing a dead connector* after the **last**
  connector of an RN leaves: from "up to the next 60 s poll" to "the first early resync".
- **5-B** decides *what a dial to a listed-but-dead connector costs*: 5 s per new flow on a stale
  pooled QUIC connection.
- With this fix, the 5-B window after a total loss shrinks to the dials before the first resync. 5-B is
  still needed for partial loss (L4: 4 × 5 s with no delivery lag) and for the first failing dial
  that triggers the resync. D1/D2 record the slow probes before `reachable=0` as 5-B evidence; they
  don't count against this fix.

## 8. Risks and backward compatibility

- **Old client + new controller:** no change needed. Every client build since Track B resolves a
  present RN through the transport plane, and an empty list means unreachable → fail closed. That is
  the intended behaviour.
- **New controller + transient connector reconnect** (the ~1 s `disconnected` window): with a single
  connector, the client may now hot-apply `reachable=0` for that window instead of masking it with
  stale ACL coords. Existing flows survive (a hot-apply never touches live flows), and new flows fail
  closed briefly. Since the make-before-break connector fix, renewals no longer cause that
  window, so only real reconnects do. Accepted: fail-closed is the correct reading of "no active
  connector".
- **RN created/deleted without a topology notify:** a new RN shows up (empty) at the next transport
  recompile. A deleted RN can linger as an empty entry in a cached snapshot until the next recompile.
  Both are harmless: an empty RN has no connectors, which matches the truth for a new RN, and the ACL
  drops a deleted RN's entries.
- **Snapshot size:** one small entry per active RN with no connectors.
- **Multi-replica:** unchanged (same single-controller assumption as before).

## 9. Implementation (2026-10-06)

**Production (controller only):**
- `controller/internal/transport/store.go`: + `GetWorkspaceRemoteNetworkIDs` (active RNs of the
  workspace, ordered by id). `GetWorkspaceConnectors` is unchanged.
- `controller/internal/transport/compiler.go`:
  - `CompileTransportSnapshot` now does two reads (connectors, then active RN ids) and either failure
    fails the compile.
  - The assembly moved into the pure `assembleTransportSnapshot(rows, activeRNIDs, version)`. It builds
    connector RNs exactly as before, then appends each active RN without rows as
    `{remote_network_id, connectors: []}`.

**Tests:**

| File | Added | What |
|---|---|---|
| `controller/internal/transport/compiler_test.go` (new, no DB) | 5 | `TestAssemble_RNWithConnectorsUnchanged`, `_EmptyActiveRNEmitted`, `_MultipleRNsIndependent`, `_LastConnectorLeavesThenReturns`, `_InactiveRNNotAdded` |
| `controller/internal/transport/compiler_integration_test.go` | 5 subtests + `mustSetConnectorStatus` helper | empty RN (no connector / only a disconnected one), multi-RN independence, last connector leaves → only its RN empties → return repopulates, deleted RN stays absent, other workspace's RNs are not emitted |
| `client/src/daemon_tests.rs` | 3 | `resolve_present_empty_transport_rn_does_not_fall_back`, `build_map_present_empty_rn_is_unreachable_and_others_unaffected`, `classify_last_connector_leaves_then_returns_with_stale_acl` (fixed shape → `TransportOnly`; pre-fix shape → `NoChange`, pinning the bug; return → equal to the original) |

**Gates (deltas):**

| Gate | Before | After |
|---|---|---|
| `go test ./internal/transport/` (real DB via `PKI_TEST_DATABASE_URL`; harness makes its own DB) | 9 tests + 2 subtests, pass | **14 tests + 7 subtests, pass** (+5 / +5); 0 skips |
| Mutation: empty-RN loop disabled (`activeRNIDs[:0]`) | — | **7 of 10 new controller tests fail** (all 4 empty-RN DB subtests + 3 of 5 pure tests). The 3 that pass are guards that pass either way: unchanged rows, deleted RN, inactive RN. File restored and `cmp`-verified |
| Full controller `go build ./... && go vet ./... && go test ./...` with CI's env vars (Postgres shared server; `AUTH_TEST_VALKEY_URL` → disposable `valkey/valkey:7.2-alpine` on 127.0.0.1:6390, removed afterwards) | — | **all 24 packages `ok`**. 33 SKIPs, all outside `internal/transport` (env-gated fixtures in other packages; untouched by this change) |
| `gofmt -l internal/transport/` | clean | clean |
| client `cargo test` | 146 passed | **149 passed** (+3), 0 failed |
| client `cargo clippy --all-targets`, hits in the added block (`daemon_tests.rs` 349–499) | — | 0 (5 existing hits at lines 93/135/183/195/1001 predate this change) |

**Client behaviour confirmed without client changes.** The 3 new client tests pass against unmodified
`daemon.rs`:
- an explicitly empty transport RN resolves to no connector (no ACL fallback);
- it maps to a `None` slot (fail closed) while an unrelated RN keeps its transport;
- the early transport-only resync classifies {manoj} → {} as `TransportOnly`, so the first resync
  hot-applies `reachable=0` instead of waiting for the 60 s ACL poll.

**Deviations from the investigation design:** none in substance. Two details were fixed during
implementation:
- only `status='active'` remote networks are emitted empty (a deleted RN stays absent);
- the connector query was left untouched instead of being rewritten as a JOIN, so existing rows and
  their order are byte-for-byte unchanged.

**Scope confirmations:** `client/src/net_stack.rs` (Fix 5-A) last modified 2026-10-05 16:53, untouched;
`client/src/daemon.rs` / `runtime.rs` (Phase 2-A) untouched; no QUIC-pool / 5-B change; no connector,
notifier, cache, proto, route/TUN/nft change. Nothing committed.

## Acceptance

Live run: [[Fix05-Empty-Network-Live-Acceptance-2026-10-06]].

- [x] Implemented; unit + integration gates (above)
- [x] D1 last-connector removal applied within one resync: lag 5.3–6.1 s (pre-fix 48.9 s), 0 `effective config unchanged` after the resync
- [x] D2 independent of poll phase: +4.6 / +24.6 / +44.6 s → 6.1 / 5.3 / 5.4 s
- [x] D3 return recorded: poll-bound 15–55 s (expected, no return trigger); first `200` 1 s after the shield re-attached
- [x] D4 unrelated network unaffected: RN2 803 × 200, 0 failures
- [ ] D5 no regression: ✅ at rest 1150 × 200, 0 restarts; ⚠ the partial-loss (two connectors in one RN) row was **not run live**, because this topology has one connector per RN
