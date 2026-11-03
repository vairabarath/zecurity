---
type: fix-investigation
sprint: 20
fix: 5
phase: 5-B
title: Stale pooled QUIC connection — investigation
status: live-accepted-with-caveat
date: 2026-10-06
component: client (tunnel_pool / transport / relay task)
depends_on: [Phase5A-NetStack-Socket-Lifecycle, Fix05-Empty-Network-Transport-Delivery]
---

# Fix 05-B · Stale pooled QUIC connection — investigation

> Investigation only. **No production code was changed.** Labels: **[src]** = read in this
> repo or in the crate source pinned by `client/Cargo.lock` (quinn 0.11.9, quinn-proto 0.11.14);
> **[live]** = journal or controller log; **[inferred]** = reasoning, not proven.
> This note supersedes the 5-B sketch in [[Fix05-Client-Dataplane-Reliability]] §4. That sketch
> proposed `connection.close()` on timeout, which §9 shows is unsafe.

## 1. Status

`implemented-pending-live` (2026-10-06), then **`live-accepted-with-caveat`** (2026-10-07). The
investigation (§2–§19) was approved as **Option A + Q4** and is implemented in §20. Unit gates are green.
Live acceptance B1–B7 (§18) ran; results and caveats are in §20 "Live acceptance". Not committed.

## 2. Problem statement

A connector process dies without sending a QUIC `CONNECTION_CLOSE`. The client keeps the pooled QUIC
connection to that connector and keeps handing it to new flows until quinn's **idle timeout** fires,
which is **30 s after the last packet received** from the connector. During that window, each new flow
gets a stream on the dead connection instantly, writes its `TunnelRequest`, and then waits the full
application-level `TUNNEL_HANDSHAKE_TIMEOUT` (5 s) for a reply that never comes. That transport is
then skipped for this flow only. Nothing is evicted and nothing is marked unhealthy, and the next flow
repeats it.

## 3. Existing live evidence

| Run | Window | What happened | Source |
|---|---|---|---|
| 5-A L3, before the empty-network fix (total loss; the stale ACL kept the dead connector in the map) | manoj `disconnected` **13:48:09** (controller) | **9** `tunnel handshake timed out after 5s` (first 13:48:14.772, last 13:48:39.180), then at **13:48:39.772** one in-flight handshake failed with `transport I/O failed: connection lost` (quinn idle `TimedOut`, **≈ 30.x s after 13:48:09**). Then `direct stream establishment exceeded 2s` ×2 (13:48:42.215, 13:48:43.802), then `direct path is in cooldown` ×21 (fast). The resync at 13:48:14.785 logged `effective config unchanged`, so no new pools were built. | client journal; `f5-events.log` `L3-END` |
| Empty-network acceptance (A/B/C + pilot) | stop at :03/:23/:43 | exactly **5** handshake timeouts per iteration. The first one triggers the early resync, the empty-network snapshot hot-applies at +5 s, and the **new map has no pool for palace** (slot `None`), so the stale connection is never offered again. | [[Fix05-Empty-Network-Live-Acceptance-2026-10-06]] |
| Fix05 investigation Row C (2026-10-05) | after kill | 1 handshake timeout each, then resync and hot-apply | [[Fix05-Client-Dataplane-Reliability]] §3 |

**Takeaway [live]:** the stale connection is reused until exactly the quinn idle timeout (L3). It is
cut short only when something rebuilds the transport map (empty-network run). The ~5 slow flows
per iteration are the flows that *started* in the 5 s before the first one timed out.

## 4. Exact pool architecture

| Object | Where | Facts |
|---|---|---|
| `TunnelPool` (direct path) | `client/src/tunnel_pool.rs:320-323` | `connections: Arc<tokio::sync::Mutex<HashMap<SocketAddr, quinn::Connection>>>`, `endpoint: quinn::Endpoint`. **Key = connector tunnel `SocketAddr`.** One pool per connector per map build, so at most one pooled connection per connector. |
| Client QUIC config | `tunnel_pool.rs:359-362` | `TransportConfig::default()` + `keep_alive_interval(Some(10 s))`. `max_idle_timeout` is **not set**, so it defaults to 30 s (`quinn-proto/src/config/transport.rs:365`). No connect/handshake timeout is configured in quinn. |
| Connector QUIC server config | `connector/src/quic_listener.rs:28-32` | `ServerConfig::with_crypto(..)`, default transport: idle 30 s, keep-alive `None`, `max_concurrent_bidi_streams` 100. |
| Selection | `TunnelPool::get_or_connect` `tunnel_pool.rs:374-407` | Under the lock: if an entry exists and **`close_reason().is_none()`**, return a clone. If it is closed, remove it. Otherwise release the lock, `endpoint.connect(addr).await`, re-lock, and if a *still-open* entry appeared meanwhile, `close(…"raced")` the new connection and return the existing one. Otherwise insert. |
| Stream open | `TunnelPool::open_authenticated_stream` `:417-427` | `get_or_connect` then `conn.open_bi()`. An `open_bi` error becomes `TunnelOpenError::Connect`. **The pool is not touched on error.** |
| `ClientTransport` | `client/src/transport.rs:51-160` | Wraps `Arc<dyn DirectOpener>` (the `TunnelPool`) + `direct_addr` + optional `RelayContext`. `open_authenticated_stream` = `timeout(DIRECT_TIMEOUT = 2 s, direct.open(addr))`. **`Ok` calls `mark_direct_success()`** (resets cooldown). `Connect` error or the 2 s timeout calls `mark_direct_failure()` (30 s → 2 h exponential cooldown) and then tries the relay (`RELAY_TIMEOUT` 5 s). `Authenticate` errors surface without relay. |
| Map build | `client/src/daemon.rs:4062-4117`, `:4119-4146` | `build_transports_by_resource_with_crl` builds a **fresh** `ClientTransport` + `TunnelPool` (+ `RelayPool`) per connector (`transport_cache` is local to one build and keyed by `connector_id`), shared by every resource entry of that connector. Each build = new pools for **all** connectors. |
| Map delivery | `net_stack.rs:639`, `:649-655`, `:568` | `TransportMap = HashMap<(ip,port), Option<Vec<Arc<ClientTransport>>>>` in a `watch` (Phase 2-A), read once per accepted flow. The flow's relay task gets the `Vec` by value. |
| Relay pool (relay path) | `client/src/relay_pool.rs:38-54`, `:108-123`, `:160-218` | Keyed by relay address string, with `MAX_RELAY_CONNECTION_AGE` 60 s. **Evicts on `open_bi` error** (`:118`). On age > 60 s or closed it removes **and `close()`s** the old connection (`:176-180`). Spawns a CRL monitor task holding a `Connection` clone (`:199`, `:253-272`). |

