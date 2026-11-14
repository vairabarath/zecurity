---
type: design-note
status: implemented
verified: 2026-11-14
created: 2026-11-03
updated: 2026-11-05
tags:
  - pending
  - connector
  - shield
  - proto
---

# PENDING-21: Per-tunnel flow control between Connector and Shield (FU-9)

**Status (verified against `main` 25a0cfb5, 2026-11-14): IMPLEMENTED.**
- **Merged:** PR #178 (`e07fe524`), with the gate-4 follow-up tests in PR #182 (`d73141fa`).
- **Correction:** §8.1 says `TunnelClose` is no longer sent or handled. That is wrong; only `TunnelData` is gone.
  `TunnelClose` still carries aborts through the connector's abort mailbox (`connector/src/agent_tunnel.rs`) and the
  shield handles it (`shield/src/control_stream.rs`), as §8.4 says.
- **Same error in code:** the proto comment at `proto/shield/v1/shield.proto` ("Neither side sends or handles these any
  more") repeats it. That comment needs a follow-up correction, since it is generated into the stubs.

**History:**
- Proposed 2026-11-03. The spike passed and Barath approved the final decisions (§7) on 2026-11-03.
- **Implemented and validated**, on branch `feat/fu9-per-tunnel-flow-control`, fast-forwarded and transplanted onto `origin/main` `07152f51` (2026-11-04).
- **Method (not a conventional rebase):**
  - the branch had no commits, so it was fast-forwarded to `origin/main`;
  - the two files main had also changed were first restored to their committed content (with backups);
  - then the uncommitted FU-9 versions of those files were three-way merged back in (`git merge-file`);
  - all other FU-9 changes and untracked files were carried over unchanged. The `device_tunnel.rs` conflict was resolved by keeping main's `shield_failure()` and `dial_host`, with FU-9's typed cases first: `SessionRevoked` → `ACCESS_DENIED`, tunnel limit → `SHIELD_TUNNEL_LIMIT`.
- Unit, integration and mutation tests (34 + 7 gate-4 mutants: all killed or documented equivalent), the network-namespace lab, and the live lab run (2026-11-04, L1–L9) pass for every FU-9 scenario (§11, §12).
- One client-side finding from the live run is tracked separately as **FU-16** (§12).

**Fixes:** FU-9 (`Sprint22/path.md`), and the second half of FU-12: new connections to a Shield timed out while a download ran.

---

## 1. Problem

A reader that is steadily slow loses its tunnel when the resource is behind a Shield (F-2, live E2E run 2026-10-29).
- **What happened:** a download read at 25 KB/s was ended by the Connector after 2–10 s in 3 of 3 runs, with `device stalled with its tunnel queue full`.
- **What survived:** 50 KB/s and faster, and a 5 s pause at 500 KB/s.
- **Impact:** a user on a slow link loses long downloads and streams from any Shield-protected resource.

## 2. How it worked before FU-9 (facts, 2026-11-03)

- **One stream per Shield.**
  - The Shield dials the Connector's `:9091` and opens a single bidirectional `ShieldService.Control` stream.
  - Every tunnel to that Shield rode it, keyed by `connection_id`, as `TunnelOpen`, `TunnelOpened`, `TunnelData` (≤16 KB chunks) and `TunnelClose`.
  - The same stream also carries resource instructions, health, the CRL and pings.
- **Only Shield-protected resources used it.** Unprotected resources use a direct socket from the Connector (`device_tunnel.rs`), so FU-9 does not apply to them.
- **Head-of-line blocking:**
  - The Connector's one read loop per Shield stream awaited `dispatch_data` inline, which waited for room in a 64-chunk device queue for up to `DEVICE_STALL_LIMIT = 2 s`.
  - While it waited, every other tunnel and every control message from that Shield waited too.
  - The Shield had the same pattern toward the resource (`RESOURCE_STALL_LIMIT = 2 s`).
- **Why the 2 s limit existed** (Sprint 20 Fix 14, F48):
  - dropping a chunk would corrupt the byte stream, so a full queue had to wait;
  - an unbounded wait would let one stuck reader freeze the whole Shield, so 2 s bounded that freeze.
- **HTTP/2 settings:** none were set, so hyper's defaults applied.
  - The Connector's server had a 1 MB stream window and a **1 MB connection window**, shared by every stream on the connection.
  - The Shield's client had hyper's client defaults: a 5 MB connection window.
- **UDP through a Shield did not keep datagram boundaries.**
  - The device side frames UDP as a 4-byte big-endian length followed by the datagram, as the direct path does (`device_tunnel.rs` `relay_udp`).
  - The Shield path forwarded the raw byte stream, length prefixes included, in chunks of up to 16 KB.
  - Each chunk was sent as one datagram, so length prefixes reached the resource and datagrams could be split or merged.
  - The client does not route UDP yet, so nothing in use was affected.

## 3. What a fix must give

1. **No cut for a slow reader that is making progress,** whatever its pace.
2. **A stuck tunnel never delays another tunnel or a control message** of the same Shield, in either direction.
3. **Byte streams are never corrupted** (F48 stays true for TCP).
4. **Bounded memory** per tunnel, per Shield and per Connector.
5. **Security invariants unchanged:**
   - FU-15 revocation cuts delivery at once;
   - Shield revoke and Shield Control-stream end close the Shield's tunnels;
   - a renewal keeps tunnels;
   - mTLS identity binding.

## 4. Options considered

- **A: credit windows on the shared Control stream.** Rejected. It would be our own flow-control protocol, with deadlock risk from lost credit updates and fairness only from a scheduler of our own. The Control stream's HTTP/2 window would still couple the tunnels.
- **B: one gRPC stream per tunnel (chosen).** HTTP/2 already gives per-stream flow control and interleaving.
- **C: raise or remove the 2 s limit.** Rejected. It only moves the cut, or brings back the unbounded freeze.

## 5. Spike results (2026-11-03)

**Setup:**
- A throwaway crate outside the repo, at `scratchpad/fu9-spike`.
- The same tonic 0.14.5, h2 0.4.13 and hyper 1.9.0 as production.
- mTLS terminated by tokio-rustls outside tonic on both ends, exactly as `agent_server.rs` `serve_on` and the Shield's `tls::build_connector_channel` do.
- Loopback only: one host, no added latency or loss.

### Experiment 1: HTTP/2 stream isolation — PASS with explicit windows

**Two tunnels, T1 stalled:**

| | Today's defaults, Shield → Connector | 1 MB / 16 MB, Shield → Connector | 1 MB / 16 MB, Connector → Shield |
|---|---|---|---|
| Data held by T1 before its sender blocks | 1.06 MB | 1.06 MB | 1.06 MB |
| T2 while T1 is stalled (needs ≥ 10 MB/s) | **0.4 MB/s, fail** | 1810 MB/s | 1387 MB/s |
| Control round-trip while T1 is stalled | 41.6 ms | 0.16 ms | 0.13 ms |
| New tunnel, open to first data | 163 ms | 2.4 ms | 2.5 ms |
| T1 after reading resumes | intact | intact | intact |

The pre-FU-9 defaults do not isolate a stall in the Shield → Connector direction. One stalled stream uses the server's whole 1 MB connection window.

**200 tunnels stalled at once:**

| Windows (per tunnel / per connection) | Result | Process memory growth |
|---|---|---|
| 1 MB / 16 MB | **Fails:** the 18th tunnel cannot open; Control and every tunnel freeze, in both directions | +27 MB |
| 1 MB / 1 GB | 200 stalled; Control 0.75 ms; a new tunnel moves about 1.8 GB/s | +320 MB |
| 256 KB / 256 MB | 200 stalled; Control 0.6 ms; a new tunnel moves about 1.8 GB/s | +51 MB |

A stalled stream keeps its full stream window charged to the connection window until its reader consumes the data. Isolation therefore holds only while:

`connection_window ≥ maximum_tunnels × per_tunnel_window + control_reserve`

### Stream-limit check — waits, it does not fail
- With the server limited to 4 concurrent streams, a 5th open got **no error after 3 s**; it was queued.
- It completed about 90 ms after another stream closed. Streams already open were unaffected.
- So the HTTP/2 limit must never be what refuses a tunnel. Admission has to be counted by the application and refused immediately.

### Experiment 2: certificate-renewal handover — PASS
- **Old tunnel survives:** with a tunnel on connection A (certificate A), the client opened connection B (certificate B) with a new Control stream and a new tunnel, then dropped every handle to A. The old tunnel kept flowing both ways, data intact (480 KB each way in 3 s), on connection A.
- **New tunnel uses the new connection:** the server saw the old tunnel on certificate A and the new one on certificate B.
- **Old connection released:** it closed within 4.7 ms of the old tunnel ending. No tasks were left over.
- **Constraint found:** when the tunnel task kept a copy of the old client handle, connection A was **never released** (still open 10 s after its last stream ended). A tunnel task must hold only its own stream.

### Limitations of the spike
- Loopback only: real round-trip times, loss and keepalive behaviour were not tested.
- The memory figures are rough process-memory (RSS) deltas, with both ends in one process.
- UDP was not tested separately.

## 6. Final architecture

- **Control stream (unchanged role):** resource snapshots and instructions, health, CRL, pings, re-enroll, and tunnel *coordination*:
  - the Connector sends `TunnelOpen`;
  - the Shield reports a tunnel it could not open with `TunnelOpened{ok: false, error}`.
- **New RPC `ShieldService.Tunnel(stream TunnelFrame) returns (stream TunnelFrame)`, one per tunnel:**
  - The Shield opens it after connecting to the resource.
  - Its first frame is `TunnelStart{connection_id}`.
  - Then come data frames, then `TunnelEnd{error}` (an empty `error` means a clean end) or the end of the stream.
  - For UDP, one data frame carries exactly one datagram.
- **Flow control is HTTP/2's own, per stream.** There is no credit protocol of ours.
  - A slow reader holds only its own stream's window. That pushes back to its sender: the resource socket through TCP at the Shield, or the device stream at the Connector.
  - The 2 s stall limits are removed. A slow reader is never cut; a stopped reader simply stops, as on a direct TCP connection.

## 7. Final decisions (approved 2026-11-03)

| # | Decision |
|---|---|
| D-1 | One dedicated gRPC stream per tunnel |
| D-2 | Temporary legacy compatibility only during a controlled migration, removed afterward. As implemented, there is **no** mixed-version data path (§8.7) |
| D-3 | 256 KB per-tunnel window, 256 MB per-connection window, **256 concurrent tunnels per Shield** |
| D-4 | Dedicated UDP streams; drop the newest datagram when a bounded queue is full |
| D-5 | No two-second stall cutoff for dedicated TCP tunnels |

**Mandatory with them:**
- HTTP/2 windows are set explicitly on both the Connector and the Shield, and adaptive window sizing is off on both.
- `connection_window ≥ maximum_tunnels × per_tunnel_window + control_reserve` holds. It is checked at compile time.
- The Shield counts active **and pending** tunnels and refuses a new one immediately at its limit. Every open has a bounded timeout; no open queues indefinitely.
- Tunnel tasks never keep a tonic client or channel handle. An old connection lives only as long as its streams need it.

### Resource budget
- **Per Shield connection:**
  - 256 tunnels × 256 KB = **64 MB per direction**, the most that stalled tunnels can hold unread;
  - the 256 MB connection window is a ceiling that keeps the invariant with ample reserve, **not preallocated memory**. h2 buffers only data that has actually arrived.
- **Per Connector:**
  - Nothing bounded the number of Shields attached at once, nor the device sessions routed to them. The Connector's Shield listener has no connection cap, and the device QUIC listener has no global session cap.
  - The per-Shield limit alone therefore gives no Connector-wide bound: N Shields could hold N × 64 MB.
  - FU-9 adds a **Connector-wide limit on Shield-routed tunnels** (active plus pending, across all Shields): `shield_tunnel_limit` in `connector.conf` (`SHIELD_TUNNEL_LIMIT`), **default 512** (configurable; decided 2026-11-04, lowered from the 1024 first implemented).
  - Worst case at the default is 512 × 256 KB = **128 MB per direction**, 256 MB across both directions, of unread data, plus small per-tunnel queues (§8.3).
  - Rationale for the default: it is two full Shields, or many lightly used ones. There are no measurements yet showing more is needed. An operator with more memory can raise it, and the cost is stated in the config documentation.
  - At the limit, a new tunnel is refused at once with `shield tunnel limit reached`.

## 8. Design details

### 8.1 Protocol (`proto/shield/v1/shield.proto`, additive)
- New `rpc Tunnel(stream TunnelFrame) returns (stream TunnelFrame)`.
- New messages:
  - `TunnelFrame { oneof frame { TunnelStart start = 1; bytes data = 2; TunnelEnd end = 3; } }`;
  - `TunnelStart { string connection_id = 1; }`;
  - `TunnelEnd { string error = 1; }`.
- `TunnelOpen` and `TunnelOpened` keep their fields and stay on Control. `TunnelOpened` now means only "this tunnel could not be opened" (`ok = false`).
- `TunnelData` (Control oneof field 10) is **no longer sent or handled**. *(Correction 2026-11-14: `TunnelClose` (field 11) **is** still sent for aborts and handled by the Shield, see §8.4.)* Their definitions stay, marked deprecated, because Sprint 22's proto rule is additive only. Deleting them (as `reserved`) is a follow-up decision.

### 8.2 Opening a tunnel
1. **Connector admission:** `AgentTunnelHub::open_relay_session` checks, under the hub lock:
   - the Shield is not revoked and is connected;
   - the Shield has fewer than 256 tunnels (active plus pending);
   - the Connector has fewer than `shield_tunnel_limit`.

   It then registers the tunnel as pending and sends `TunnelOpen` on the Shield's current Control stream. A refusal is immediate, with a clear error.
2. **Shield admission:**
   - the CRL check and the "protected resource only" check (D21-R D), as before;
   - its own count of active plus pending tunnels against 256, so it never exceeds the limit even if the Connector misbehaves.

   A refusal goes back as `TunnelOpened{ok: false}`.
3. **Resource connection:** the Shield connects to the resource (TCP: 5 s timeout; UDP: bind and connect). A failure goes back as `TunnelOpened{ok: false}`.
4. **Stream open:**
   - The Shield opens `Tunnel` on its **current** client, the newest connection, with `TunnelStart` as the first frame, within a 5 s timeout.
   - The client handle is a temporary clone, dropped as soon as the call returns.
   - If the Connector answers `Unimplemented` (an old Connector), the Shield reports `TunnelOpened{ok: false, "connector does not support dedicated tunnel streams"}`.
5. **Connector binding** (in the `Tunnel` handler):
   - the Shield is identified from the peer certificate (`extract_shield_identity`) and admitted as for Control: revocation list applied, the Connector serving, the Shield not revoked;
   - the first frame must arrive within 5 s and must be `TunnelStart`;
   - its `connection_id` must be a **pending** tunnel opened **to this Shield**.

   Errors:

   | Case | Error |
   |---|---|
   | Unknown or expired id | `NotFound` |
   | Another Shield's id | `PermissionDenied`; the other Shield's pending tunnel is not disturbed |
   | An id already bound | `AlreadyExists` |

   On success the id is bound once and the stream is handed to the waiting session.
6. **The Connector's open timeout is 10 s** (the Shield's 5 s connect plus 5 s stream open). Past it, the pending tunnel is removed, and a late stream for it gets `NotFound`.

