---
type: fix
sprint: 20
fix: connector-session-tuple-revocation
title: "F-3: connector sessions survive resource address/port/protocol changes"
status: implemented-live-accepted
component: connector (policy diff only)
source_finding: "F-3 in [[Phase5C-Live-Acceptance-2026-10-08]] §5"
related:
  - Fix05-Resource-Hot-Apply-Investigation (§5.3, §11 — port change "connector does not cancel")
  - Phase5C-Resource-Hot-Apply (frozen, untouched)
  - F-1 (client log wording) — separate, NOT in this fix
  - F-2 (shield learns port edit ~5 min late) — separate, NOT in this fix
tags:
  - connector
  - acl-diff
  - session-revocation
---

# F-3: Connector sessions survive resource address/port/protocol changes

Path (unchanged by this fix): **Client → Zecurity tunnel → Connector → Shield → Internal application**.

Labels: **[src]** proven from source, **[live]** proven by recorded live evidence, **[test]** proven by a
unit/integration test in this fix, **[open]** not proven / decision pending.

## 1. Problem and impact

The connector is the authorisation point for tunnelled traffic. On every ACL push it tears down live
sessions whose authorisation was removed. It does this by diffing `(spiffe_id, resource_id)` pairs only.
When an admin edits a resource's **port** or **protocol** (or the address of an entry whose
`resource_id` stays the same), the pair is still allowed, so the connector cancels nothing. An already
established session keeps carrying traffic to a `(address, port, protocol)` tuple that no ACL entry
authorises any more.

- New connections to the old tuple **are** denied promptly. Admission re-reads the live cache
  (`resolve_resource` + `is_allowed` recheck). Only *existing* sessions are affected. [src][live]
- Today the only thing that ends such a session is the client's per-key reset (Phase 5-C), at the
  client's next 60 s poll. The client is not an enforcement point:
  - a modified client never resets;
  - a client that cannot reach the controller keeps its cached snapshot and never applies
    (`background ACL sync failed — keeping cached snapshot`; ENV-1 in the 5-C run), so the session
    lives as long as the TCP connection does. [src] (indefinite survival not live-tested [open]).
- No privilege escalation: the user was authorised for exactly that tuple when the session opened.
  Classification: **connector session-invalidation defect → bounded access-control enforcement gap**.

## 2. Root cause [src]

| Item | Location (verified 2026-10-09 at `eac891a`) |
|---|---|
| Session key `(spiffe_id, resource_id)` | `connector/src/session_registry.rs` `SessionKey` |
| Admission + registration | `connector/src/device_tunnel.rs` `handle_stream`: `resolve_resource(dest, port, proto)` → spiffe check → `registry.register((spiffe, resource_id))` → `is_allowed` recheck |
| Session = one tunnel stream = one upstream connection, tuple fixed at open, never re-resolved, no pooling, no lifetime cap | `device_tunnel.rs` connector route `TcpStream::connect(target)` + `copy_bidirectional`; shield route `open_relay_session(shield_id, dest, port, proto)` |
| Only invalidation path | `control_stream.rs` `handle_controller_msg` → `CBody::AclSnapshot` arm → `policy_cache.update_and_revoked(snap)` → `registry.cancel_all(key)` per returned key |
| The diff | `policy/mod.rs` `update_and_revoked` = `allow_set(old) − allow_set(new)`, where `allow_set` yields `(spiffe, resource_id)` only. **Address, port and protocol are never compared.** |
| The shield does not compensate | `shield/src/control_stream.rs` `TunnelOpen` → `tunnel::handle_tunnel_open` connects to any `dest:port` with no resource check. The shield nft `resource_protect` chain is input-hook, stateless, exempts `lo`/127.0.0.0/8, so it never acts on tunnelled sessions. |
| Address edits | `UpdateResourceInput` has no `host` field (`controller/graph/resource.graphqls`), so an address change normally arrives as delete + create with a new `resource_id`, which the existing diff already cancels. The tuple diff still covers a same-id address change defensively. |

## 3. Evidence