## 5. Exact failure path

1. **Connector dies uncleanly.** `systemctl stop` sends SIGTERM. The connector has no signal handler and
   no `endpoint.close()` (`grep signal|shutdown|.close(` in `connector/src/main.rs` and
   `quic_listener.rs` finds nothing) **[src]**, so the process exits and no `CONNECTION_CLOSE` is sent.
   The host stays up, so later client datagrams get ICMP port-unreachable, which quinn does not act on.
   The connection lived the full 30 s in L3 **[live]**.
2. **The pooled connection stays "open".** `close_reason()` returns `state.error`
   (`quinn/src/connection.rs:385-387`), which is set only on close/idle-timeout/peer close. The idle
   timer is reset only in `on_packet_authenticated` (`quinn-proto/src/connection/mod.rs:1903-1915`,
   `reset_idle_timeout` `:1957-1968`, `dt = max(idle, 3·PTO)`). The client's own keep-alive PINGs (every
   10 s) do **not** extend it. Negotiated idle = `min(30 s, 30 s)` = 30 s (`negotiate_max_idle_timeout`
   `:4065-4071`).
3. **New flow.** `net_stack` accepts the SYN, then `transports_for` hands the `Vec<Arc<ClientTransport>>`
   to `relay_tcp_to_quic` (`net_stack.rs:568-591`, `:844`).
4. **`ClientTransport::open_authenticated_stream`** (`transport.rs:112`) runs `TunnelPool::get_or_connect`,
   which **returns the dead connection** because `close_reason()` is still `None` (`tunnel_pool.rs:379-381`).
   `open_bi()` succeeds **locally**: `poll_open` only checks `state.error` and local stream credit
   (`quinn/src/connection.rs:696-709`). No peer round-trip happens.
   So `direct.open` is `Ok` well inside 2 s, `mark_direct_success()` runs (cooldown reset), and **the
   relay fallback is never tried**.
5. **Higher-level handshake** (`net_stack.rs:895-899`). `write_framed_json(TunnelRequest)` succeeds (the
   bytes are buffered in the quinn send buffer), and `read_framed_json::<TunnelResponse>` waits forever.
6. **Timeout.** `tokio::time::timeout(TUNNEL_HANDSHAKE_TIMEOUT = 5 s)` (`net_stack.rs:36`) returns
   `Err(Elapsed)`. The arm at `:915-923` logs **`tunnel handshake timed out after 5s`**, sets
   `saw_transport_failure = true`, and `continue`s to the next candidate. The `AuthenticatedStream` is
   dropped. `SendStream::drop` calls `finish()` and `RecvStream` drop sends stop
   (`quinn/src/send_stream.rs:344-364`). **The connection itself is untouched.**
7. **No eviction or marking.** The timeout arm does not call into `ClientTransport` or `TunnelPool`.
   The pool entry stays in place and `direct_unhealthy_until` stays 0 (it was just reset in step 4).
8. **After the loop.** If no candidate succeeded and any transport failure was seen,
   `resync.notify_one()` (`:957-958`) starts the daemon's early transport resync, which runs the
   Phase 2-A classifier. A rebuild happens **only if the effective config changed** (empty-network
   total loss: yes; partial loss: no; pre-fix total loss: no, `effective config unchanged`). If another
   candidate succeeded (partial loss), **no resync is signalled at all**.
9. **Next flow.** Steps 4–8 repeat with the same connection, until either:
   - **(a) idle timeout.** About 30 s after the last received packet, `state.error = TimedOut`. Streams
     waiting on it fail with `connection lost` (L3 13:48:39.772). The next `get_or_connect` sees
     `close_reason()` is `Some`, removes the entry and calls `endpoint.connect` to the dead peer.
     `DIRECT_TIMEOUT` 2 s fires, then `mark_direct_failure()` (cooldown 30 s), then the relay if one is
     configured. Later flows skip direct immediately (`direct path is in cooldown`). This is L3 from
     13:48:42 on.
   - **(b) a map rebuild** (hot-apply or structural restart). The new map has new `TunnelPool`s. The
     old pool is dropped when the last `Arc<ClientTransport>` goes (the map plus in-flight relay tasks'
     `Vec`s). The dead connection's last `ConnectionRef` then drops, and `implicit_close` runs
     (`quinn/src/connection.rs:927-940`).

**Answers to §2 questions**

| Question | Answer |
|---|---|
| Which function reports the timeout? | `relay_tcp_to_quic`, `net_stack.rs:915-923`. |
| QUIC connect, stream open, or higher-level handshake? | The **application-level** `TunnelRequest`/`TunnelResponse` exchange on an already-open stream. Not the QUIC handshake, not `open_bi`. |
| Error type | `tokio::time::error::Elapsed`. Never converted into a `TunnelOpenError`. Only logged. |
| Pool removes it? | No. |
| Still eligible? | Yes, until `close_reason()` becomes `Some` (idle timeout) or the pool is replaced. |
| Connector/transport marked unhealthy? | No. Worse, step 4 just called `mark_direct_success()`. |
| Retry? | Only the next candidate in this flow's list. No retry on the same transport, and no relay fallback (relay is only tried when `direct.open` fails). |
| Transport-map rebuild? | Only indirectly: the resync is signalled if **every** candidate failed, and the rebuild happens only if the effective config changed. |
| Fix 01 / Phase 2-A recovery path? | Same as above. Phase 2-A hot-apply replaces all pools, which masks the problem whenever it fires. |
| Next attempt | Same stale connection (step 9). |

