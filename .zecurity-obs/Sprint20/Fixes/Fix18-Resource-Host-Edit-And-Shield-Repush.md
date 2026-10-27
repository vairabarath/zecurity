---
title: Sprint 20 Fix 18 — a resource's host can be edited; a protected resource's edit reaches its shield (F24 + a port-edit gap)
date: 2026-10-27
type: fix
status: FIXED and verified live (2026-10-27) — commits 29b00684, 85a482cc, 8f3ec1a5, 3e6e5b4a, 599a7804 (local). 22 mutants killed.
owner: Sathiya (M1)
findings: F24 (Sprint20/path.md → Post-Sprint Fixes); new finding F52 (port/protocol edit of a protected resource never reached its shield); new finding F53 (Edit Resource dialog lost typed values on each poll)
---

# Fix 18 — resource host edit, and shield re-push on protected edits

## F24 — a resource's host can't be edited

**Issue:** `UpdateResourceInput` had no `host`, so the only way to change a host was to delete and re-create the resource. That also drops its group assignments (F25), which is how F25 was found.

**Decision (user, 2026-10-27):**
- the host is editable on protected resources too;
- port edits must reach the shield;
- the admin UI gets a Host field, as an approved exception to the "don't modify `admin/`" rule.

## F52 (new, found while checking F24) — editing a protected resource's port left the new port open

**Issue:**
- `updateResource` on a protected resource never re-pushed its shield. The comment on `PushSnapshotForShield` says every mutation does, but `UpdateResource` didn't.
- The reconciler compares only resource IDs, so it never noticed either.
- The shield kept firewalling the **old** port, and the **new** port stayed open on the LAN until something else rebuilt that shield's snapshot (a controller or connector reconnect, or another mutation on the same shield).

**Reproduced live** (controller `28371b2a`, before the fix):
1. 06:20:50: `f24-res` (manoj .53) was protected on :8000.
2. 06:21:00: its port was edited to 8003.
3. 30 s later, from the LAN: :8003 → **200**, :8000 → 000. The shield logged no new rule.

## Fix

`internal/resource/store.go` `Update` now runs in one transaction with the row locked, and returns an `UpdateResult`:

- **Value comparison, not presence.**
  - "Shield-relevant" means host, protocol, `port_from`, `port_to` or remote network actually differ from the stored values.
  - This matters because the admin UI sends every field on each save.
- **Shield-enforced resource** (`protected` or `failed`) with a shield-relevant change:
  - its shield is re-pushed (`connector.PushSnapshotForShield`), so the shield rebuilds the chain for the new port immediately.
- **Host or network change on a shield-enforced resource:**
  - It re-matches the active shield with `lan_ip = new host` in the (new) remote network, as `protectResource` does.
  - On a match it binds the resource to that shield as `protecting`/`apply` in the same transaction. The resolver then delivers the apply instruction and pushes **the new shield first, then the old one**.
  - The old shield gets only a snapshot without the resource, never a `remove` instruction, so it sends no `unprotected` ack.
  - With no active shield on the new host (missing, or revoked), the update is refused with `ErrNoMatchingShield` and nothing changes. The error says: *"no active shield on <host> in the resource's remote network; a protected resource's host must be a shield's LAN IP (unprotect it first, or install a shield there)"*.
- **`protecting` / `deleting`:** a shield-relevant change is refused (`ErrResourceBusy`). A name or description edit is still allowed.
- **Empty host:** refused.
- **`ACLRelevantUpdate`** now includes host, so a host edit gives one notify (the Fix 02 rule).
- **`RecordAck`:** `unprotected` is accepted only while the resource is `protecting`/`remove`. A stray `unprotected` ack (for example from the shield a resource just moved away from) can no longer flip a protected resource to connector routing.
  - A late `protected` or `failed` ack from the old shield in that window only shows the wrong status until the new shield's own ack corrects it. Both statuses are shield-routed and enforced.
  - Making acks shield-scoped would need an additive `shield_id` on `ResourceAck`, stamped by the connector. That's a possible follow-up, but it's a proto change, so it isn't done here.
- **GraphQL:** `UpdateResourceInput.host`; the resolver applies the shield pushes.
- **Admin UI** (`admin/src/components/EditResourceModal.tsx`):
  - a Host field, with a note for protected resources that the new host must be a shield's LAN IP and the resource moves to that shield;
  - the mutation now returns `host`.

## F53 (new, found in the live UI test) — the Edit Resource dialog lost what was typed

**Issue:**
- The dialog reloaded every field in a `useEffect([resource])`.
- `ResourceDetail` polls every 3–10 s and hands it a new `resource` object each time.
- So typed values were overwritten before Save. Live, the first UI save sent the old host `192.168.1.44`.

**Fix:** load the form only when the dialog opens or shows another resource. The state is adjusted during render, keyed by resource id. This also clears the existing `react-hooks/set-state-in-effect` lint error.

