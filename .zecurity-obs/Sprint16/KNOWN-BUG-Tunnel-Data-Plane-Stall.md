---
type: bug
severity: P0
status: resolved
discovered: 2026-08-05
resolved: 2026-08-06
discovered_during: Sprint 16 Gate 1 (PENDING-14 Stage 1 E2E)
caused_by_sprint16: false
tags: [bug, p0, data-plane, client, net_stack, smoltcp, quic, tunnel]
---

# P0 — Tunnel opens but no data flows (data-plane stall)

> **Not caused by Sprint 16.** Verified: `git diff 5498d78..HEAD -- client/src/net_stack.rs` touches
> only the `ResourceTarget` import, the `resource_id` handshake field, the `run()` signature, and the
> `Some(Some(target))` destructure. The accept/promotion logic, the relay loop, `transport.rs`, and
> `tunnel_pool.rs` are **untouched** by this sprint. Sprint 16's authorization work is verified
> working (see Gate 1 evidence below).

## ✅ RESOLVED (2026-08-06) — routing loop from client/connector co-location

**Root cause.** The client's nft interception chain is `type route hook output priority
mangle` and matches purely on `(destination IP, destination port)` — so it applies to
**every process on the host**, with no uid/cgroup filter. When the connector runs on the
**same host** as a client (our dev setup: both on `192.168.1.87`), the connector's own
`TcpStream::connect(<resource>)` matched that rule, was marked `0x5a`, and got routed into
the client's TUN. The connector was talking to the client's own smoltcp stack instead of
the resource:

```
curl → TUN → smoltcp → QUIC → connector
                                  ↓ connect(resource)
                          nft marks it → table 105 → zecurity0 (our own TUN!)
                                  ↓ smoltcp accepts it as a NEW connection
                          → another relay → another tunnel → connector → ↺
```

This explains every symptom, including the one no earlier hypothesis could: **~10 tunnels
per single curl** (each loop iteration created another connection). The resource never
received anything, so nothing ever came back.

**Fix (A + C).**
- **A — connector marks its egress, client skips it.** Shared constant
  `appmeta::CONNECTOR_EGRESS_MARK = 0x5b` (both crates). The connector sets `SO_MARK` on
  resource sockets *before* connecting (`connect_marked_tcp` via `tokio::net::TcpSocket`,
  so the first SYN carries it; also applied to the UDP path). Best-effort — logs and
  continues without `CAP_NET_ADMIN`. The client inserts `meta mark 0x5b return` as the
  **first** rule of its nft chain, so marked packets follow normal kernel routing.
  **Both halves are required** — the connector's mark alone is overwritten by the client's
  rule.
- **C — detect and warn.** The client checks whether the connector's tunnel address is a
  local IP (`is_local_ip`, via a bind attempt — no new dependency) and warns loudly. Turns
  a silent hang into one log line.

**Verified end-to-end (2026-08-06):** `HTTP 200 in 0.067s`; exactly **one** tunnel per
connection (was ~10); no `flow queue full`; no `direct stream establishment exceeded 2s`;
fix C's warning observed. Connector 89 tests, client 39 tests green.

**Production note.** In the normal topology (client on a user device, connector inside the
remote network) the components are on different hosts and this could never occur. It is a
co-located dev/test hazard — which is exactly why fix C exists.

---

## Symptom

A managed resource is unreachable through the tunnel even though **authorization fully succeeds and
both sides report the tunnel open**. No bytes are transferred.

```
curl through tunnel:  HTTP 000   (8s timeout, or 13ms fast-fail once degraded)
curl direct (tunnel down): HTTP 200 in 0.008s, 620 bytes   ← resource is healthy
```

## Reproduction

1. Controller + connector + client all on one LAN, no relay provisioned.
2. Resource: `react.dev` → `192.168.1.164:5173` (a Vite dev server on a **different** host from the
   client — required; see "Testing trap" below).
3. `zecurity-client up`, then `curl http://192.168.1.164:5173/`.

Reproduces consistently, including after a clean restart of **both** connector and client.

## Evidence chain

| Boundary | Result |
|---|---|
curl → client TUN | ✅ accepted, `connect=0.0024s` |
client smoltcp | ✅ `new TCP connection`, socket promoted at **`state=SynReceived`** |
client → connector handshake | ✅ `tunnel opened` (client) |
connector authorization | ✅ `access allowed resource_id=… route="connector"` |
connector → resource | ✅ `tunnel_opened ok dest=192.168.1.164:5173` (TCP connect succeeded) |
**bytes back to curl** | ❌ **none** |
client, after ~64 chunks queued | ❌ `flow queue full; closing TCP flow` |
after degradation | ❌ `direct stream establishment exceeded 2s` → `direct path is in cooldown` |

