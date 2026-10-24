---
title: Sprint 20 Fix 12 — the client must accept concurrent connections to the same resource (F37)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: FIXED and verified live (2026-10-24) — 3 new tests, 6/6 mutants, live burst test PASS. Commit c01ae337 (local).
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

Each socket has 64 KiB receive + 64 KiB send buffers, allocated zero-filled (`vec![0u8; N]` → calloc), so **idle listeners cost address space, not resident memory**, until they take a connection. The live check measures the client's RSS before and after.

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

**Decision D-1 (user, 2026-10-24):** `LISTEN_BACKLOG = 16`.

## Implementation (`c01ae337`)

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
- baseline = the installed Phase C client `2485de8f`; Fix 12 = `d87fa33e` (built from `c01ae337` with `client/scripts/dev-build.sh --release`, installed by the user with sudo; backup `/usr/local/bin/zecurity-client.pre-fix12`).

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

Nothing is pushed until the user asks.
