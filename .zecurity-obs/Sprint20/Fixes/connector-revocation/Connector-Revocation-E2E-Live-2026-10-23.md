---
title: Connector Revocation — end-to-end live run (all layers, one revocation)
date: 2026-10-23
type: live test record
design: Connector-Revocation-Implementation-Design-2026-10-14.md
branch: feat/sprint20-m1-phase2 (code at 4994638; schema 041 applied to the lab DB)
status: PASS for revocation enforcement on every layer; the open observations are listed below
---

# End-to-end live run

**Goal:** one revocation of a connector that carries real client traffic and holds the shield, with every layer observed at once:
- the connector itself (B2);
- the client (C);
- the relay (D);
- the shield (E + Phase F fix B);
- the delete guard (Phase F fix A);
- the tombstone and no reuse (041).

## Setup (final, valid run)

| Component | Where | Build |
|---|---|---|
| controller | this PC (`k2-controller`) | `4994638` |
| relay | manojinkyank (installed, systemd) | Phase D `51d35a2a` |
| client | this PC (installed daemon, logged in) | Phase C build; the **tunnel was down at the start** (no `zecurity0`), so it was brought up with `zecurity-client up` |
| `e2e-d` (the connector revoked) | manojinkyank .49, user-level | HEAD `e174ee78` |
| `e2e-e` (healthy peer) | udaya .41, user-level | HEAD `e174ee78` |
| `e2e-shield` | this PC, user-level | HEAD `82442a7e` |
| resource `e2e-web` | udaya 192.168.1.41:18090 (python http.server), group `g9-users` (the client user) | — |

The source address in the web server log shows which connector carried each request (.49 = `e2e-d`, .41 = `e2e-e`). The probes ran on this PC through the client: a fresh connection every 0.5 s, and a keep-alive one.

## Result (revoke `e2e-d` at 10:15:01.853 UTC; it carried 100 % of the traffic and held the shield)

| Layer | Observed | Time after revoke |
|---|---|---|
| connector `e2e-d` | `connector identity REVOKED — data plane closed (terminal)`, `fail-closed: every session cancelled` (10:15:01.860) | ~7 ms |
| traffic | last request via `.49` in second 15:45:01 local; **every request from 15:45:02 via `.41` (`e2e-e`)**; no request through the revoked connector afterwards | ≤ 1.2 s |
| client | one probe failed once (10:15:02.6, a relay attempt to `e2e-d` refused by `e2e-d` itself), then OK at 10:15:03.1 | 1.3 s |
| delete guard | `deleteConnector(e2e-d)` at 10:15:01.860 → refused, `1 shield(s) still attached` | 7 ms |
| shield | failed over to `e2e-e` (row re-pointed by 10:15:18) | < 17 s |
| relay | `revoked connector: Relay connection closed`, `Connector removed from Relay registry`, registered 2 → 1 (10:15:27.89) | 26 s (one 30 s heartbeat) |
| delete | after the shield moved, `deleteConnector(e2e-d)` → true; row gone, **tombstone kept** | — |
| reuse | re-inserting `e2e-d`'s id → `connector identity … was revoked and cannot be reused` | — |

**Verdict: PASS.**
- The revoked connector carried no traffic after the revocation second.
- Clients and the shield moved to the healthy peer.
- The relay dropped it.
- The delete was guarded until the shield moved.
- The identity is tombstoned and can't be reused.

## Earlier runs in the same session (kept for honesty)

1. **Run 1, invalid:** the client tunnel was down (no `zecurity0`, the route to .41 went over the LAN), so the probes bypassed ZTNA. Only the delete guard and the shield failover (2.4 s in the Phase F record) were meaningful.
2. **Run 2, topology artifact:**
   - The healthy peer `e2e-c` ran on the **same host as the client**. The client switched to it 57 ms after the revoke (`tunnel_opened ok` on `e2e-c`), but `e2e-c`'s own connection to the resource was captured by the client's resource routing (fwmark 0x5a → `zecurity0`) and looped, so no request reached the server.
   - Lesson: a connector must not run on a host whose client captures that connector's resources.
   - The revoked `e2e-b` failed closed at 10:09:53.096. The delete was refused while the shield was attached, then succeeded after it moved; the tombstone was kept.
3. **Shield failover sample (revoking `e2e-c`):** the terminal refusal came 5 ms after the revoke, but re-attaching to `e2e-d` on manojinkyank took **12.9 s**. The shield retried after 2 s and the connect to .49:9091 took ~11 s, with nothing logged on either side. **Not explained** (the earlier sample to udaya took 2.4 s); recorded as an open observation.

## Observations / not covered

> Filed in `Sprint20/path.md` → Post-Sprint Fixes (2026-10-23): isolated refusals → **F37** (explained by the systemd run); 12.9 s shield failover → **F42**.

- **Probe limitation:** the test web server speaks HTTP/1.0 and closes each connection, so the "held" probe reconnected on every request. Cutting a truly long-lived flow was not re-shown here; Phase C's live run (AC-L) showed it.
- **Isolated single connection refusals** appear around client ACL hot-applies, also when nothing is revoked (10:09:12, 10:09:30, 10:14:40). They are not caused by revocation; not investigated.
- **The client tunnel was found down** although `status` said "Running". It was brought up for this test and **left up**: `zecurity-client down` restores the previous state.
- An honest revoked connector keeps its relay registration until the relay's next heartbeat sweep (≤ 30 s), but refuses every bridge itself (`Relay Lookup failed`) in the meantime.

Nothing is pushed (user rule).
