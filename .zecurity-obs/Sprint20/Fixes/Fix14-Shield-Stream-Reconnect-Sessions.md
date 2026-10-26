---
title: Sprint 20 Fix 14 — shield Control stream reconnect freezes open sessions (F44, F45), tunnel data dropped under backpressure (F48) + live-run follow-ups (F46, F47)
date: 2026-10-26
type: fix note (investigation + proposed design; not implemented)
status: IMPLEMENTED locally (2026-10-26) — D-1 = B as a renewal handover + expiry rule; F45, F48 fixed; unit tests pass; live acceptance pending; deployment order connectors before shields (accepted)
owner: Sathiya (M1)
findings: F44, F45, F46, F47, F48 (Sprint20/path.md → Post-Sprint Fixes), found in the certificate-audit live run, [[Sprint21/Certificate-Audit-Live-Run-Sheet-2026-10-26]] (observations O1, O3, O4)
---

# Fix 14 — what the certificate-audit live run turned up

The certificate-audit fixes (bugs 1–10, PR #133) all passed live on `fixed-pendings` `3b1f8511`. These are separate flaws found along the way. F44 is the one that matters: users notice it. F45 is the same code path, found by reading it. F46 and F47 are low.

None of them weakens revocation: a revoked shield or connector is still cut off (stages H and I passed).

## F44 — Medium: a shield Control stream reconnect freezes every open session through that shield

**Problem.** When a shield's Control stream ends and reconnects, every client session through that shield stops moving data. The client gets no data and no close; it hangs until its own timeout. New sessions work at once. A routine cert renewal always causes this, because the shield reconnects to present its new certificate. A network blip or connector restart causes it too.

**Evidence (live, 2026-10-26).**
- 05:50:05.63 shield `cert renewed — reconnecting Control stream`; 05:50:07.13 new stream established.
- The open session's last echo was at 05:50:06; the next `recv` timed out at 05:50:11 (`session-ended-at-shield-renewal.log`). No EOF, no reset.
- The connector logged no tunnel close for it.
- A new connection right after worked.

**Impact.** Production lifetimes: shield cert 168 h, renewal window 48 h, so every shield renews about every 5 days and drops all its sessions each time (SSH, RDP, DB connections hang rather than close). Any reconnect does the same. In the lab with 15 min certs it happens every 9 minutes.

**Root cause.** Tunnels are multiplexed on the shield's Control stream (`TunnelOpen` / `TunnelData` / `TunnelClose`), but session state is tied to one stream, and nothing ends it when that stream ends:
1. **Connector** (`connector/src/agent_tunnel.rs`):
   - `unregister_shield` (line ~83), called when the stream ends (`agent_server.rs` ~1121), removes only `shield_txs[shield_id]`. The shield's entries in `relay_sessions` stay. Their event channel never gets `Close`, so the client side never sees an end.
   - `RelaySession` keeps the `shield_tx` it captured at open time (line ~226), i.e. the dead stream's sender. Client → shield data goes nowhere.
   - Compare `revoke_shield` (bug 3), which does end the shield's sessions. That's why revocation (stage H) closes sessions cleanly and a reconnect doesn't.
2. **Shield** (`shield/src/control_stream.rs`):
   - every stream gets a fresh `TunnelHub` (`let tunnel_hub = tunnel::new_hub()`, line ~220 on `fixed-pendings`). The old hub's tunnel tasks (`shield/src/tunnel.rs`) keep their TCP sockets to the resource until the resource sends something, then exit on the failed upstream send. Leaked sockets and tasks, no `TunnelClose`.

**Age.** On `main`, `fixed-pendings` and `sprint20/fix13-revocation-low-findings`. Introduced with streaming control (`372744dd`, 2026-04-22) and the device tunnel (`d935844b`, 2026-05-02). Not a regression from the certificate-audit fixes.

### D-1 (your call): end the sessions cleanly, or keep them across the reconnect?

| Option | What changes | Effect | Size |
|---|---|---|---|
| **A — end cleanly (recommended now)** | Connector: when a shield's stream ends, close that **stream's** relay sessions (like `revoke_shield`, without the revoked mark), so the client sees a close at once. Shield: drop the old hub when its stream ends and close its resource sockets. | Sessions still break on renewal, but fail fast and visibly (apps reconnect) instead of hanging. No leaks. | Small; connector + shield only, no proto change |
| B — keep sessions across the reconnect | Shield keeps one `TunnelHub` across reconnects and rebinds its upstream sender; connector looks up the shield's current sender per send (not one captured at open time) and keeps sessions for a grace period while the shield reconnects. | Renewal becomes invisible to users. | Large; overlaps open design decision **PENDING-18** (sessions are cut, not migrated) — needs a Decision Record entry first |
| C — renew without reconnecting | Shield keeps the running stream after renewal; the new cert is used from the next connect. | No session impact from renewal (blips still hit, so A is still needed). But a stream stays authenticated by a cert that may expire under it — a security-policy choice. | Small, but policy |

Recommendation at the time: A now, B later under PENDING-18. C only if the Decision Record accepts streams outliving their certificate.

**Decision D-1 (user, 2026-10-26): B, with an expiry rule.**
- If the shield **renewed before its certificate expired**, its open sessions are kept across the reconnect.
- If the certificate **expired** (no renewal in time), the sessions do **not** continue: they end.

Scope note: this keeps sessions across a reconnect **to the same connector** only. A shield failing over to another connector still cuts its sessions; that is PENDING-18 (migration), unchanged. Revocation (bug 3) still ends everything at once, unchanged.

### Design for B (proposed, for review before code)

**The rule as code.** A shield's sessions survive a stream end only while the shield's identity stays continuously valid:
1. Each registered stream records its certificate's `not_after` (`VerifiedShieldIdentity.cert_not_after_unix`, already parsed at accept, `agent_server.rs` ~943).
2. When a stream ends, its sessions are **held**, not closed, until the earlier of:
   - the held stream's `not_after` (the certificate expired → **end them**: the expiry rule);
   - a grace period `SHIELD_SESSION_RESUME_GRACE` (default 15 s; reconnect after renewal took ~1.5 s live) → end them (the shield didn't come back);
   - the shield being revoked → end them now (existing `revoke_shield`).
3. A new stream from the same shield, **on this connector**, with a valid certificate whose `not_after` is later than the held one (i.e. a renewed cert) and arriving before the held cert expired, **adopts** the held sessions. A reconnect with the *same* cert (network blip) also adopts them while that cert is still valid.
4. A stream whose certificate reaches `not_after` while still open is closed by the connector, and its sessions end. Today nothing does this; a long-lived stream can outlive its cert. This makes "don't continue if the cert is expired" hold even without a reconnect.

**Connector** (`connector/src/agent_tunnel.rs`, `agent_server.rs`):
- `HubState.shield_txs` becomes per-stream: `shield_id → (stream_id, sender, cert_not_after)`. Unregister removes the entry only if `stream_id` matches (this also fixes **F45**).
- `RelaySession` no longer keeps the sender captured at open time. Device → shield data looks up the shield's **current** sender for each chunk (or waits on a `watch` for the next one). While the shield is between streams, the device side is back-pressured (bounded wait, then the session ends at the hold deadline).
- On stream end: mark the shield's sessions held with the deadline above, and start one timer per shield. On adopt: clear the hold. At the deadline: close them like `revoke_shield` does (`RelayEvent::Close`), so the client sees a close, never a hang.
- On stream registration: close any stream whose cert expired (rule 4), using a timer at `cert_not_after_unix`.

**Shield** (`shield/src/control_stream.rs`, `shield/src/tunnel.rs`):
- One `TunnelHub` for the process (created in `reconnect_loop`'s caller), not per stream.
- The hub owns a swappable upstream sender. Tunnel read tasks send through the hub's current sender; between streams they wait (bounded by the same grace), then close.
- On every new stream, the hub switches to the new stream's sender. Tunnels keep their `connection_id`; the connector routes by `connection_id`, so **no proto change** is needed.
- If the shield cannot reconnect before its cert expires, or the grace passes, it closes its tunnels and their resource sockets (no more leaks).
- Failover to a **different** connector: close tunnels (PENDING-18 unchanged).

**Not changed:** proto, schema, controller, client. Revocation paths. ACL enforcement: carried sessions stay subject to ACL-diff teardown and fail-closed as today.

> **Correction (2026-10-26, before any code): the "hold and adopt" design above loses data.** Each Control stream owns in-flight queues: on the connector the stream's `tunnel_tx`/`tunnel_rx` (64) and `out_tx` (32), and on the shield `run_once`'s `out_tx` (32) plus whatever h2 received but the shield didn't read. When a stream ends, those messages are dropped. A session adopted by the next stream then has a hole in its byte stream. SSH or TLS break confusingly, and a plaintext protocol (DB wire, HTTP) can carry wrong data silently. That is worse than F44's hang. So:
> - **"No proto change" holds only for a renewal handover**, not for surviving an unclean stream end (blip, crash, connector restart). Surviving those needs per-tunnel sequence numbers and acks with replay buffers, i.e. new proto fields, which this sprint's proto rule (KI-2 option A only) does not allow without an exception.
> - **Safe B without a proto change = handover on renewal only:** the shield opens the new stream (new cert) **before** closing the old one. The connector registers it per `stream_id` (F45). Each side finishes the old stream in order (flush, half-close, read to EOF), and buffers per `connection_id` any tunnel traffic that arrives on the new stream until the old stream's EOF. If the cut-over doesn't finish within the grace, the sessions close. **Any unclean stream end closes the sessions cleanly** (option A's behaviour).
> - Live acceptance check 3 (a `kill -STOP` blip survives) applies only to the proto variant.
> - Whichever variant is chosen must not build on the F48 paths below.
>
> Awaiting the user's choice between handover-only B and B with additive `seq`/`ack` fields.

**Decision Record.** B touches session lifetime across a shield reconnect, next to PENDING-18. This decision (D-1 above) is the user's; add it to the Decision Record as a short entry before merging: "a shield session survives a Control stream reconnect to the same connector while the shield's certificate stays valid; it ends at certificate expiry, revocation, failover or a 15 s grace."

### Acceptance for B
- **Unit, connector:**
  - stream 1 ends, stream 2 (renewed cert) registers within the grace → sessions are adopted, data flows both ways, no `Close`;
  - stream 1 ends, no new stream within the grace → `Close` on every held session;
  - stream 1 ends, its cert expires before stream 2 arrives → sessions end at expiry, and stream 2 does **not** adopt them;
  - an open stream reaching its cert's `not_after` is closed and its sessions end;
  - revocation while held → ended at once, and stream 2 is refused;
  - F45: stream 2 registers before stream 1's cleanup → stream 2 stays registered.
- **Unit, shield:** the hub survives a reconnect and switches its upstream sender; with no stream within the grace, tunnels close and their resource sockets close.
- **Live (lab, 15 min certs):**
  1. a long session through a shield **survives** a shield renewal (no gap beyond the reconnect, keeps echoing);
  2. stop the shield's connector access past its cert's expiry (e.g. stop the connector until the shield cert expires, then restart) → the session **ends**, and is not resumed;
  3. a network blip shorter than the grace (`kill -STOP` the shield ~5 s, then `-CONT`) → the session survives;
  4. revoke the shield mid-session → ends within ~1 s (stage H unchanged).

## F45 — Low (from code reading, not seen live): an old stream's cleanup can unregister the shield's newer stream

**Problem.** The connector registers a shield's channels by shield id only (`instruction_txs`, `snapshot_txs` and the tunnel hub's `shield_txs`, `agent_server.rs` ~920–935). The old stream's cleanup removes them by shield id without checking which stream they belong to (`agent_server.rs` ~1110–1121; only `stream_cancels` is per `stream_id`). If the shield's new stream registers **before** the connector notices the old one ended (a half-open TCP after a network drop, rather than the clean close seen at renewal), the late cleanup deletes the new stream's channels.

**Effect.** New tunnels to that shield fail (`shield … not connected`) and resource instructions/snapshots stop, until the shield reconnects again. At renewal the order was safe in the lab (old stream closed ~1.4 s before the new one).

**Proposed fix.** Remove an entry only if it still belongs to this stream: keep `stream_id` next to each sender (or compare with `Sender::same_channel`). Fits naturally with F44 option A (close only the ending stream's sessions).

**Acceptance.** Unit: register stream 1, register stream 2, end stream 1 → stream 2's channels and sessions remain.

## F48 — Medium (code reading, 2026-10-26): tunnel data is dropped silently under backpressure

**Problem.** Both ends of a shield tunnel hand each data chunk to a bounded channel with `try_send` and ignore the result:
- shield `shield/src/tunnel.rs` `handle_tunnel_data` → the tunnel's `inbound_tx` (64 chunks) towards the resource socket;
- connector `connector/src/agent_tunnel.rs` `dispatch_data` → the relay session's event channel (64) towards the device.

When the channel is full (slow resource, slow device, or a burst of up to 64 × 16 KB), the chunk is discarded and the session carries on with a hole in its byte stream. No reconnect is involved; it can happen on any busy session today.

**Proposed fix.** Backpressure instead of drop: `send().await` (the Control-stream reader then slows down, which is what TCP flow control expects), or end the session with an error when the queue is full. Never drop a chunk and continue.

**Acceptance.** Unit: fill a tunnel's queue → the next chunk is delivered once there is room (or the session ends with an error), never dropped while the session continues. Live: a bulk transfer to a slow reader (`pv -L` on the resource side) arrives byte-identical (checksum).

## F46 — Low: a shield keeps redialling a revoked connector

**Problem.** After its only connector was revoked (stage I, 06:07:13), the shield retried the connector about every 2.4 s; each attempt is accepted at mTLS (`verified shield mTLS identity on Control stream`) and then closed by the terminal connector. Not a security issue (the connector serves nothing), but noise and wasted work, like F32 (relay) and F38 (connector relay selector).

**Root cause (to confirm).** The connector's terminal fail-closed closes shield streams but still accepts new ones; the shield sees a clean close and reconnects with a short backoff. It learns of a revoked connector only from a `PeerConnectorList`, which a terminal connector no longer sends.

**Proposed fix (pick one when implementing).**
- Connector, terminal state: refuse new shield Control streams with a marker (e.g. `PermissionDenied` `connector revoked`), so the shield marks that connector revoked and fails over or backs off.
- Or shield: grow the backoff for repeated clean closes with no messages.

**Acceptance.** Revoke the shield's only connector → the shield logs once that its connector is revoked and stops redialling it (or backs off to minutes).

## F47 — Low / to confirm: a suspended admin's access token keeps working until it expires

**Observation.** With the admin user set to `suspended` (stage G, SQL), admin GraphQL calls with the access token issued before the suspension still succeeded (`createGroup` at 05:55:17 and 05:55:44). The refresh path does reject suspended users, so the window is the access-token lifetime (15 min).

**To decide.** Expected for stateless JWTs (it matches bug 4's known "token still valid after suspension" window, which the runbook leaves to automated tests). If an admin suspension must take effect at once, the tenant auth middleware needs a per-request user-status (or `identity_generation`) check. Record as accepted, or fix.

## Runbook corrections (`docs/certificate-audit-live-runbook.md`)

Found while running it; the run sheet's deviations list has the rest.
1. **§4 `asconn`:** `connector/v1/connector.proto` imports `proto/client/v1/client.proto`, so use `-import-path . -proto proto/connector/v1/connector.proto` (and `proto/shield/v1/shield.proto`). As written, the connector call fails to parse.
2. **§13 step 2 / I1:** the CRL lists only **unexpired** revoked serials (`AND not_after > NOW()`, `controller/internal/pki/workspace.go` ~600). With 15 min certs `c0`–`c2` have expired by stage I; expect every *unexpired* serial on the CRL, and every history row with `revoked_at`.
3. **§10 F1 / F4:** the older token is refused with `token expired or already used` (its jti is dropped from Valkey on re-issue), not `… not the connector's current token`.
4. **§5.1 relay on a LAN:** the controller must run with `RELAY_DEV_ALLOW_PRIVATE_ADDR=true` and the relay must dial the controller's LAN address, or connectors get an empty relay list.
5. **§11 stage G:** run G3 (reactivate, no re-login) before G2 (logout/login), because `logout` wipes the device's local state. `zecurity-client logout` also stops the daemon; it needs `sudo systemctl start zecurity-client` before the next login.
6. **§2.4 `buf generate`:** add `--exclude-path .worktrees` when worktrees exist under the repo.

## Implementation (2026-10-26, branch `sprint20/fix14-shield-reconnect-sessions`, local)

**User decisions (2026-10-26):** D-1 = B **as a handover on renewal** (no proto change), with the expiry rule; groundwork first. Blip survival (seq/ack, proto) is **not** in scope.

| Commit | What |
|---|---|
| `4b9b69e` | connector: sessions belong to their stream; a stream end closes its sessions (no hang); an older stream's cleanup keeps the newer stream's channels (**F45**) |
| `796a812` | connector: a shield stream is closed at its certificate's `not_after` (**expiry rule**) |
| `0e7d934` | connector: F48, first cut (end the session as soon as the queue is full) — superseded by `5eaa9c4` |
| `711b151` | shield: a stream's end closes its tunnels and their resource sockets; F48 first cut |
| `5eaa9c4` | connector + shield: F48 waits for room (flow control) instead of ending at once |
| `00cb0bc` | connector: **handover** (adopt on a clean end when a newer stream is registered; per-send stream lookup with retry; old queue `close()` + drain; held early events delivered after the old stream's end) + F48 **stall budget** (2 s per 10 s per session) |
| `23b411b` | shield: **handover** on renewal (open new stream → pause → half-close old → read old to its end → resume); one swappable upstream for all tunnel messages; F48 stall budget per tunnel |
| `d58957e` | connector: handover backlog capped by bytes (1 MiB) and time (15 s), not only count |
| `437577d7` | connector + shield: F48 back to a per-chunk 2 s limit; total budget reverted (ended normal slow transfers) |

**Behaviour as built.**
- Renewal (new shield + new connector): sessions survive, in order both ways.
- Renewal fails to reach the same connector with the new cert, or the new stream is refused: the shield reconnects as before; tunnels close cleanly (F44 groundwork).
- Any unclean stream end (blip, crash, connector restart, revocation, close-all): sessions close cleanly, never hang.
- Certificate expired without renewal: the connector closes the stream at `not_after`; its sessions end.
- New connector + **old** shield (closes before reopening): sessions close cleanly — no hang.

**Tests.** Connector 261, shield 51 (2 ignored), all passing. New tests (each mutation-checked: the fix removed → the test fails):
- connector hub: stream end closes its sessions; F45; other shields untouched; handover order both ways (`1,2,4,3,5`); device data retried on the new stream; clean end without a newer stream closes; unclean end closes; early close after held data; backlog count, byte and time limits; slow device gets every chunk; stalled device ends; trickling device uses up its budget;
- connector, real mTLS streams: F45 (newer stream keeps its snapshot channel); stream closed at cert expiry; session survives a make-before-break handover both ways;
- shield: `close_all` closes the resource socket; slow resource gets every chunk; stalled / trickling resource ends; paused traffic goes to the next upstream in order; a message whose stream closed is retried; no upstream within the grace → given up.
- Not unit-tested: the shield's `hand_over` sequence itself (needs a live connector) → live acceptance.

### Deployment order

A **new shield with an old connector** hits F45 on every renewal: the shield opens its new stream, then half-closes the old one, and the old connector's cleanup of the old stream removes the new stream's tunnel, instruction and snapshot channels. New tunnels to that shield then fail and resource updates stop until its next reconnect (days at production lifetimes). The shield cannot tell the connector's version from any existing message.

→ **Upgrade connectors before shields.** (New connector + old shield is safe.) **Decision (user, 2026-10-26): accepted.** Record in the release notes / PR.

### F48 policy and its limit

- A full queue **waits** for room (flow control); a session or tunnel that makes **no progress for 2 s** ends. This replaces "drop and carry on" (data corruption).
- A total stall budget (2 s per 10 s, commits `00cb0bc` / `23b411b`, from the security review) was tried and **reverted** (`see below`): it ended every transfer to a peer steadily slower than the sender after ~2 s (tests: a reader at one chunk per 10 ms for 30 s failed).
- **Decision (user, 2026-10-26): per-chunk limit; accepted limit:** all tunnels of a Shield share one Control stream, so a peer reading just under the 2 s pace can delay the other tunnels through that Shield. The real fix is per-tunnel flow control (window updates) — a proto change, the same one blip survival would need. Follow-up, not this fix.
- F48 acceptance: a **slow** reader (`pv -L`, or `curl --limit-rate`) gets the transfer **byte-identical**; a reader that **stops** for more than ~2 s ends the session (not a corrupted stream).

### Live acceptance (to run; one redeploy of connector + shield, lab certs 15 min)

1. **Renewal survives:** a continuous counter stream (client → shield-protected resource, checked byte for byte in both directions) across at least one shield renewal → no gap, no reorder, session stays up.
2. **Expiry ends:** stop the connector until the shield's cert expires, start it again → the session ended, is not resumed; the shield re-enrolls/fails as today.
3. **Unclean end closes cleanly:** restart the connector mid-session → the client sees a close within ~1 s (no 5 s+ hang).
4. **Revocation:** revoke the shield mid-session → ends within ~1 s (stage H unchanged).
5. **Bulk transfer:** ≥ 100 MB download through the shield to a normal client → checksum matches, not cut.
6. **Old/new mix:** new connector + old shield, one renewal → sessions close cleanly (no hang).

### Still open

F46 (shield redials a revoked connector), F47 (suspended admin's access token until expiry), per-tunnel flow control (follow-up for F48 and blip survival). Decision Record entry for D-1 (user's call).
