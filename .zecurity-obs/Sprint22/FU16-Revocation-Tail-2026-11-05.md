---
type: investigation + fix record
date: 2026-11-05
author: Barath (M2) with Claude
branch: fix/fu16-revocation-tail (uncommitted, base main e07fe524)
status: measured, root causes found, client fix implemented and validated live; not committed
---

# FU-16: the tail after a tunnel ends (measurement, root cause, fix)

**Reporting rule:** each number below comes from a run file or a log line in this
investigation. Anything inferred is marked as inferred, and hypotheses are marked as
hypotheses.

## Summary

- **Access revocation (the original FU-16 report): there is nothing left to fix in
  Zecurity.**
  - The 0.84–1.10 MB that reached curl after a member removal was already in **curl's
    own kernel socket buffer** (`Recv-Q`) at the cut.
  - Zecurity stops delivering within about 20–40 ms of the cut.
  - Linux still hands an application the data queued in its socket before it reports the
    reset.
  - A plain TCP connection reset by the server, with no Zecurity, leaves the same or a
    larger tail.
  - The original suspicion (FU-8's hot-remove clashing with FU-15's queue drop) is
    **refuted**: the client drops the flow at once.
- **The investigation found a real client defect on another path: the transport is
  lost** (for example, the connector stops).
  - After a QUIC connection closes, quinn still hands out the data it had buffered for a
    stream. The client relayed that data to the app at the app's pace: **844–897 KB over
    ~34 s** at 25 KB/s, and **2.15 MB** at 300 KB/s.
  - Separately, a relay task blocked on a full app queue could not see a stream reset.
  - **Fixed in the client** (`client/src/stream_watch.rs`). Live, the same connector
    stop now leaves **78 KB** at 25 KB/s and **922 KB** at 300 KB/s, which is curl's own
    `Recv-Q` in both cases. The client ends the flow within about 0.7 s of the stop,
    against about 30 s before.
- **Recommended acceptance boundary:** after a tunnel ends, by revocation or by transport
  loss, the application receives at most what its own kernel socket held at the cut,
  plus what its reading admits in the ~40 ms until the client resets the connection. In
  these runs that is ≤ 1.05 MB at 300 KB/s and ≤ 83 KB at 25 KB/s. This tail is
  irreducible without shrinking the app's TCP window (see Trade-offs).

## Root causes

| ID | Status | Cause | Effect |
|---|---|---|---|
| **R1** | **Confirmed** (live and unit test) | After a QUIC connection is lost, quinn's `RecvStream::read` keeps returning the data buffered for the stream (up to its receive window, `STREAM_RWND` 1.25 MB by default) and reports `ConnectionLost` only once that data is drained. The client relayed it to the app at the app's pace. | Connector stop, baseline: 844–897 KB after the cut at 25 KB/s, last byte at +33–35 s; 2.15 MB at 300 KB/s. The client logged `tunnel read failed … connection lost` 30 s after the stop at 25 KB/s (10:54:31 → 10:55:00.76), when the buffer ran dry, not when the close arrived. Unit control: `a_closed_connection_hands_out_no_buffered_data` shows raw quinn returning data after the peer closed the connection. |
| **R2** | **Confirmed** (deterministic test) | The relay task (`relay_tcp_to_quic_ctl`) notices a stream reset only when it reads, and it doesn't read while it waits in `quic_to_tcp_tx.send().await` for room in the full app queue. | A slow or paused reader keeps a reset flow alive until it next makes room. `a_reset_ends_a_slow_readers_flow_without_draining_the_backlog` measured 302 ms without the fix (one read every 600 ms), against < 200 ms required. **Not the cause of the revocation tail:** FU-8's hot-remove drops the flow 20–40 ms after the cut anyway. |
| **R3** | **Confirmed, irreducible** | Data the client's TCP stack had already delivered sits in **the app's kernel socket buffer**. Linux delivers queued data to the application before it reports a RST (checked locally: 1 MB read, then ECONNRESET). | Every revocation run: bytes after the cut ≤ curl's `Recv-Q` at the cut + about 3 KB. Plain TCP reset (no Zecurity): the same or larger, because data in flight on the wire also arrives. |
| H1 | **Hypothesis** (not tested) | curl's `Recv-Q` at 300 KB/s is larger through Zecurity (0.53–1.05 MB) than over plain TCP (0.34–0.44 MB). Likely the kernel's receive-buffer autotuning reacting to the client stack's near-zero-RTT TUN delivery. | A larger R3 tail at high rates through Zecurity than through plain TCP. Bounded by `tcp_rmem` max (32 MB on this host) and by what the app has left unread. |

