---
title: Sprint 20 Fix 15 — a saturating upload must be throttled, not cut off (F49)
date: 2026-10-26
type: fix
status: IMPLEMENTED locally on `sprint20/fix14-shield-reconnect-sessions` (`2e02938c`); unit test + mutation check pass; live PASS with the Fix 14 connector and shield (2026-10-26)
owner: Sathiya (M1)
finding: F49 (found in the Fix 12 rework live run, 2026-10-26)
---

# Fix 15 — client upload backpressure

**Problem.** `drive_relay` (`client/src/net_stack.rs`) read the app's bytes with `recv_slice`, then `try_send`-ed them into the 64-slot `tcp_to_quic` channel. A full channel closed the flow (`flow queue full; closing TCP flow`, since `1766dca0`). Any upload faster than the tunnel drains was cut off. Live: 50 MB each way through an echo, 5.37 MB came back, then EOF, with the pre-Fix-12 and the Fix 12 client alike.

**Fix (`2e02938c`).** Backpressure: reserve a channel slot (`try_reserve`) **before** reading. With the channel full the bytes stay in smoltcp's receive buffer, the advertised window closes, and the app's TCP stops sending. The next loop pass after the relay task drained reads again, and smoltcp sends the window update by itself (`window_to_update`). The relay task's `send().await` on the QUIC stream carries the backpressure on towards the connector.

**Unchanged.**
- The download direction already pulled from the channel only while the socket can send.
- The app's FIN is still passed on only after all its bytes were read.

**Limit.** After the channel drains, reading resumes at the next pass, at most 5 ms later. With 64 × 4 KiB that caps uploads near 50 MB/s, well above the measured ~9 MB/s.

**End to end.** Bytes must also survive the connector and shield: with `fixed-pendings` they still drop chunks under backpressure (F48). Fix 14 fixes that, hence the live check on Fix 14's binaries.

**Tests.** `a_saturating_upload_is_throttled_not_closed`: a 2 MiB upload (8 × the channel) against a relay draining 4 chunks per step arrives complete, in order, flow open. Mutation: the old read-then-close code fails it ("cut off after 97008 bytes"). Client suite 266 pass.

**Live (2026-10-26).** With the Fix 14 connector, shield and client, the same bulk test that had stopped at 5.37 MB now completes: 200 MB each way, checksum intact, twice, 13.4–13.6 MB/s, no `flow queue full`. See Fix 14 → Live acceptance, check 5.