- **[live] Phase 5-C row 2** (`Phase5C-Live-Acceptance-2026-10-08.md`; raw logs `~/s20-run/logs/`):
  resource A2 (shield-routed via connector `manoj`) port 51722 → 51723, saved 15:26:28.6.
  - `f5c5-newconn.log`: new :51722 connections denied from **15:26:30.134** (`000 rc=56`, client
    `tunnel denied: access denied`), so the connector already held the new ACL.
  - `dead/f5c5-hold-A2.log`: the existing hold (one socket, local port 50000) logged req=60 200 at
    15:26:29.114, then requests 61–76 (all 200; the script exits on any failure, non-200 or socket
    change) until **req=77 FAIL ConnectionResetError at 15:27:03.319**, the client's per-key RST
    after its 09:57:02Z tick. So the old session carried ~31 s of traffic after the connector had the
    new policy, and no connector cancel line appeared.
- **[live] row 3** (access removal): connector `ACL diff: session cancelled — authorization revoked
  mid-session`, so the existing pair diff works for removals.
- **[src] test gap:** before this fix, `update_and_revoked`/`allow_set` had **no** unit tests, and no
  test drove the ACL arm into a live session cancel.

## 4. Implementation (implemented 2026-10-09; record in §10)

Only `connector/src/policy/mod.rs` `update_and_revoked` changes:

1. Keep the existing pair diff `allow_set(old) − allow_set(new)` unchanged.
2. Add a tuple diff. For every `resource_id` present in **both** snapshots whose set of
   `(address, port, protocol-lowercased)` tuples differs, add every `(spiffe, resource_id)` pair the
   **old** snapshot allowed for that id. Those are exactly the sessions that may be bound to the old
   tuple. Protocol is compared case-insensitively, matching `resolve_resource`
   (`eq_ignore_ascii_case`), so a `TCP`→`tcp` edit does not revoke. Address is compared as an exact
   string, also matching `resolve_resource`.
3. The first snapshot (no previous) still returns an empty set.

No change to `control_stream.rs` (it already cancels every returned key), `session_registry.rs`,
`device_tunnel.rs`, or the data path. The resource identity model is unchanged.

**Granularity note:** cancellation is per `(spiffe, resource_id)` key. A device with sessions on the
changed resource loses all of them, which is the intended result for a tuple change because all of
them were admitted on the old tuple. Sessions on other resources (other keys) are untouched.

## 5. Scope

In scope: the connector policy diff, its unit tests, one in-crate connector integration test, and this
doc.

Out of scope (explicit):

- **Name-only changes**: must not revoke (tested).
- **`route_type` / `shield_id` (protect/unprotect)**: excluded per decision. Excluding them does not
  make the tuple fix incorrect: a protect toggle doesn't change the tuple the session reaches, and
  `resolve_resource` still admits it. Recorded as follow-up FU-1 below.
- F-1 (client log wording), F-2 (shield snapshot delivery on `UpdateResource`), the controller double
  notify (Fix02), client hot-apply (Phase 5-C), Phase 2-A, Fix 05-B, and shield enforcement.

## 6. Test plan

### 6.1 Unit tests: `policy/mod.rs` (`tests` module)

| # | Test | Expectation |
|---|---|---|
| U1 | `revokes_on_port_change_same_resource_id` | `{(spiffe, rid)}` revoked |
| U2 | `revokes_on_protocol_change_same_resource_id` | revoked |
| U3 | `revokes_on_address_change_same_resource_id` | revoked |
| U4 | `name_only_change_revokes_nothing` | empty |
| U5 | `tuple_change_does_not_touch_unrelated_resource` | only the changed rid's pairs |
| U6 | `access_removal_still_revokes_pair` | removed spiffe's pair only |
| U7 | `first_snapshot_revokes_nothing` | empty |
| U8 | `protocol_case_change_revokes_nothing` | empty (case-insensitive decision) |
| U9 | `tuple_change_revokes_all_previously_allowed_spiffes` | every old spiffe for the rid, none for other rids |

U1–U3 and U9 must fail against the pre-fix code; U4–U8 must pass both before and after.

### 6.2 Integration test: `control_stream.rs` `acl_diff_session_tests` (in-crate, `#[cfg(test)]`)

Drives the real `handle_stream` over `tokio::io::duplex` (connector route, real `TcpStream::connect`
to local `TcpListener`s) and pushes the ACL through the real `control_stream::handle_controller_msg`
ACL arm.