## 6. Pool lifecycle

| Event | Current behaviour | Evidence |
|---|---|---|
| Connection established | `endpoint.connect(addr,"connector").await` inside `get_or_connect`, bounded only by the caller's 2 s `DIRECT_TIMEOUT` | `tunnel_pool.rs:387-394`, `transport.rs:116` |
| Added to pool | `conns.insert(addr, new_conn.clone())`, unless a still-open entry raced in (the new connection is then `close()`d) | `:396-406` |
| Existing flow uses it | The flow holds only its `AuthenticatedStream` = `join(RecvStream, SendStream)`. Each holds a `ConnectionRef` clone. The flow never holds the `TunnelPool` or a pool entry. | `:422-426`; `quinn/src/connection.rs:689-692` |
| Connector disappears | Nothing happens on the client until packets stop arriving. The idle deadline stays at last-rx + 30 s. Keep-alive PINGs go unanswered. | quinn-proto `:1903-1968` |
| New flow selects it | Returned because `close_reason().is_none()`. `open_bi` succeeds locally. `mark_direct_success()`. | `tunnel_pool.rs:379-381`; `transport.rs:119-121` |
| Handshake/open fails | `open_bi` does not fail. The `TunnelRequest` write is buffered. The response read pends. | `net_stack.rs:895-899` |
| Timeout occurs | 5 s `Elapsed`, warn, `saw_transport_failure`, next candidate. The stream is dropped (finish/stop queued, never ACKed). | `net_stack.rs:915-923` |
| Pool entry after timeout | **Unchanged**, still returned to the next flow | no code touches it |
| Next attempt | Same connection, another 5 s | L3: 9 timeouts in 25 s |
| Idle timeout expires | `state.error = Some(TimedOut)`. Pending stream reads fail with `connection lost`. The pool entry is still present but now has `close_reason()` = `Some`. | L3 13:48:39.772 |
| Pool entry finally disappears | On the **next** `get_or_connect` (`conns.remove`, `:383`), or when the whole pool is dropped by a map rebuild | `:383`; `daemon.rs:4083` |

## 7. Timeout configuration

| Timer | Value | Defined at | What it bounds |
|---|---|---|---|
| **`TUNNEL_HANDSHAKE_TIMEOUT`** (the observed 5 s) | 5 s | `client/src/net_stack.rs:36`, applied `:895` | Application code, **not quinn**. It bounds the `TunnelRequest` write plus the `TunnelResponse` read on an opened stream. It has nothing to do with QUIC connection liveness. |
| `DIRECT_TIMEOUT` | 2 s | `transport.rs:14`, `:116` | `get_or_connect` (incl. a fresh QUIC handshake) + `open_bi`. This is what a fresh connect to a dead peer hits (L3 13:48:42.215). |
| `RELAY_TIMEOUT` | 5 s | `transport.rs:15`, `:141` | The relay-path open. |
| Direct cooldown | 30 s × 2ⁿ, max 2 h | `transport.rs:16-17`, `:89-104` | Set only by an open/connect failure or the 2 s timeout. Never by the handshake timeout. |
| QUIC `max_idle_timeout` | 30 s negotiated (client default 30 s, connector default 30 s) | quinn-proto default `config/transport.rs:365`; client `tunnel_pool.rs:360` (not overridden); connector `quic_listener.rs:32` (default) | Dead-peer detection. It is the **only** thing that ends a stale connection without a rebuild. L3: ≈ 30.x s. |
| Client keep-alive | 10 s | `tunnel_pool.rs:361` | Keeps NAT/peer state alive. A healthy connector ACKs each PING, so a healthy connection receives at least one packet per ~10 s. It does **not** shorten dead-peer detection. |
| Connector `max_concurrent_bidi_streams` | 100 (default) | quinn default | See §9 (credit exhaustion). |
| Relay connection max age | 60 s | `relay_pool.rs:54` | Relay pool only. |

**Three different things:**

```text
5 s observed failure   = TUNNEL_HANDSHAKE_TIMEOUT, app code (net_stack.rs:36), per flow, per candidate
30 s stale lifetime    = QUIC max_idle_timeout (quinn default, negotiated), per connection
2 s fresh-connect cap  = DIRECT_TIMEOUT (transport.rs:14); the first fresh connect after the idle timeout hits it
```

Lowering the 5 s would not fix the root cause, which is reuse after a timeout. It would also break
connector-routed resources that need more than 5 s to answer (see §9).

## 8. Eviction and invalidation behaviour

| Action | Direct `TunnelPool` | Relay `RelayPool` |
|---|---|---|
| Insert | `get_or_connect` after a successful connect (`:405`) | `:210-216` |
| Select | `close_reason().is_none()` (`:380`) | `close_reason().is_none() && age < 60 s` (`:171-172`) |
| Remove | Only when the entry is already closed, at the next lookup (`:383`) | Closed, aged, or `open_bi` error (`:118`, `:176`), or missing CRL (`:162`, `close_cached`) |
| Close | Only the losing side of a connect race (`:401`) | Aged/closed entry (`:179`), CRL failure (`:222`), revocation monitor (`:264`), race (`:206`) |
| Invalidate on app-level timeout | **None** | **None** (the relay `open_bi` error path is the closest) |
| Replace | A new connect after removal, or a whole new pool at map rebuild | same |
| Expire | Only via quinn idle timeout | 60 s age, or the idle timeout |
| Discard after error | Never (an `open_bi` error is returned but the entry is kept) | Yes, on `open_bi` error |

## 9. Concurrency and safety analysis

