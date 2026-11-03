---
type: fix
sprint: 20
fix: client-restart-log-wording
title: "F-1: misleading 'restarting tunnel' log lines on the client sync callers"
status: implemented-live-accepted
component: client (log text only)
source_finding: "F-1 in [[Phase5C-Live-Acceptance-2026-10-08]] §5"
related:
  - Fix-Connector-Session-Tuple-Revocation (F-3) — separate, already merged path
  - F-2 (shield port-edit delivery) — separate, NOT in this fix
tags:
  - client
  - observability
  - logging
---

# F-1: misleading "restarting tunnel" log lines

Labels: **[src]** proven from source, **[test]** proven by a test in this fix, **[live]** proven by a
live run.

## 1. Problem

The client's sync callers log **before** the restart decision classifies the change, and the old text
claimed a restart that usually does not happen. Since Fix 01 Phase 1 / 2-A and Phase 5-C, most
changes are `NoChange`, `TransportOnly` or `ResourceDelta` and are hot-applied without a restart. The
journal still printed `background sync: version changed, restarting tunnel` before every hot-apply,
every pending retry (where no version changed), and even when the VPN was down (`perform_tunnel_restart`
returns early when `tun_slot` is None). [src] Seen in every row of the 5-C run and in the F-3 L3 recovery.
[live]

Logging and observability only; no functional defect.

## 2. Exact sites [src] (`client/src/daemon.rs`, verified at `7a37b5f`)

Call path: caller → `restart_tunnel_if_running` → coordinator → `perform_tunnel_restart` →
`run_restart_decision` → `classify_delta`. The caller lines are printed before classification.

| # | Function | Old | New |
|---|---|---|---|
| 1 | IPC `Sync` arm | `ACL changed, restarting VPN or tunnel` | `ACL changed, evaluating tunnel configuration` |
| 2 | `run_transport_recovery` | `early transport resync: version changed, restarting tunnel` | `early transport resync: transport version changed, evaluating tunnel configuration` |
| 3 | `run_transport_recovery` (error) | `early transport resync: tunnel restart failed` | `early transport resync: tunnel configuration apply failed` |
| 4 | `sync_and_restart_if_changed` | `background sync: version changed, restarting tunnel` | `background sync: change detected, evaluating tunnel configuration` |
| 5 | `sync_and_restart_if_changed` (error) | `background sync: tunnel restart failed` | `background sync: tunnel configuration apply failed` |

Structured fields are kept unchanged (`version`; `acl_changed`, `transport_changed`,
`transport_pending`, `resource_pending`; `error`). Sites 3 and 5 also fire on errors that are not
restarts (e.g. `PreMutationFailed`, transport `BuildFailed`), so "apply failed" is the accurate wording.

**Unchanged on purpose** (these are the only lines that mean a real restart, all inside
`run_restart_decision`):

- `structural configuration change, restarting VPN`
- `net_stack task is not running, restarting VPN`
- `hot-apply not possible, falling back to full restart`
- `resource apply failed after mutation, recovering with full restart`

Also unchanged: `ACL unchanged, skipping VPN or tunnel restart` and `acl unchanged after login, tunnel
restart not required` (accurate: no evaluation happens).

**Reading the journal after F-1:** the caller line is followed by exactly one decision line:
`effective config unchanged, keeping tunnel` | `transport-only change, hot-applying connector map` |
`resource change, hot-applying` | one of the restart lines above.

## 3. Scope

In scope: the 5 strings above, one regression test, the monitor scripts that grep the old text,
the runbook's expected-log list, and this doc.

Out of scope: any tunnel/restart/hot-apply/retry/recovery behaviour, F-2, F-3, Phase 2-A, Phase 5-C,
Fix 05-B.

## 4. Implementation record

### 4.1 Files changed

| File | Change |
|---|---|
| `client/src/daemon.rs` | 5 log strings (`git diff -U0` deletes exactly those 5 lines; no logic touched) |
| `client/src/daemon_tests.rs` | 3 tests appended (§4.2) |
| `docs/sprint20-live-acceptance-runbook.md` | expected-log list (§15 Client) and the Stage checkbox use the new wording; the old wording is noted as pre-F-1 |
| `.zecurity-obs/Sprint20/live-lab-scripts/p1-mon.sh` | `version changed=` counts old + new caller line; the tail grep also matches `evaluating tunnel` |
| `~/s20-run/{p1-mon.sh,mon.sh,f5c-summary.sh}` (lab, not in repo) | same matching-only edits; `f5c-summary.sh` `early transport resync: (transport )?version changed` |

