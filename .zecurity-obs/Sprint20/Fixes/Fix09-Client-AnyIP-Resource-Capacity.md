---
type: fix
sprint: 20
fix: 9
title: Client serves only one hot-added resource IP (smoltcp address limit); shared-port resources misrouted
status: fixed and verified live (2026-10-22). Pushed 2026-10-23 on `feat/sprint20-m1-phase2` (PR to `fixed-pendings` pending).
source: Phase B2 live run 2026-10-21, finding 5 ("client hot-apply admits one new resource IP"); Phase M results (Lab finding)
supersedes: Phase 5-C rule H1 (capacity admission, approved 2026-10-08), which is now moot
severity: medium (a resource IP outside the tunnel goes straight over the LAN, bypassing Zecurity)
component: client (src/net_stack.rs, src/daemon.rs, src/tun.rs)
branch: feat/sprint20-m1-phase2
date: 2026-10-22
tags:
  - rust
  - client
  - smoltcp
  - hot-apply
---

# Fix 09: AnyIP capture interface (no per-resource smoltcp addresses) + IP-bound listeners

## Symptom

During B2 (2026-10-21), adding resources while the client was up applied only one new resource IP. The rest needed a `down`/`up`.

## Root cause

**1. Address capacity.**
- smoltcp 0.11 accepts a packet only if its destination is an interface address (`any_ip` off). The client therefore gave every resource IP its own smoltcp `/32`.
- The address list is a `heapless::Vec` of `IFACE_MAX_ADDR_COUNT` = **2** (no `iface-max-addr-count-*` feature), and one slot is the tunnel's `100.64.0.1`.
- **Hot-apply:** the planner (`plan_resource_apply`, rule H1) admitted new IPs only into free slots, at most one. The rest stayed "pending: address capacity". Their kernel route (table 105) wasn't installed, so **their traffic went straight over the LAN, outside Zecurity**.
- **Startup:** `net_stack::run` pushed every resource IP with `let _ = addrs.push(..)`, so beyond 2 addresses IPs were **silently dropped**. Their table-105 route was installed anyway, so the traffic was captured into `zecurity0` and dropped, and the application timed out.

**2. Shared-port misrouting (found while fixing 1).**
- Listeners were created with `socket.listen(port)`, which matches **any** destination IP.
- With two resources on the same port (e.g. `10.0.0.1:80`, `10.0.0.3:80`), the first listener took every SYN on that port. `FlowTable::service` then tunnelled the connection to *that listener's* resource.
- A connection to `10.0.0.3:80` was opened to `10.0.0.1:80`.

H1's design (Fix 05 investigation, Q8/H1) was a rule for *what to do when capacity runs out*. AnyIP was never evaluated or rejected (finding P4 just notes `any_ip` off).

## Fix

**`client/src/net_stack.rs`:**
- **New `configure_capture_iface`:**
  - the only smoltcp address is `TUNNEL_IFACE_ADDR` (`100.64.0.1/32`);
  - `set_any_ip(true)`;
  - a default IPv4 route via `100.64.0.1`. AnyIP accepts a destination only when a route's gateway is an interface address. The route table holds 2, and 1 is used.

  smoltcp now accepts every destination the kernel routes into `zecurity0`. That is only the resource /32s in table 105, and nft marks only allowed flows. `net_stack::run` uses it in place of the per-resource address push, so the silent drop is gone.
- **`new_listen_socket(sockets, (ip, port))`:** `socket.listen((ip, port))`. Each listener accepts only its exact resource. This fixes the shared-port misrouting and keeps a socket-level filter now that the interface accepts any IP. Anything else gets a RST.
- **`ResourceCmd::Add { keys }`:** no `addrs`. `ResourceCmd::RemoveAddrs`, `add_iface_addrs`, `remove_iface_addrs` and `smol_v4` are removed.

**`client/src/daemon.rs`:**
- `plan_resource_apply(applied, candidate)`: no capacity parameter. Every addition applies.
- `addr_add` / `addr_del` are now purely the table-105 route changes: IPs newly served, or no longer served by any key.
- Removed:
  - `unapplied`;
  - `RESERVED_IFACE_ADDRS`;
  - the capacity-pending log;
  - the address-removal step of the apply path.

**`client/src/tun.rs`:** `ResourceKernel::addr_capacity` removed.

**Unchanged:**
- the remove → nft → routes → publish → add → routes → nft-last ordering;
- the 5-C failure and recovery rules;
- kernel capture (nft + table 105).

## Tests

**New:**

