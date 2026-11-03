---
title: Sprint 20 Fix 13 — connector-revocation low findings (F38, F39, F40, F41)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: FIXED and verified live (2026-10-24) — 4 commits, 19 mutants (17 killed, 2 documented survivors), live F38/F39/F40 PASS; F41 not reproducible in the lab (unit only). Commits 14acdf1, 0289eba, f1412b8, 95af83e (local).
owner: Sathiya (M1)
findings: F38, F39, F40, F41 (Sprint20/path.md → Post-Sprint Fixes), found in Connector-Revocation-Systemd-Live-2026-10-23.md and Connector-Revocation-E2E-Live-2026-10-23.md
---

# Fix 13 — four low findings left by connector revocation

None of the four weakens enforcement: a revoked connector is refused everywhere today. They are wasted work, a stale shutdown path, a less precise state, and log noise. One commit per finding, so each can be reverted alone. No proto change, no schema change (041 already exists).

## F38 — a terminal connector keeps dialling relays (connector)

**Problem.** After revocation the connector is terminal (`Revoked` / `UnknownIdentity`), its control loop stops, and fail-closed ends sessions and shield streams. The relay selector doesn't know: it keeps registering with relays on its backoff (10 s, 20 s, …), and the relay refuses every attempt (Phase D sticky set).

**Root cause.** `relay_selector::run` (`connector/src/relay_selector.rs:127`) takes no serving state. Nothing tells it to stop.

**Fix.**
1. `run` also takes the `AuthzGate` (already built in `main.rs` and cloned for the relay handler).
2. At the top of each selector iteration: if the state is terminal →
   - abort the active relay session, if any (`JoinHandle::abort`, as `migrate` does);
   - clear the attachment slot, so the heartbeat stops reporting a relay;
   - log once `relay selector stopped: connector is revoked or unknown to the controller`;
   - park forever. Terminal is never left within a process, so there is nothing to resume.
3. The three places where the selector idles also wake on a gate change, so it stops at once rather than after the current backoff or the 10-minute re-probe timer:
   - the wait for the first relay list;
   - `wait_backoff` (new wake reason `Terminal`);
   - the `connected_step` select.
4. Not terminal (`Disabled`, `Recovering`, `Starting`, `ServingDisconnected`) → no change. The selector keeps its relay so serving resumes quickly.

It doesn't drop the selector future from outside: dropping a tokio `JoinHandle` detaches the session task rather than stopping it.

## F39 — the shield's goodbye uses the startup peer list (shield)

**Problem.** On SIGTERM `main.rs:211` calls `control_stream::goodbye(&state, …)` with the `ShieldState` loaded at startup. After a revocation or a failover, goodbye dials the old head first: a revoked connector, or one that is gone. Non-fatal, but slow and wrong.

**Root cause.** The control stream task owns its own copy of the state. Changes to the peer list never reach `main`.

**Fix.**
1. A `tokio::sync::watch` channel holds the **live peer list** (`Vec<ConnectorRef>`). `main` keeps the receiver; the control stream publishes to it.
2. It publishes at every place the list changes:
   - after the failover rotation in `run_once`;
   - after each `PeerConnectorList` (refresh and revoked prune);
   - after the terminal head rotation in `reconnect_loop`. This goes through a new `ControlSession` hook with a default no-op, so the test fakes don't change.
3. At SIGTERM, `goodbye` runs with the live list.
   - If every peer is revoked (empty list), goodbye logs that there is no connector to tell and dials nothing. It must **not** fall back to the startup list.
4. Not hooked on `save()`: the terminal rotation and the all-revoked prune deliberately don't save.

## F40 — a deleted revoked connector is told `unknown`, not `revoked` (controller)

**Problem.** When a revoked connector's row is deleted, its next Control stream is refused with `NotFound` + marker `unknown`. The connector logs "unknown to the controller" rather than "revoked". Both are terminal, and relay, client and CRL still refuse it.

**Root cause.** `lookupConnector` (`controller/internal/connector/authorization.go:67`) reads only `connectors`. With no row it returns `absent`. It doesn't consult the 041 tombstone (`connector_revocations`).