**Multiplexing [src].** One `ClientTransport` (one `TunnelPool`, one connection) per connector per map
build. It is shared by **every flow to every resource** routed via that connector
(`daemon.rs:4094-4107`). Each flow has its own bidirectional stream on the shared connection. So one
pooled connection can carry many independent active flows.

**Remove from the pool vs close the connection [src].**
- *Remove from the pool:* `conns.remove(&addr)` drops the pool's `Connection` clone only. Flows that
  already hold streams keep their own `ConnectionRef`s (`SendStream::new(conn.clone(), …)`,
  `quinn/src/connection.rs:689-692`), so the connection and their streams continue exactly as before.
  If the peer is really dead, they fail at the idle timeout as they would today. If it is alive, they
  are unaffected. When the last ref drops, quinn `implicit_close`s it (`:927-940`). Cost: the next flow
  pays one new QUIC handshake (on LAN: ms; to a dead peer: the 2 s `DIRECT_TIMEOUT`, then cooldown).
- *Actively close:* `Connection::close()` closes the connection for **all** streams. Every other flow
  multiplexed on it is reset immediately, and the peer drops buffered data. Unsafe as a reaction to one
  flow's timeout. The earlier §4 sketch in [[Fix05-Client-Dataplane-Reliability]] proposed exactly this
  and is withdrawn.

**A handshake timeout is not proof of a dead connection [src].** The connector replies to a
`TunnelRequest` only after it finishes the downstream step:
- connector route: `TcpStream::connect(&target).await` with **no timeout**
  (`connector/src/device_tunnel.rs:496`). A blackholed or slow resource takes up to the kernel SYN
  timeout.
- shield route: `open_relay_session(...)` (`device_tunnel.rs:339`), then `event_rx.recv().await`
  (`connector/src/agent_tunnel.rs:143`). A slow shield can take any time.

So a perfectly healthy connector can produce `tunnel handshake timed out after 5s`. Any eviction must be
**gated on evidence that the peer is silent**, not on the timeout alone.

**Discriminator available today [src].** On a live peer, quinn ACKs ack-eliciting packets (the
`TunnelRequest` STREAM frame) within `max_ack_delay` (25 ms default), retransmitting on PTO. So during a
5 s wait a live connector produces received datagrams on that connection even if the resource never
answers. A dead one produces none. `Connection::stats().udp_rx.datagrams` is public
(`quinn/src/connection.rs:534`, `quinn-proto/src/connection/stats.rs:10-18,163-171`), and
`Connection::stable_id()` (`:582`) identifies the connection. The rule: if no datagram was received on
that connection between this flow's stream open and its timeout, the connection is dead. That is a
safe trigger. **[inferred until the §17 test proves it with real quinn endpoints.]**

**Races.**

| Scenario | Today | With "evict-if-same, no close" |
|---|---|---|
| Two new flows select the same pooled connection concurrently | Both get clones (lock held only for lookup) and both open streams | Same |
| One times out while another flow has a healthy stream on the same connection | Nothing happens | The pool reference is removed, the healthy flow keeps its `ConnectionRef` and continues (the evidence gate also means a live connection is not evicted at all) |
| Connection removed while another task is using it | Safe: users hold their own refs | Same |
| New connection created while the old one is being evicted | `get_or_connect` releases the lock during connect. After connect, a still-open existing entry wins and **the new healthy connection is `close()`d** (`:399-403`), so a stale entry can beat a fresh one | Eviction must compare `stable_id` (remove only if the entry is still the connection that stalled). Otherwise a late eviction could remove a freshly inserted healthy connection |
| Two flows both time out on the same stale connection | Both continue | First evict-if-same removes it, second is a no-op (id mismatch or absent) |

Locks and ownership: `tokio::sync::Mutex` around the pool `HashMap`, held only for lookup/insert,
never across `connect().await`. The `Arc<ClientTransport>` is held by the map and by in-flight relay
tasks (the `Vec` clone) **only during candidate selection**; after selection the flow keeps only the
stream. Atomics for the cooldown. The `watch` channel (Phase 2-A) is for the map only.

**Credit exhaustion edge [src, not observed].** Timed-out streams are never ACKed by a dead peer, and
stream credit comes back only through the peer's `MAX_STREAMS`. After 100 opens on a dead connection,
`open_bi` would **pend** instead of succeeding. The 2 s `DIRECT_TIMEOUT` then fires and cools down that
transport. It was not reached in any live window (≤ 10 opens).

## 10. Existing test coverage

| Test | File | Proves |
|---|---|---|
| `direct_success_skips_relay`, `direct_timeout_falls_back_to_relay`, `direct_connect_error_falls_back_to_relay`, `direct_connect_error_surfaces_when_no_relay`, `direct_authenticate_error_never_falls_back`, `direct_only_success` | `client/src/transport.rs:266-335` | Direct/relay fallback and error classification of `ClientTransport` with mock openers. Cooldown is **not** asserted. |
| `connection_failure_is_not_misclassified_as_authentication`, `malformed_handshake_json_is_not_transport_failure`, `disconnected_handshake_is_transport_failure`, `oversized_handshake_is_not_transport_failure`, `connectivity_io_kinds_…`, `unrelated_io_kinds_…` | `net_stack.rs:1047-1160` | Handshake error classification (resync or not) |
| `established_flow_survives_map_swap_that_removes_its_connector`, `connector_added_leaves_existing_flows_and_new_flow_uses_new_map`, `relay_mid_session_failure_reports_failed_and_resyncs`, half-close tests | `net_stack.rs:1263-1460` | Flow vs map swap, relay end, resync on mid-session failure (with `PipeOpener` duplex mocks) |
| `connector_removed_hot_applies_and_new_flows_cannot_select_it`, `restart_decision_*`, `connector_*_hot_applies*` | `daemon.rs:1847-2260` | Phase 2-A hot-apply (a new map means new pools) |
| `relay_connection_fails_closed_without_crl`, `…rejects_revoked_serial`, `…accepts_non_revoked…` | `relay_pool.rs:390-414` | Relay CRL gate |
| `tpm_backed_client_auth_completes_a_real_mtls_handshake` | `tunnel_pool.rs:652` | A real quinn mTLS handshake (TPM-gated, skips without a TPM) |