## Topology

- **Client:** this PC, 192.168.1.223, Arch Linux.
  - `zecurity-client` daemon with a TUN interface.
  - Baseline binary `bin/zecurity-client.baseline`: main `e07fe524`, sha256 prefix `fb0d6518e0a466aa`.
  - Fixed binary `bin/zecurity-client.fixed`: this branch, `be6bea664eaffd9f`.
  - The daemon was restarted after each install. (The first "fixed" runs ran the old
    binary because `enable --now` doesn't restart; they were discarded.)
- **Controller:** this PC, unit `fu16-controller`.
  - Throwaway database `fu16_live`; isolated Valkey `ci_valkey_6390`, database 4.
  - No controller code change.
- **Connector:** inkyank, 192.168.1.75, built from main (`6d60bdbe1aec854c`).
  - Resource: an HTTP file server on `192.168.1.75:8817` (a connector-route resource,
    serving the 60 MB file `big60.bin`).
- **Control (no Zecurity):** `ctlserver.py` on `192.168.1.75:8818`, reached over the LAN
  directly.
  - A connection to port 8819 resets every live download (`SO_LINGER 0` + close, i.e. a
    RST from the server).
- **Downloader:** `curl -s --limit-rate {25K|300K}`.

## Method

- `measure.py`:
  - samples every ~1 ms: the size of curl's output file, and curl's socket `rx_queue`
    from `/proc/net/tcp` (bytes the kernel received and curl hasn't read);
  - at a fixed time (8 s at 300K, 20 s at 25K) it performs the cut;
  - after the cut it records: `Recv-Q` at the last sample before the cut, bytes after
    the cut, time of the last byte, and curl's end and exit code (56 = receive error, in
    every run).
- **Cuts:**
  - *revoke*: `removeGroupMember` through the admin API (call: 11–20 ms);
  - *rst*: the control server's RST;
  - *stop*: `sudo systemctl stop zecurity-connector` on inkyank, sent through an Orca
    terminal.
- Between revoke trials the member is re-added and access re-checked (200).
- Client-side timing comes from the client's journal: change hint, ACL sync, hot-remove,
  and the relay's end reason.
- **Accuracy limits:**
  - `Recv-Q` is sampled, so up to 1 ms (≈ 0.3 KB at 300 KB/s) can be missing before the
    cut.
  - Bytes are counted from curl's file, at curl's write granularity.
  - For *stop*, the cut time is when the Orca command was **sent**; the command call took
    645–660 ms. The stop happened inside that window, so "bytes after the cut" overstates
    by up to 0.66 s of flow (≈ 16 KB at 25K, ≈ 200 KB at 300K).
  - Two or three trials per cell only, on one LAN.
  - Throughput figures are spot checks, not a benchmark.

## Results

### Revocation and plain-TCP control (bytes in curl's socket at the cut vs bytes after the cut)

| Series | Client | `Recv-Q` at cut | Bytes after cut | Last byte / curl end |
|---|---|---|---|---|
| Z300 (revoke, 300K) | baseline | 534 KB / 998 KB / 906 KB | 340 KB / 1,001 KB / 909 KB | +1.0–3.1 s / +1.0–3.1 s |
| Z300 | fixed | 1,001 KB / 1,001 KB / 1,049 KB | 926 KB / 1,004 KB / 926 KB | +2.0–3.1 s / +3.0–3.1 s |
| C300 (plain TCP RST, 300K) | baseline | 440 / 336 / 349 KB | 618 / 311 / 673 KB | +0.03–2.0 s / +1.0–2.0 s |
| C300 | fixed (same, no Zecurity) | 409 / 431 / 345 KB | 618 / 483 / 618 KB | +1.0 s / +1.0–2.0 s |
| Z25 (revoke, 25K) | baseline | 82 / 82 KB | 74 / 78 KB | +2.0–3.0 s / +3.0 s |
| Z25 | fixed | 83 / 83 KB | 74 / 74 KB | +2.0 s / +3.0 s |
| C25 (plain TCP RST, 25K) | baseline | 84 / 89 KB | 176 / 115 KB | +4.0–6.0 s / +4.0–7.0 s |
| C25 | fixed (same, no Zecurity) | 90 / 87 KB | 135 / 176 KB | +5.0–6.0 s / +5.0–7.0 s |

- **Through Zecurity**, bytes after the cut were within `Recv-Q` at the cut + 3.3 KB in
  every run.
  - The +3.3 KB is what curl's reading admitted between the cut and the client's reset.
    A revocation in client log order (fixed run at 11:28:14.168):
    - change hint at +12 ms;
    - the relay ended with `tunnel stream reset by peer: 1381387076` (`RVKD`) at +19 ms;
    - hot-remove done at +40 ms.
  - On the baseline the hot-remove ends the flow at +25 ms (10:50:50.847 → .873).
  - The revocation tail is therefore R3 on both binaries, as expected: the fix doesn't
    change it.
- **Plain TCP** (no Zecurity) leaves the same or more: up to 270 KB beyond `Recv-Q`,
  because data in flight on the wire also arrives.

### Connector stop (transport loss)

| Series | Client | `Recv-Q` at cut | Bytes after cut | Last byte / curl end | Client end of flow |
|---|---|---|---|---|---|
| stop 25K | baseline | 83 / 82 KB | **844 / 897 KB** | +33.0 / +35.0 s; +34.0 / +36.0 s | `tunnel read failed … connection lost` at +30 s (10:54:31 → 10:55:00.76) |
| stop 25K | fixed | 83 / 83 KB | **78 / 78 KB** | +3.0 s / +4.0 s | `tunnel ended while the app was not reading … tunnel transport closed` at +0.71 / +0.72 s |
| stop 300K | baseline | 1,050 / 997 KB | **2,154 / 2,154 KB** | +7.1 s / +8.1 s | — |
| stop 300K | fixed | 1,050 / 951 KB | **922 / 922 KB** | +3.1 s / +4.1 s | at +0.69 / +0.70 s |

- Fixed: the client's end of the flow (+0.69–0.72 s) falls inside the 0.65 s Orca
  command latency plus the connector's shutdown. Bytes after the cut ≤ `Recv-Q` (R3
  only).