## Tests

- **`internal/resource/update_shield_test.go`** (real DB). Cases:
  - unprotected host edit;
  - protected port edit re-pushes with the new port in the shield's desired set;
  - `failed` counts as enforced;
  - name or description edit needs no push;
  - a UI save with unchanged values does no shield work;
  - a host edit moves the resource (`protecting`/`apply`, pushes `[new, old]`, removed from the old desired set, a stray `unprotected` ack ignored);
  - no shield, or only a revoked one, is refused with nothing changed;
  - a network change without a shield in that network is refused;
  - mid-operation edits are refused while a name edit passes;
  - an empty host is refused.
- **`recordack_route_test.go`:** stray `unprotected` on `protected` and on `failed` is guarded out.
- **`acl_relevance_test.go`:** host is ACL-relevant.
- **Resolver** (`resource_acl_coherence_test.go`): a host edit notifies once; a protected host edit moves the resource to shield-2 with one notify; a host with no shield is refused without a notify.
- **Mutants, all killed (17):**
  1. no re-push;
  2. no move;
  3. no `RecordAck` guard;
  4. no busy check;
  5. ACL check ignores host;
  6. old shield pushed first;
  7. `failed` not treated as enforced;
  8. no-shield host accepted;
  9. string presence counts as a change;
  10. int presence counts as a change;
  11. port changes ignored;
  12. plus the first-pass equivalents of mutants 1 and 4 before the switch to value comparison.
- **Suites:** controller 1426 pass, 0 fail (DB-backed tests running); admin 114 pass, build OK, ESLint clean for the dialog.

## Live check — PASS (2026-10-27)

**Setup:**
- controller `763cf49c` (Fix 18) on .164;
- connector `f20-conn` on inkyank .75;
- shield `f24-s1` on manoj .53 and `f24-s2` on nika .44;
- web servers on manoj :8000/:8003 and nika :8000;
- `f24-res` in no group, so the client doesn't capture it and LAN `curl` tests the shield firewall directly.

| Time (UTC) | Action | Result |
|---|---|---|
| 06:32:22.934 | port 8003 → 8000 (API) | s1 `firewall rules applied … port=8000` at .954 (**20 ms**), generation 3; LAN .53:8000 000, :8003 200 |
| 06:32:44.294 | host .53 → .44 (API) | returns `protecting`; s1 rebuilt with 0 resources (.395), s2 applied port 8000 (.432); then `protected` on `f24-s2`; LAN .44:8000 000, .53:8000 200 |
| 06:33 | host → .164 (no shield) | refused with the error above; resource unchanged |
| 06:38:00.988 | host .44 → .53 (**admin UI** Edit dialog, after a poll) | request carries `host: 192.168.1.53`; s1 applied (06:38:01.101), s2 dropped it (.126); `protected` on `f24-s1`; LAN .53:8000 000, .44:8000 200 |

There were 0 `compile acl` errors in the controller log.