**Fix.**
1. In `lookupConnector`, when there's no row, check the tombstone **in the same trust domain**:
   ```sql
   SELECT 1 FROM connector_revocations cr
     JOIN workspaces w ON w.id = cr.tenant_id
    WHERE cr.connector_id = $1 AND w.trust_domain = $2
   ```
   - Found → `connectorStateRevoked` → `PermissionDenied` + marker `revoked`.
   - Not found → `absent` / `unknown`, as today.
   - Query error → `Unavailable` with no marker, as today.
2. ~~A non-UUID id → `absent`, with no query.~~ **Dropped in implementation:** the tombstone query only runs after the `connectors` query returned no row, which already needs a valid UUID (a non-UUID id makes that first query fail → `Unavailable`, as before Fix 13). Non-UUID behaviour is unchanged; no guard needed.
3. `classifyRefusal` uses the same lookup, so refusals after a guarded UPDATE also get it.
4. **Not changed:** `LoadConnectorStanding` (certificate-audit path). Its callers already refuse when there is no row, the same as when revoked.
5. **Compatibility:** the connector classifies by the trailer **marker**, not the gRPC code (`connector/src/authorization.rs` `classify_status`). The code change NotFound → PermissionDenied has no other effect.

### D-1 (your call): should a tombstoned identity be reported as `revoked`?

| Option | Effect |
|---|---|
| **A — yes (recommended)** | A revoked connector is always told `revoked`, deleted or not. This matches the tombstone's purpose ("its identity stays revoked", 041). It refines the D10 marker meaning: `unknown` = never existed here, or deleted without having been revoked. |
| B — no, close F40 as "won't fix" | Keep `unknown` for every deleted row. No code change. |

**Decision D-1 (user, 2026-10-24): A.** Design approved the same day.

## F41 — `ignored empty PeerConnectorList` warning every 15 s (shield)

**Problem.** If a shield's network has no resources, connectors now send lists holding only revoked ids (by design, Phase E). The shield warns `ignored empty PeerConnectorList` on every heartbeat for as long as any revoked connector's certificate is valid.

**Fix.**
1. In `apply_peer_connector_list`, note whether the list carried revoked ids **before** they are consumed by the prune.
2. Empty peers **with** revoked ids → `debug`: the list did its job (the prune).
3. Empty peers **without** revoked ids → `warn`, as today: that one is unexpected.
4. The choice is a small pure function, so it can be tested without capturing logs.

## Acceptance

| # | Check | Level |
|---|---|---|
| AF13-1 | F38: selector connected, gate set to `Revoked` → active session aborted, slot cleared, no further connect attempts (connect counter / events stay still) | connector unit |
| AF13-2 | F38: in backoff, gate set to `UnknownIdentity` → `wait_backoff` returns `Terminal` at once (paused clock), selector parks | connector unit |
| AF13-3 | F38: gate `Disabled` / `Recovering` → selector keeps its relay and keeps running | connector unit |
| AF13-4 | F39: the published list follows the failover rotation, a refresh, a revoked prune and the terminal rotation | shield unit |
| AF13-5 | F39: goodbye with an empty live list dials nothing and doesn't use the startup list | shield unit |
| AF13-6 | F40: no row + tombstone in the same trust domain → `revoked` marker; tombstone in **another** trust domain → `unknown`; no row, no tombstone → `unknown`; tombstone query error → `Unavailable`, no marker (DB-backed test; xlangharness case added). Non-UUID: unchanged (see Fix item 2) | controller unit + DB |
| AF13-7 | F41: empty peers + revoked ids → debug; empty peers, no revoked ids → warn; non-empty → applied | shield unit |
| AF13-8 | Existing suites green: connector, shield, controller (DB tests run, not skipped), client, relay | suites |
| AF13-9 | Mutation checks: no terminal check at loop top; no gate wake in each of the 3 waits; terminal treated as `Disabled`; one missed publish site; goodbye falling back to startup state; tombstone not scoped by trust domain; no UUID guard; F41 condition flipped | mutants |
| AF13-L | **Live, one run:** revoke f11-a → f11-a's journal: `relay selector stopped`, then **no** relay registration attempts for ≥ 2 min (F38); f11-shield fails over to sd-b, no repeating `ignored empty PeerConnectorList` warning (F41). Stop f11-shield → goodbye goes to sd-b (F39). Delete f11-a (shield has failed over, so the Phase F delete guard allows it), restart its service → refused with marker `revoked` (F40) | live |