### 4.2 Tests [test]

`daemon_tests.rs` (no new dependency; the crate has no log-capture crate, so the test pins the wording
against the daemon source with `include_str!("daemon.rs")`):

| Test | Asserts |
|---|---|
| `caller_log_lines_say_evaluating_not_restarting` | each of the 5 new strings appears exactly once |
| `misleading_restart_wording_is_gone` | none of the 5 old strings appears |
| `genuine_restart_log_lines_unchanged` | the 4 genuine restart lines are still present |

Against the HEAD `daemon.rs` (fix removed, restored afterwards byte-identical with `cmp`): the first two
**FAIL** and the guard passes, as expected. With the fix, all 3 pass.

### 4.3 Gates (deltas vs HEAD `7a37b5f`)

| Gate | Before | After |
|---|---|---|
| `cargo test` (client) | 196 passed | **199 passed** (+3), 0 failed |
| `cargo clippy --all-targets` warnings+errors | 23 | 23 (same files; pre-existing) |
| `cargo fmt --check` `Diff in` | 84 | 84 (pre-existing drift; `daemon.rs` was not fmt-clean at HEAD, so only my lines were formatted, and they match `rustfmt --emit stdout`) |
| `cargo build --release` | — | OK, sha `032a0cf43277cff7` |
| `strings` on the release binary | — | the 5 new strings ×1 each; `restarting VPN or tunnel`, `version changed, restarting tunnel`, `tunnel restart failed` ×0; the 4 genuine restart lines ×1 each |

## 5. Live acceptance (2026-10-09, PASS)

Client host .164: the F-1 release build (sha `032a0cf43277cff7`) was installed with `sudo install` +
`systemctl restart zecurity-client` (MainPID 1084154, 17:51:51 IST), then `zecurity-client up`. Lab as
in [[F3-Live-Acceptance-2026-10-09]] (resource `s20f3-web-renamed` on .75, shield route, F-3
connector). The client tick was `hh:mm:51.1`. A port edit 51712 → 51714 was saved at 17:53:55 (push
`version=28`).

```
12:24:51.139330Z INFO ACL snapshot synced version=28 entries=1
12:24:51.145804Z INFO background sync: change detected, evaluating tunnel configuration acl_changed=true transport_changed=false transport_pending=false resource_pending=false
12:24:51.147304Z INFO resource change, hot-applying removed=1 added=1 pending=0
12:24:51.182213Z INFO resources hot-applied resources=1 removed=1 added=1 pending=0
```

| # | Criterion | Observed | |
|---|---|---|---|
| L1 | ResourceDelta logs evaluation + hot-apply, no restart claim | the lines above; `restarting VPN` / `full restart` = 0 since install; tunsamp unchanged (ifindex 14, PID 1084154); :51714 tunnelled 18–25 ms from 17:54:53 (direct ~3 ms before) | ✅ |
| L2 | No old wording from the new build | 0 `version changed, restarting tunnel` / `tunnel restart failed` / `restarting VPN or tunnel` since 17:51:50 | ✅ |
| L3 | Monitor scripts read the new build | `p1-mon.sh` → `version changed=1`; `mon.sh` → `version changed (caller): 1` | ✅ |

Genuine restart lines: not triggered live in this check (it would need a structural change, e.g. an RN
move). They are proven present by `genuine_restart_log_lines_unchanged` and by `strings` on the
installed binary (×1 each), and the decision code is untouched (`git diff -U0`).

Rollback: `sudo install -m 0755 ~/s20-run/backup/zecurity-client.pre-f1 /usr/local/bin/zecurity-client
&& sudo systemctl restart zecurity-client && zecurity-client up` (backup sha `7d2da415`).

## 6. Risks

Tracked as open items in [[F1-Restart-Log-Wording-Remaining-Risks]] (`.zecurity-obs/Sprint20/Findings/`).

- External log consumers that grep the old text will stop matching. The in-repo and lab scripts are
  updated to count both; no other consumer is known. [open]
- The source-scan test fails if someone rewords one of these lines. That is intended: it forces the
  rewording to be deliberate and keeps the restart lines honest.
