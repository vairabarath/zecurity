---
title: Sprint 20 Fix 12 — the client must accept concurrent connections to the same resource (F37)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: PROPOSED — awaiting the user's sign-off before any code change
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

Nothing is pushed until the user asks.