**Observation:** a shield process stopped with Ctrl-C keeps its nftables rules (the revoked f20 shield's :8000 drop was still active). That fails closed, as intended.

## Revision — edit flow for a protected resource (user design, 2026-10-27)

**The user's spec:**
- With no shield involved, add, edit and delete are free.
- When a shield protects the resource, an edit is allowed, but the admin is first told it will be unprotected and protected again with the changes.
- On confirm, the shield removes the old rule and protects the changed resource.
- If the shield is offline, the dialog says so, and the edit is applied when the shield comes back.

**What was built** (`3e6e5b4a`, `599a7804`):
- **Status on a rule-relevant edit:**
  - Applies to a `protected`, `failed` or in-flight (`protecting`/`apply`) resource when host, protocol, ports or network change.
  - The resource goes back to `protecting`/`apply` and its shield is re-pushed.
  - The shield swaps the old and new rule **in one nftables transaction**. This is deliberately not a literal unprotect-then-protect, which would leave the resource open between the two steps.
  - The shield's ack turns the resource `protected`, or `failed` if the port isn't listening.
- **Edits while waiting:** allowed while a protect is in flight (for example the shield is offline). Only an unprotect or delete in flight refuses them.
- **Offline shield:** the connector caches each shield's latest snapshot and replays it on reconnect. Until then the resource shows `Protecting`, the old rule stays, and the **new** settings are not firewalled. The dialog says exactly that.
- **Host or network move:** needs an **active** target shield.
  - A first version also allowed a `disconnected` target. The automated security review of `3e6e5b4a` flagged that the old shield drops its rule at once while the new host stays unfirewalled until the target reconnects. Reverted in `599a7804`.
- **Admin dialog:** Save on a protected resource with a rule change first shows a confirmation, with **Back** and **Unprotect & re-protect** buttons. It names the shield, says which shield takes over on a host move, and adds the offline notice when the shield isn't `ACTIVE`.

**Security review of `3e6e5b4a`, finding 1 — FIXED by the follow-up below (`9a535189`):**
- Acks carry only a resource id and status. For a moment after an edit, a periodic `protected` ack the shield sent for the **old** rule can turn `protecting` into `protected` before the new rule is applied.
- Routing and enforcement are the same either way, and the next ack corrects the status.
- The fix is a rule generation and the sending shield's id on `ResourceAck`, stamped by the connector and checked in `RecordAck`. That's a proto change, so it waits for the user's decision.

**Extra tests:**
- port edit → `protecting`/`apply` → the shield's `protected` ack → `protected`;
- `failed` → `protecting`;
- an edit while `protecting`/`apply` is allowed and pushed;
- `protecting`/`remove` and `deleting` are refused;
- a move to an offline, pending or revoked shield, or to a host with no shield, is refused;
- 5 more mutants (re-protect status dropped, in-flight not enforced, busy on `apply`, offline target accepted, pending target accepted), all killed.

**Live (controller `45951e66`, then `23397601`):**

| Time (UTC) | Action | Result |
|---|---|---|
| 07:19:02.246 | UI: port 8000 → 8003, Save → confirmation shown (nothing saved yet) → **Unprotect & re-protect** | s1 applied port 8003 at .312 (66 ms); `protected` within 0.6 s; LAN :8003 000, :8000 200 |
| 07:19:18 | shield s1 stopped (Ctrl-C) | marked `disconnected` at 07:21:45 (120 s threshold) |
| 07:22:05.717 | UI: port 8003 → 8000, Save → confirmation with the offline notice → confirm | `protecting`/`apply` port 8000; LAN :8003 000 (old rule kept), :8000 200 (as warned) |
| 07:26:03.146 | s1 restarted | connector replayed the snapshot: port 8000 rule at .150, generation 7; resource `protected`; LAN :8000 000, :8003 200 |

## Follow-up — acks name their shield and rule (user-approved, `9a535189`)

**Proto** (additive): `shield.v1.ResourceAck` gains `shield_id` (6), `host` (7), `protocol` (8), `port_from` (9) and `port_to` (10).

- **Shield:** every ack is built by one constructor (`make_ack` / `health_ack` in `resources.rs`) that echoes the rule it is about.
- **Connector** (`agent_server.rs`): `stamp_shield_id` overwrites `shield_id` with the stream's verified mTLS identity, so a shield can't ack for another.
- **Controller** (`RecordAck(ctx, db, tenant, Ack)`) is strict:
  - only the resource's current shield may ack it, including the tombstone reap;
  - a `protected`/`failed` verdict counts only if host, protocol, `port_from` and `port_to` equal the current rule;
  - an ignored ack from the wrong shield, or for a stale rule, is logged (`resource ack ignored: …`).
- **Controller, `handleShieldStatus`:** a shield LAN-IP change re-synced `resources.host` but never pushed that shield its rules. The comment claimed the snapshot fingerprint would pick it up, but nothing triggered a push. It now pushes the snapshot. This is required now, because acks for the old host are ignored.
- **Known limit (ABA):** after 8000 → 8003 → 8000 in quick succession, an in-flight ack for the first 8000 matches the current rule again. Only a per-edit generation would tell them apart. The window is momentary and the next health ack is correct.

**Tests and mutants:**
- **Shield:** `acks_echo_their_rule`. **Connector:** `ack_is_stamped_with_the_verified_shield_id`.
- **Controller:** a wrong-shield ack is ignored for every status and for the reap; a stale rule is ignored (port, host, protocol, `port_to`); the current rule is accepted.
- **Mutants:** shield check, whole rule check, host, protocol, `port_to` and the reap shield check were all killed by the logic, with no SQL errors. Applying the rule check to `unprotected` acks too is an equivalent mutant: a remove instruction always carries the current rule.
- **Suites:** controller 1435 pass, connector 269, shield 59.

**Live (2026-10-27, all components new):**
- Controller `1b71e604`, connector `6d135575`, shields `85745516` on manoj (s1) and nika (s2).
- **Upgrade window:** an ack from the old shield binary, which echoes no rule, was ignored as stale (logged). This is the intended strict behaviour.

| Time (UTC) | Action | Result |
|---|---|---|
| 09:01:34.051 | port 8000 → 8003 | `protecting` → `protected` (verified 09:01:34) on s1 |
| 09:01:44.619 | host .53 → .44, port 8000 | `protected` on s2 (09:01:44); then at 09:01:48 s1's next periodic `protected` ack was **ignored: wrong shield** |
| 09:01:52.297 | host .44 → .53 | `protected` on s1 (09:01:52); at 09:01:59 s2's late ack was **ignored: wrong shield** |
| 09:02:53 | final state, resource in no group | LAN: .53:8000 000 (protected), .53:8003 200, .44:8000 200 |