**Untested:**
- `TunnelPool::get_or_connect`: reuse, closed-entry removal, and the race path. There are no tests
  with real quinn endpoints except the TPM-gated one.
- The handshake-timeout arm `net_stack.rs:915-923`: no test drives an opener whose stream never
  answers.
- That a timeout leaves the pool entry and the cooldown untouched (the bug), and that
  `mark_direct_success` runs on a stale open.
- Dead-peer behaviour (idle-timeout expiry, `close_reason` transition).
- Evidence gating (a live but slow peer vs a dead peer).
- The partial-loss flow: a timeout on candidate 1, success on candidate 2, no resync.

## 11. Reproduction status

- **Live: already reproduced.** L3 (2026-10-06 13:48) is a clean natural reproduction: one stale
  connection, 9 × 5 s timeouts over 25 s, closed by the idle timeout at ≈ +30 s, then 2 s connect
  failures and cooldown.
- **Lab, deterministic, without changing behaviour:** use the case 5-B matters most for, **partial
  loss**. Take a connector-routed resource in an RN with **two** connectors and stop the preferred one.
  Candidate 2 succeeds, so no resync is signalled and no rebuild happens. Predicted [inferred from
  §5]: every new flow for about 30 s pays 5 s then succeeds via connector 2, then one 2 s penalty, then
  0 (cooldown). A 1 s probe would show about 25 × 5 s-slow `200`s. This is also the D5 partial-loss row
  that was not run in the empty-network acceptance.
- **Unit, deterministic:** feasible with real quinn on loopback. A server endpoint with a test cert
  (rcgen, same PKI shape as the TPM test `test_pki`, `tunnel_pool.rs:607`) sits behind a small UDP
  forwarder task that can be switched to **blackhole**. A short server-side `max_idle_timeout` (e.g.
  2 s) bounds test time, because the negotiated idle is the minimum, so no client change is needed.
  Steps: open a connection through `TunnelPool`, blackhole, `get_or_connect` again and assert the
  **same `stable_id`** (the bug), open a stream and send a request, assert no reply and `udp_rx`
  unchanged, wait for the idle timeout, assert the entry is removed on the next lookup. The smallest
  extra seam needed is none for observing, because `stable_id()` and `stats()` are public quinn APIs
  on the returned `Connection`.

## 12. Interaction with Fix 5-A, Phase 2-A and empty-network delivery

| Fix | Relation |
|---|---|
| Fix 5-A (`net_stack.rs` socket lifecycle) | Independent. 5-A decides what the **app socket** does once the relay task ends (a failed open becomes RST). 5-B is about **which transport/connection** the relay task gets and how long it waits. The timeout arm lives in `relay_tcp_to_quic` (candidate loop, pre-5-A code), not in the socket lifecycle 5-A changed. |
| Phase 2-A (hot-apply) | Every hot-apply builds new pools for all connectors, which masks 5-B whenever it fires. 5-B must not depend on or change the classifier/watch logic. |
| Empty-network delivery (controller) | Turns **total** loss into "first timeout, then resync, then map without the connector" (5 s). So in total loss 5-B only matters for the ≤ 5 flows already in flight. **Partial** loss gets no help from it (no resync is signalled when another candidate works), and connector freeze/partition (controller notices late) gets none either. That is where 5-B pays off. |
| Connector | No change needed. (Optional, separate: a graceful `endpoint.close()` on SIGTERM would remove the planned-stop case entirely, but not crash/partition. Out of scope.) |

```text
connector failure
  → existing flows: keep their own stream/ConnectionRef; end by their own lifecycle (data, or idle timeout)
  → new flow: candidate 1 = stale pooled conn → 5 s TUNNEL_HANDSHAKE_TIMEOUT   ← 5-B acts HERE (once)
      → evidence: no datagram received since this flow's open → evict pool ref (no close) + cooldown
  → next new flow: candidate 1 in cooldown (skipped fast) / or fresh connect (2 s) → next candidate or relay
  → resync / hot-apply / empty-network unchanged
```

## 13. Root cause

`TunnelPool` judges reusability only by `close_reason().is_none()`. After an ungraceful peer death
that stays `None` until quinn's 30 s idle timeout. The only failure signal the client gets in the
meantime, the application-level 5 s `TUNNEL_HANDSHAKE_TIMEOUT` in `relay_tcp_to_quic`, is **not fed
back** to `ClientTransport` or `TunnelPool`. And because `open_bi` succeeds locally, `ClientTransport`
records the dead path as a **success** (`mark_direct_success`), which also suppresses relay fallback.

## 14. Proposed Fix 5-B options

| # | Option | Effect | Safety | Verdict |
|---|---|---|---|---|
| A | **Evidence-gated eviction on handshake timeout.** On timeout, if the stalled stream's connection received **no datagram since this flow opened** (`stats().udp_rx.datagrams` unchanged, same `stable_id`): remove that entry from the pool **if it is still the same connection** (compare `stable_id`), **without `close()`**, and `mark_direct_failure()` on that `ClientTransport`. | Stops reuse after the **first** timeout. The next flow skips direct during cooldown (relay if configured, else next candidate), or reconnects fresh after cooldown. | Healthy flows untouched (no close). Slow resource or shield on a live connector is not evicted (ACKs observed). Races handled by `stable_id` compare. | **Recommended** |
| B | Pool-side liveness on reuse: per-connection last-rx watermark (sampled `stats()`), and on lookup refuse reuse if nothing was received for > keep-alive + margin | Detects about 12 s after death without any flow failing | Needs a sampler task per connection (holds a `Connection` clone, which changes lifetime like the relay CRL monitor), and lazy sampling has false positives (see note) | Optional follow-up, not alone |
| C | Evict and `close()` on every handshake timeout (the earlier sketch) | Fast | **Unsafe**: resets every multiplexed flow and evicts live connections on slow resources/shields | Rejected |
| D | Lower `TUNNEL_HANDSHAKE_TIMEOUT` and/or `max_idle_timeout` | Shorter windows | Breaks slow-but-valid resources. Idle 30 s → x only shrinks the window. Not root cause. | Rejected as the fix (could be tuned separately with evidence) |
| E | Count a handshake timeout as a transport failure that always triggers the early resync | More rebuilds | Rebuilds help only when the snapshot changed. Partial loss / freeze: no change, so no help. Churns all pools. | Rejected |
| F | Connector graceful close on SIGTERM | Planned stops close cleanly at once | Connector change, doesn't cover crash/partition | Separate, out of scope |

