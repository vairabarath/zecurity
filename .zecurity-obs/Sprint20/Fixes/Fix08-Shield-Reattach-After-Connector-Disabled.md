---
type: fix
sprint: 20
fix: 8
title: Shield re-attaches up to ~40 s after the connector serves again (client recovery lag)
status: fixed and verified live (2026-10-22). Committed locally, not pushed.
source: Phase B2 live run 2026-10-21, finding 1 ("client recovery lag ~21 s, direct-path retry backoff, runbook KI-5")
severity: medium
component: shield (src/control_stream.rs)
branch: feat/sprint20-m1-phase2
date: 2026-10-22
tags:
  - rust
  - shield
  - connector
  - fail-closed
  - reconnect
---

# Fix 08: the shield polls a "not serving" connector instead of backing off

## Symptom

B2 L-3 (2026-10-21): the controller was down 90 s. The connector was `Serving` again at 11:21:02.38, but the client's held flow resumed only at 11:21:21.9, about 19.5 s later. The finding blamed the client's direct-path cooldown (runbook KI-5).

## Root cause: corrected (not the client)

The client journal for L-3 shows the client reached the connector throughout:
- 11:19:41 → 11:21:01: `tunnel handshake failed … stream reset by peer`, while the connector was `Disabled` and refusing streams;
- **11:21:03 → 11:21:19: `shield not attached, trying next connector`.** The connector was serving, but the resource's shield wasn't attached;
- 11:21:21.92: `tunnel opened`.

So the lag was the **shield** re-attaching to the connector:
1. The connector, `Disabled` while the controller is unreachable, closes shield Control streams after `DISABLED_SHIELD_CLOSE_SECS` (30 s, D15, by design). While it's not serving, it refuses new ones in `control()` with `Unavailable("connector is not authorized to serve")` (`connector/src/agent_server.rs`, B1 admission gate).
2. The shield's `reconnect_loop` (`shield/src/control_stream.rs`) treated every error the same: exponential backoff 5 → 10 → 20 → 40 → 60 s. It reset only after a *clean* close.
3. When the connector served again, the shield was usually in the middle of a long sleep. The next attempt came up to 40–60 s later.

The client's direct-path cooldown (KI-5) wasn't involved. The client's QUIC connection to the connector stayed up.

## Live reproduction (2026-10-22, baseline shield = `6a4970c`)

**Setup:**
- controller `bda9244` (`controller-head`);
- connector `s3-relaylist` (Fix 07 build, connector code unchanged by this fix);
- disposable **user-level shield** `s3-shield` on this PC, enrolled into `k2-net` (`generateShieldToken` + `POST /api/shields/{id}/token`), state in `~/.cache/zecurity-s3/shield/`, runner `run-shield.sh`. The shield needs no root until it applies nftables rules, so its Control stream runs unprivileged.
- **Outage** (`outage.sh`, as in L-3): sha-check the running controller, stop it for 90 s, restart it. `outage-report.sh` prints the connector authz states and the shield's stream events.

Times are UTC.

| Time | Event (baseline) |
|---|---|
| 09:05:36.8 | controller stopped |
| 09:05:46.8 | connector `Serving → ServingDisconnected` |
| 09:06:00.4 | connector `→ Disabled` |
| 09:06:30.4 | connector closes the shield stream (`Disabled` + 30 s): shield `not authorized to serve`, backoff 5 s |
| 09:06:36.1 / 09:06:46.6 / 09:07:07.6 | shield refused (`not authorized to serve`), backoff **10 / 20 / 40 s** |
| 09:07:07.0 | controller listening again |
| **09:07:10.959** | connector `Recovering → Serving` |
| **09:07:48.297** | shield `Control stream established`: **37.3 s after the connector served** |

## Fix (shield only)

`shield/src/control_stream.rs`:
- **New `connector_not_serving(&anyhow::Error)`.** It looks for a `tonic::Status` in the error chain with code `Unavailable` and one of the connector's "not serving" messages (`CONNECTOR_NOT_SERVING_MESSAGES`):
  - `connector is not authorized to serve` (B1 gate and fail-closed close-all);
  - `connector has not yet applied the controller's shield revocation list` (startup gate).

  These are the only refusals a **live** connector gives while it's temporarily not serving. A comment ties them to `connector/src/agent_server.rs` ("keep in lockstep").
