---
title: Connector Revocation — Phase M results
date: 2026-10-21
type: measurement results (design §6.6)
plan: Connector-Revocation-PhaseM-Measurement-Plan-2026-10-21.md
build: measure/phase-m 08ad699 (Phase A base 4d10fb9 + timing logs) — controller fcbb86db3a67, connector aac7cbea37a5
status: COMPLETE (R1, R2, R3; R4 WAN not run — optional, needs sudo on inkyank)
---

# Phase M — results

**Lab:**
- controller on this PC (192.168.1.164), user unit `k2-controller`, with `CONNECTOR_CERT_TTL=15m` and `CONNECTOR_RENEWAL_WINDOW=10m`;
- connector `pm-conn` (7cc1af4b…) on inkyank (192.168.1.75), 6 cores;
- local Postgres and Valkey in Docker;
- no shield (nika away), no relay;
- H = 15 s, so a health request is sent every 15 s.

## Setup notes (affect how to read the data)

- `pm-conn` was enrolled while another agent's controller build was running, so it first got a 7-day cert. At 08:01 UTC one forced renewal (a controller run with a 200 h window) moved it onto 15-minute certs. R1's clock was restarted after that, at **08:01:36 UTC**.
- R1's single reconnect is that controller restart at the start of the window. It is counted as one R3 sample, not as an R1 event.

## R1 — idle baseline (08:01:36–09:01:36 UTC, 60 min)

Connector:

| Metric (ms) | n | p50 | p99 | max | mean |
|---|---|---|---|---|---|
| **B: enqueue → ack processed** | 252 | 4.5 | 66.2 | 69.4 | 8.1 |
| transport send → ack processed | 252 | 4.4 | 66.1 | 69.3 | 8.0 |
| renewal (CSR round trip) | 12 | 61.5 | 65.6 | 65.6 | 60.3 |
| renewal stream switch (open → switched) | 12 | 29.4 | 36.9 | 36.9 | 30.2 |

Controller:

| Metric (ms) | n | p50 | p99 | max | mean |
|---|---|---|---|---|---|
| handleConnectorHealth total | 252 | 3.1 | 23.6 | 41.5 | 3.9 |
| guarded UPDATE (DB) | 252 | 2.4 | 12.5 | 41.0 | 3.1 |
| relay placement upsert/delete | 252 | 0.7 | 2.2 | 2.5 | 0.7 |
| ACL compile + enqueue | 252 | 0.0 | 0.1 | 17.3 | 0.1 |
| ack mailbox: enqueue → stream.Send | 252 | 0.1 | 0.2 | 0.2 | 0.1 |

**Completeness:** 252 requests and 252 accepted acks. No rejected, unacknowledged or withheld acks; 12/12 renewals completed and switched; no unplanned reconnect.