Note on B: the sampling has to be per flow (count at *this* flow's open) or a live connection whose
last ACK landed before another flow's sample looks silent. A per-connection sampler avoids that but
adds a task. That is why A (per-flow before/after) is preferred.

## 15. Recommended option

**A: evidence-gated, no-close, compare-and-evict + cooldown on that transport.** Concretely:

1. `TunnelPool::open_authenticated_stream` also returns a small `PathProbe { stable_id, rx_at_open }`
   for direct streams (relay streams: `None`).
2. `ClientTransport` gains `report_handshake_stall(&self, probe: &PathProbe)`. If
   `conn.stats().udp_rx.datagrams == rx_at_open`, then `TunnelPool::evict_if_same(addr, stable_id)`
   (no `close()`) and `mark_direct_failure()`. Otherwise do nothing: the peer is alive and only the
   resource or shield is slow.
3. `relay_tcp_to_quic`'s timeout arm (`net_stack.rs:915-923`) calls it for the transport it just tried
   (`transport` is in scope in the candidate loop). One call, no socket-lifecycle change.

Open decision (recommended default first):
- **Q1. API shape for the probe:** (a, default) a new `ClientTransport::open_authenticated_stream_probed()`
  returning `(AuthenticatedStream, Option<PathProbe>)`, keeping the existing method for other callers.
  (b) Change the existing return type (touches mocks in `transport.rs` and `net_stack.rs` tests).
- **Q2. Also evict on a transport-failure handshake error** (`Ok(Err(e))` with
  `is_transport_failure()`)? Default **no**: those already mean `state.error` is set, so
  `close_reason()` handles it.
- **Q3. Relay pool:** default **out of 5-B**. A relay-path stall means the relay is alive (it ACKs) and
  the connector or shield behind it is not, so evicting the relay connection would be wrong. Record the
  pre-existing `relay_pool.rs:176-180` close-on-age (kills flows multiplexed on an aged relay
  connection) as a separate finding.
- **Q4. Touching `net_stack.rs`:** 5-A froze the socket lifecycle. The one-call hook is in the
  relay-task candidate loop. Default: allow this single hook, with a test proving 5-A behaviour is
  unchanged. Alternative: Option B (pool-only, no `net_stack.rs` change), which has weaker detection.
  **Reviewer decision required.**

## 16. Risks

- **False eviction** if a live peer's ACKs are delayed more than 5 s (extreme loss). Impact: one extra
  QUIC handshake plus a 30 s cooldown on a working direct path, with relay fallback during the cooldown.
  Mitigation: the evidence gate. Cooldown back-off stays as today.
- **Cooldown on a shield route's only connector:** direct is skipped while cooling down, and if no relay
  is configured the flow fails fast instead of waiting 5 s. Same as today's behaviour after a connect
  failure. The cooldown expires (30 s first step) and `mark_direct_success` resets it.
- **`mark_direct_failure` semantics widen** from "couldn't open" to "opened, but peer silent". Log it
  distinctly.
- **Race with a concurrent fresh connect:** handled by `stable_id` compare. The existing "raced, close
  the new one" branch can still prefer a not-yet-detected stale entry. That is pre-existing; leave it,
  but cover it in a test.
- **Credit exhaustion** (§9) remains a theoretical alternative stall. Already bounded by the 2 s
  `DIRECT_TIMEOUT`.

## 17. Required tests

Unit (`cd client && cargo test`, report the delta):
1. **Bug pin (real quinn, loopback, blackhole forwarder):** after the peer goes silent, `get_or_connect`
   returns the same `stable_id`, `open_bi` succeeds, the request gets no reply, and `udp_rx` is unchanged.
2. **Evict on silent stall:** `report_handshake_stall` removes the entry, the next `get_or_connect` has a
   different `stable_id` (or fails within `DIRECT_TIMEOUT` if still blackholed), and the transport is
   in cooldown.
3. **No evict on live-but-slow peer:** the server accepts the stream and ACKs but never answers;
   `udp_rx` advances, the entry is kept, and there is no cooldown.
4. **Other flows survive eviction:** an established stream on the same connection keeps transferring
   bytes both ways after the eviction (no `close()`).
5. **Compare-and-evict race:** a stall report carrying an old `stable_id` does not remove a newer entry.
6. **net_stack timeout arm calls the hook** (`PipeOpener` that never answers, with a probe): one call,
   the candidate loop continues to the next transport, and the resync signal is unchanged.
7. **Partial loss in the candidate loop:** candidate 1 stalls, candidate 2 answers. The flow succeeds,
   candidate 1 is in cooldown, and the next flow does not wait 5 s on it.
8. All existing 5-A, Phase 2-A and empty-network client tests unchanged (149 baseline).
9. Mutation check: disable the hook and tests 2, 6 and 7 must fail.

## 18. Live acceptance plan

Topology: one RN with **two** connectors and a **connector-routed** resource (shield resources only
work through the shield's connector, so they can't show fallback). 1 s new-connection probe, client
journal, controller log.

| Row | Action | Pass |
|---|---|---|
| B1 partial loss (main) | Stop the preferred connector (ungraceful: `systemctl stop`, no close) | ≤ the flows already in flight pay 5 s. After the first `handshake timed out`, an evidence-gated eviction is logged and **0 further 5 s timeouts** on that connector until it returns. All probes `200` via connector 2. 0 tunnel restarts. |
| B2 baseline comparison | Same row on the current build (before 5-B) | Expect about 25 × 5 s-slow `200`s over ~30 s. Documents the delta. |
| B3 slow resource, live connector | Resource that blackholes SYNs (filtered port) behind a live connector | `handshake timed out` occurs but **no eviction / no cooldown** is logged (ACKs seen), and other resources on the same connector stay fast. |
| B4 shared connection survival | A long download (≥ 60 s) through connector 1, then trigger one stall on connector 1 (B3-style) | The download completes, no reset. |
| B5 total loss with empty-network (regression) | Stop the last connector | Same as the empty-network D1 (hot-apply ≤ 6 s), with ≤ 5 slow flows. |
| B6 freeze | `kill -STOP` connector 1 for 60 s, then `-CONT` | One 5 s, then fast (no repeat) while frozen. Recovery after `-CONT` once the cooldown expires or a hot-apply runs. |
| B7 soak | 30 min at rest, both connectors | 0 evictions, 0 cooldowns, 0 slow probes. |

## 19. Production files that would change (not changed)

- `client/src/tunnel_pool.rs`: `PathProbe` capture on stream open; `evict_if_same(addr, stable_id)` (no close).
- `client/src/transport.rs`: `report_handshake_stall` (evidence gate, evict, `mark_direct_failure`);
  probed open variant; `DirectOpener` extension (default no-op for mocks).
- `client/src/net_stack.rs`: **one call** in the `relay_tcp_to_quic` handshake-timeout arm (`:915-923`)
  only. Socket lifecycle (5-A) untouched. **Needs reviewer approval (Q4).**
- Not touched: `client/src/daemon.rs` / `runtime.rs` (Phase 2-A), controller (empty-network), connector,
  `relay_pool.rs` (Q3).

**Production code changed:** NONE.

---

## 20. Implementation (Option A + Q4), 2026-10-06

### Production files changed (only these three)

| File | Change |
|---|---|
| `client/src/tunnel_pool.rs` | `PathProbe { stable_id, rx_at_open }` (new pub struct). `TunnelPool::open_probed_stream(addr)`: the old `open_authenticated_stream` body, plus a probe taken from the pooled `Connection` right after `open_bi` (local) and before anything is written: `stable_id()` and `stats().udp_rx.datagrams`. `open_authenticated_stream` now delegates to it (same behaviour). `TunnelPool::evict_if_silent(addr, &probe) -> bool`: under the pool lock, **Gate 3** (pooled entry's `stable_id == probe.stable_id`, else no-op) then **Gate 2** (entry's `udp_rx.datagrams == probe.rx_at_open`, else no-op), then `conns.remove(&addr)`. **No `close()`.** `get_or_connect` is unchanged. |
| `client/src/transport.rs` | `DirectOpener` gains two **default** methods: `open_probed` (default: `open` + `None`) and `evict_if_silent` (default: `false`), so existing mocks are untouched. `TunnelPool` overrides both. `ClientTransport::open_authenticated_stream_probed()` is the old `open_authenticated_stream` body, returning `(stream, Option<PathProbe>)`: `Some` for a pooled direct stream, `None` for relay. Cooldown, relay fallback and error classification are unchanged. `ClientTransport::report_handshake_stall(Option<&PathProbe>) -> bool`: `None` returns false. Otherwise `direct.evict_if_silent(direct_addr, probe)`, and only if that evicted, the **existing** `mark_direct_failure()` plus a warn log. The probe-less `open_authenticated_stream` wrapper is now `#[cfg(test)]` because it has only test callers (removes a dead-code warning). |
| `client/src/net_stack.rs` | In `relay_tcp_to_quic`'s candidate loop only: the open call becomes `open_authenticated_stream_probed()` (to carry the probe), and the `TUNNEL_HANDSHAKE_TIMEOUT` arm gains **one call**, `transport.report_handshake_stall(probe.as_ref()).await`, before the existing `saw_transport_failure = true; continue`. **Gate 1** is structural: no other arm calls it. Socket lifecycle (Fix 5-A), the flow/accept code and resync signalling are untouched. |

Not touched: `daemon.rs`, `runtime.rs` (Phase 2-A), `relay_pool.rs` (incl. the separate §9 finding at
`:176-180`, still open), controller, connector, TUN/routes/nft, `Cargo.toml`.

New log line (live monitors should grep it):
`connector silent since stream open: stale pooled QUIC connection evicted (not closed), direct path cooling down`
(warn, `zecurity_client::transport`, field `direct_addr`).

### Deviations from §15

1. **`net_stack.rs` has two touched lines, not one.** The stall report needs the probe from this
   flow's open, so the existing open call was switched to the probed variant (Q1 option a), plus the
   one hook call. There is no other change in the loop.
2. **Gates 2 and 3 run inside `TunnelPool::evict_if_silent`, under the pool lock**, rather than in
   `ClientTransport`. `ClientTransport` never holds the `Connection`. Checking identity, the RX count
   and removing under one lock leaves no window for a replacement to slip in between the check and
   the remove.
3. **The net_stack tests use real time** (each waits the real 5 s once). Paused tokio time needs the
   `test-util` feature, i.e. a `Cargo.toml` change outside the approved scope.

### Tests added (+8; client suite 149 → 157, 0 failed)

Real quinn on loopback (`tunnel_pool.rs`, `mod stale_pool_tests`). The harness: rcgen PKI (workspace CA +
intermediate, SPIFFE client/connector leaves), a quinn server on 127.0.0.1 that echoes (or holds,
never answering, for a `HOLD` stream), **no server keep-alive** (like the connector), a server-side
`max_idle_timeout`, and a UDP forwarder between the real `TunnelPool` and the server that can be
switched to drop everything (an ungraceful connector death).

| # | Test | Proves |
|---|---|---|
| 1 | `stale_pooled_connection_is_reused_after_peer_goes_silent` | **Bug pin:** after silence, same `stable_id` reused, `open_bi` immediate, no answer, `udp_rx` unchanged, `close_reason` None until the (1.5 s here) idle timeout, entry removed only at the next lookup |
| 2 | `silent_stall_evicts_pool_entry_and_cools_down_direct_path` | Through `ClientTransport`: stall report evicts, entry gone, the connection is **not closed** (`close_reason` None), next open fails fast with `direct path is in cooldown`; a `None` probe never evicts |
| 3 | `live_connector_with_slow_resource_is_not_evicted` | **Safety gate:** the request is never answered but `udp_rx` **advances** (the server's real ACKs), so no eviction, no cooldown, and the next open reuses the same connection and works |
| 4 | `eviction_does_not_close_connection_and_existing_flow_survives` | Flow X and flow Y share one connection. Y's stall evicts it. After the path recovers, X still echoes both ways; new flows get a fresh `stable_id` |
| 5 | `late_stall_report_cannot_evict_replacement_connection` | Two stalled flows on the old connection. The first report evicts, a fresh connection is pooled, and the second (late) report is ignored. The replacement stays and works |

`net_stack.rs` tests (relay task with a `StallOpener` whose streams never answer):

| # | Test | Proves |
|---|---|---|
| 6 | `handshake_timeout_reports_stall_for_the_stalled_transport` | The timeout arm reports exactly once, with this transport's address and this stream's probe. The flow still fails after ≥ 5 s and the resync is still signalled |
| 6b | `handshake_io_failure_does_not_report_stall` | Gate 1: an I/O handshake failure does not report |
| 7 | `partial_loss_evicts_silent_connector_and_falls_through_to_healthy_one` | A silent, B healthy: flow 1 pays one timeout on A, A is reported/evicted, the flow opens via B; **no resync**; flow 2 goes straight to B (A not reopened, < 5 s); both flows relay through B |

### Gates (commands and results)

| Gate | Result |
|---|---|
| `cargo test` (client) | **157 passed, 0 failed** (baseline 149 at `091837d`; +8) |
| `cargo test stale_pool_tests` ×10 | 10/10 ok (5 passed each, ~3.3 s) |
| `cargo build --release` | ok, 0 warnings |
| `cargo clippy --all-targets` | per-file hit counts **identical to baseline**: 0 in `tunnel_pool.rs`/`transport.rs`/`net_stack.rs`. Two hits the change first introduced (dead `open_authenticated_stream`, test-helper type complexity) were fixed |
| `rustfmt --edition 2021 --check` | `net_stack.rs` 0 diffs (HEAD 0). `tunnel_pool.rs` 1 (HEAD 1, pre-existing indent at `:111`). `transport.rs` 4 (HEAD 4, pre-existing 2-space body). No new formatting drift; pre-existing drift not mass-reformatted |
| No `close()` added | `git diff` of the three files has no added `.close(` line |

### Mutation check (scratch copies, restored and `cmp`-identical, no mutation left in the tree)

| Mutation | Fails |
|---|---|
| M1: hook call removed from the timeout arm | 6, 7 (6b passes, as designed) |
| M2: `evict_if_silent` never removes | 2, 4, 5 (1 and 3 pass, as designed: bug pin and no-evict guard) |
| M3: RX-silence gate removed (evict on any timeout) | **3** (slow resource would be evicted) |
| M4: `stable_id` gate removed | **5** (late report evicts the replacement) |

### Confirmations

- **Pool eviction does NOT close the QUIC connection:** `evict_if_silent` only does `conns.remove`
  (test 2 asserts `close_reason().is_none()` after eviction).
- **Active flows are preserved:** test 4, a flow on the evicted connection keeps relaying both ways.
- **Slow-resource safety gate works:** test 3 uses real RX activity (the server's ACKs), not a mock
  flag. M3 proves the gate is what protects it.
- **Stable-ID race protection:** test 5. M4 proves the id gate is what protects it.
- **Partial loss stays a partial loss:** test 7, fallthrough to B, no resync, no restart.

### Live acceptance

**`live-accepted-with-caveat` (2026-10-06/07).** All rows B1–B7 ran, plus the B2 old-build baseline:
[[Fix05-Stale-QUIC-Pool-Live-Acceptance-2026-10-06]].

| Row | Result |
|---|---|
| B2 (old `e2e36c45`) | baseline: 25 × 5 s timeouts, stale connection 30.75 s (quinn idle), 5 `connection lost` |
| B1 (new `1f960fe2`) | PASS: 5 in-flight 5 s timeouts, 1 eviction, stale 5.19 s, then 0 further 5 s, 143/143 × 200 |
| B3 slow resource | PASS: 12 handshake timeouts, **0 evictions, 0 cooldowns**, other flows on the same connector fast |
| B4 shared connection | PASS: 1 eviction mid-download; ≥ 5.7 MB crossed the evicted connection afterwards; 200, full size, sha match, no reset |
| B5 total loss | PASS with caveat: 2 evictions (one per connector), hot-apply `reachable=0` 10.3 s after the stop, fast `000`, recovered; 10 slow flows vs §18 "≤ 5" (two connectors) |
| B6 freeze | PASS: 1 eviction, 0 repeat stalls while frozen, recovery on cooldown expiry |
| B7 30 min soak | PASS: 1785 probes, 0 slow, 0 evictions, 0 cooldowns, 0 restarts |

Caveats (record §15): B5 slow-flow count; one unexplained connector-side code-0 close during B4 (not the
download's connection); day 2 ran on a rebuilt lab. Found during the run, pre-existing and out of scope:
the client's smoltcp interface holds only 2 resource IPs (`IFACE_MAX_ADDR_COUNT=2`, `net_stack.rs:713-719`),
so further resource IPs are dropped silently. Not committed.