| Test | What it proves | On the old code |
|---|---|---|
| `net_stack::connection_is_attributed_to_its_real_destination_when_ports_are_shared` | `10.0.0.1:80` + `10.0.0.3:80`: each connection tunnelled to its own destination | **FAIL**: `10.0.0.3:80` tunnelled to `10.0.0.1:80` |
| `net_stack::anyip_serves_more_resource_ips_than_the_address_limit` | 4 resource IPs (2 on a shared port) with only `100.64.0.1`: handshake, data both ways, correct attribution | FAIL |
| `net_stack::anyip_refuses_destinations_that_are_not_resources` | another IP, or a resource IP on another port → RST, not tunnelled | FAIL |
| `net_stack::hot_added_resource_ips_beyond_the_address_limit_are_served` | 3 IPs added at runtime are all served; still 1 interface address | FAIL |
| `daemon::plan_applies_every_new_ip`, `plan_addition_on_a_served_ip_adds_no_route`, `plan_route_is_kept_while_another_key_uses_the_ip`, `plan_removal_and_addition_in_one_pass` | planner without capacity; routes follow IPs | replace the 4 H1 capacity tests |
| `daemon::several_new_resource_ips_hot_apply_in_one_pass` | harness: 3 new IPs → one pass, exact order of operations, nothing pending | replaces `capacity_overflow_applies_the_rest_and_keeps_pending` |

**Mutation checks:**
- `set_any_ip(false)` → both AnyIP tests FAIL.
- A port-only listener → the shared-port, refuse and hot-add tests FAIL (3).

**Removed:** `loop_error_on_address_removal_triggers_recovery_restart`, because that step no longer exists. Expected operation logs no longer contain `loop:addr-`.

**Gate:**
- `cd client && cargo test`: **252 PASS**, 0 FAIL, 0 ignored.
- `cargo build`: no new warnings.
- `cargo clippy --all-targets`: 24 warnings before and after, all pre-existing.

## Live reproduction and verification (2026-10-22)

**Setup:**
- controller `26e7779`;
- connector **`s3-luffy`**, a user-level process on luffy (192.168.1.40), no sudo. It was started from luffy's Orca terminal with a one-shot launcher served from this PC; the token-bearing launcher was deleted afterwards. It runs on a different host from the client, so the client's capture rules don't catch the connector's own connections.
- resources `s3-r1` `192.168.1.75:22`, `s3-r2` `.41:22`, `s3-r3` `.49:22`, `s3-r4` `.40:22` (unprotected, k2-net), assigned to `g9-users`;
- the ACL also still holds `pm-udaya-web` (.37) and `g9-nika-web` (.39), both stale since the hosts' IPs changed.
- **Check** (`~/.cache/zecurity-s3/s3-check.sh`), per resource: table-105 route; SSH banner via `/dev/tcp`; client log `tunnel opened dest=<ip>`. A banner without a route means it went **direct over the LAN**; a route with no banner means **captured and dropped**.

| Step | Baseline: installed client `18d28f57…` | Fixed client `7a76a99b…` |
|---|---|---|
| A. only `s3-r1`, `down`/`up` | r1 routed and tunnelled | r1 routed and tunnelled |
| B. assign r2–r4 **while up** | `resource additions pending: address capacity pending=[.40, .41, .49]`. **r2–r4: no route, banner direct over the LAN, not tunnelled** | `resource change, hot-applying removed=0 added=3` → `hot-applied resources=6 added=3` (14 ms). **r1–r4 routed and tunnelled**; no restart |
| C. `down`/`up` with all 6 ACL IPs | 6 routes, but **r1 (.75) and r3 (.49) timed out** (captured, silently dropped; smoltcp's 2 slots went to .41 and .40) | 6 routes, **r1–r4 all tunnelled**, 0 capacity messages |

The fixed client was installed with `install-client-fix.sh`, run with sudo by the user in an Orca terminal. The previous binary was kept as `/usr/local/bin/zecurity-client.pre-fix09` (`… rollback` restores it).

## Docs superseded

- Phase 5-C rule **H1** and "a second new IP must go pending (H1), and that is not a defect":
  - `Fixes/fix 5/Phase5C-Live-Acceptance-2026-10-08.md`;
  - `Handoff-Fix05-5C-Live-2026-10-08.md`;
  - `Fixes/fix 5/Phase5C-Resource-Hot-Apply.md`;
  - `Fixes/fix 5/Fix05-Resource-Hot-Apply-Investigation.md` (Q8/H1).

  Since Fix 09 there is no address capacity; every addition applies.

## Lab state after the run

- **This PC:** the fixed client is installed and up (6 resources). Roll back with `sudo bash ~/.cache/zecurity-s3/install-client-fix.sh rollback`.
- **luffy:** connector `s3-luffy` (k2-net) is running as user `nika`, with binary, state and log in `~/s3-conn/`. Stop it with `kill $(cat ~/s3-conn/pid)`.
- **Resources:** `s3-r1`…`s3-r4` assigned to `g9-users`. Delete them, and revoke `s3-luffy`, when no longer needed.