1. A client flow opens a session to `127.0.0.1:P_old` and echoes bytes.
2. A session to an unrelated resource is opened and echoes.
3. An ACL with the same `resource_id` and port `P_new` is pushed through `handle_controller_msg`.
4. The old session's `handle_stream` returns (cancelled) and its client side sees EOF.
5. A new request to `P_old` is answered `access denied`.
6. A new request to `P_new` gets `ok:true` and echoes.
7. The unrelated session still echoes.

Must fail with the fix disabled (step 4 times out).

## 7. Live acceptance criteria

> **Run 2026-10-09: L1–L8 PASS.** Record: [[F3-Live-Acceptance-2026-10-09]].

Lab: resource on the shield host, shield-routed through its connector; one unrelated resource with
its own hold. Instruments as in the 5-C run (`f5c5_hold.py`, newconn, connector journal, client
journal, event markers). The UI action is fired at `hh:mm:06` (just after the client tick) so the
client tick is ~56 s away and cannot be the closer.

| # | Criterion | Pass |
|---|---|---|
| L1 | Existing hold on old port, port edited | connector journal `ACL diff: session cancelled … reason=acl_diff` for the hold's `(spiffe, rid)` within **2 s** of the controller `acl push … version=N`; hold FAIL within 2 s of that, **before** the next client tick |
| L2 | Who closed it | the hold fails with a FIN-style error (`RemoteDisconnected`/EOF), not `ConnectionResetError` (client RST); the client journal has no `hot-applying` before the hold FAIL |
| L3 | Client cannot sync (D) | with the client's controller egress blocked (below), repeat L1: the connector cancel still lands; the client journal shows `background ACL sync failed` and no apply |
| L4 | New connections to the old port | connector `access denied` / client `tunnel denied` from the push onward |
| L5 | New connections to the new port | `tunnel opened … port=<new>` once the client has applied (after unblock for L3). Direct reachability before the client applies depends on F-2, so it is recorded, not graded |
| L6 | Unrelated hold | 0 FAIL across the whole run |
| L7 | Name-only edit with a hold open | no connector cancel, hold 0 FAIL |
| L8 | Regression: access removal | connector cancel, as in 5-C row 3 |

**Client-isolation procedure (L3), as run.** It needs the user's sudo on the client host. The
client runs as a non-root user, so match its systemd cgroup, not a uid. The rule drops only
client → controller traffic, leaving the client → connector QUIC data plane alone
(`~/s20-run/f3-client-block.sh <s>`, which reverts itself via `trap EXIT`):

```
table inet f3_block { chain out { type filter hook output priority 0; policy accept;
  socket cgroupv2 level 2 "system.slice/zecurity-client.service"
    ip daddr <CONTROLLER_IP> tcp dport { 8080, 9090 } counter drop } }
```

Launch with `sudo -v; sudo setsid nohup bash f3-client-block.sh 300 > /dev/null 2>&1 < /dev/null &`.
A bare `sudo … &` suspends on the password prompt. Rollback: `sudo nft delete table inet f3_block`.

## 8. Risks and rollback

- **Intended mass cancel:** every session of a resource whose tuple changes is cancelled. This is the
  desired security behaviour.
- **False revokes** would only happen if the controller re-emits the same resource with a different
  address string or port for the same id without an admin edit. Case-insensitive protocol comparison
  avoids the one known cosmetic difference. [open] Not observed.
- **Duplicate `resource_id` in one snapshot** (not produced by the controller): compared as tuple
  *sets*, so ordering doesn't matter. Any set difference revokes, which is fail-safe.
- **Rollback:** revert the single `update_and_revoked` change. No schema, protocol or state
  migration is involved.

## 9. Follow-ups (not in this fix)

- FU-1: decide whether `route_type`/`shield_id` changes should cancel sessions.
- F-1 and F-2: separate fixes.

## 10. Implementation record

Status: **implemented-live-accepted** (L1–L8 pass on 2026-10-09, see
[[F3-Live-Acceptance-2026-10-09]]). Nothing committed.

### 10.1 Files changed

