---
title: Sprint 20 Fix 12 — the client must accept concurrent connections to the same resource (F37)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: REWORKED after review (2026-10-26) — SYN gate + 1 listener per resource replaces the 16-listener pool (PR #134 review items 1–3); live PASS (64/64 and 128/128 bursts, no RST, no duplicate, RSS as before Fix 12). First version `d6548792` (pushed in PR #134), rework `e87b0b34` (local until pushed).
owner: Sathiya (M1)
finding: F37 (Sprint20/path.md → Post-Sprint Fixes), found in Connector-Revocation-Systemd-Live-2026-10-23.md (Finding B)
---

# Fix 12 — a listen backlog per resource in the client's userspace stack

## Problem

Several new connections opened at the same moment to the same resource mostly fail with a TCP reset from the client itself.

- **Live 2026-10-23**, same resource and minute, through the installed client: **24/24 sequential OK**, but **5/24 OK in parallel bursts of 8** (19 reset).
- The refused attempts never reach the client's flow table: there's no `new TCP connection` line in the journal.
- The same effect caused the "isolated single refusals" seen at probe start and around ACL hot-apply.

**Impact:** browsers open ~6 parallel connections per host, so pages through the tunnel randomly fail to load parts.

## Root cause (from source)

`client/src/net_stack.rs`:
- `FlowTable` keeps **one** smoltcp listening socket per resource `(ip, port)`: `listen_handles: HashMap<(Ipv4Addr, u16), SocketHandle>`, created by `new_listen_socket`.
- smoltcp processes all queued packets in one `iface.poll`. The first SYN for a resource moves its only listener out of `Listen`.
- Every further SYN for that resource in the same poll finds no listening socket, and smoltcp answers it with a **RST**.
- `FlowTable::service` (after the poll, ~:694) only then creates the replacement listener, too late for those SYNs.

## Proposed fix

smoltcp supports a backlog the standard way: **several sockets listening on the same endpoint**.
- A SYN goes to the first socket still in `Listen`.
- A socket that has left `Listen` matches only its own 4-tuple.
- smoltcp's `accepts()` explicitly refuses ACK-bearing packets on a listening socket, so another listener's connection can't be stolen.

1. `FlowTable.listen_handles` becomes `HashMap<(Ipv4Addr, u16), Vec<SocketHandle>>`, a pool of `LISTEN_BACKLOG` listening sockets per resource.
2. **Accept:** `service()` promotes **every** pooled socket that has left `Listen` (one relay each, as today), then refills the pool to `LISTEN_BACKLOG`. That bounds the number of simultaneous new connections per resource **per poll** (a 5 ms tick), not overall.
3. **Removal** (`remove_listeners_and_close`, Phase 5-C R1): every socket of the key's pool is handled. Listening ones are removed; ones that already took a SYN are aborted and reaped, as today for the single listener.
4. **Add** (`add_listeners`, `FlowTable::new`): creates the full pool; idempotent per key.
5. **`LISTEN_BACKLOG = 16`** (decision D-1, see below).
6. Nothing else changes: relays, transports, revocation (Phase C), the Fix 09 per-IP binding, the RST for non-resource ports.

### D-1: backlog depth (your call)

| Option | Simultaneous new connections per resource per 5 ms tick | Address space for idle listeners |
|---|---|---|
| 8 | 8 (one burst of our live test) | 1 MiB per resource |
| **16 (recommended)** | 16 (≥ 2 browsers' worth) | 2 MiB per resource |
| 32 | 32 | 4 MiB per resource |

Each socket has 64 KiB receive + 64 KiB send buffers, allocated zero-filled (`vec![0u8; N]` → calloc).

> **Correction (2026-10-26, PR #134 review):** "idle listeners cost address space, not resident memory" is **wrong** and should not be used for sizing. 64 KiB is below glibc's mmap threshold, so the buffers come from the heap, and calloc zeroes a reused heap chunk (making it resident); refilled listeners land on chunks freed by finished relays. Measured on 2026-10-26 with 50 resources: 16 listeners per resource cost **+12.5 MB** RSS right after `up` (47.7 vs 35.2 MB). D-1 is superseded by D-2 below.

## Acceptance

| # | Check | Level |
|---|---|---|
| AF12-1 | A burst of `LISTEN_BACKLOG` connections to one resource opened in the **same poll** is accepted in full: that many relays spawned, 0 RST (Lab harness with real smoltcp interfaces) | client unit |
| AF12-2 | After the burst the pool is refilled to `LISTEN_BACKLOG` listening sockets; a second burst also succeeds | client unit |
| AF12-3 | Two resources sharing a port (Fix 09): concurrent bursts to both are each tunnelled to the right resource | client unit |
| AF12-4 | Resource removal: every pooled socket is removed or reset, including one mid-handshake; zero resources leaves 0 sockets; `add_listeners` is idempotent | client unit |
| AF12-5 | Existing client tests pass (socket-count asserts updated from 1 listener to the pool size); other suites unaffected | suites |
| AF12-6 | Mutation checks: backlog of 1; no refill; only one promotion per poll; removal leaking a pooled socket | mutants |
| AF12-L | **Live:** through the installed client, 3 × 8 and 3 × 16 parallel bursts to a lab resource → all 200 (before: 5/24); 24 sequential still 200; a long-lived connection unaffected; client RSS recorded before/after | live |

**Decision D-1 (user, 2026-10-24):** `LISTEN_BACKLOG = 16`. **Superseded by D-2 (2026-10-26).**

## Implementation, first version (`d6548792`, PR #134)

- `client/src/net_stack.rs`:
  - `LISTEN_BACKLOG = 16`;
  - `FlowTable.listen_handles: HashMap<(Ipv4Addr, u16), Vec<SocketHandle>>`;
  - `new_listen_pool` (used by `FlowTable::new` and `add_listeners`);
  - `service()` collects **every** pooled socket that left `Listen`, refills each slot, then promotes them (relay or fail-closed, unchanged);
  - `remove_listeners_and_close` walks the whole pool.
- Existing asserts "only the listener is left" (6) now expect `LISTEN_BACKLOG`.

## Tests

- Client **265 → 268**:
  - `a_burst_of_connections_in_one_poll_is_accepted_and_the_pool_refills` (AF12-1/2: 2 rounds of 16 SYNs in one poll, all Established, pool back to 16, 0 fail-closed RSTs);
  - `bursts_to_resources_sharing_a_port_go_to_their_own_destinations` (AF12-3);
  - `removing_a_resource_removes_its_whole_pool` (AF12-4: incl. a socket mid-handshake, 0 sockets left, `add_listeners` idempotent).
- **Mutation checks: 6/6 killed:**
  - pool of one;
  - no refill;
  - one promotion per poll (caught only by the new burst tests);
  - removal leaking pooled sockets;
  - `add_listeners` single;
  - `FlowTable::new` single.
- rustfmt: no new differences (the file's 11 pre-existing ones unchanged).

## Live check AF12-L (2026-10-24): PASS

**Setup:**
- installed client on this PC, through the tunnel (every attempt logged as `new TCP connection`);
- resource `f12-web` = udaya `192.168.1.59:18091` (`python3 -m http.server --protocol HTTP/1.1`), connector-routed;
- baseline = the installed Phase C client (binary SHA-256 prefix `2485de8f`); Fix 12 = binary SHA-256 prefix `d87fa33e`, built from commit `d6548792` with `client/scripts/dev-build.sh --release`, installed by the user with sudo; backup `/usr/local/bin/zecurity-client.pre-fix12`. (The two 8-hex values are binary hashes, not commits.)

| Test | Baseline `2485de8f` | Fix 12 `d87fa33e` |
|---|---|---|
| 24 sequential | 24/24 | 24/24 |
| 3 × 8 parallel | **4/24** | **24/24** |
| 3 × 16 parallel | **8/48** | **48/48** |
| client RSS | 18.6 MB | 23.0 MB |
| keep-alive hold 60 s during the bursts | — | 118 requests, **0** failures/reconnects |

RSS grew ~4.4 MB with 16 listeners per resource for ~8 captured resources. The idle buffers are lazily backed, so most of the 1 MiB per resource stays virtual.

**Lab notes (not Fix 12):**
- udaya came back on **192.168.1.59** (was .41) after a reboot.
- The protected resource `sd-web` (host .41) stayed stale: it is bound to the old `sd-shield`, uninstalled 2026-10-24 but its row still exists, and the controller's host re-sync follows the bound shield. The current shield `f11-shield` reported .59 correctly. This is lab clean-up, related to F20 (orphaned protected resources).

Pushed later as PR #134 (base `fixed-pendings`).


## PR #134 review (2026-10-26) and rework

Review by Barath (changes requested). What it found, and what changed:

| Item | Finding | Rework (`e87b0b34`) |
|---|---|---|
| 1 | A retransmitted SYN could open a **second connection** for one 4-tuple: smoltcp 0.11 gives a SYN to the first socket whose `accepts()` is true; a listener refilled into a freed low `SocketSet` slot sits ahead of a half-open socket. Its later abort would RST the real connection too. | **Reproduced** in the lab harness first (2 relays for one 4-tuple), then fixed: a **SYN gate** in front of smoltcp's receive path drops a bare SYN (no ACK) for a connection a socket already owns (`SynReceived` / `Established`). The owning socket retransmits its SYN-ACK (smoltcp re-arms its retransmit timer in `SynReceived`). |
| 2 | 16 listeners × 128 KiB per resource, resident after reuse; no cap. | **1 listener per resource** (as before Fix 12). |
| 3 | 16×N listeners walked on every packet and every poll. | Back to N. |
| F37 overflow | More than 16 SYNs to one resource in one poll still got RSTs. | The gate lets into a poll only as many SYNs per resource as it has free listeners and **holds** the rest for the next poll (≤ 1024 SYNs, ≤ 1 s each; past that a SYN is **dropped**, never reset — the app retransmits). The loop polls again at once while held SYNs make progress. **F37 is closed for any burst size.** |
| `listen()` error | ignored; dead sockets. | Logged; such a key gets no listener (the gate then lets its SYNs through, smoltcp resets them). |
| stats | `sockets` included idle listeners. | `sockets` = relay sockets only; new `listeners`, `syn_deferred`, `dup_syn_dropped`, `syn_overflow_dropped`. |
| test helper | `res_key_default()` duplicated `res_key(RES_PORT)`. | Removed. |
| docs | stale "local" status, `d654879`, "address space" note. | Corrected above (`d654879` is the short form of `d6548792`; full hashes now). |

**Decision D-2 (2026-10-26, rework after review):** one listener per resource plus the SYN gate, replacing D-1's pool of 16.

**Design notes.**
- Production and the test lab poll through the same `FlowTable::gated_poll` (prepares the gate from the SocketSet, then `iface.poll` through `GatedDevice`).
- The packet parser reads only unfragmented IPv4/TCP (fragment offset 0, MF clear), honours IHL incl. options, bounds-checks every read.
- "Owned" covers only `SynReceived` and `Established`: a reconnect on the 4-tuple of a closing socket still gets through.

**Tests.** Client 268 → **272** (net_stack 44). New or rewritten:
- `a_retransmitted_syn_does_not_open_a_second_connection` (item 1; failed on the 16-listener code with 2 spawns);
- `a_burst_of_connections_in_one_poll_is_accepted_and_the_pool_refills` (now **24** SYNs in one poll, > any pool; 2 rounds; 0 RST; gate held SYNs);
- `bursts_to_resources_sharing_a_port_go_to_their_own_destinations` (24 + 24);
- `bare_syn_reads_only_unfragmented_tcp_syns` (flags, IP options, fragments, non-TCP, truncation at every length, IPv6);
- `a_held_syn_is_dropped_once_it_waited_too_long_or_the_queue_is_full`;
- `a_resource_that_cannot_listen_gets_no_listener`.

Mutation checks: owned-tuple check off → item-1 test fails; gate lets every SYN through → both burst tests and the hold test fail.

### Live check AF12-L2 (2026-10-26): PASS

Setup: controller `fixed-pendings` `3b1f8511` (no client or proto difference from this branch's base `2352a390`); connector on inkyank, shield on manoj, resource = TCP echo on manoj `192.168.1.53:5555` (backlog 256) + 49 padding resources (ports 6000–6048) so the client holds 50 resources. Three clients built from this tree with the lab root: base `2352a390`, PR `7bbcabbf`, gate `e87b0b34`; swapped with sudo. Burst = N connections released at once, each with a unique payload checked byte for byte.

| | base (before Fix 12) | PR #134 (16 listeners) | **gate (rework)** |
|---|---|---|---|
| 3 × 64 parallel | 2/64, 2/64, 2/64 | 16/64, 16/64, 19/64 | **64/64 ×3** (p50 32–45 ms, max ≤ 91 ms) |
| 1 × 128 parallel | 2/128 | 32/128 | **128/128** (max 110 ms) |
| RSS after `up` (50 resources) | 35.2 MB | 47.7 MB | **34.8 MB** |
| RSS after 300 sequential connections | 36.7 MB | 49.3 MB | **36.3 MB** |

Gate client stats after the bursts: `listeners=50 sockets=0 syn_deferred=316 dup_syn_dropped=0 syn_overflow_dropped=0 rst_fail_closed=0 fin_on_relay_end=321`; exactly 320 `new TCP connection` lines for 320 connections (no duplicate). Logs: `/tmp/cert-live/logs/f12-*.log` (lab machine).

Not reproduced live: item 1's lost SYN-ACK (needs packet loss on the TUN path); covered by the lab-harness test above.
