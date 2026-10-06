---
type: fix
sprint: 20
fix: 5
title: Client data-plane reliability — smoltcp socket leak + stale connector transport
status: investigated-proposed
date: 2026-10-05
component: client
related: [fix-01-phase-2a]
tags:
  - client
  - net-stack
  - data-plane
  - fix-05
---

# Fix 05 · Client data-plane reliability (investigation + proposed design)

> Origin: the two open findings in [[fix 1/Phase2A-Live-Acceptance-2026-10-05]] (§6 / §7.2 / §9).
> **Investigation only. No production code changed. Not implemented. Phase 2-A stays frozen.**
> Separate from Fix 01 Phase 2-A. Both defects predate it; see §1.3.

Legend used below: **[LIVE]** = shown by live evidence; **[CODE]** = shown by reading the source;
**[INFERRED]** = reasoned from both, not directly observed; **[UNKNOWN]** = not established.

---

## 1. Finding 1: background `newconn=000 8.00x` timeouts with both connectors healthy

### Classification: **real product bug** (client `net_stack`). Pre-existing, not caused by Phase 2-A.

### 1.1 Live evidence

- **[LIVE]** In session 1 before any row (13:51:56–14:25:24) there were 22 timeouts in 901 samples. All 22
  are `000 8.00x`, which is curl's own `-m 8` limit.
- **[LIVE]** For **22 of 22** of those timeouts, the client journal has **no** `new TCP connection` line
  in the 8.2 s before the timeout. Across the same window, 879 × 200 match 880 accepts one-for-one. So the
  failed connections never reached the client's accept path at all. The connector, shield and QUIC were
  never involved.
- **[LIVE]** Deterministic reproduction, run twice, with no lab change:
  1. `curl --local-port 47123 …:51711` → `200`. The kernel side then sits in `FIN-WAIT-2`.
  2. Wait 70 s (`tcp_fin_timeout=60`) so the kernel frees the 4-tuple.
  3. `curl --local-port 47123 …:51711` again → **`000 8.002408`**.
  4. Control from a fresh port 47999, 8 s later → `200 0.016`.

  The second run (port 47555) gave the same result.
- **[LIVE]** The kernel holds 24–28 sockets to `192.168.1.39:51711` in `FIN-WAIT-2` at any time, which
  is the steady state for a 2 s new-connection sampler with a 60 s fin timeout.
- **[LIVE]** Client RSS was 187.8 MB after ~3 h 20 min on one net_stack instance, and grew by about
  2.7 MB across the four probe connections.
- **[LIVE]** Rate vs net_stack age:

  | Window | Approx. accepts since the stack started | Expected collisions (model) | Observed `000 8` |
  |---|---|---|---|
  | S1 13:51–14:25 (stack up since 13:09) | ~1 300 → ~2 300 | 14.3 | 22 |
  | S1 14:25–15:07 | … | 42.9 | 77* |
  | S2 baseline 15:30:49–15:36 (fresh stack, after login) | < 200 | 0.4 | 2 |
  | S2 15:38–16:10 | … | 17.0 | 23 |

  \*This window also contains the Row A/C connector-loss events.

  The model treats every connection since the stack started as leaked, and every new connection as a
  uniform pick from 28 232 ephemeral ports. Linux picks ports by hash, not uniformly, so the model is
  only an order-of-magnitude check. It does match, and the rate climbs with stack age as predicted.

### 1.2 Code evidence (`client/src/net_stack.rs` @ `b376106`)

- **[CODE]** Each accepted flow becomes an `ActiveRelay` and a relay task (`:324-393`). The task ends
  when the QUIC stream reaches EOF (`:642`), or on an error (`:626`, `:643`). It then drops
  `quic_to_tcp_tx`.
- **[CODE]** The poll loop reads `quic_to_tcp_rx.try_recv()`. On `Err(_)` it just `break`s (`:453`),
  and it treats *Empty* and *Disconnected* the same way. **Nothing ever calls `socket.close()` when
  the relay side finishes.** The only `socket.close()` calls are the three overflow paths (`:412`,
  `:428`, `:445`).
- **[CODE]** Clean-up only happens when `!socket.is_active() && !socket.is_open()` (`:459`), which
  means `Closed`/`TimeWait`. When the application closes first (curl, browsers), smoltcp goes to
  `CLOSE-WAIT` and stays there, because we never send our FIN. So the socket is never removed. Its
  64 KiB rx + 64 KiB tx buffers and its 4-tuple stay in the `SocketSet` for the life of the stack.