- Baseline: the excess over `Recv-Q` (≈ 760–815 KB at 25K, ≈ 1.1 MB at 300K) was held by
  the client, mostly in quinn's stream buffer (inferred from the 1.25 MB window bound and
  R1's unit control). It was delivered at curl's pace.

### Throughput (spot checks, unthrottled download)

- Baseline 27.9–28.7 MB/s; fixed 27.4–27.7 MB/s.
- The difference (≤ 3%) is within what a handful of LAN runs can resolve. No significance
  is claimed.

## Changes (client only)

| File | Change | Why |
|---|---|---|
| `client/src/stream_watch.rs` (new) | `WatchedStream<S>`: a normal read first checks whether the QUIC connection is closed (`Connection::closed()`) and fails if it is (`ConnectionAborted`, "tunnel transport closed"). A **zero-length read** is a "watch": it completes only with the error that ends the tunnel, and stays pending otherwise. `ResetAwareRecv`: a quinn `RecvStream` whose zero-length read polls the cancel-safe `received_reset()` without consuming data. `tunnel_ended(&mut recv)`: the watch as a future. | Fixes R1: no data buffered on a dead connection is handed out. In QUIC a closed connection ends every stream abruptly, like a reset. Provides the R2 watch without reading data, so no extra buffer and no extra task. |
| `client/src/tunnel_pool.rs` (`open_probed_stream`) | The direct-path stream is `WatchedStream::new(join(ResetAwareRecv(recv), send), quic_connection_gone(&conn), true)`. | Direct path: both the loss and the reset are watched. |
| `client/src/relay_pool.rs` (`open_authenticated_stream`) | The relay-path stream is `WatchedStream::new(RelayFlowStream{…}, quic_connection_gone(&conn), false)`. | Relay path: the connection loss is watched. The inner TLS stream doesn't get watch reads, because a zero-length read would make rustls read and buffer records. |
| `client/src/net_stack.rs` (`relay_tcp_to_quic_ctl`) | When it has data for the app, the relay task `select!`s (biased) between `tunnel_ended(&mut recv)` and `quic_to_tcp_tx.send(…)`. A tunnel end while waiting fails the flow (`relay_failed`, "tunnel ended while the app was not reading"), so the poll loop drops the flow's queue and resets the app connection (FU-15). | Fixes R2. The queue stays bounded (`FLOW_QUEUE_CAP`, unchanged). There is no new task or channel: the watch is a poll on the stream the task already owns. |
| `client/src/main.rs` | `mod stream_watch;` | — |