**Live notes:**
- F40 needs the new controller binary in `k2-controller`.
- The run uses up f11-a. A replacement connector is needed afterwards for further tests.
- sudo is typed by you.

## Implementation (2026-10-24)

| Finding | Commit | Change |
|---|---|---|
| F38 | `14acdf1` | `connector/src/relay_selector.rs`: `run(…, gate)` → `run_until_terminal`, then parks. Loop top: terminal → `stop_on_terminal` (abort the session `JoinHandle`, `set_active(None)`, log `relay selector stopped: connector is revoked or unknown to the controller`, event `StoppedTerminal`). `wait_terminal` (cancel-safe, ignores recoverable states) added to the first-list wait, `wait_backoff` (new `BackoffWake::Terminal`) and `connected_step`. `connector/src/main.rs` passes the gate |
| F39 | `0289eba` | `shield/src/control_stream.rs`: `PeerPublisher` (watch of `Vec<ConnectorRef>`, `send_if_modified`), published after a terminal head rotation (`reconnect_loop`), a connect-time failover (`adopt_failover_head`, extracted from `run_once`) and each `PeerConnectorList` (`handle_connector_msg`). `with_live_peers` builds the goodbye state; `goodbye` sends nothing for an empty list. `shield/src/main.rs` keeps the receiver and uses it at SIGTERM |
| F41 | `f1412b8` | `apply_peer_connector_list` notes `carried_revoked` before the prune; empty peers → `debug` if it carried revoked ids, else `warn` (`empty_list_is_expected`) |
| F40 | `95af83e` | `controller/internal/connector/authorization.go` `lookupConnector`: on no row, `EXISTS` over `connector_revocations` joined to `workspaces` on the same trust domain → `connectorStateRevoked`; query error → error (Unavailable). xlangharness seeds `revoked_del` (revoked, then row deleted); `connector/tests/xlang_authz_markers.rs` case 2b expects PermissionDenied + revoked |

## Tests

- Connector **244 → 250** (`relay_selector::terminal_tests`):
  - `terminal_ends_backoff_at_once`, `a_recoverable_state_does_not_end_backoff`;
  - `stopping_aborts_the_session_and_clears_the_attachment`;
  - `terminal_wakes_a_connected_selector`;
  - `the_selector_stops_on_terminal_only` (Disabled for 60 s keeps it running; Revoked stops it at once);
  - `terminal_stops_a_selector_waiting_for_its_first_list`.
- Shield **44 → 49**: `terminal_failover_is_published`, `a_failover_at_connect_is_published`, `a_received_peer_list_is_published`, `goodbye_uses_the_live_list_even_when_empty`, `an_empty_list_warns_only_when_it_carried_nothing`.
- Controller (DB-backed, run, not skipped): `TestAuthz_DeletedRevokedConnectorIsStillRevoked` (open, mid-stream, activation), `TestAuthz_TombstoneLookup`. Existing "deleted, never revoked → unknown" tests unchanged and passing. Full `go test ./...` with the test DB: all packages ok.
- Go↔Rust marker harness (`scripts/xlang-authz-markers.sh`): 1/1 pass, with the new case.
- Client and relay: **not re-run**; this diff touches neither crate nor the proto. CI covers them on push (AF13-8 partly by argument).
- rustfmt: no new differences (shield `control_stream.rs` keeps its 4 pre-existing ones). gofmt clean on the changed files.

### Mutation checks

| # | Mutant | Result |
|---|---|---|
| F38-M1 | no terminal check at the loop top | killed (test hangs → timeout) |
| F38-M2/M3/M4 | no terminal wake in backoff / connected step / first-list wait | killed |
| F38-M5 | any closed state treated as terminal | killed (timeout) |
| F38-M6 | session handle dropped instead of aborted | killed |
| F38-M7 | attachment slot not cleared | killed |
| F39-M1/M2 | no publish after the terminal rotation / after a peer list | killed |
| F39-M3 | no publish after a connect-time failover | killed (after extracting `adopt_failover_head`) |
| F39-M3b | `run_once` doesn't call `adopt_failover_head` | **survives**: the call needs a live TLS connector; this wiring had no unit test before Fix 13 either |
| F39-M4 | goodbye falls back to the startup list | killed |
| F39-M5 | publish never updates | killed |
| F39-M6 | no early return on an empty list in `goodbye` | **survives, equivalent**: without it `build_client` fails and logs a warning; nothing is sent either way |
| F41-M1/M2 | condition flipped / always debug | killed |
| F40-M1 | tombstone ignored | killed |
| F40-M2 | tombstone not scoped by trust domain | killed |
| F40-M3 | tombstone query error read as `absent` | killed |

