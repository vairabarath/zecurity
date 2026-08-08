---
type: phase
sprint: 16
stage: 2
phase: 9
title: Client Binding Registry + Synthetic Routing
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target
status: not-started
tags: [sprint16, client, rust, synthetic-ip, registry, smoltcp, nftables, routing, security, adr-002]
---

# Sprint 16 · Phase 9 — Client Binding Registry + Synthetic Routing

> Goal: give every name-addressed resource a stable, client-local **synthetic IP**, route that IP into
> the TUN, and map it back to a `resource_id` on the handshake. This is the phase that finally makes an
> FQDN resource reachable, and the first one that can exercise Phases 6–7 end-to-end.
> Depends on **Phase 8**. **The largest and most security-sensitive phase of Stage 2.**

## Why the client allocates, and why this is a registry not a cache

**Only the client can see local-network collisions** with the synthetic CIDR — the controller has no
view of the user's LAN, coffee-shop Wi-Fi, or other VPNs. So the client allocates, and the **controller
never allocates, stores, or sees a synthetic IP**.

That makes the mapping client-owned state, and it is **security-critical**: `synthetic IP → resource_id`
decides **which identity the client asserts** on the handshake. If a synthetic IP is silently remapped
across a restart, the client asserts the wrong `resource_id` for an already-open app connection — the
connector authorizes correctly and dials the *wrong resource*. So this is a **durable registry with
quarantine**, never an in-memory cache.

## Current state (verified)