### 8.3 Data
- **TCP:**
  - Each direction is a loop: read the source, then send a frame and await it.
  - The await is HTTP/2 backpressure, plus a 4-frame (≤64 KB) channel into tonic. Nothing grows without bound.
  - Ordering and integrity come from the single stream.
  - A clean end is `TunnelEnd{}` followed by the end of the stream:
    - the Shield half-closes the resource socket;
    - the Connector shuts the device stream down cleanly (TLS `close_notify`, F-7);
    - an error end aborts.
- **UDP:**
  - At the Connector, the device stream's `[u32 big-endian length][datagram]` frames (the same framing as the direct path, at most 65 535 bytes) become one data frame each, and back.
  - Queues are bounded at 32 datagrams and are fed with `try_send`.
  - When a queue is full, **the newest datagram is dropped**, counted and logged at most once a second. A UDP tunnel never blocks its reader.
  - The Shield keeps the 30 s idle timeout.

### 8.4 Ending a tunnel
- **FU-15** (access revoked mid-session): the Connector aborts the device stream at once and stops sending on the `Tunnel` stream. Two signals tell the Shield, and the Shield resets the resource connection on the first one:
  1. **`TunnelClose` on Control**, via the Shield's **abort mailbox** (below). This is the one that always gets through.
  2. **An error status on the tunnel's own stream.** It can leave only while that stream has send window: hyper sends nothing at all, not even an end, on a stream at zero window (`PipeToSendStream`, hyper 1.9).
