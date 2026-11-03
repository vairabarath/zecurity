---
type: findings
sprint: 20
source: "[[Fix-Client-Restart-Log-Wording]] (F-1)"
date: 2026-10-09
status: open
---

# F-1 (client restart-log wording): remaining risks

F-1 is implemented and live-accepted (2026-10-09; [[Fix-Client-Restart-Log-Wording]] §5). These
risks stay open after acceptance. Nothing here blocks F-1. Each item has an owner action and a way to
close it.

Labels: **[src]** proven from source, **[test]** proven by a test, **[live]** proven live,
**[open]** not proven.

| # | Risk | Severity | Status |
|---|---|---|---|
| R-1 | Genuine restart log lines were not triggered in the F-1 live check | low | open, closable with one live row |
| R-2 | Log consumers outside the repo and lab scripts may still grep the old wording | low | open, unknown consumers |

## R-1: genuine restart lines not exercised live

**What.** The F-1 live check (L1–L3) drove only a `ResourceDelta` hot-apply. None of the four lines
that mean a real restart fired on the F-1 build:

- `structural configuration change, restarting VPN`
- `net_stack task is not running, restarting VPN`
- `hot-apply not possible, falling back to full restart`
- `resource apply failed after mutation, recovering with full restart`

**Why the risk is low.**

- The decision code in `run_restart_decision` is untouched: `git diff -U0` on `client/src/daemon.rs`
  deletes only the 5 caller strings. [src]
- `genuine_restart_log_lines_unchanged` (`client/src/daemon_tests.rs`) pins all four strings in the
  source. [test]
- `strings` on the installed release binary (sha `032a0cf43277cff7`) finds each of the four once. [src]
- The same lines fired live on earlier builds, with identical decision code: two `structural
  configuration change, restarting VPN` in the 5-C run (rows 9a/9b), and
  `resource apply failed after mutation, recovering with full restart` in the zero-resource run (Z3).
  [live, earlier build]

**Residual.** "The F-1 build prints the right line on a real restart" has no live proof. [open]

**How to close it.** One structural row on the F-1 build, e.g. move an applied resource to another
remote network (D3), or a device re-login (D4). Pass: the journal shows
`background sync: change detected, evaluating tunnel configuration` followed by
`structural configuration change, restarting VPN`, and the tunsamp ifindex changes.

## R-2: external consumers of the old log text

**What.** Anything outside this repo that greps the old wording stops matching on F-1 builds and
silently reads 0:

- `background sync: version changed, restarting tunnel`
- `background sync: tunnel restart failed`
- `early transport resync: version changed, restarting tunnel`
- `early transport resync: tunnel restart failed`
- `ACL changed, restarting VPN or tunnel`

**What is already covered.** Every in-repo and lab script that matched the old text now counts both
the old and new wording:

- `.zecurity-obs/Sprint20/live-lab-scripts/p1-mon.sh`
- `~/s20-run/p1-mon.sh`, `~/s20-run/mon.sh` and `~/s20-run/f5c-summary.sh` (lab, not in the repo)

`docs/sprint20-live-acceptance-runbook.md` lists the new wording and notes the old one as pre-F-1.
Live L3 confirmed both monitors count the new line. [live]

**Residual.** No other consumer is known, but none was searched for outside this machine:
dashboards, log-based alerts, other members' lab scripts, CI greps in other repos. [open]

**Specific trap.** Before F-1, `grep -c 'restarting VPN'` also matched the IPC line
`ACL changed, restarting VPN or tunnel` and over-counted restarts. On F-1 builds the same grep counts
only real restarts. Counts from mixed pre- and post-F-1 windows are therefore not comparable.

**How to close it.**

1. Ask the team whether anything alerts or graphs on these strings.
2. Search other repos and CI for the old phrases (`version changed, restarting tunnel`,
   `tunnel restart failed`, `restarting VPN or tunnel`).
3. Update any hit to match both strings (as `p1-mon.sh` does), then mark R-2 closed here.

## Related, not F-1 risks

- F-2 (shield learns resource port edits late), separate fix: [[Phase5C-Live-Acceptance-2026-10-08]] §5.
- F-3 FU-1 (`route_type`/`shield_id` changes don't revoke sessions):
  [[Fix-Connector-Session-Tuple-Revocation]] §9.
- O-1 (admin `EditResourceModal` resets its form on refetch): [[F3-Live-Acceptance-2026-10-09]] §5.