**Runtime lag** (100 ms timer, 60 one-minute windows):
- worst-window mean 1.6 ms (the timer's 1 ms granularity);
- max 12.0 ms;
- 12 ticks over 5 ms, about one every few minutes, at no fixed point in the renewal cycle.

### Where the tail comes from

| Acks > 15 ms | Count | Cause |
|---|---|---|
| 60–69 ms | 12 | **Renewal head-of-line blocking (connector).** Each falls on the heartbeat whose handler sent `ReEnroll`. The connector awaits the renewal (CSR round trip ≈ 61 ms) inside its Control-stream loop (`run_session` → `ops.renew().await`), so the `AuthorizationAck` queued behind `ReEnroll` is read only after the renewal finishes. |
| 26–42 ms | 4 | **DB spikes (controller).** The guarded UPDATE took 25–41 ms on those heartbeats. |

**Implication for B2:** the idle B is dominated by these two effects, not by the network.
- Without renewal blocking, the p99 would be about 40 ms.
- Making renewal not block ack processing would remove the largest term, which grows with the controller round trip (WAN).
- So would counting the ack before running the renewal.

## R2 — under load (from 09:05:28 UTC, 30 min)

Back-to-back 200 MB downloads from udaya (192.168.1.37:8090, resource `pm-udaya-web`, connector route) through the tunnel, 09:05:28–09:35:28 UTC.

**Load:** 248 downloads, 52 GB, **28.9 MB/s** on average (27.8–29.3), 0 failures. That is about 230 Mbit/s through the connector for the whole window.

Connector:

| Metric (ms) | n | p50 | p99 | max | mean |
|---|---|---|---|---|---|
| **B: enqueue → ack processed** | 126 | 4.0 | 78.6 | 82.6 | 7.6 |
| transport send → ack processed | 126 | 3.9 | 78.5 | 82.5 | 7.5 |
| renewal (CSR round trip) | 6 | 71.3 | 79.7 | 79.7 | 68.6 |
| renewal stream switch (open → switched) | 6 | 45.9 | 51.8 | 51.8 | 45.6 |

Controller:

| Metric (ms) | n | p50 | p99 | max | mean |
|---|---|---|---|---|---|
| handleConnectorHealth total | 126 | 2.8 | 10.5 | 10.8 | 3.1 |
| guarded UPDATE (DB) | 126 | 2.2 | 7.0 | 8.1 | 2.3 |
| relay placement upsert/delete | 126 | 0.6 | 2.3 | 3.7 | 0.7 |
| ACL compile + enqueue | 126 | 0.0 | 0.1 | 0.1 | 0.0 |
| ack mailbox: enqueue → stream.Send | 126 | 0.1 | 0.2 | 0.2 | 0.1 |

**Completeness:** 126 requests, 126 accepted acks, 6/6 renewals, no reconnects.

**Runtime lag:** worst-window mean 1.3 ms; **max 26.5 ms**; 7 ticks over 5 ms.

**Reading:**
- Data-plane load doesn't move the typical B (p50 4.0 ms vs 4.5 ms idle).
- The tail is still renewal head-of-line blocking, now larger: the CSR round trip grows from 61 to 71 ms and the stream switch from 29 to 46 ms, so B's p99 is 79 ms.
- The controller was quieter than in R1 (no DB spikes in this window).
- The connector's runtime lag under load peaks at 26 ms. That is small against H, but it is part of B's budget.

### Lab finding (client, not connector): a second resource IP is never hot-applied

The client only reached udaya after a manual `zecurity-client down` / `up`. Before that, traffic went straight to udaya, outside the tunnel.

What the client daemon logged:
- **07:42:25** ACL v2: "resource change, hot-applying", added 1 (nika `.39`). Fix 05 Phase 5-C (`167ffa5`) worked.
- **07:48:25** ACL v4, 2 entries: `resource additions pending: address capacity pending=["192.168.1.37:8090"]`. It was retried every 60 s and never applied.

**Cause:** the planner (`client/src/daemon.rs` `plan_resource_apply`, unchanged since `167ffa5`) caps new IPs at `addr_capacity − RESERVED_IFACE_ADDRS`. `addr_capacity` is `smoltcp::config::IFACE_MAX_ADDR_COUNT`, which is **2** with the default features in `client/Cargo.toml` (`smoltcp` 0.11 `proto-ipv4, socket-tcp, socket-udp`). So only **one** resource IP can ever be hot-added; every further IP stays pending until a full restart. A full `up` then routes both IPs (table 105 showed `.37` and `.39`), so the tunnel start path and the hot-apply planner disagree about capacity.

Not fixed here; reported to the user.

**Also seen:** from 07:56 UTC the client's background ACL and transport sync failed with "session expired; re-login required". Its session had been refreshed at 07:48 while the other agent's controller build was running. The data plane (device cert) was unaffected; a `zecurity-client login` restores sync.

## R3 — reconnects (09:37:13–09:45:33 UTC)

The controller was restarted 5 times, 90 s apart (`systemctl --user restart k2-controller`; same binary and settings each time).

| Restart | Stream lost (UTC) | Backoff slept | Connect after backoff | First ack after open | **Lost → first ack** |
|---|---|---|---|---|---|
| 1 | 09:37:23.26 | 16 s | 41 ms | 26 ms | **16.07 s** |
| 2 | 09:39:03.28 | 32 s | 34 ms | 18 ms | **32.05 s** |
| 3 | 09:40:43.31 | 60 s | 42 ms | 69 ms | **60.11 s** |
| 4 | 09:42:23.34 | 60 s | 41 ms | 20 ms | **60.06 s** |
| 5 | 09:44:03.38 | 60 s | 43 ms | 47 ms | **60.09 s** |

**Reading:**
- **The reconnect work itself is about 0.1 s:** SPIFFE preflight, channel and `Control()` take 34–43 ms, and the first ack follows 18–69 ms later.
- **Everything else is backoff, and it ratchets.** It continued from R1's 8 s: 16 → 32 → 60 → 60 → 60 s. It never resets after a successful reconnect, only after a cleanly ended session. This confirms the R1 finding.
- **Controller drain:** on a restart the controller drains for up to 10 s (`gRPC graceful shutdown timed out; forcing stop`) and **keeps acking** during the drain (seq 484 was acked 1 s after "shutdown requested"). The stream ends 10 s after the restart command, and authorization stays fresh until then.

## Conclusions for B2 (for the user's decisions)

### B — response budget

| Run | p50 | p99 | max |
|---|---|---|---|
| R1 idle | 4.5 ms | 66 ms | 69 ms |
| R2 under ~230 Mbit/s | 4.0 ms | 79 ms | 83 ms |

**The tail is the renewal awaited inline in the connector's Control-stream loop** (61–80 ms), plus occasional controller DB spikes (≤ 41 ms) and runtime lag (≤ 26 ms under load). On this LAN, B is below 0.1 s; WAN wasn't measured (R4 not run). For P, the useful observation is that **B ≪ H** in every run: the worst B is about 0.6 % of H.

### P — request period (decision: the user's)

With D = sent[k] + H and H = 15 s:
- Tolerating **n consecutive lost requests** needs `(n+1)·P + B < H`.
- If B is budgeted generously at 1 s, about 12× the measured max for WAN headroom:

| P | Lost requests tolerated before D passes |
|---|---|
| 6.5 s | 1 (2·6.5 + 1 = 14 < 15) |
| 4.5 s | 2 (3·4.5 + 1 = 14.5 < 15) |
| 3.4 s | 3 (4·3.4 + 1 = 14.6 < 15) |

The controller cost per request is small: handler p50 about 3 ms, DB about 2.3 ms. At P = 5 s that is about 0.5 ms of DB time per connector per second, or about 0.5 s/s per 1000 connectors. That is an estimate from these single-connector numbers, not measured at scale.

### O6 — full report vs a lightweight authorization request

- The full report costs about 3 ms, of which the guarded UPDATE is about 2.3 ms, the relay placement upsert or delete about 0.7 ms, and the ACL compare about 0 ms (17 ms once, on a cache miss).
- A lightweight request (guarded UPDATE only) would save about 25–30 % per request.

The measured cost is low either way. O6 can be decided on design simplicity rather than load at this scale.

### O5 — reconnect backoff (decision: the user's)

- The measured reconnect work is about 0.1 s.
- Today's backoff: 2 s initial, ×2 per error, **never reset after a successful reconnect**, capped at 60 s.
- Once B2 enforces D, a ratcheted connector is closed for up to about 60 s after any controller blip, even though reconnecting would take about 0.1 s.

The measurements support:
1. resetting the backoff once a session has received an accepted ack;
2. a much lower cap while `Disabled`/`ServingDisconnected` (seconds, with jitter, not 60 s).

### Also for B2 design

- **Renewal head-of-line blocking:** run the renewal off the Control-stream read loop, or keep processing acks while it runs. This removes the largest term in B's tail.
- **Controller restarts:** the 10 s drain is harmless (acks continue).

### O8

Not measurable here: no shield or relay in this lab (nika away). The renewal stream switch takes 29–52 ms, which is the window a make-before-break switch has to fit in.

## Not run

**R4 (WAN emulation).** It needs sudo on inkyank (`tc netem`). B under +50/+150 ms RTT is therefore unmeasured; the renewal blocking term scales with RTT.
One sample from R1's start (controller restart at 08:01:36):

| Step | Time |
|---|---|
| stream lost → backoff sleep | **8.0 s** |
| SPIFFE preflight + channel + `Control()` | 42 ms |
| initial request → accepted ack | 31 ms |
| **stream lost → first accepted ack** | **8.07 s** |

**Finding for O5 — the backoff ratchets.** The sleep was 8 s, not the 2 s initial value, because two earlier forced restarts (08:00–08:01) had doubled it 2 → 4 → 8 s. `run_control_stream` resets the backoff only when a session ends **cleanly** (`Ok`), never after a successful reconnect. Over a long uptime, every transient error ratchets it up to the 60 s cap, where it stays. Every later disconnection then costs up to 60 s before the first retry. Once B2 fails closed at D (≤ 15 s), that is up to about 60 s of closed data plane per controller blip. O5 should cover both the backoff cap and resetting it after a healthy session.
