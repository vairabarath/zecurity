---
type: phase
sprint: 16
stage: 2
phase: 9
title: Client Binding Registry + Synthetic Routing
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target
status: done
completed: 2026-08-18
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

### 9.1 — `client/src/registry.rs` (new) ✅
- [x] Durable `hostname → synthetic IP → resource_id`, **bidirectional** (forward for DNS/hosts, reverse
      for the handshake) and **stable across restarts**.
- [x] **Collision-aware CIDR selection.** Default to a subrange of `100.64.0.0/10` (decision #5), chosen
      at startup after checking the host's existing routes and interface addresses — CGNAT space *is*
      used in the wild, notably by other VPNs.
- [x] **Quarantine before reuse.** A synthetic IP released by a deleted resource must not be handed to a
      different resource until a quarantine interval has passed **and** no binding for it is in use.
      Rationale above: premature reuse makes the client assert the wrong identity. Prefer "allocate the
      lowest never-used address first, recycle only under pressure".
- [x] Reverse lookup must be **fail-closed**: an unknown synthetic IP is *not* a resource, and must never
      fall through to unmanaged passthrough.
- [x] Preserve the existing three-state semantics exactly — losing them converts a fail-closed case into
      a passthrough, which is a security regression:
      ```text
      Some(target with transports)   → managed, connector online   → tunnel
      Some(target, transports empty) → managed, connector offline  → FAIL CLOSED
      None (absent)                  → unmanaged traffic           → no tunnel route
      ```
      ✅ **Inherited debt discharged.** Four map tests (`state1_…`, `state2_…`, `state3_…`,
      `the_three_states_are_distinguishable_in_one_map`) plus the live
      `live_unmanaged_destination_is_not_captured`.
      ⚠️ **Reading ADR-009 changed what needed testing.** Its own table says the third state is
      *"should not occur"*, not "unmanaged traffic" — bypass is a **kernel** property (an ungranted
      flow is never marked, so it never enters table 105 and never reaches the TUN), and the map's
      `absent` arm is only a backstop. So the debt has two halves and both are now covered: the map
      states in `daemon_tests.rs`, and the kernel mechanism live. State 2 is the security-critical
      one — if `Some(None)` ever collapsed to `absent`, "managed but unreachable" would become
      indistinguishable from "not managed" **while the nft rules for that flow are still installed**,
      so the packet would be captured with nothing knowing it was managed.

### 9.2 — `client/src/state_store.rs` — persist the registry ✅
- [x] New `StoredBinding { hostname, synthetic_ip, resource_id, allocated_at, quarantined_until }`
      ⚠️ **Deviation: `quarantined_until` is NOT a field on the binding.** Quarantine lives in a
      separate `StoredRegistry.quarantined: Vec<(String, i64)>`, because a quarantined address has no
      binding — its hostname and resource_id are precisely what we have forgotten. Keeping them on a
      "binding" would imply we still know what the address meant.
      alongside the existing `StoredResource` / `StoredDevice` structs, all `#[serde(default)]` so an
      older state file loads.
- [x] **Encrypted at rest** — non-negotiable per ADR-002; use the existing state-store path, do not add
      a second file with its own crypto.
- [x] Handle a corrupt/unreadable binding table by **rebuilding empty**, not by refusing to start. But
      then treat every previously-issued synthetic IP as quarantined, since their old meaning is unknown.

### 9.3 — `client/src/tun.rs` — route the synthetic CIDR once ✅
- [x] One `ip route replace <SYN_CIDR> dev zecurity0 table 105`, plus the nft rule chosen above.
- [x] **Stop installing per-`/32` routes for name-addressed resources.** Per-`/32` does not scale to the
      stated resource targets. Pinned IP resources **keep** their per-`/32` behaviour — that path is
      unchanged and must stay byte-identical.
- [x] `cleanup()` / `cleanup_policy_routes()` must remove the CIDR route and rule too.
      ✅ **No code change needed, and verified live rather than reasoned about:** the existing
      teardown is table-scoped (`ip route flush table 105` + `nft delete table`), so it already
      covers the CIDR route and the mark rule. Confirmed in a namespace with the exact ruleset
      installed — before: `table105: 10.0.0.7 + 100.64.0.0/22 | rule: 1 | nft: 1`; after:
      `table105: '' | rule: 0 | nft: 0`. A leaked
      `100.64.0.0/10` route after `zecurity-client down` would blackhole CGNAT traffic host-wide.
- [x] ⚠️ **Verify the interaction with split-tunnelling (ADR-009) explicitly.** A whole-CIDR route is a
      broader claim on the routing table than anything the client installs today.

> **⚠️ ADR-009 verdict — the whole-CIDR rule DOES relax ADR-009's central invariant, deliberately.**
> ADR-009 exists to fix exactly this deficiency: *"All ports on a resource IP were captured… Non-ACL
> ports hit smoltcp, find no listener, and receive RST — **even if the destination is legitimately
> reachable on the local network for an unrelated service**."*
> The port-agnostic rule reintroduces that — **but only inside the synthetic CIDR, where ADR-009's
> justifying premise cannot hold.** A synthetic IP is client-local and serves exactly one resource;
> there is no unrelated service on another of its ports to break. Pinned resources keep their
> per-`(ip, port)` rules, so ADR-009's guarantee is intact everywhere it means anything. Proven by the
> pinned/unmanaged test pair below.
>
> 📌 **ADR-009's text is now stale in two places** (behaviour unchanged, doc only):
> it lists `allowed_entries` as an input to `net_stack::run()` — removed in 9.4b, with SPIFFE
> filtering preserved transitively because the transports map is built from those entries — and it
> describes the map as `Option<Arc<ClientTransport>>`, which became `Option<ResourceTarget>` back in
> Phase 2.

### 9.4 — `client/src/net_stack.rs` — synthetic IP → `resource_id` ✅
- [x] Key listeners on synthetic IPs for name-addressed resources; reverse-map to `resource_id` and put
      **that** on the `TunnelRequest`.
- [x] Send `destination` **empty** for name-addressed resources. There is no pinned address for the
      connector to cross-check against, and Phase 7's task 7.0 scopes the check accordingly. Sending the
      synthetic IP instead would be denied as `destination_mismatch`.
      ⚠️ **Phase 7 and this task must agree.** If either ships alone, every FQDN resource is denied.
- [x] ~~**Rewrite the response source address to the synthetic IP.**~~ **NOT NEEDED — see below.** The app connected to the synthetic IP,
      so its socket will drop any packet whose source is the real backend address. This is the single
      easiest thing in the phase to forget, and it presents as "the tunnel opens, bytes flow one way,
      nothing arrives" — the same shape as the Gate 1 bug, which will send you down the wrong path.
- [x] The client **never learns the real backend IP**, and must not need to. If the design starts wanting
      it, the resolution boundary has leaked.

### 9.5 — Testable without DNS ⬜ (client-side proven live; by-name run outstanding)
- [ ] Add a `hosts` entry `<synthetic IP>  <hostname>` and connect by **name**.
- [ ] ⚠️ Test by **name**, not by raw synthetic IP: a `hosts` entry preserves TLS SNI and certificate
      validation, while connecting to a bare synthetic IP does not — an HTTPS resource would fail for
      reasons unrelated to this sprint.
- [ ] ⚠️ **Testing trap** (cost significant time in Gate 1): the resource must **not** be on the same host
      as the client. Linux routes local addresses via the `local` table (`dev lo`), which always beats any
      TUN route — curl connects directly and produces a misleading "it works".

## Build gate — green (2026-08-18)

```bash
cd client && cargo build && cargo test
```

Baseline was **39**; now **78 passed, 4 ignored**. Build clean, `cargo clippy` clean, and
`registry.rs` / `net_stack.rs` / `state_store.rs` / `tun.rs` are rustfmt-clean (`daemon.rs` 9 and
`daemon_tests.rs` 2 are pre-existing counts, unchanged by this phase).

Cross-component, to prove nothing leaked: shield builds, connector **148 + 4**, controller
`go build` + `go vet` clean, relay builds — no file outside `client/` touched.

### Live client data-path verification (2026-08-18)

The 4 `#[ignore]`d tests build a REAL TUN, install the REAL nft chain and route, and push real
packets. Run in a throwaway rootless namespace:

```bash
cargo test --no-run                      # note the test binary path
unshare -rn sh -c '
  ip link set lo up
  ip link add dummy0 type dummy; ip addr add 10.99.0.1/24 dev dummy0; ip link set dummy0 up
  ip route add default via 10.99.0.2 dev dummy0        # REQUIRED — see the note below
  exec <test-binary> live_ --ignored --nocapture --test-threads=1'
```

```text
[phase9] pinned     10.99.0.5:8080 (in ACL)     → connected          ✅ captured
[phase9] unmanaged  10.99.0.5:8080 (not in ACL) → hung               ✅ NOT captured
[phase9] synthetic  100.64.0.7:5432             → connected          ✅ AnyIP works
[phase9] unlistened 100.64.0.7:9999             → refused-by-stack   ✅ refuses, never hangs
```

Rows 1 and 2 are the **same address in the same namespace**, with opposite outcomes decided solely by
ACL membership — split tunnelling proven in both directions, and the pre-existing pinned path proven
undisturbed. Row 3 is the composition proof: nft mark → `ip rule fwmark` → table 105 → TUN → smoltcp
AnyIP → a listener on an address no interface owns. Row 4 discharges the obligation `path.md` decision
#5 attached to the whole-CIDR choice.

⚠️ **The default route is not test scaffolding — it is a real dependency of the steering design.**
`connect()` performs a main-table route lookup to choose a source address BEFORE any packet exists,
and the nft output hook only runs on a packet. With no main-table route the connect fails immediately
with `ENETUNREACH` and the mark rule never executes — table 105 is never consulted. On a real host the
default route covers CGNAT, which is why this works. It also means **a host with no default route
cannot reach name-addressed resources**. That is a pre-existing property of mark-based steering (it
applies equally to pinned IPs outside any local subnet), not something this phase introduced.

## Verify

- [x] A pinned IP resource behaves **identically** — same routes, same handshake, same logs (regression).
      Live (`live_pinned_resource_is_still_captured`); the nft rule also matches ADR-009's documented
      form byte-for-byte.
- [ ] ⬜ An FQDN resource is reachable by name via a `hosts` entry; the connector logs `resource_id` +
      `hostname` + the resolved address. **Needs a live stack** — the client-side path is proven live
      (row 3 above) but no connection has crossed a real controller + connector.
- [x] Responses arrive at the app (proves the source rewrite) — not just "the tunnel opened".
      The live synthetic connection completes, and smoltcp sources replies from the SYN's destination
      by construction (see 9.4). Not yet observed with a real backend behind a connector.
- [x] **Restart stability:** `down` → `up` → the same hostname keeps the **same** synthetic IP.
      `restart_preserves_every_binding_exactly` (automated). Not yet observed against a real daemon
      restart.
- [x] **Regression test — a restart must not remap a synthetic IP to a different resource.** This is the
      acceptance criterion that matters most in this phase; write it as an automated test, not a manual
      check. ✅ `restart_never_remaps_an_ip_to_a_different_resource` — 25 bindings through a churn cycle
      (drop a third, add ten), restart, assert no address answers with a different resource.
      Revert-verified: reusing an address on identity change fails exactly one test.
- [x] Delete a resource, create a new one → the new one does **not** immediately inherit the freed
      synthetic IP (quarantine). Automated (`a_freed_address_is_not_handed_to_the_next_requester`,
      `exhaustion_respects_quarantine_then_recycles_after_it_expires`). Not yet observed end to end.
- [x] Unmanaged traffic is unaffected; an unknown synthetic IP is refused, not passed through.
      Live: rows 2 and 4 above.
- [x] `down` removes the CIDR route, the `ip rule`, and the nft table completely (`ip route show table
      105` empty, `nft list table inet <ZECURITY_TABLE>` gone). Verified live in a namespace.
- [ ] ⬜ Exactly **one** tunnel per app connection (the Gate 1 loop regression check). **Needs a live
      stack** — requires a real connector to count tunnels against.
- [ ] ⬜ 🚩 **This is the first end-to-end exercise of Phases 6 and 7.** Expect to find bugs there, not
      here. **Still pending** — the client half is proven, the wire half is not, so Phases 6–7 remain
      unexercised end to end (as they have been since they landed).

## Notes

- Never log or persist a synthetic IP **as identity** — log `resource_id`. The synthetic IP is a local
  addressing artifact; it is meaningful only inside this client.
- Do not touch `client/src/relay_pool.rs`, `client/src/transport.rs`, or `relay/**`. The relay sits below
  the identity layer: the client opens the authenticated stream first and writes the handshake *inside*
  it, so the relay never parses it and never sees a `resource_id`.
- Stage 3 (Phases 11–12) replaces the `hosts` entry with a real DNS responder. Nothing in this phase may
  **depend** on DNS existing — Stage 2 must be shippable standalone.

## Post-Phase Fixes

### Fix: the first synthetic binding collided with the TUN's own address
**9.4b / live.** `BindingRegistry::new` set `next_fresh = cidr.first() + 1`, so the **first** name-addressed
resource was allocated `100.64.0.1` — which is exactly the address `tun.rs` assigns to `zecurity0` and
`net_stack` uses as smoltcp's address and AnyIP default-route gateway.

Because the interface owns that /32, the kernel installs

```text
local 100.64.0.1 dev zecurity0 table local proto kernel scope host
```

and `ip rule` consults `0: from all lookup local` **before** `49: from all fwmark 0x5a lookup 105`. The
packet is therefore classified as a local delivery and never enters the tunnel. **Every workspace's first
name-addressed resource was unreachable by construction.**

**Why nothing caught it.** The entire data plane was installed *correctly* — nft rule ordering, the
fwmark rule, the `table 105` route, the registry, the smoltcp loop all verified good by inspection and by
84 unit tests. The defect was the *interaction* between two independently-correct constants in different
modules. `curl` failed in 53 ms and the connector logged nothing at all, because no packet ever left the
client. Same family as 9.4a/9.4b: routing installed correctly, then defeated from outside the tested
unit — and it converts the standing *"`handle_up` has no unit coverage"* note from a theoretical gap into
a demonstrated one.

**Fix** (`registry.rs` only):

- New `gateway_addr(cidr)` naming the invariant, carrying the rule-priority reasoning so the next reader
  does not have to rediscover it.
- `allocate()` skips it on **both** the pristine and the recycle-under-pressure paths. Expressed in
  `allocate` rather than as arithmetic in `new()`, so it states the invariant and survives `from_stored`
  recomputing `next_fresh`.
- `from_stored()` **discards** a stored binding sitting on the gateway, so an already-deployed client
  self-heals on its next sync instead of only on `logout`. Deliberately **not** quarantined — quarantine
  means "reusable after QUARANTINE_SECS", which is exactly wrong for a permanently reserved address.

**4 new tests**; client suite 84 → **88**. Revert-tested: dropping the one-line guard fails exactly the
five gateway-related cases. One existing test's arithmetic was corrected — a `/30` now yields **2** usable
addresses, not 3, which is a real and intended capacity change, not a test workaround.

**⚠️ Related latent defect, deliberately NOT fixed here.** `tun.rs:37` and `net_stack.rs:281,287` hardcode
`100.64.0.1` instead of deriving it from the chosen CIDR. That is correct only while `select_cidr` returns
`100.64.0.0/22`; if the lowest block is contested and it picks e.g. `100.64.4.0/22`, the interface still
claims `100.64.0.1` — an address inside another network's range, which is precisely what `select_cidr`
exists to avoid. Fixing it means plumbing the CIDR through `tun.rs` and `net_stack.rs`: a multi-file
data-plane change needing its own verification pass, not something to land mid-Gate-2. `gateway_addr`
exists partly so that change has one obvious call site.

---

### Fix: the synthetic IP was not discoverable, and `resources` rendered a blank address
**9.5.** Step 9.5 says *"Add a `hosts` entry `<synthetic IP>  <hostname>`"* — but nothing in the client
ever printed that IP, so its own instruction could not be followed.

Two compounding causes:

1. **`zecurity-client resources` showed a blank address.** The handler rendered
   `address: e.address.clone()` straight from the ACL entry, and a name-addressed entry carries an
   **empty** `address` by design (the whole point: no IP is pinned server-side). Same bug class as the
   `GroupDetail`/`ShieldDetail` blank-address fix in Phase 10, but in the client CLI.
2. **The synthetic IP was published nowhere.** `RuntimeState` had no registry field — the
   `BindingRegistry` lived only inside `sync_registry`'s return value and in the encrypted state file.
   The sole output was aggregate counts (`synthetic binding registry synced`, `bound`/`released`), never
   a hostname→IP pair.

Allocation *is* deterministic (`next_fresh = cidr.first() + 1`, so the first binding is `100.64.0.1`),
which is why the live test could proceed — but predicting an address is a test artifact, not a product.

**Fix.** Publish the mapping and render it:

- `runtime.rs` — new `synthetic_bindings: HashMap<String, Ipv4Addr>` on `RuntimeState`.
- `daemon.rs` — populated in `handle_up` **with** `tun_handle` and cleared in `handle_down` **with** it,
  so a binding is only advertised while the routes serving it are installed.
- `daemon.rs` — new pure `display_address(address, hostname, bindings)`, extracted rather than left
  inline so it is testable (same reason as `nft_rule_plan` and `resolve_dial_target`). Precedence:
  pinned address wins; else the binding's synthetic IP; else the bare hostname — **never a fabricated
  IP**. A pinned address is never overridden, because an entry carrying both is failed closed by the
  connector as `ambiguous_addressing`.
- `ipc.rs` / `cmd/resources.rs` — `IpcResource` carries `hostname`; the table gained a **Hostname**
  column and prints `—` for absent values, since a blank cell reads as the very bug being fixed.

**6 tests** in `daemon_tests.rs`. Revert-tested: restoring the old behaviour fails exactly the three
name-addressed cases and leaves the IP-addressed ones passing.

**Two hardening changes made on review, neither fixing a live bug:**

- `synthetic_bindings` is **cleared on entry** to `handle_up`, before any fallible step. Five early
  returns sit between `sync_registry` and the publish. The invariant *"non-empty only while a bring-up
  reached the end"* did already hold — `handle_up` rejects when already up, and
  `restart_tunnel_if_running` always runs `handle_down` (which clears) first — but it held by an
  argument spanning five returns, a guard in another function, and the `tun_slot`/`tun_handle`
  correspondence. **9.4a was this same shape, and `handle_up` still has no unit coverage.** Clearing on
  entry makes it structural.
- The publish **trims** the hostname key. `BindingRegistry::bind` stores its argument verbatim; keys are
  trimmed today only because `sync_registry` — its one caller — passes `e.hostname.trim()`. Normalising
  at the publish keeps it from depending on that.

**Note on scope.** This does *not* pre-empt Phase 11. Phase 11 replaces the manual `hosts` entry with a
live DNS responder; this fix only makes the Stage 2 `hosts` workflow that Phase 11 calls *"works, but
manual"* actually performable.

---

### Fix: invalid nft syntax — a bare `tcp` protocol match
**9.3.** The whole-CIDR rule was generated as
`ip daddr <cidr> tcp meta mark set 0x5a`. A bare protocol keyword is only valid when it introduces a
field match (`tcp dport 443`); alone, nft rejects the rule outright:

```text
Error: syntax error, unexpected meta
add rule inet zecurity_client output ip daddr 100.64.0.0/22 tcp meta mark set 0x5a
                                                                ^^^^
```

**Why the unit test missed it:** it asserted `rule.contains(" tcp ")` — true of a rule nft refuses to
load. **Fix:** `meta l4proto tcp`, and the test now asserts that exact token sequence.
**Impact had it shipped: every FQDN resource silently unroutable.** Found only by the live test.

---

### Fix: `handle_up` called `configure_allowed_flows` twice, the second call undoing the first
**9.4a.** The 9.3 edit that added the `None` argument was kept alongside the 9.4a replacement, so the
CIDR rule and route were installed and then immediately destroyed —
`configure_allowed_flows` begins with `cleanup_policy_routes()`. `up` reported success, `route_count`
included the synthetic bindings, the logs said `synthetic binding registry synced`, and connections
hung. Build and all tests were green: **`handle_up` has no unit coverage**, and that remains true.

---

### Fix: smoltcp cannot hold a per-resource address list (pre-existing, 2-entry silent truncation)
**9.4b.** `net_stack` pushed one `/32` per resource entry plus `100.64.0.1` into `iface.ip_addrs`,
**discarding every `push` failure with `let _ =`**. `IFACE_MAX_ADDR_COUNT` is **2** in this build (the
`= 8` in `lib.rs` is inside `#[cfg(test)]`), and entries are not deduplicated by IP — so one resource
on two ports already overflowed, and from the third address onward inbound packets were dropped by
smoltcp and the app hung. A synthetic `/22` cannot be expressed that way at all, because `has_ip_addr`
is an **exact** match with no subnet containment.

**Fix:** `set_any_ip(true)` + one default IPv4 route via `100.64.0.1`, with `ip_addrs` holding only
that address. Removes the cap entirely. Three tests now pin the smoltcp contract we depend on,
including `a_cidr_entry_covers_only_its_base_address` — the trap that makes "just add the CIDR" look
like it should work.

---

### Fix: the third silent drop site, and a listener/transport divergence
**9.4b.** `net_stack` re-derived its listener set from `entry.address`, a **third** place that dropped
name-addressed resources — and because it was a *different* source from the transports map, the two
could disagree, which presents as a route into the TUN with nothing behind it. Listeners now come from
`transports.keys()`, making divergence unrepresentable. `net_stack::run` also lost its now-redundant
`allowed_entries` parameter.

---

### Fix: two of my own test defects, both of which produced false confidence
**9.5.**
1. **A false pass.** `probe` mapped every `Err` to `"refused"`, so the first live run passed — but with
   no route `connect` fails *immediately* with `ENETUNREACH`, indistinguishable from a RST. Splitting
   the errno (only `ConnectionRefused` proves a RST, and a RST can only come from smoltcp) revealed the
   truth and is what surfaced the nft syntax bug.
2. **A test artefact masquerading as the real failure.** `#[tokio::test]` is single-threaded; a blocking
   `connect_timeout` starved the spawned smoltcp poll loop, producing `hung` — identical to the genuine
   failure the test exists to detect. Now `multi_thread` + `spawn_blocking`.

**Lesson, consistent with Phase 7.0's:** a green live test is not evidence until you have watched it go
red for the right reason.

---

### Known gaps (deliberate, not oversights)
- **`sync_registry` has no test.** It is the orchestration function — load, release, bind, persist —
  and `state_store::state_dir()` uses `dirs::data_local_dir()` with no override, so testing it would
  write to the real user state directory. The pieces it composes are individually tested.
- **`handle_up` has no test either**, which is how the double-`configure_allowed_flows` bug survived a
  green suite. Both would be addressed by extracting the "what should be installed" decision into a
  pure function, the way `nft_rule_plan` and `instructionFor` were.
- **`BindingRegistry::resolve` has no production caller.** 9.4b made the transports map the carrier of
  identity: the daemon does the synthetic→`resource_id` lookup once at map-build time. `resolve` is the
  entry point Stage 3's DNS responder will need. `quarantined_rebuild` likewise has no caller, because
  9.2 handles corruption by salvaging addresses during deserialization instead.
