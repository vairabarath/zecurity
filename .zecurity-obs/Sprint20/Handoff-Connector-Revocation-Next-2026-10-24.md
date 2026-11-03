---
title: Handoff — after connector revocation (start here on 2026-10-24)
date: 2026-10-23 (written end of day)
type: handoff
owner: Sathiya (M1)
---

# Handoff — where things stand and what's next

**Re-verify every lab fact before acting.** Hosts reboot and certificates expire.

## Done (2026-10-23)

- **Connector revocation:** phases B1–F plus migration 041 implemented, unit + mutation + live tested.
  - End-to-end live run PASS.
  - **Systemd / root-shield run PASS.** Revoke → long-lived flow cut 437 ms, client back 2.5–2.8 s, shield failover 2.35 s, direct LAN access blocked throughout.
  - Records: `Sprint20/Fixes/connector-revocation/` (E2E, Systemd, phase records).
- **PR #133** `feat/sprint20-m1-phase2 → fixed-pendings` is **open**, mergeable, `[schema: reset DB]`. Waiting for Barath's review.
  - After it merges, everyone recreates the local DB: `cd controller && docker compose down -v && docker compose up -d`.
- **Not pushed yet:**
  - the commits after `0d83b776`: the systemd record `3f3ceb86` and today's findings and handoff docs;
  - push only when the user asks.
  - Pushing updates PR #133.

## Findings to work on (all in `Sprint20/path.md` → Post-Sprint Fixes)

| # | What | Severity | Suggested next step |
|---|---|---|---|
| **F7** (F18/F23/F31) | Installers enable the update timer, which fires at once and **replaces local builds with GitHub releases**, ignoring `AUTO_UPDATE_ENABLED=false` (reproduced 2026-10-23) | High | Fix in installers + `--check-update` + the unit's `Environment=AUTO_UPDATE_ENABLED=true`. Needs a fix doc and acceptance list |
| **F37** | Client resets concurrent new connections to the same resource (19/24 parallel failed, 0/24 sequential) | High | Listening-socket pool per `(ip, port)` in `client/src/net_stack.rs`. Reproduce with the 3×8 curl burst first |
| F38 | A revoked connector keeps retrying relay registration | Low | Stop the relay selector on a terminal state |
| F39 | The shield's shutdown goodbye uses the startup peer list | Low | Pass the live state to `goodbye` |
| F40 | A deleted revoked connector gets `UNKNOWN`, not `revoked` | Low | Controller: tombstone → revoked |
| F41 | `ignored empty PeerConnectorList` warning noise | Low | Debug-level when the list carried revoked ids |
| F42 | One 12.9 s shield failover, not reproduced (2.35 s since) | Watch | — |
| F43 | 5-B leftovers: cooldown re-dial of dead connectors; `relay_pool` close-on-age | Low | — |
| F35 | 4 resolver ACL tests fail (R2), hidden in CI | Medium | Fix02 (Barath's area unless reassigned) |

**Open design decisions:** O10 (recovery after revocation), PENDING-18 (session migration), same-key connector renewal.

**Phase V (Sprint 20 live verification):** run on 2026-09-26/28, closed out 2026-10-17 as `failed` (40/45 PASS). Remaining:
- R2/F35 fix;
- U2 re-run without the TEMP patch (use `RELAY_DEV_ALLOW_PRIVATE_ADDR`);
- F33 live check (revoked shield);
- G4 decision (unit coverage vs live);
- tick `M1-V1`/`M1-V2` in `Sprint21/path.md`;
- clean-up (sudo).

## Lab state at end of day (re-verify)

| Where | What |
|---|---|
| this PC 192.168.1.164 | `k2-controller` user unit, binary `~/.cache/zecurity-f/controller` (`6d729a7`), env `RELAY_CERT_TTL=1h`, `RELAY_DEV_ALLOW_PRIVATE_ADDR=true`. Lab DB has **041 applied by hand** (backup `~/.cache/zecurity-f/lab-db-before-041-*.sql`). Installed client = Phase C build; **tunnel was brought up** with `zecurity-client up` (it had been down) |
| inkyank .75 | `zecurity-connector` = **sd-a, revoked + deleted**, still running (terminal, retries relay — F38). Old crash-looping connector removed. **To do:** uninstall (`sudo bash ~/sd-bundle/scripts/connector-uninstall.sh -y`) |
| manojinkyank .49 | relay (Phase D build, backup `~/zecurity-relay.pre-phased`); `zecurity-connector` = **sd-b** active (our build, timer disabled) |
| udaya .41 | `zecurity-shield` = **sd-shield** (root, our build, timer disabled) attached to sd-b, protecting `sd-web` (:18091). Web server `~/sd-web` running (`~/sd-web.pid`) |
| DB | test rows: `sd-*`, `e2e-*`, `pf-*`, `pe-*`, `rt-*`, `pd-hostile`, `pc-hostile` |
| files | `~/.cache/zecurity-{c,d,e,f,sd}`; on hosts `~/sd-bundle`, `~/e2e-*`, `~/pf-*`, `~/pe-*`, `~/pd-*`, `~/rt-*`, `~/connector-pf`, `~/connector-hostile` |

**Orca terminals** (re-derive with `orca terminal list`):
- inkyank `term_46267a54-…`
- udaya `term_23b2142b-…`
- manojinkyank `term_19fd59f3-…`

Safe send: `~/.cache/zecurity-b2/tools/rsend.py`. The user types every sudo password; check the tab's host before sending.

## Suggested order for tomorrow

1. Ask whether PR #133 is reviewed/merged; push today's local doc commits if the user wants.
2. **F7** (install-time auto-update): fix doc → acceptance list → fix → test → live (systemd). It affects every install.
3. **F37** (client parallel connections): reproduce → fix → tests → live burst test.
4. Low items F38–F41 as a small batch.
5. Phase V remainder and lab clean-up (needs sudo on the hosts).