- **Reliable abort** (FU-9 review, 2026-11-04):
  - **What was wrong:** the first implementation sent `TunnelClose` with a `try_send` on the Shield's 64-slot Control channel. If the Control loop was blocked (the Shield not reading, its queue and window full), the abort was **dropped**.
  - **The mailbox:** aborts now go to a per-Shield mailbox in the hub:
    - one entry per tunnel, deduplicated;
    - only tunnels that have a stream at the Shield (a pending one's late stream is refused instead);
    - posting never blocks and never fails.
  - **Delivery:** the Shield's Control loop drains it with priority, ahead of everything queued (`deliver_aborts`). Aborts a closing stream could not send go back, in order, for the Shield's next stream.
  - **Bounded:** at most `ABORT_MAILBOX_CAP` = 512 entries (2 × the per-Shield tunnel limit). While Control is not drained, no new tunnel can get a stream, so in practice it holds about the Shield's live tunnels. Past the cap it fails closed: the draining stream delivers what it holds, then ends with `Unavailable`, and the Shield closes every tunnel when Control ends (F44).
  - **No unbounded queue was added.**
- **Revocation racing an open or the first frames** (FU-9 review gate 4, 2026-11-04):
  - **Problem:** the session's access-revocation token (FU-15) and the tunnel's abort were separate. The revocation reached the tunnel only when the relay loop next ran: up to 10 s later during an open, and the device→shield task could forward one more frame (≤16 KB) in that gap.
  - **Fix** (existing `CancellationToken` tree, no new state):
    - device sessions open with `open_relay_session_revocable(…, revoked)`. The unlinked `open_relay_session` is test-only, so production code cannot open a tunnel without a revocation token;
    - the tunnel's abort is a **child** of the session's revocation token, so `cancel()` sets it synchronously;
    - the open watches the revocation and fails at once (`SessionRevoked` → `ACCESS_DENIED` to the device), removing its pending registration;
    - the device→shield task's stop token is a child of the abort, and the task checks it before it starts.
  - **Race-safety argument:**
    - the stream to the Shield (`ToShield`) reads the abort flag, one atomic load, before handing **each** frame to HTTP/2;
    - a frame is handed over only if the flag was clear at that read, so the read is the frame's linearization point;
    - so once `cancel()` has returned, no frame is handed over again, wherever the device→shield task was (reading, queueing or sending), and frames still queued are discarded;
    - frames handed over before the revocation can still be in flight.
- **What can still reach the resource after an abort:** only data already delivered to the Shield (at most the stream window, 256 KB, plus the resource socket's kernel buffers), and only until the Shield reads Control and processes the `TunnelClose`. Then it drops the stream's buffered data and resets the resource connection. If Control is not being read at all, nothing new is delivered either; the abort is held, not lost, until it is.
- **Shield revoked** (`revoke_shield`): every tunnel of that Shield is cancelled and every pending open fails. The `Tunnel` handler also refuses a revoked Shield.
- **The Shield's Control stream ends** (not by a renewal handover): the Connector cancels that stream's tunnels, as before (F44). The Shield closes all its tunnels when its Control session ends.
- **Connector shutdown or stop serving:** the Control streams close, which ends every tunnel and pending open through the rule above.
- **Certificate expiry:** a `Tunnel` stream ends when the certificate of the connection it runs on expires, matching the Control stream's expiry rule.
- **Dead peer:** HTTP/2 keepalive is on, with a ping every 30 s and a 20 s timeout, on both ends. A dead connection, including an old one kept for its tunnels after a renewal, fails all its streams. A slow reader is not a dead peer: pings are not flow-controlled.

### 8.5 Renewal
- The Shield builds a new client with the new certificate, opens a new Control stream, and makes the new client current for tunnel opens.
- It then drains the old Control stream to its end and drops the old client.
- Existing `Tunnel` streams keep running on the old connection, which closes after its last stream ends (Experiment 2).
- On the Connector, tunnels are bound to the Control stream that opened them. After a clean handover they move to the Shield's newer Control stream, as Fix 14 did for sessions, so a later end of the new stream still closes them.
- The handover no longer carries tunnel data, so Fix 14's early-event holding and paused tunnel upstream are removed.

### 8.6 HTTP/2 settings

| | Connector (server) | Shield (client) |
|---|---|---|
| Stream window | 256 KB | 256 KB |
| Connection window | 256 MB | 256 MB |
| Adaptive window | off | off |
| Max concurrent streams | 256 tunnels + 16 for Control and unary RPCs; never reached, because admission refuses first | — |
| Keepalive | 30 s interval, 20 s timeout | 30 s interval, 20 s timeout, while idle |

### 8.7 Migration (D-2)
- There is no mixed-version data path. The legacy `TunnelData` and `TunnelClose` handlers, the 2 s stall limits and the handover early-event machinery are removed in this change.
- Connector and Shield are upgraded together, as every lab host is internal and nothing is in production.
- A mismatched pair fails cleanly, never hangs:
  - **Old Shield, new Connector:** the old Shield answers with the legacy `TunnelOpened{ok: true}` on Control. The Connector refuses the tunnel with "shield does not support dedicated tunnel streams; upgrade the shield".
  - **New Shield, old Connector:** the old Connector has no `Tunnel` RPC. The Shield reports `TunnelOpened{ok: false}`, and the old Connector fails the open.

## 9. Tests

The list required by the approval is in §11 with the results.

## 10. Not changed

- The client, the relay, the controller and the Connector↔client QUIC path. Unprotected resources keep their direct path.
- Shield heartbeats go only to the Connector (`:9091`).
- Resource instructions stay on the Control stream.

## 11. Implementation status (2026-11-03)

**Implemented and merged:** branch `feat/fu9-per-tunnel-flow-control` (commit `3545017f`), merged in PR #178 as `e07fe524` on 2026-11-05. The design in §6–§8 is what was built.

### Files
- **Proto:** `proto/shield/v1/shield.proto`.
- **Connector:**
  - `connector/src/agent_tunnel.rs` (rewritten) and `agent_tunnel_tests.rs` (new);
  - `agent_server.rs`: the `Tunnel` handler, HTTP/2 settings, the Control loop, real-mTLS tests;
  - `device_tunnel.rs` (`SHIELD_TUNNEL_LIMIT` code), `config.rs` (`shield_tunnel_limit`), `main.rs`;
  - `fu9_lab.rs` (new, ignored lab harness).
- **Shield:**
  - `shield/src/tunnel.rs` (rewritten) and `tunnel_tests.rs` (new);
  - `tls.rs` (`tunnel_endpoint`), `control_stream.rs`, `enrollment_tests.rs` (fake server stub);
  - `fu9_lab.rs` (new, ignored lab harness), `main.rs`.

### Unit and integration tests
- **Connector:** 314 pass, 0 fail, 1 ignored (the lab harness), on the transplanted tree (FU-9's 303 plus main's 11 new tests). Integration tests: 4 pass, 1 ignored (existing).
  - **Hub, with a fake Shield:**
    - data both ways and the clean end (F-7);
    - half-close, error end, lost stream;
    - D-5: a slow device over 30 s, a device stalled 300 s, no head-of-line blocking;
    - UDP boundaries, drop-newest, oversized frame;
    - the 257th tunnel refused at once (pending opens counted), the Connector-wide limit, 400 simultaneous opens cannot pass 256;
    - open timeout and late stream; binding (unknown, other Shield, reuse); `TunnelOpened` failure, legacy `ok = true`, other Shield;
    - FU-15 abort, including with the Shield not reading;
    - Shield revoke (active and pending), Control-stream end, clean handover keeps tunnels, F45;
    - certificate expiry; a dropped session frees its place.
  - **Real tonic over mTLS:**
    - a stalled tunnel in each direction does not hold up another tunnel, Control, or a new tunnel, and holds less than 600 KB (the 256 KB window is in effect);
    - 200 tunnels stalled, then Control and a new tunnel still work;
    - binding over mTLS: another Shield's certificate → `PermissionDenied`, unknown → `NotFound`, second stream → `AlreadyExists`, a data first frame → `InvalidArgument`, no first frame → `DeadlineExceeded`;
    - refused while the Connector is not serving;
    - revocation ends the live tunnel stream with an error and refuses new ones;
    - Control handover keeps the tunnel.
- **Connector, reliable abort and limit (2026-11-04):**
  - Control channel full → the abort is still posted and the tunnel ends at once;
  - only tunnels with a stream post an abort, once;
  - the mailbox is bounded and its overflow is reported once;
  - unsent aborts are kept in order and delivered by the next stream;
  - **over the real server:** a Control stream congested with ~12 MB of unread snapshots, then 100 tunnels revoked at once → all 100 sessions end at once, and all 100 `TunnelClose` arrive once the Shield reads again;
  - mailbox overflow → every held abort delivered, then Control ends with `Unavailable`;
  - `shield_tunnel_limit` defaults to 512 and is configurable.
- **Connector, gate-4 race (2026-11-04):**
  - a revocation while opening fails the open within 100 ms, cleans up the pending tunnel, and refuses the late stream (`NotFound`);
  - a session revoked before the open sends no `TunnelOpen`;
  - a revocation before the first frame, with the device's data already written, forwards nothing (50 rounds);
  - **real server:** a revocation while the Shield is connecting to the resource refuses its late stream;
  - **real server, stress:** 100 rounds of a device sending continuously, revoked at a varying moment, with marker bytes written after `cancel()` returned. No marker byte reached the Shield's stream in any round; data flowed before the cut in some rounds (the race is exercised); every registration was cleaned up; one `TunnelClose` per revoked tunnel.
  - **What the tests observe:** in the connector crate the farthest observable point is the Shield's received stream, and everything on it is written to the resource. The Shield resetting the resource on abort is covered by the shield tests.
- **Shield:** 88 pass, 0 fail, 3 ignored (2 existing kernel tests, the lab harness). Against a fake Connector (real tonic, counting connections):
  - relay both ways with clean ends;
  - a slow or paused (3 s) resource gets every byte;
  - no head-of-line blocking; the client windows isolate 300 stalled streams;
  - the 257th tunnel refused at once (pending counted, places freed);
  - stream-open timeout; an old Connector gets a clear refusal; resource connect failure; duplicate id;
  - a Control abort and a Connector error each reset the resource (RST);
  - `close_all`;
  - **renewal: the old connection lives only as long as its tunnels** (the spike's retention trap, through the production code);
  - UDP boundaries and drop-newest.
- **Controller:** `go build ./...` and `go vet` pass (only the generated stubs changed).

### Mutation tests (34 mutants, all killed)
- **Connector:**
  - binding: Shield check, single use, `TunnelOpened` from another Shield;
  - limits: per Shield (off by one), per Connector;
  - revoke and Control-stream end not cancelling;
  - FU-15 cancel ignored; no Control abort; the stream not ended on abort;
  - open timeout (first survived: a paused clock fires any timeout, so the test now also checks *when*);
  - guard not freeing the place;
  - UDP blocking instead of dropping;
  - no certificate expiry (killed by timeout);
  - server stream window and connection window at their defaults;
  - `Tunnel` ignoring the serving gate.
- **Shield:**
  - limit off by one;
  - a tunnel keeping the client handle;
  - abort sends a FIN, not an RST;
  - Control abort ignored;
  - UDP blocking;
  - client stream and connection windows at their defaults;
  - `close_all` a no-op.
- **Reliable abort and the 512 default (added 2026-11-04, 9 mutants, all killed):**
  - the first implementation's droppable `try_send` restored (killed by both the hub test and the congested-Control test);
  - the mailbox not storing; the Control loop never draining;
  - overflow not ending Control;
  - pending tunnels posting; no de-duplication;
  - unsent aborts dropped instead of put back;
  - posting without waking the drainer;
  - the default back to 1024.
- **Gate-4 race fix (2026-11-04, 7 mutants):**
  - **Killed:**
    - G1: the tunnel abort not linked to the revocation. After the transplant, the stress test alone missed it in one run, so its detection was timing-dependent. A deterministic test now kills it every time (5 of 5): with no relay loop running, `cancel()` must end the stream to the Shield at once;
    - G2: the open ignoring a revocation;
    - G4: the per-frame gate removed;
    - G7: device sessions using the unlinked open, a **compile error** in the production build because the unlinked open is test-only.
  - **Equivalent** (no observable difference, documented): G3 and G5. The explicit pre-checks are redundant with the `biased` selects, which already take the revocation before the `TunnelOpen` or the first read.
  - **Survives by design:** G6, the device→shield task not linked to the abort. The per-frame gate still blocks every post-revocation frame (proven by G4 and G1); G6 only removes defence in depth.
- All 34 earlier mutants were run again on the final code: all killed. C9's target moved into the new gate and was re-targeted there.

### Network lab (rootless network namespaces)
**Setup:**
- Connector side and Shield side in two network namespaces joined by a veth pair, with `tc netem` delay on both ends.
- Production code on both sides: the Connector's mTLS server, `Control` and `Tunnel` handlers and tunnel hub; the Shield's mTLS channel and tunnel module.
- Simulated: the devices (in memory on the Connector side), the resources (on the Shield side's loopback), and a reduced Shield Control loop that performs the renewal handover.
- Harnesses: `connector/src/fu9_lab.rs` and `shield/src/fu9_lab.rs` (ignored tests).
- **Lost:** the ~20-line `unshare`/`ip`/`tc` wrapper script and the run logs. They were in the session scratchpad, which a reboot on 2026-11-04 wiped; the figures below were recorded before that.

| Scenario | 20 ms RTT | 100 ms RTT |
|---|---|---|
| One tunnel, 32 MB bulk | 9.75 MB/s, SHA-256 ok (ceiling 256 KB / RTT ≈ 12.8 MB/s) | 1.95 MB/s, SHA-256 ok (ceiling ≈ 2.56 MB/s) |
| Slow reader 25 KB/s × 60 s (1.5 MB) | complete, SHA-256 ok, clean end, 60.8 s | complete, SHA-256 ok, clean end, 61.0 s |
| One tunnel stalled; another moves 8 MB | 8.88 MB/s, SHA-256 ok | 1.85 MB/s, SHA-256 ok |
| Control round trip during the stall (worst of 5) | 20.8 ms (base 20.6) | 224 ms (base 100.6; the link carried the 8 MB transfer just before) |
| New tunnel during the stall (open + 64 KB) | 123.5 ms | 442 ms |
| Stalled tunnel resumed | 64 MB complete, SHA-256 ok, not cut | same |
| Renewal during the slow download | old tunnel finished on the old connection; new tunnel (1 MB) SHA-256 ok; Control on the new stream 20.7 ms; old connection closed once the old tunnel ended (2 connections → 1) | same (100.8 ms) |
| UDP saturation (5000 × 1 KB, reader paused 2 s) | 536 delivered, in order, the newest dropped; Control 20.8 ms during it; ping → pong after | 286 delivered, in order; Control 100.8 ms; ping → pong |
| Revocation during a 200 KB/s download | device saw the end 424 ms later; no clean end; resource connection reset at the Shield; new tunnel refused | 436 ms; same |

**About the revocation row:**
- The device read 64 KB after the revocation in both runs. That is exactly the in-memory device pipe's buffer, which the simulated device drained.
- An in-memory pipe cannot be aborted. A real QUIC device stream is reset (FU-15), which discards it.

### Limitations
- **The namespace lab** used emulated latency only (no loss, no bandwidth cap). The full product path was exercised in the live run (§12).
- **Dead-peer keepalive** (HTTP/2 ping 30 s / 20 s) is configured on both ends but not exercised by a test.
- **Throughput:** at most stream window ÷ RTT per tunnel, as measured. Raising it needs a larger window and a matching memory budget (§7); adaptive windows stay off.
- **Memory:** the per-direction worst case is 64 MB per Shield connection and 128 MB per Connector at the default `shield_tunnel_limit` (512). These are budget figures; process memory was measured only in the spike (§5).
- **Proto cleanup:** `TunnelData` and the old meaning of `TunnelOpened{ok: true}` remain in the proto as deprecated. Removing them as `reserved` is a follow-up decision (Sprint 22's proto rule is additive only).
- **Mixed versions** fail cleanly but are not supported (§8.7). Connector and Shield must be upgraded together.
- **Follow-up tests for the gate-4 linkage** (deferred at review, 2026-11-05; **added 2026-11-05**, branch `test/fu9-gate4-linkage`). They extend the deterministic G1 test, which proves that `cancel()` closes the gate to the Shield without the relay loop running:
  1. `agent_tunnel::tests::after_the_abort_the_stream_to_the_shield_ends`: after the initial `Err(Aborted)`, every later poll is `None`.
  2. `agent_tunnel::tests::a_frame_queued_before_the_revocation_is_discarded`: a data frame queued before the cancel is discarded, not sent. Control: without the cancel the same frame is delivered.
  3. `agent_server::shield_server_tls_tests::a_revocation_ends_the_shields_stream_with_no_relay_loop`: over the real server and HTTP/2 with no relay loop, the Shield's tunnel stream ends `Aborted` after the revocation. Control: it stays open before.

  Mutants, each caught:
  - `done` not set after the abort: caught by tests 1 and 2.
  - Queued frames handed out before the gate: caught by test 2.
  - The tunnel abort not linked to the revocation (G1): caught by tests 1, 2 and 3, and by the original G1 test.

  `cd connector && cargo test`: 317 passed, 0 failed, 1 ignored (the FU-9 lab harness).

## 12. Live lab validation (2026-11-04)

**Record:** [[Sprint22/Live-Run-FU9-2026-11-04]], with commands, timings and evidence. Summary:

**Setup:**
- Controller and client (real QUIC client, real `curl`) on the PC.
- Connector on inkyank (.75), Shield and HTTP test resource on manoj (.53).
- 40 ms round trip between Connector and Shield (`tc netem` on manoj toward inkyank only).
- Shield certificates of 15 min, so two renewals happened during the run.
- Branch builds: connector `16144cf7bba50db1`, shield `d54773a6803e5602`, client `e1bde368577e3bfd`.

### Verified
| # | Case | Result |
|---|---|---|
| L1 | 25 KB/s reader | 1.5 MB in 60 s and 20 MB in 819 s (13.7 min), SHA-256 ok, never cut; no stall line on the connector. Before FU-9: cut after 2–10 s (F-2) |
| L2 | One tunnel stopped 68 s (SIGSTOP) | another moved 60 MB at 4.84 MB/s, SHA-256 ok; the stopped one resumed and completed intact |
| L3 | FU-12 probe, one new request a second | 782 of 782 OK; p50 0.18 s, p99 0.26 s, max 0.46 s; including across both renewals |
| L4 | Control while a tunnel is stalled | a new protected resource active 0.21 s after the request |
| L5 | Renewal during the 20 MB slow download | handover logged with `tunnels=1`; two connections, then the old one closed; the download completed intact |
| L6 | Throughput of one tunnel | 4.91 and 4.99 MB/s, about 77% of the 6.4 MB/s ceiling (256 KB per 40 ms) |
| L8 | Shield revoked mid-download | the Connector ended the Shield's tunnels 6 ms after the revocation; new requests no longer reach the resource |
| L9 | Delay removed | 60 MB through the connector route at 28 MB/s, SHA-256 ok |

### Inferred, not observed
**L5:**
- The old connection closed between 11:09:23 and 11:09:33, while the download ran until 11:13:05 and completed intact.
- Inferred: the Shield had sent the last byte around 11:09:28, ending the old tunnel stream and so the connection, and the remaining ~5.5 MB were in client-side buffers.
- **Not observed:** the exact time the old tunnel stream ended. Neither side logs a tunnel stream's end at info level.
- The old connection eventually closed, but its cleanup is not fully verified.

### Not covered by the live run
- **The reliable-abort change and the 512 default came after the live run** (2026-11-04, from the final review). They were validated by unit, real-server integration and mutation tests, not on the lab hosts. The live builds used the earlier `try_send` abort; L7 and L8 did not congest Control, so their results are unaffected.

### Untested
- **Certificate expiry:** a tunnel's end at its connection certificate's expiry. L5's tunnel ended first. *Tested live 2026-11-05 (LT-5): pass; the tunnel kept across a renewal ended ~5 s after the old certificate's recorded expiry.*
- **Dead-peer keepalive:** the HTTP/2 ping. *Tested live 2026-11-05 (LT-1): detection passes (shield +39–46 s, connector +51 s); the failed tunnel's device stream ends gracefully instead of with a reset (FU-17).*
- **UDP through a real client:** the client does not route UDP. UDP was exercised only by the namespace harness and unit tests. *Still untested.*
- **Mixed versions.** *Tested live 2026-11-05 (LT-6): a pre-FU-9 shield gets a clear refusal on the connector, and the client errors in 10 ms with no hang.*

Record: [[Sprint22/Live-Run-Gaps-2026-11-05]].

### Separate finding: FU-16 (not FU-9)
**L7, a member removed mid-download, and L8:**
- curl kept receiving for 2.3–3.2 s (0.84–1.10 MB) after the cut.
- **Isolation:** two runs on a connector-route resource (no FU-9 code in the path) gave the same tail as two shield-route runs.
- **Connector side:** in every run the Connector aborted the session at once (logged), and this PC's UDP receive counter shows no Connector traffic after the cut.
- **Inference:** the evidence points to data already held by the client being handed to the app before the reset. FU-15 measured 0.52 s / 0.18 MB before FU-8's immediate hot-remove existed.
- The root cause is **not proven**. It is tracked as **FU-16** in `Sprint22/path.md`.