- Transports are keyed by `(Ipv4Addr, u16)` → `Option<ResourceTarget>`
  ([client/src/net_stack.rs:213](../../../client/src/net_stack.rs#L213)), and `ResourceTarget` already
  carries `resource_id` (Phase 2). **The value shape is already right; this phase changes the key
  source.**
- Name-addressed entries are silently dropped today at
  [client/src/daemon.rs:657](../../../client/src/daemon.rs#L657) — `filter_map(... .ok()?)` on
  `e.address.parse::<IpAddr>()`. This is why Phase 5 could ship safely, and it is the line this phase
  replaces.
- **Steering is nft-mark based, not plain routes**
  ([client/src/tun.rs:59](../../../client/src/tun.rs#L59) `configure_allowed_flows`):
  1. nft chain `type route hook output priority mangle`, first rule `meta mark 0x5b return`
     (skip connector egress — do not disturb it),
  2. one rule per flow: `ip daddr <ip> tcp dport <port> meta mark set ZECURITY_MARK`,
  3. `ip rule add fwmark ZECURITY_MARK lookup 105`,
  4. one `/32` route per IP into table `105` dev `zecurity0`.
- smoltcp keys listeners on `(ip, port)` and promotes at `is_active()`
  ([net_stack.rs:304](../../../client/src/net_stack.rs#L304)).

## Decision required before coding — how the synthetic CIDR is steered

9.3's "route the synthetic CIDR once" is **necessary but not sufficient**: a route in table 105 only
matters for packets that were *marked*, and marking is what the nft chain does per `(daddr, dport)`. A
route alone will not pull traffic into the TUN.

| Option | nft | route | Verdict |
|---|---|---|---|
| Per-`(ip, port)` rule, as today | 1 rule per resource | 1 CIDR route | Works, but keeps the per-resource rule growth this phase is meant to remove |
| **Whole-CIDR mark, port-agnostic** | **1 rule:** `ip daddr <SYN_CIDR> meta mark set ZECURITY_MARK` | 1 CIDR route | **Recommended.** The synthetic CIDR is entirely ours — no legitimate traffic to it exists outside the tunnel. Constant-size ruleset. |

Consequence of the recommended option, state it explicitly: any port on a synthetic IP is steered into
the TUN, and a port that isn't in the ACL then has **no listener** in smoltcp. That must be a clean
refusal, not a hang — confirm the behaviour and record it.

⚠️ The `meta mark 0x5b return` rule must stay **first**. Both halves of the co-location fix are
required ([[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]]); a rebuilt chain that reorders it
reintroduces the routing loop that cost a day in Gate 1.

## Tasks

### 9.1 — `client/src/registry.rs` (new)
- [ ] Durable `hostname → synthetic IP → resource_id`, **bidirectional** (forward for DNS/hosts, reverse
      for the handshake) and **stable across restarts**.
- [ ] **Collision-aware CIDR selection.** Default to a subrange of `100.64.0.0/10` (decision #5), chosen
      at startup after checking the host's existing routes and interface addresses — CGNAT space *is*
      used in the wild, notably by other VPNs.
- [ ] **Quarantine before reuse.** A synthetic IP released by a deleted resource must not be handed to a
      different resource until a quarantine interval has passed **and** no binding for it is in use.
      Rationale above: premature reuse makes the client assert the wrong identity. Prefer "allocate the
      lowest never-used address first, recycle only under pressure".
- [ ] Reverse lookup must be **fail-closed**: an unknown synthetic IP is *not* a resource, and must never
      fall through to unmanaged passthrough.
- [ ] Preserve the existing three-state semantics exactly — losing them converts a fail-closed case into
      a passthrough, which is a security regression:
      ```text
      Some(target with transports)   → managed, connector online   → tunnel
      Some(target, transports empty) → managed, connector offline  → FAIL CLOSED
      None (absent)                  → unmanaged traffic           → no tunnel route
      ```

### 9.2 — `client/src/state_store.rs` — persist the registry
- [ ] New `StoredBinding { hostname, synthetic_ip, resource_id, allocated_at, quarantined_until }`
      alongside the existing `StoredResource` / `StoredDevice` structs, all `#[serde(default)]` so an
      older state file loads.
- [ ] **Encrypted at rest** — non-negotiable per ADR-002; use the existing state-store path, do not add
      a second file with its own crypto.
- [ ] Handle a corrupt/unreadable binding table by **rebuilding empty**, not by refusing to start. But
      then treat every previously-issued synthetic IP as quarantined, since their old meaning is unknown.

### 9.3 — `client/src/tun.rs` — route the synthetic CIDR once
- [ ] One `ip route replace <SYN_CIDR> dev zecurity0 table 105`, plus the nft rule chosen above.
- [ ] **Stop installing per-`/32` routes for name-addressed resources.** Per-`/32` does not scale to the
      stated resource targets. Pinned IP resources **keep** their per-`/32` behaviour — that path is
      unchanged and must stay byte-identical.
- [ ] `cleanup()` / `cleanup_policy_routes()` must remove the CIDR route and rule too. A leaked
      `100.64.0.0/10` route after `zecurity-client down` would blackhole CGNAT traffic host-wide.
- [ ] ⚠️ **Verify the interaction with split-tunnelling (ADR-009) explicitly.** A whole-CIDR route is a
      broader claim on the routing table than anything the client installs today.

### 9.4 — `client/src/net_stack.rs` — synthetic IP → `resource_id`, and rewrite the response source
- [ ] Key listeners on synthetic IPs for name-addressed resources; reverse-map to `resource_id` and put
      **that** on the `TunnelRequest`.
- [ ] Send `destination` **empty** for name-addressed resources. There is no pinned address for the
      connector to cross-check against, and Phase 7's task 7.0 scopes the check accordingly. Sending the
      synthetic IP instead would be denied as `destination_mismatch`.
      ⚠️ **Phase 7 and this task must agree.** If either ships alone, every FQDN resource is denied.
- [ ] **Rewrite the response source address to the synthetic IP.** The app connected to the synthetic IP,
      so its socket will drop any packet whose source is the real backend address. This is the single
      easiest thing in the phase to forget, and it presents as "the tunnel opens, bytes flow one way,
      nothing arrives" — the same shape as the Gate 1 bug, which will send you down the wrong path.
- [ ] The client **never learns the real backend IP**, and must not need to. If the design starts wanting
      it, the resolution boundary has leaked.

### 9.5 — Testable without DNS
- [ ] Add a `hosts` entry `<synthetic IP>  <hostname>` and connect by **name**.
- [ ] ⚠️ Test by **name**, not by raw synthetic IP: a `hosts` entry preserves TLS SNI and certificate
      validation, while connecting to a bare synthetic IP does not — an HTTPS resource would fail for
      reasons unrelated to this sprint.
- [ ] ⚠️ **Testing trap** (cost significant time in Gate 1): the resource must **not** be on the same host
      as the client. Linux routes local addresses via the `local` table (`dev lo`), which always beats any
      TUN route — curl connects directly and produces a misleading "it works".

## Build gate

```bash
cd client && cargo build && cargo test
```

Baseline: **39 tests** (verified 2026-08-06).

## Verify

- [ ] A pinned IP resource behaves **identically** — same routes, same handshake, same logs (regression).
- [ ] An FQDN resource is reachable by name via a `hosts` entry; the connector logs `resource_id` +
      `hostname` + the resolved address.
- [ ] Responses arrive at the app (proves the source rewrite) — not just "the tunnel opened".
- [ ] **Restart stability:** `down` → `up` → the same hostname keeps the **same** synthetic IP.
- [ ] **Regression test — a restart must not remap a synthetic IP to a different resource.** This is the
      acceptance criterion that matters most in this phase; write it as an automated test, not a manual
      check.
- [ ] Delete a resource, create a new one → the new one does **not** immediately inherit the freed
      synthetic IP (quarantine).
- [ ] Unmanaged traffic is unaffected; an unknown synthetic IP is refused, not passed through.
- [ ] `down` removes the CIDR route, the `ip rule`, and the nft table completely (`ip route show table
      105` empty, `nft list table inet <ZECURITY_TABLE>` gone).
- [ ] Exactly **one** tunnel per app connection (the Gate 1 loop regression check).
- [ ] 🚩 **This is the first end-to-end exercise of Phases 6 and 7.** Expect to find bugs there, not here.

## Notes

- Never log or persist a synthetic IP **as identity** — log `resource_id`. The synthetic IP is a local
  addressing artifact; it is meaningful only inside this client.
- Do not touch `client/src/relay_pool.rs`, `client/src/transport.rs`, or `relay/**`. The relay sits below
  the identity layer: the client opens the authenticated stream first and writes the handshake *inside*
  it, so the relay never parses it and never sees a `resource_id`.
- Stage 3 (Phases 11–12) replaces the `hosts` entry with a real DNS responder. Nothing in this phase may
  **depend** on DNS existing — Stage 2 must be shippable standalone.

## Post-Phase Fixes

_(none yet)_