| File | Change |
|---|---|
| `connector/src/policy/mod.rs` | `update_and_revoked`: tuple diff added after the unchanged pair diff; new private `tuples_by_resource`; import `BTreeSet, HashMap`; 9 unit tests (U1–U9) |
| `connector/src/control_stream.rs` | **test-only**: new `#[cfg(test)] mod acl_diff_session_tests` appended (2 tests). Production code untouched (`git diff -U0` shows 0 deleted lines). |
| this doc | new |

Not touched: `session_registry.rs`, `device_tunnel.rs`, the controller, the shield, the client.

### 10.2 Tests

Pre-fix run (production code at HEAD, tests added):

- unit: **16 passed, 5 failed**: U1 port, U2 protocol, U3 address, U5 unrelated, U9 all-spiffes
  (each returned an empty revoked set). U4 name-only, U6 access removal, U7 first snapshot and U8
  protocol case passed, as expected (regression guards).
- integration `port_edit_cancels_old_tuple_session_keeps_unrelated`: **FAILED** at step 3 with
  `old-tuple session must be cancelled by the ACL diff: Elapsed(())`. Steps 1–2 (sessions open and
  echo) passed: this reproduces F-3. `name_only_edit_keeps_session` passed.

Post-fix: `policy::tests` + `acl_diff_session_tests` **23 passed, 0 failed**.

Mutation check (each mutant applied with `sed`, run, restored, then `sha256sum -c` OK):

| Mutant | Killed by |
|---|---|
| M1 fix disabled (`if false && old != new`) | U1, U2, U3, U5, U9 + integration port-edit (6 fail) |
| M2 always revoke (`if true`) | U4, U5, U6, U8, U9 + both integration tests (7 fail) |
| M3 protocol not lowercased | U8 (1 fail) |

All 3 mutants were killed.

Integration test detail: the in-memory `tokio::io::duplex` client calls the real
`device_tunnel::handle_stream` (connector route, real `TcpStream::connect` to local echo
`TcpListener`s, valid signed empty CRL built with rcgen in the test). The ACL goes through the real
`handle_controller_msg` ACL arm. Verified:

1. the session opens and echoes;
2. the unrelated session opens and echoes;
3. port pushed with the same `resource_id`;
4. the old handler returns `Ok` and the client sees EOF;
5. a new old-port request gets `access denied`;
6. a new-port request is admitted and echoes;
7. the unrelated session still echoes.

### 10.3 Gates (deltas)

| Gate | Before (HEAD `eac891a`) | After |
|---|---|---|
| `cargo test` connector lib | 123 passed | **134 passed** (+11 = 9 unit + 2 integration), 0 failed |
| `cargo test` other targets | 0 / 4 passed / 1 ignored | unchanged |
| `cargo build --release` | — | OK |
| `cargo clippy --all-targets` warnings+errors | 20 | 20 (the `policy/mod.rs:22` `new_without_default` hit is pre-existing) |
| `cargo fmt --check` `Diff in` | 20 | 20, same files (pre-existing drift elsewhere) |
| `rustfmt --check` on the 2 touched files | clean at HEAD | clean |

Baseline clippy/fmt counts were taken from a detached `git worktree` at HEAD (since removed), so the
working tree was never stashed.

### 10.4 Deviations from the plan

- The integration test lives in `control_stream.rs`, not `device_tunnel.rs`: `handle_controller_msg`
  is private to `control_stream`, and `handle_stream` is `pub`, so this is the module that can drive
  both without a visibility change.

### 10.5 Not covered by automated tests

- **Shield route** (`route_type = "shield"`, relay through `AgentTunnelHub`): not unit-tested (the test
  exercises only the connector route). Covered live by L1 and L3.
- Real QUIC/TLS accept, the relay transport, and the client's behaviour on a connector FIN: live only.
- Indefinite survival when the client can't sync (pre-fix) was not re-run live. Post-fix connector
  enforcement in that state is proven by live L3.

### 10.6 Acceptance checklist

- [x] Failing tests first; pre-fix failures recorded
- [x] Fix in `update_and_revoked` only
- [x] Mutation check, 3/3 killed
- [x] Full connector tests, release build, clippy/fmt deltas = 0
- [x] Live L1–L8 (§7), 2026-10-09, all pass, including the shield route (L1) and client isolation (L3)
- [ ] Commit (only when told)
