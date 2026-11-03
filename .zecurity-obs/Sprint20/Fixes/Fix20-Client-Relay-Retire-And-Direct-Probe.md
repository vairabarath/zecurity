---
title: Sprint 20 Fix 20 — relay connections retire instead of closing on age; a background probe re-checks the direct path (F43)
date: 2026-10-27
type: fix
status: FIXED and verified live (2026-10-27) — commit f8906bb0 (on the #146 branch). 11 mutants killed; old-vs-new live comparison.
owner: Sathiya (M1)
findings: F43 (Sprint20/path.md; from Fix 05-B's live record §3 Finding 3 and §15, and Fix05-Stale-QUIC-Pool-Investigation §15 Q3)
---

# Fix 20 — F43: client relay close-on-age and the cooldown re-dial

## Part 1 — a relay connection was closed at 60 s with its flows on it

**Issue:**
- `client/src/relay_pool.rs` `get_or_connect` reused a cached relay QUIC connection for at most `MAX_RELAY_CONNECTION_AGE` (60 s).
- After that, the next lookup **closed** it (`relay CRL revalidation`), killing every flow still multiplexed on it.
- This was found in the Fix 05 investigation (§15 Q3) and never fixed.

**Why the close was not needed:** each relay connection already has a revocation monitor (`spawn_relay_revocation_monitor`). It closes the connection within 1 s of the relay's serial appearing on the CRL, or of the CRL becoming unavailable.

**Fix:** an aged connection is now **retired**.
- It leaves the cache, so new flows dial a fresh connection, and it is closed when its **last flow ends**.
- Each `CachedRelay` carries `RelayFlows`, an open-flow count plus a `retired` flag.
- `get_or_connect` hands out a `RelayFlowGuard` **under the pool lock**, and retiring also happens under that lock, so a connection can't be closed between being handed out and its flow being counted.
- The guard lives inside the returned stream (`RelayFlowStream`). The last guard dropped on a retired connection closes it (`relay connection retired`).
- **The count is needed:** the relay sends keep-alives (`relay/src/tls.rs`: `keep_alive_interval = idle_timeout / 3`) and the revocation monitor holds a strong handle, and quinn 0.11 has no weak handle. A retired connection would otherwise stay open for good.
- The fail-closed paths still close immediately: the CRL is unavailable at lookup, or the monitor sees a revoked serial or an unavailable CRL.
- **Trade-off:** an old relay connection now lives as long as its flows rather than at most 60 s. Revocation is still enforced within 1 s by the monitor.

## Part 2 — after a cooldown, user flows paid the re-dial

**Issue (Fix 05-B Finding 3):**
- When the direct path's cooldown expired, the next **user flows** dialed the possibly dead connector themselves. Each paid up to `DIRECT_TIMEOUT` (2 s) before falling back to the relay.
- The cooldown doubled per failure up to **2 h**, which also delayed reuse of a recovered connector.

**Fix** (`client/src/transport.rs`):
- With a relay configured, an expired cooldown after a failure no longer sends flows to the direct path. They take the relay, and **one** single-flight background probe re-dials:
  - `compare_exchange` on `DirectHealth.probing`, cleared by a drop guard on every exit;
  - the cooldown state moved into an `Arc<DirectHealth>` the probe task shares.
- The probe is `DirectOpener::probe`. For the pool it is `TunnelPool::get_or_connect`: a QUIC and TLS handshake that checks the connector's exact identity, pooled for the next flow.
- **Results:**
  - success clears the cooldown, and the next flow goes direct;
  - a connect failure or timeout re-arms the cooldown with backoff;
  - an **identity failure** re-arms it too and is logged. A revoked connector is never marked healthy.
- Without a relay, the old behaviour stays: the flow dials itself.
- `DIRECT_RETRY_MAX_COOLDOWN` dropped from 2 h to **2 min**. Probing no longer costs user flows anything, so a long cap only slowed recovery.
- **Limit:** if a stale pooled connection to a dead connector hasn't idled out yet, the probe can "succeed" on it. The pool pings every 10 s and idles out after 30 s, and the first probe comes 30 s or more after the failure, so this is unlikely. If it does happen, the next flow fails once and re-arms the cooldown, which is the old behaviour once.

## Tests

- **`relay_pool.rs`**, on a real local QUIC connection:
  - a retired connection closes when its last flow ends, and not before;
  - retiring an idle one closes it at once;
  - a cached connection outlives its flows.
- **`transport.rs`:**
  - the cooldown sends flows to the relay without dialing;
  - after expiry, 5 concurrent flows finish in under 500 ms with **exactly one** probe dial, and the probe's timeout re-arms the cooldown;
  - a successful probe restores the direct path;
  - a probe identity failure keeps the cooldown;
  - without a relay, the flow dials itself;
  - the backoff is capped at 120 s.
- **Mutants, all killed (11):**
  - in the guard: last flow closes when not retired; last flow never closes; retire closes despite open flows; retire never closes an idle connection;
  - in the probe: flows dial after expiry; no single-flight; success not cleared; identity failure marks healthy; probe without a relay; 2 h cap; probe failure not re-armed.
- **Client suite:** 282 pass.

## Live (2026-10-27)

**Setup:**
- Lab controller (`:8080/:9090`), connector `f20-conn` on inkyank .75, and a new relay `f43-relay-local` on .164; the connector registered with it 80 ms after the relay list.
- The client on .164 reached resource `f20-direct` (nika .44:8001).
- The **direct path was blocked** by a local nft rule (`ip daddr 192.168.1.75 udp dport 9092 drop`), so flows had to use the relay.

| Check | Old client `2073ac31` | New client `a183dc5c` |
|---|---|---|
| Flow held through the relay, a new flow at 65 s, the held flow used at 75 s | **reset** (`Connection reset by peer`) | **200 OK** |
| The new flow at ~65 s (cooldown expired) | **2.09 s** (dialed the blocked direct path itself) | **0.07 s** (log: `direct path is being re-checked`) |
| 3 min of requests every 2 s while blocked (several cooldown expiries) | — | 87 requests, all 200, **none > 1 s** (max 0.08 s), 2 background probes |
| Unblocked at 11:44:39 → back on the direct path | backoff up to 2 h | `direct path healthy again (background probe)` at 11:46:18 (**99 s**, under the 120 s cap); 73 requests in the window, none > 1 s |