**Not changed:** connector, shield, relay, controller, proto, admin, FU-9 code.

## Trade-offs

- **Throughput:** one extra `poll` of a boxed `closed()` future per read, plus a
  zero-length poll while blocked. No measurable cost in the spot checks.
- **The relay path doesn't see a stream reset while the app is not reading.**
  - Through TLS the watch only covers the connection loss.
  - A reset there is still seen at the next read, i.e. at most one app-queue's worth
    later. Revocations are also cut at once by FU-8's hot-remove.
  - Accepted, not fixed: fixing it would mean watching the QUIC stream under TLS, which
    the relay stream type doesn't expose.
- **A finished stream is not an error:** `received_reset` resolving `Ok(None)` (finished
  or stopped) ends the watch with no error, so the normal end (F-7 clean close) is
  unchanged. `a_finished_stream_is_delivered_and_is_not_an_error` and
  `a_watched_flow_delivers_everything_and_ends_normally` pin this.
- **R3 could be reduced only by limiting what the client stack puts into the app's
  socket.** That means a smaller TCP window towards the app, which costs throughput on
  every flow, to shave a tail that plain TCP has as well. Not done; not recommended.

## Tests

| Test | What it pins | Without the fix |
|---|---|---|
| `stream_watch::tests::a_watch_sees_a_reset_without_consuming_data` (real quinn, loopback) | A watch sees a reset while 100 KB sits unread; no data is handed out afterwards. | Fails under mutant M2 |
| `stream_watch::tests::a_closed_connection_hands_out_no_buffered_data` | Control: raw quinn hands out buffered data after a close (R1). Watched: the read fails. | Fails under mutant M3 |
| `stream_watch::tests::a_watch_ends_when_the_connection_closes` | Watch resolves on close. | — |
| `stream_watch::tests::a_finished_stream_is_delivered_and_is_not_an_error` | 150 KB delivered; no error afterwards. | — |
| `stream_watch::tests::a_watch_stays_pending_while_the_tunnel_is_up` | No false end; still writable. | — |
| `net_stack::tests::a_lost_transport_ends_the_flow_while_the_app_queue_is_full` | R1 + R2 at the relay task. | Red before the relay change; fails under M1 |
| `net_stack::tests::a_reset_ends_the_flow_while_the_app_queue_is_full` | R2. | Red before; fails under M1 and M2 |
| `net_stack::tests::data_held_by_a_lost_transport_is_not_relayed` | Regression: data written after the loss never reaches the app queue. | Not mutation-sensitive (see below) |
| `net_stack::tests::a_watched_flow_delivers_everything_and_ends_normally` | 200 KB, ends `Normal`. | — |
| `net_stack::tests::a_reset_ends_a_slow_readers_flow_without_draining_the_backlog` | Slow reader (one chunk per 600 ms): flow ends < 200 ms after the reset; ≤ queue + 1 chunks after it. | Fails under M1 (302 ms) |
| `net_stack::tests::many_blocked_flows_all_end_when_their_tunnels_end` (stress, 4 threads) | 64 blocked flows with the connector writing; 32 lose the transport, 32 are reset; all end `Failed` < 2 s; each queue ≤ 1 chunk. | Fails under M1 (`flow 0 did not end`) |