## Live check AF13-L (2026-10-24)

**Setup:**
- Controller `1bfb3ede` (Fix 13) in `k2-controller`.
- f11-a (inkyank .75) connector → `3a6150ea`; f11-shield (udaya .59) → `62b96523`. Swapped by `f13-swap.sh`, backups `*.pre-f13`, sudo typed by the user.
- sd-b on manojinkyank (now **.53**), unchanged.
- **Relay re-provisioned first:** the lab relay on manojinkyank had been crash-looping since boot (`/etc/zecurity/relay.conf` missing, 5,514 restarts). New registration `f13-relay-manoj` (`f3319105…`, allowlist .53) via the provider API (password typed by the user); installed with `relay-local-install.sh`, the existing relay binary, and the token into a hidden prompt (removed from `relay.conf` after provisioning). sd-b and f11-a both registered with it.

| Step | Result |
|---|---|
| **F38** revoke f11-a at 09:24:06 UTC (gate flipped while the relay session was up) | Same millisecond: `relay session ended: connector is terminal`, `relay selector stopped …`. Over the next 150 s: **0** relay lines in f11-a's journal; **0** accepts from .75 on the relay. The relay's second `Connector removed from Relay registry` at +31 s is the idle-timeout cleanup of the aborted QUIC connection, not a new attempt. (2026-10-23, before the fix: re-registration attempts every 10 s, 20 s, … all refused.) **PASS** |
| Shield failover | f11-shield: terminal refusal → next peer → stream to sd-b in **2.3 s**; f11-a pruned (`pruned=1 remaining=1`) |
| **F39** stop f11-shield at 09:27:02 | `goodbye sent to connector` 7 ms after SIGTERM; **sd-b** logged `shield goodbye received` for 551396ef; f11-a logged nothing. (Before the fix the goodbye used the startup list, headed by the revoked f11-a: from the code, not re-run today.) **PASS** |
| **F40** delete f11-a (delete guard allowed it: the shield's row was on sd-b), restart its service at 09:34:56 | Row gone, tombstone kept. Controller: `refused at open: code=PermissionDenied marker="revoked"`. Connector: `controller: connector is REVOKED (authoritative marker; terminal)`; the selector stopped from its first-list wait. (Before: NotFound + `unknown`.) **PASS** |
| **F41** | **Not reproducible here:** sd-net has resources, so the lists always carry peers. 0 `ignored empty PeerConnectorList` warnings since the swap, as before. Unit + mutants only |

**Known bound (F38):** if the relay drops the session *before* the connector sees its terminal state, the biased `connected_step` takes the session-ended arm into `failover`, which can dial the ranked relays once before the loop top stops the selector. That is one pass at most, refused by the relay's sticky revoked set. The live run covered the other order (terminal first).

**Lab after the run:**
- f11-a: revoked + deleted, process up in its terminal state. To uninstall.
- f11-shield: **stopped** (F39 step), row `disconnected` on sd-b.
- Relay `f13-relay-manoj` active on .53. Stale relay rows `s3-relay-manoj` / `pm-relay-manoj` (.49, inactive) remain for clean-up.
- Controller runs the Fix 13 build.
- Bundle server on this PC `:18093` (stopped after the run).
- `target/` dirs cleaned.

## Records to update after the fix

- `Sprint20/path.md` → F38–F41 marked FIXED (Fix 13).
- Post-Phase Fixes in the phase records:
  - F38: Phase B2 implementation record (fail-closed);
  - F39 and F41: Phase E implementation record;
  - F40: Phase F schema 041 record.
- Session log entry.

Nothing is pushed until you ask.