- **[CODE]** smoltcp 0.11 `tcp::Socket::accepts` (`socket/tcp.rs:1337-1363`): a socket with a tuple
  accepts any packet matching that exact 4-tuple. When the kernel reuses a source port, the new SYN
  matches the leaked `CLOSE-WAIT` socket, not the listener. The listener is never promoted, there is no
  `new TCP connection`, and the SYN gets a challenge ACK or nothing. The kernel retransmits the SYN until
  curl gives up at 8 s.
- **[CODE]** The same missing close also affects the fail-closed branches. `Some(None)` (connector
  offline, `:380-384`) and `None` (`:385-392`) drop the channels, and the comment says this "causes
  smoltcp to RST the connection". **It does not.** The socket stays `ESTABLISHED`, so the application
  hangs until its own timeout instead of getting a RST. It also leaks.
- **[CODE]** `git log -S` dates the clean-up condition to `95b3955` (2026-05-05, "implement
  bidirectional TCP↔QUIC relay"). It is unchanged at `5a4f940`, `6d26479` (Phase 1) and `b376106`
  (Phase 2-A).

### 1.3 Why it showed up now

**[INFERRED]** Before Fix 01, every snapshot change caused a full VPN restart (a new `net_stack` and a
new `SocketSet`), which flushed the leaked sockets as a side effect. Phase 1 and Phase 2-A
deliberately removed those restarts. So one `net_stack` now lives for hours, and the leak builds up
and starts colliding with reused ports. Fix 01 didn't introduce the defect, but it removed the thing
that hid it.

### 1.4 Unknown

- **[UNKNOWN]** The exact smoltcp reply to the colliding SYN (a challenge ACK or a silent drop). The
  outcome is the same either way: no accept, and an 8 s client timeout. A pcap on `zecurity0` would
  settle it, but isn't needed to classify the finding.
- **[UNKNOWN]** The impact on browsers. Their connection reuse lowers new-SYN volume, but their long
  sessions increase leak lifetime.

### 1.5 Correction to the acceptance record

§7.2 says the background timeouts are "unexplained". They are now explained, as above. Phase 2-A's
verdict is unaffected: every row's decision lines, ifindex/PID evidence and hold outcomes stand.

---

## 2. Finding 2: `:51711` kept serving 200 while the client held the `:51712` snapshot

### Classification: **test/instrumentation issue**. The 200s did not go through the tunnel. No product port-enforcement bug is shown. A secondary environment effect comes from an out-of-band SQL write.

### 2.1 Live evidence

- **[LIVE]** From 15:07:35 to 15:12:48 the sampler logged 155 × 200 on `:51711`. Over the same window
  the client journal has **0** `new TCP connection` lines and **0** `tunnel opened` lines. The client
  never saw those connections.
- **[LIVE]** Mean latency of those 200s was **2.8 ms**. Tunnel 200s average **17.9 ms** in both
  sessions (S1 14:00–14:20 and S2). That is a direct-LAN round trip, not a QUIC tunnel.
- **[LIVE]** Shield journal (nika):
  - 09:37:35Z (15:07:35): `firewall rules applied … port=51712`, generation 6, so only 51712 was
    protected.
  - 09:42:50Z (15:12:50): `firewall rules applied … port=51711`, generation 7.
- **[LIVE]** Controller log 15:12:49–50: connector inkyank `ReEnroll sent` → `cert renewed` → new
  stream → **`pushing pending instructions for connector d7de9603`**. That is the same second the
  shield re-applied 51711.
- **[LIVE]** From 15:12:50 every sample is `000 8.00x` (55 in a row). The client session only died
  later, at **15:15:19** (`refresh session dead`).

### 2.2 Code evidence

- **[CODE]** `client/src/tun.rs:56-141` (`configure_allowed_flows`): nft marks only `ip daddr <ip> tcp
  dport <port>` for each allowed flow, and only marked packets take table 105 → `zecurity0`. Other
  ports on the same IP "use the normal kernel route" (comment, `:60-61`). After the Row D restart the
  client's v24 flow set was `{192.168.1.39:51712}`, so traffic to `:51711` was unmarked and went over
  the LAN.
- **[CODE]** `controller/internal/policy/compiler.go:294`: the client snapshot version is
  `notifier.Version(...)`, so the SQL revert never reached the client (already Finding 7.3). The shield's
  resource snapshot travels separately, through the connector's pending instructions. It picked up the
  DB row at the next control-stream (re)connect.

### 2.3 Conclusion

1. **15:07:35–15:12:48, 200s:** client on 51712, shield protecting only 51712. So `:51711` was an
   unprotected port reached **directly over the LAN**. Both sides behaved correctly for their own
   snapshots. The sampler reports only the HTTP code, so it can't tell tunnel from direct, and it
   counted these as "success through the tunnel". → **instrumentation gap.**
2. **15:12:50 onward, timeouts:** the SQL revert reached the **shield** (pending-instruction push on the
   renewal reconnect) but not the **client**. The shield protected 51711 again, so direct access was
   blocked, while the client still routed only 51712. Result: `:51711` was unreachable both ways.
   → **caused by the test's out-of-band DB write.** The acceptance record §6 blames the logged-out
   session for this, which is **wrong** for 15:12:50–15:15:19. The session loss came 2.5 min later.
3. **[INFERRED]** A real product edit can't produce this split, because the UI path bumps the notifier
   for clients and the shield together. Only bypassing the controller does. Whether shield-only
   delivery of a DB-only change should be possible at all is a design question (the controller
   re-reads resource rows on reconnect). It isn't a data-plane bug and isn't pursued here.
4. **[UNKNOWN]** The UI showing `UNPROTECTED` / `Failed` / `port not listening` after the port edit. At
   15:07:35 nothing was listening on 51712 on nika (summa listens on 51711), so `port not listening` is
   probably correct for 51712. That has not been confirmed from the shield's status report. It needs no
   action unless it reproduces with a listening port.

---

## 3. Stale / dead-connector transport (the P1-A "5 s" behaviour)

### Status: **confirmed, still a valid separate issue, but narrower than first described.**

- **[LIVE]** Both Row C runs show exactly one `tunnel handshake timed out after 5s`, right after the
  kill (14:54:59, 15:36:25), then `shield not attached` → `early transport resync` → hot-apply.
  Across both sessions (13:00–16:10) there are only **2** such lines. So most background timeouts
  (Finding 1) are **not** this issue.
- **[CODE]** `tunnel_pool.rs:381-406`: a pooled QUIC `Connection` is reused while
  `close_reason().is_none()`. After an unclean peer death, quinn only sets that reason after the
  **idle timeout** (quinn-proto default `max_idle_timeout` 30 s; keep-alive 10 s, `:361`).
  `open_bi()` succeeds locally, so the failure appears only as the 5 s `TUNNEL_HANDSHAKE_TIMEOUT`
  (`net_stack.rs:537-565`). A handshake timeout neither evicts the pooled connection nor marks the
  `ClientTransport` unhealthy. `mark_direct_failure` is only called for open/connect errors
  (`transport.rs:112-138`).
- **[CODE]** `daemon.rs:4141`: every transport-map build creates **new** `TunnelPool`s. So a
  version-changing resync (`run_transport_recovery`, `daemon.rs:3349-3390`) or a Phase 2-A hot-apply
  replaces the dead pool. That bounds the damage.
- **[INFERRED]** Damage window = from peer death to the earlier of (a) a transport/ACL version change
  followed by a rebuild, or (b) the quinn idle timeout (~30 s). Each new flow started in that window
  pays 5 s for each dead transport listed ahead of a live one. When the controller notices quickly
  (stream close on kill: < 1 s here), the window is a few seconds. When it doesn't (host freeze or
  partition, where the controller's disconnect watcher takes ~100 s), `run_transport_recovery` keeps
  retrying with no version change, and the quinn idle timeout is the only bound. This case was
  **not observed live**.

---

## 4. Proposed fix (design only, not implemented)

### Scope (Fix 05, its own phase, after approval)

**5-A: net_stack socket lifecycle** (fixes Finding 1, plus the fail-closed RST bug). This is the
priority: it affects every long-lived client and has no workaround.

1. When the relay side ends (`quic_to_tcp_rx` reports **Disconnected**, told apart from Empty via
   `TryRecvError`) and `write_buf` is drained, call `socket.close()` (send FIN). If the relay ended
   with an error, call `socket.abort()` (RST).
2. When the application side closes (`CLOSE-WAIT`, `!socket.may_recv()`), drop `tcp_to_quic_tx` so the
   relay task sees `None`, shuts down the QUIC send half and ends.
3. Fail-closed branches (`Some(None)`, `None`, empty list): `socket.abort()` immediately, so the app
   gets a RST, which matches the existing comment's intent.
4. Back-stop: track per-socket last-activity time and abort any relay socket stuck in
   `CLOSE-WAIT`/`FIN-WAIT-*`/`LAST-ACK` past a bound (for example 60 s), so a missed path can't leak
   forever. Optionally set `socket.set_timeout(...)`.
5. Metric/log: `net_stack: active_relays=N sockets=M` at debug on change, and a warn if it exceeds a
   threshold, so this is visible next time.

**5-B: stale transport eviction** (narrows §3). Lower priority.

1. On `tunnel handshake timed out`, or on an `open_bi` failure, call
   `connection.close(0, b"handshake timeout")` and remove the pool entry. The next flow then reconnects
   (failing fast with the 2 s `DIRECT_TIMEOUT` if the peer is dead) instead of reusing a dead
   connection.
2. Treat a handshake timeout as `mark_direct_failure()` on that `ClientTransport`, so the cooldown
   (30 s → 2 h back-off) demotes it below healthy transports for the current map.
3. Optional: lower the tunnel `max_idle_timeout` (for example 15 s, with keep-alive 5 s) so dead peers
   are detected without traffic. Weigh this against relay/NAT behaviour.

**Not in scope:** Phase 2-B (resource/route/listener hot-apply), flow→connector ownership, any
controller change, and any Phase 2-A change.

### Architecture notes

- 5-A is local to `net_stack.rs` (the poll loop and the relay-task channel contract). No protocol,
  controller or daemon changes. Phase 2-A's `TransportMap` watch channel is untouched.
- 5-B touches `tunnel_pool.rs` (evict API), `transport.rs` (failure marking) and the handshake-timeout
  branch in `net_stack.rs`. The relay pool (`relay_pool.rs`) needs the same evict-on-timeout.

### Tests (unit, `cd client && cargo build && cargo test`, report delta vs 134)

- 5-A, driven by a loopback smoltcp device (the existing test harness style):
  1. App closes first → smoltcp socket reaches `Closed`/`TimeWait` and is removed from the
     `SocketSet`; `active_relays` returns to 0.
  2. Relay ends first (QUIC EOF) → FIN sent to the app; socket removed.
  3. Relay error → RST to the app; socket removed.
  4. Fail-closed (`Some(None)`) → the app receives RST within one poll; nothing leaks.
  5. **Regression for this bug:** open/close N flows from the same source port in sequence; every
     reconnect is accepted (a new `ActiveRelay`) and `sockets.len()` stays bounded.
  6. Back-stop: a socket held in `CLOSE-WAIT` past the bound is aborted.
- 5-B: a fake `DirectOpener` whose stream never answers → after one handshake timeout the pool entry
  is gone and the transport is in cooldown; the next flow tries the next transport first.

### Live acceptance criteria (proposed)

- **5-A, L1:** one `net_stack` instance kept up ≥ 60 min with the 2 s new-connection sampler. `000 8.00x`
  count ≤ the session-2 fresh-stack baseline rate (≈ 1 / 70), with **no upward trend** with stack age.
  Client RSS flat (± 5 MB) over the hour. `ss` shows no growing `FIN-WAIT-2` population beyond the
  kernel's 60 s window.
- **5-A, L2:** the deterministic repro from §1.1 (`--local-port` reuse after 70 s) returns **200**, 5/5.
- **5-A, L3:** connector-offline fail-closed: the app gets a reset (curl `000` in < 1 s, not 8 s).
- **5-B, L4** (only if 5-B is taken): freeze the shield's connector with `kill -STOP` (no stream
  close; the controller watcher needs ~100 s). New flows pay at most one 5 s timeout, then use the
  other connector, with no further 5 s timeouts before the controller notices. `kill -CONT` within
  the cert TTL.
- **Instrumentation** (applies to all future rows): the new-connection sampler must also log whether
  each 200 went through the tunnel. Either check for a matching client `new TCP connection` line, or
  record `time_total` and treat < 5 ms as direct. Never revert a resource via SQL during a row.

---

## 5. Files

Inspected (read-only): `client/src/net_stack.rs`, `client/src/tun.rs`, `client/src/transport.rs`,
`client/src/tunnel_pool.rs`, `client/src/daemon.rs` (transport build `:4062-4160`, early resync
`:3349-3390`), `controller/internal/policy/compiler.go:294`, smoltcp 0.11.0 `src/socket/tcp.rs`, and
quinn-proto 0.11.14 `src/config/transport.rs`. Also the Phase 2-A acceptance record, the phase doc,
playbook §7.4, `~/s20-run/logs/p2a-*.log` and `p2c-*.log`, `controller.log`, the client journal, and
the shield journal on nika. Derived extracts: `~/s20-run/logs/inv-client-s1.log`, `inv-accepts.log`.

Changed: **this file only** (new).