Key inference: `flow queue full` proves curl's request bytes **did** reach smoltcp and were queued into
`tcp_to_quic` (64-slot channel), but **the relay task never drained them** — so the relay `select!`
loop is blocked, most likely in `send.write_all(&buf).await` (QUIC flow control, i.e. the peer is not
reading) or `quic_to_tcp_tx.send(...).await` (channel full, i.e. smoltcp is not draining).

Also observed: **~10 tunnels opened per single curl connection**, both sides agreeing, within ~50ms.

## Hypotheses tested and DISPROVED (do not re-litigate)

| # | Hypothesis | Disproved by |
|---|---|---|
1 | Leaked QUIC streams exhausted `max_concurrent_bidi_streams` | A fresh `TunnelPool` (via `down`/`up`, which rebuilds it) failed identically |
2 | Runaway promotion loop creating ~100 connections | Instrumented `DIAG promote` fired **exactly once per connection** with correct bookkeeping; the ~100 count was the *degraded* retry state, not the cause |
3 | The `/32` route for a resource hijacks the client's own connector traffic | nft marks only `ip daddr <ip> tcp dport <port>`; the connector's UDP/9092 flow is unmarked and `ip route get` confirms it resolves via `lo` |
4 | Blocking audit-log send (`control_tx.send().await`) gated `copy_bidirectional` | After switching to `try_send`, **no `control mailbox full` was ever logged** — the mailbox was never saturated, and the stall persisted |

## Strongest remaining lead

Two facts that should be explained together:

1. Sockets are promoted to a relay at **`state=SynReceived`** — *before* the TCP handshake with the
   local client completes ([net_stack.rs:304](../../client/src/net_stack.rs#L304) checks `is_active()`,
   which is true for `SynReceived`).
2. **~10 tunnels open per single curl connection.**

These point at the smoltcp accept/promotion interaction — possibly the newly-created listener and the
promoted socket interfering, or the flow being torn down and re-accepted before data can move.

## Next diagnostic step

Packet capture on the TUN, to compare what smoltcp emits against what the local client expects:

```bash
sudo tcpdump -i zecurity0 -n -vv 'host 192.168.1.164 and port 5173'
# then, in another shell:
curl -v --max-time 8 http://192.168.1.164:5173/
```

Look for: does the SYN-ACK reach curl? Does curl's GET appear? Is there a RST, and who sends it?
That distinguishes "handshake never completes" from "handshake completes but payload is dropped".

## Impact

**P0.** Any managed resource is unreachable through the tunnel. Worse, the failure is self-amplifying:
one `direct stream establishment exceeded 2s` calls `mark_direct_failure()`, which puts the direct path
into cooldown — and with **no relay configured there is no fallback**, so every subsequent attempt
fails instantly with `direct path is in cooldown` until the cooldown expires.

## Secondary defect (design, worth fixing on its own)

`client/src/transport.rs` — the direct-path cooldown assumes an alternative path exists. With
direct-only (no relay), `mark_direct_failure()` converts a single transient timeout into a hard
outage. Cooldown should be skipped, or drastically shortened, when there is no fallback transport.

## Hardening applied (NOT a fix for this bug)

`connector/src/device_tunnel.rs` — `emit_access_log` now uses `try_send` instead of
`send().await`. This did **not** resolve the stall (hypothesis #4 above), but it is a legitimate
defect on its own: the call sits immediately before `copy_bidirectional`, so a blocking send on a full
mailbox *would* stall the tunnel's byte pump. It now matches the fail-fast contract already documented
on `connectorStreamClient::send` ("a wedged connector can't stall a GraphQL resolver").

## Testing trap (cost us significant time — document for others)

**A resource on the same host as the client cannot be tested.** `react.app` → `192.168.1.87:5174` is
this host's own IP; Linux routes traffic to a local address via the `local` table (`dev lo`), which
always wins over any TUN route. `ip route show dev zecurity0` shows no route for it, and curl connects
directly, bypassing the tunnel entirely — producing a misleading "it works".

Related latent hazard: **`react.app`'s IP is also the connector's own address.** It happens to work
because local addresses bypass the TUN, but a resource sharing the connector's IP is a trap.

## What IS verified working (Sprint 16 Gate 1)

```
DEBUG received tunnel request  resource_id="32e69282-…"  auth_path="resource_id"
                              destination=192.168.1.164 port=5173 protocol=tcp
                              spiffe_id=spiffe://ws-yoge.zecurity.in/client/5b59fa0d-…
INFO  access allowed          resource_id=32e69282-…  route="connector"
INFO  tunnel_opened ok        resource_id=32e69282-…  dest=192.168.1.164:5173
```

Identity-based authorization (Sprint 16 Phases 1–3) is confirmed end-to-end on a live stack: the
client asserts `resource_id`, the connector authorizes on it and dials **its own ACL's address**. Gate
1's authorization criteria pass; only the byte-transfer criterion is blocked by this bug.