- **`reconnect_loop`:** on such an error it logs `connector is not serving yet; retrying`, sleeps **`NOT_SERVING_RETRY_SECS` = 2 s** (fixed) and **resets** the backoff. Every other error (connector process down, TLS or transport failure) keeps the old 5 → 60 s exponential backoff.
- **`run_once`:** a mid-stream receive error now keeps the `Status` in the chain (`anyhow::Error::new(e).context("control stream recv error")`). Before, it was flattened into a string, so the D15 close at `Disabled` + 30 s couldn't be recognised.
- **Cost:** while a connector is `Disabled`, the shield makes one TLS handshake plus one refused RPC every 2 s on the LAN. This is bounded and stops once the connector serves.
- **No change** to the connector, the D15 grace, or the client.

## Tests

- **New** (`control_stream::tests`, paused clock, fake `ControlSession`):
  - `connector_not_serving_is_polled_at_a_fixed_short_interval`: a mid-stream close, then 4 refusals → attempts at **0, 2, 4, 6, 8, 10 s**;
  - `other_errors_keep_exponential_backoff_reset_after_not_serving`: transport errors keep 5/10 s; a not-serving refusal resets the next wait to 5 s (attempts 0, 5, 15, 17, 22, 32);
  - `only_the_connectors_not_serving_refusals_are_fast_retried`: other `Unavailable` messages, other codes, and plain strings aren't matched.
- **Mutation check:** with `connector_not_serving` forced to `false`, **all 3 FAIL**; restored, all pass.
- **Gate:** `cargo build --manifest-path shield/Cargo.toml` passes with no new warnings (15 pre-existing before and after). `cargo test` gives 35 PASS, 0 FAIL; 2 ignored are pre-existing live-kernel nftables tests (`unshare -rn`).

## Live verification (fixed shield, same setup and outage)

| Time | Event (fixed) |
|---|---|
| 09:08:58.6 | controller stopped |
| 09:09:08.6 | connector `Serving → ServingDisconnected` |
| 09:09:22.9 | connector `→ Disabled` |
| 09:09:52.9 | connector closes the shield stream (`Disabled` + 30 s). The shield recognises it (the mid-stream `Status` is kept): **`connector is not serving yet; retrying retry_secs=2`** |
| 09:09:55.6 … 09:10:31.5 | 14 more refusals, one every **~2.7 s** (2 s wait + connect and setup), no growth |
| 09:10:28.9 | controller listening again |
| **09:10:33.973** | connector `Recovering → Serving` |
| **09:10:34.210** | shield `Control stream established`: **0.24 s after the connector served** (baseline 37.3 s) |

**Result:** the shield re-attach lag after a controller outage drops from **37.3 s to 0.24 s**. The same scenario in B2 gave ~19.5 s.

**Not covered live:** the client's end-to-end flow through this shield. That needs a protected resource on the shield host, and the shield needs root for nftables; this PC's shield ran unprivileged. The L-3 client journal shows the client opens the tunnel as soon as the shield is attached (`shield not attached` until 11:21:19, `tunnel opened` 11:21:21.9). Re-check end to end on the lab hosts (udaya shield) when they're back: in L-3, the flow should resume within ~3 s of the connector serving.

## Follow-up noted (not changed)

- **Shield failover:** with several connectors, `build_client` picks the first one that accepts TLS. A `Disabled` connector still accepts TLS, so the shield keeps retrying the same head connector instead of failing over to a serving one (the intent in D15's "the shield fails over"). Single-connector labs aren't affected. Worth its own finding.
- **Runbook:** `docs/sprint20-live-acceptance-runbook.md` KI-5 (client direct-path cooldown when *forcing the relay*) is still accurate. Only the B2 attribution of the recovery lag to KI-5 was wrong.

## Lab state after the run

- Connector `s3-relaylist` and shield `s3-shield` (both k2-net) are **enrolled, processes stopped**. Restart them with:
  - `~/.cache/zecurity-s3/run-conn.sh ~/.cache/zecurity-s3/connector-fix`
  - `~/.cache/zecurity-s3/run-shield.sh ~/.cache/zecurity-s3/shield-fix`

  Revoke both in the admin UI when no longer needed.
- Controller `controller-head` (`bda9244`) is running.