**Mutants** (applied to copies, original restored and byte-compared after each):

| Mutant | Change | Failing tests |
|---|---|---|
| M1 | The watch arm replaced by `pending()` | `a_lost_transport_…`, `a_reset_ends_the_flow_…`, `a_reset_ends_a_slow_readers_flow_…`, `many_blocked_flows_…` |
| M2 | Watch reads not passed to the inner stream | `a_reset_ends_the_flow_…`, `a_watch_sees_a_reset_…` |
| M3 | Normal reads ignore a closed connection | `a_closed_connection_hands_out_no_buffered_data` |

M3 is caught only by the unit test: at the relay level, the watch arm also ends the flow
on a closed connection, so the per-read gate is defence in depth. That's also why
`data_held_by_a_lost_transport_is_not_relayed` doesn't separate M3.

**Results:**
- `cd client && cargo test`: **350 passed, 0 failed, 0 ignored** (348 before the two new
  tests).
- The two new tests repeated 10× pass 10/10.
- `cargo clippy --all-targets`: no warning in the changed files except the existing
  `end_reason` "value assigned is never read" (`net_stack.rs`, also on main).
- `rustfmt --check` on the new files: clean.
- Release build (`ZECURITY_PLATFORM_ROOT_PEM_FILE` = the lab root): OK.
- Live: the tables above, 4 stop runs and 10 revoke/control runs per binary.

## Limitations

- **Relay path not measured live.** The lab run used the direct (connector) path. The
  relay path's connection-loss watch is covered by code review and shares
  `WatchedStream`; its reset is not watched (Trade-offs).
- **Shield-route resource not re-measured** in this investigation. FU-9's live run showed
  the same tail on both routes, and the fix sits below the route choice.
- **Two or three trials per cell.**
- **H1 is untested.**
- **Not covered:** idle-timeout loss (30 s, no close frame), e.g. a pulled cable. The
  same `closed()` future resolves then, but it wasn't run live.

## Recommended acceptance boundary

After a tunnel ends (access revoked, stream reset, or transport lost), the client:

1. relays no more data to the application, and ends the flow within ~50 ms of learning of
   the end, whether or not the app is reading;
2. drops what it holds for the flow and resets the app's connection (FU-15).

Explicitly:

- **TCP bytes already queued in the application's socket at revocation cannot be retracted
  by a later reset.** The application may still read what its own kernel socket held at
  that moment. This is the same as for any TCP connection reset by its server, and FU-16
  does not count it as a leak.
- **Quinn's delivery of buffered stream data after the connection closed (R1) was a
  separate client defect**, and a relay task blocked on a full app queue missing a reset
  (R2) was another. Both are addressed by this implementation.
- **Not every connection-loss or stream-reset scenario has been validated.** Validated
  live:
  - the direct (connector) path;
  - revocation by member removal;
  - a connector stop that sends a connection close.

  Outstanding live-validation gaps (see Limitations):
  - the relay path, where stream resets inside TLS are not watched;
  - a connection lost without a close frame (idle timeout, pulled cable);
  - a shield-route resource.
