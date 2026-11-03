---
type: fix-phase
sprint: 20
fix: 5
phase: 5-A
title: net_stack socket lifecycle (FIN/RST/CLOSE-WAIT/fail-closed/back-stop)
status: live-accepted
depends_on: []
component: client
tags:
  - client
  - net-stack
  - fix-05
---

# Fix 05 · Phase 5-A — net_stack socket lifecycle

> Parent: [[Fix05-Client-Dataplane-Reliability]] (investigation, §1 and §4 "5-A").
> Live acceptance: [[Phase5A-Live-Acceptance-2026-10-05]] (partial) and
> [[Phase5A-Live-Acceptance-2026-10-06]] (completion).
> Separate from Fix 01 Phase 2-A, which stays frozen and is not touched.
> Status: **LIVE-ACCEPTED 2026-10-06 (L1–L5 pass).** Not committed.

## Problem (from the investigation)

When a relay ended, `net_stack` never closed the smoltcp socket. It removed sockets only in
`Closed`/`TimeWait`. A client that closed first left the socket in `CLOSE-WAIT` for the life of the stack,
still holding its 4-tuple. When the kernel later reused that source port, the SYN matched the leaked
socket instead of the listener, and the connection timed out (`curl 000 8.00x`). The fail-closed
branches never sent the RST their comment promised.

## Change (only `client/src/net_stack.rs`)

| # | Case | Behaviour |
|---|---|---|
| 1 | relay ends normally | FIN after every buffered byte is in the socket; socket removed after the close |
| 2 | relay fails (or dies without an outcome) | RST at once |
| 3 | app closes first (CLOSE-WAIT) | client→relay channel closed once the app's bytes are handed over; the relay forwards the half-close to the connector and **keeps reading**, so the response still arrives |
| 4 | fail-closed (`Some(None)`, `None`, empty list) | socket aborted at accept → RST |
| 5 | back-stop | a closing-state socket idle 60 s is aborted; ESTABLISHED is never affected |
| — | removal | TIME-WAIT, CLOSED after its RST has been dispatched, or LISTEN |
| — | observability | `net_stack: socket lifecycle stats` (info, ≤ 1/min, only on change) + a warning at ≥ 1024 live relays |

Structure: the accept and lifecycle code moved out of `run()` into `FlowTable::service` and
`drive_relay`, so the unit tests run the production code. `relay_tcp_to_quic` now returns `RelayEnd`.
The relay task stores its outcome before dropping the last sender. The relay also ends when the
net_stack side drops the flow.

Differences from the design note: (a) half-close instead of "shut down and end"; (b) the
relay stops when the net_stack side drops the flow; (c) client bytes with no relay attached are drained
and discarded; (d) `FlowTable` was extracted; (e) the stats line is `info`, not `debug`; (f) smoltcp
`set_timeout` was not used.

Not touched: daemon / Phase 2-A classifier and hot-apply, `TransportMap`, routes/TUN/nft, controller,
connector, 5-B.

## Unit gates (2026-10-05)

- `cargo test`: **146 passed, 0 failed** (baseline 134, **+12** new; 1 existing Phase 2-A test's
  ending adjusted for the half-close).
- `cargo build --release` OK, clippy 0 hits in `net_stack.rs`, rustfmt clean.
- Mutation check: with the old lifecycle restored temporarily, 8 of the 9 new smoltcp-level tests
  fail. The 9th, "idle flow untouched", is designed to pass either way.

## Live acceptance criteria

| Id | Criterion |
|---|---|
| L1 | One net_stack instance up ≥ 60 min with the 2 s new-connection sampler: `000 8.00x` count at or below the fresh-stack baseline rate (≈ 1/70), with **no upward trend** with stack age; client RSS flat (± 5 MB). |
| L2 | The deterministic repro (`curl --local-port P`, wait 70 s, `curl --local-port P` again) returns **200**, 5/5. |
| L3 | Connector-offline fail-closed gives the app a reset (curl `000` in < 1 s, not 8 s). |
| L4 | No regression: a long-lived keep-alive flow survives the whole run; a connector loss/return still hot-applies (Phase 2-A behaviour unchanged). |
| L5 | `net_stack: socket lifecycle stats` shows `active_relays` returning to ~0 between bursts (no accumulation). |

## Acceptance

- [x] Implemented; unit gates above
- [x] L1: 60 min on one net_stack instance, 1768 × 200, 0 × 000, no trend; RSS +1.9 MB (2026-10-06;
  10-05 had 30 clean min)
- [x] L2: 5/5 source-port reuse → 200 via the tunnel (2026-10-05)
- [x] L3: both connectors offline → 58/58 probes `000` in < 1 s (max 8 ms), `connector offline — failing
  closed`, `rst_fail_closed=88`, 0 restarts (2026-10-06)
- [x] L4: hold survived 90 min on one socket; connector loss and return each hot-applied, 0
  `restarting VPN`, ifindex/PID unchanged, shield failed over (2026-10-06)
- [x] L5: no accumulation over the whole run (`active_relays=1` → 0, removed == FIN + RST, 0 back-stop
  aborts)

Open observations outside 5-A scope (detection lag before `reachable=0`, shield reconnect back-off,
admin UI Protect gate): see [[Phase5A-Live-Acceptance-2026-10-06]] §Findings.
