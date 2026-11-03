---
title: Connector Revocation — Phase M measurement plan
date: 2026-10-21
type: measurement plan (design §6.6)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (rev 4)
status: DONE — measurements recorded in Connector-Revocation-PhaseM-Results-2026-10-21.md (R4 WAN optional, not run)
---

# Phase M — timing measurements for B2

**Goal.** Measure the response budget **B** (connector enqueues a report → connector processes its ack) and what it is made of. B2 then picks a request period **P < H − B**, with H = 15 s fixed. The same data informs O6 (full health report vs a lighter authorization request), and the reconnect and renewal numbers inform O5 and O8.

**Constraint.** The B1 connector can't run in the lab (B1-Q1 = a). Measurements therefore run on a **Phase A base** (`4d10fb9`) plus measurement-only logging, on branch `measure/phase-m`. The controller code is identical between `4d10fb9` and B1, so controller numbers carry over. Every component is built from one commit (§15).

## 1. Instrumentation (measurement build only)

| Where | What is logged | Gives |
|---|---|---|
| Controller `handleConnectorHealth` | one `authz_timing` line per report: `db_us` (guarded UPDATE), `relay_us` (relay placement upsert/delete), `acl_us` (ACL compile + enqueue), `total_us` | handler latency, DB latency, ACL cost (§6.6 rows 2–4) |
| Controller writer goroutine | `authz_timing ack_sent seq queue_us` (ack enqueued → `stream.Send` returned) | mailbox queueing delay (row 5) |
| Connector, ack accepted/rejected | `since_send_us` (transport send stamp → ack processed) | ack round trip (row 1). Combined with the existing `health tick` line, this also gives enqueue → ack processed (= B) |
| Connector, new runtime probe | every 60 s: count, mean and max lateness of a 100 ms timer | runtime scheduling lag (row 8) |
| Existing connector logs (µs timestamps) | `opening replacement Control stream` → `switched to renewed Control stream`; `control stream error` → `Control stream established` → first `ack_accepted` | renewal switch latency (row 6), reconnect latency (row 7) |

An analysis script turns the logs into p50/p99/max per metric.

## 2. Runs (lab: controller on this PC, connector `k2-conn` on inkyank .75, shield `k2-shield` on nika .39, client on this PC)

| Run | Duration | Conditions | Measures |
|---|---|---|---|
| R1 idle baseline | ≥ 1 h | no client traffic; 15 m connector certs (≈ 12 renewals) | B, handler/DB/ACL, mailbox, renewal switch, runtime lag |
| R2 under load | ≥ 30 min | client keeps a continuous transfer through `g9-nika-web` (shield route) | same, under data-plane load |
| R3 reconnects | ~10 events | restart the controller 5 times; also drop the connector's TCP connection 5 times (`ss -K`, needs sudo on inkyank) | reconnect latency up to the first accepted ack |
| R4 WAN-emulated (optional) | 20 min each | `tc netem` on inkyank: +50 ms, then +150 ms delay with 1 % loss (needs sudo) | B under WAN conditions |

**DB load at candidate P (row 9)** is derived, not measured with many connectors: per-report DB time × reports per second per connector × expected connector count. The lab has one connector.

## 3. Output

`Connector-Revocation-PhaseM-Results-<date>.md` will contain:
- p50/p99/max per metric, per run;
- a recommended **B** and the resulting range for **P**;
- an O6 recommendation;
- data for O5 (reconnect latency) and O8 (renewal and reconnect behaviour).

The user decides P, O5, O6 and O8 from it.

## 4. What the user needs to do

1. Open SSH sessions to **inkyank (.75)** and **nika (.39)** (Orca terminals, as in earlier runs), or tell me which existing terminals to use.
2. Allow the controller to be started on this PC as the user unit `k2-controller`, built from `measure/phase-m`. It runs with the same short-cert settings as before (15 m connector certs).
3. Allow the measurement connector binary to be installed on inkyank, with the update timer still disabled. **sudo is needed on inkyank** for the install, and for R3's `ss -K` and R4's `tc netem`.
4. For R2: a logged-in client on this PC (already set up) and the `g9-nika-web` test server on nika.

After the runs: reinstall the Phase A connector binary and stop the measurement controller, or keep them, as you prefer.
