---
type: planning
status: in-progress
sprint: 16
progress: Stage 1 complete (Gate 1 fully closed 2026-08-10) · Stage 2 phases 4–9 complete · **Phase 8's wire hop VERIFIED on a live stack 2026-08-20** · Phase 10 code complete + pushed (`36fb39e`) · **outstanding: Gate 2** — the live E2E, which subsumes Phase 9's by-name run
solo: true
owner: M3
tags:
  - sprint16
  - dependencies
  - execution-path
  - data-plane
  - dns
  - fqdn
  - resource-identity
  - pending-14
---

# Sprint 16 — Identity-Based Resource Access & FQDN Resources (PENDING-14)

> **Read this before writing a single line of code.**
> Source of truth: `.zecurity-obs/pending/PENDING-14-FQDN-Resource-Access.md` (**Option A**).
> Branch: `feat/pending-14-fqdn-resource-access` (copy of `fixed-pendings`).
> Architecture record: **ADR-023** (to be written from this plan) — supersedes the *routing* role of
> **ADR-022** once Stage 2 lands.
> **Solo sprint — M3 (Yogesh).** Go + Rust + frontend, executed strictly in phase order.

## Sprint Goal

Move the data plane from **IP-centric** to **identity-centric**, then use that to support resources
addressed by **FQDN with dynamic backends**.

Today a resource *is* an IP. The client keys transports on `(Ipv4Addr, u16)` and **silently drops any
ACL entry whose address doesn't parse as IPv4** — so a hostname-only or dynamic-IP resource cannot be
expressed at all. The handshake also sends a **client-supplied destination string** which the
connector then dials.

```text
BEFORE                                        AFTER
------                                        -----
handshake: { destination: "10.0.3.7", ... }   handshake: { resource_id, port, protocol }
connector dials the client's string           connector dials what ITS OWN ACL says for that id

resource = an IP, pinned in the DB            resource = an identity; the endpoint is resolved
  → dynamic backend goes stale                  live at the connector (DNS | Static)
  → IP change ⇒ ACL bump ⇒ tunnel restart       → backend IP changes touch NOTHING upstream

FQDN resource: impossible (entry dropped)     FQDN resource: client DNS → synthetic IP →
                                                resource_id → connector resolves → connect
```

## Solo Scoping — read this first

This is **~30–35 files across 5 components**. For one person that is not a one-week sprint. Treat the
three stages as **independently shippable milestones**, each mergeable on its own:

| Stage | Scope | Files | Ships standalone value | Realistic solo effort |
|---|---|---|---|---|
| **1** | Identity on the wire | ~4 | **Yes** — security hardening (connector stops trusting client-supplied addresses) | Small (days) |
| **2** | Delivery split + connector resolution + synthetic routing | ~20 | **Yes** — FQDN/dynamic-IP resources work (addressed via synthetic IP / hosts entry) | **The bulk of the sprint** |
| **3** | Client DNS responder + OS DNS integration | ~7 (mostly new) | Yes — type-the-hostname UX | High risk, platform-specific |

**Recommendation:** ship **Stage 1** first as its own PR, then **Stage 2**. Consider deferring
**Stage 3 to Sprint 17** — it is the only stage with genuinely new, OS-specific subsystems, and it buys
UX rather than capability. Stopping after Stage 2 leaves a coherent, useful system.

## Why identity-on-the-wire comes first

Two verified facts make Stage 1 both cheap and the foundation for everything after:

1. **`ACLEntry.resource_id` already exists** (field 1) — the connector already logs it. **No proto
   change needed for Stage 1.**
2. **The tunnel handshake is plain serde JSON**, not protobuf (`TunnelRequest`/`TunnelResponse` in
   `client/src/net_stack.rs`). Extending it is a struct field, not a schema migration.

So Stage 1 is ~4 files for a standalone **security** win: the connector stops dialing a
client-supplied address (a confused-deputy / SSRF-shaped surface).

## Key Design Decisions

| Decision | Detail |
|---|---|
| Delivery vs Discovery | **Shield is not a resolver — the Shield *is* the endpoint.** Protected resources are delivered over the existing Shield session; only connector-reachable resources need endpoint *resolution*. |
| Reuse `route_type` | `ACLEntry.route_type` (field 7, `"connector"`\|`"shield"`) is **already** the Protected/ConnectorReachable discriminator. Extend its semantics — do **not** invent a parallel `delivery` field. |
| Resolution point | **At the connector, per connection, TTL-cached.** Never in the control plane: endpoint churn must never bump the ACL version (that fires `restart_tunnel_if_running` → fleet-wide tunnel drops). |
| Resolver is pluggable | `resolver { type: dns\|static, config }`; `k8s` is a later type behind the same interface. |
| Synthetic IPs are client-local | The **client** allocates them (only the client can see local-network collisions with the synthetic CIDR). The controller **never** allocates, stores, or sees a synthetic IP. |
| Binding registry, not a cache | `hostname → synthetic IP → resource_id` must be **durable** and **reuse-safe**. It is security-critical: its integrity decides which identity the client asserts. |
| Client-facing name ≠ backend spec | `hostname` (what the app/DNS/TLS uses) is hoisted onto the resource. The backend endpoint spec lives in `resolver.config`. They are frequently different strings. |
| Relay is below the identity layer | The client opens the authenticated stream first, then writes the handshake **inside** it — the relay never parses it and never sees `resource_id`. **No relay files change this sprint.** |
| ADR-022 stays until Stage 2 | It is load-bearing while the client still routes real IPs. Its routing role becomes vestigial (not deleted) after Stage 2. |
| No new RPCs | Everything rides existing ACL delivery + the existing tunnel handshake, per the standing rule. |

## Architectural Invariants (must hold; test them)

1. **Lookup before validate.** `resource_id` → resolve to a resource → *then* validate principal +
   port + protocol. Policy lives on the resource; you cannot authorize before you look it up.
2. **Validate before resolve.** Resolution/shield-selection never runs for an unauthorized request.
3. **Protected never falls back to direct.** If every shield for a Protected resource is offline, the
   connector **fails closed**. A direct-dial fallback would silently bypass shield enforcement.
4. **`requested_host` (if adopted) is pattern-validated** against the resource's declared pattern —
   otherwise the wildcard field reintroduces free-form dialing.
5. **One policy decision per flow.** The response path is pure byte movement: no re-authorization,
   no re-resolution, no new transport choice (the request's transport carries the response).
6. **Binding registry is durable; synthetic IPs are quarantined before reuse.**
7. **Default-deny preserved.** Unknown `resource_id`, missing ACL, or unresolvable endpoint → deny.

## Execution Path (strictly sequential)

> Solo rule: **do not start a phase until the previous phase's build gate is green.** Each stage ends
> in an E2E gate that is the merge point.

### ── STAGE 1 — Identity on the Wire ──

#### Phase 1 — Connector accepts `resource_id` (tolerant)
> See [[Sprint16/Member3-Go-Rust/Phase1-Connector-Identity-Handshake]].
- [x] **1.1** `connector/src/policy/mod.rs` — **`ResourceAcl` += `address`** (its absence is the root
      cause: the lookup result carries no address, so the handler must use the client's string);
      add `resolve_by_resource_id(resource_id, port, protocol)` reusing the **existing** private
      `find_entry_by_id`; populate `address` in `resolve_resource` too.
- [x] **1.2** `connector/src/device_tunnel.rs` — `TunnelRequest` (line 41) gains **optional**
      `resource_id`; reorder `handle_stream` (line 133) to lookup → authorize → destination-cross-check
      → branch; dial **`acl.address`**, not `req.destination` (audit every `req.destination` use).
- [x] **1.3** Keep the legacy destination path working (tolerant reader) so the client can roll after.
- [x] **Gate:** `cd connector && cargo build && cargo test`
- [x] 📌 **Already exists — do not rebuild:** `is_allowed(resource_id, client_spiffe_id)`
      (`policy/mod.rs:42`) already performs identity-based SPIFFE authorization, and
      `find_entry_by_id` (`:109`) already does the id lookup. **Real scope: ~1 new function +
      1 struct field + 1 handler reorder.**

#### Phase 2 — Client sends `resource_id`
> See [[Sprint16/Member3-Go-Rust/Phase2-Client-Identity-Handshake]]. Depends on Phase 1.
- [x] **2.1** `client/src/net_stack.rs` — `TunnelRequest` += `resource_id`; populate it.
- [x] **2.2** `client/src/daemon.rs` — thread `resource_id` through `build_transports_by_resource`
      (it is already on the ACL entry — pass it along, don't re-derive).
- [x] **Gate:** `cd client && cargo build && cargo test`

#### Phase 3 — Connector requires `resource_id`
> See [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]].
> Depends on Phase 2 being deployed everywhere.
- [x] **3.1** `device_tunnel.rs` — reject a handshake with no `resource_id` (default-deny).
- [x] **3.2** Distinct denial reasons: unknown id · port/proto mismatch · unauthorized SPIFFE ·
      destination mismatch.
- [x] **Gate:** `cd connector && cargo build`
- [x] **🚩 GATE 1 (E2E, merge point) — PASSED.** Authorization verified 2026-08-05; byte transfer
      verified 2026-08-06. On a live stack (controller + connector + client, one LAN, no relay) the
      client asserts `resource_id`, the connector logs `auth_path="resource_id"` → `access allowed` →
      `tunnel_opened ok`, and dials **its own ACL's address**. Phases 1–3 confirmed end-to-end.
      - [x] **Byte transfer — RESOLVED (2026-08-06).** `HTTP 200 in 0.067s`; exactly **one** tunnel per
            connection (was ~10). Root cause was a routing loop from running the client and connector on
            the **same host**: the client's nft chain matches on `(daddr, dport)` for every process, so
            the connector's own egress was captured into the client's TUN. Fixed with a shared
            `CONNECTOR_EGRESS_MARK = 0x5b` (connector sets `SO_MARK`, client returns early on it) plus a
            co-location warning. **Not caused by this sprint.** Full analysis:
            [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]].
      - [x] **Negative cases — DONE (2026-08-10, in Phase 7).** All five now covered by the
            `device_tunnel.rs` test harness: `missing_resource_id` · `unknown_resource` (asserted to
            attempt **no dial**) · `destination_mismatch` · `unauthorized_spiffe` · shield route with
            every shield offline **fails closed** as `SHIELD_NOT_ATTACHED` and is never resolved.
            They were deferred while the data plane was broken; writing them alongside Phase 7 was what
            surfaced the 7.0 correction recorded there.

### ── STAGE 2 — Delivery Split + Connector Resolution ──

#### Phase 4 — Migration 030 + resource model
> See [[Sprint16/Member3-Go-Rust/Phase4-Migration-030-Resource-Model]].
- [x] **4.1** `controller/migrations/030_fqdn_resources.sql` — add `hostname`, `resolver` (jsonb),
      `local_target`; make `host` nullable for FQDN resources; keep `route_type` semantics.
      *(027–029 are taken by Sprint 14 — **030 is the next free number**.)*
      Also replaces the `(tenant_id, remote_network_id, host, name)` unique index with one on
      `COALESCE(host, hostname)` — NULLs are distinct in Postgres, so the old index would have
      allowed unlimited duplicate FQDN resources.
- [x] **4.2** `internal/resource/store.go` — Create/Update carry `hostname` + `resolver`;
      `AutoMatchShield` applies only to IP-hosted (Protected-eligible) resources.
      ⚠️ Making `host` nullable breaks **every** `Scan` into a non-nullable Go `string`; all
      read sites now `COALESCE(host, '')`.
- [x] **4.3** Decide `shield_ids[]` shape — **join table** `resource_shields`, written alongside
      the singular `shield_id` while both exist.
- [x] **Gate:** `cd controller && go build ./...` — **PASS**
      *(Covered by `internal/resource/create_addressing_test.go`, DB-gated on
      `RESOURCE_TEST_DATABASE_URL`.)*

#### Phase 5 — Proto + ACL emission + GraphQL
> See [[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]].
- [x] **5.1** `proto/client/v1/client.proto` — `ACLEntry` **add fields 11+**: `hostname` (11),
      `resolver` (12, nested `ACLResolver` message: `type` + `config`). **Fields 1–10 are in use;
      never renumber.** `reserved 13` (see the design correction below), `reserved 14` for
      `pattern` if wildcards are adopted.
- [x] **5.2** `buf generate` (Go stubs) + `cargo build` (Rust stubs, via build.rs).
- [x] **5.3** `internal/policy/store.go` + `compiler.go` — select and emit the new fields.
      Also `graph/resolvers/policy_helpers.go`, the **second** resource read path, whose raw
      `SELECT r.host` would hard-fail on a NULL host (see design correction below).
- [x] **5.4** GraphQL schema + `resource.resolvers.go` create/edit + `helpers.go` presenter;
      regenerate with **`make gqlgen`** (`go generate ./graph/...` is a no-op — no directive exists).
- [x] **Gate:** `cd controller && go build ./... && go vet ./...` — **PASS**
- [x] **Rust gate (was missed):** `cd connector && cargo build && cargo test` — **PASS (89 + 4)**.
      The original Phase 5 commit contained zero Rust files, so `cargo test` was left broken: the
      exhaustive `AclEntry` literal in `policy/mod.rs`'s test helper needs `hostname: String::new(),
      resolver: None`. `cargo build` passed (the literal is under `#[cfg(test)]`), which is why the
      Go-only gate did not catch it. **A proto change is never Go-only.**
- [x] 💡 **Solo tip:** hand-insert one FQDN resource row via SQL here — it unblocks Phases 6–9 without
      waiting on the UI (Phase 10). *Done: scratch DB `ztna_p5test` built from migrations 001–030.*

> **⚠️ Design correction — `local_target` does NOT belong in `ACLEntry`.**
> 5.1 originally listed `local_target` alongside `hostname`/`resolver`, because migration 030
> adds all three columns together. That grouping is right for the **database** (a shared store)
> and wrong for the **wire** (point-to-point). `ACLSnapshot` reaches only clients and connectors;
> neither dials a resource's on-host address. The **Shield** does, and Shields receive
> `shield.v1.ResourceInstruction` — built by the controller in
> `internal/connector/control_stream.go` — and never see an `ACLEntry`.
> Keeping it there would have leaked an internal loopback address to every client, churned the
> ACL version on every edit, and still not delivered it to the Shield.
> **Resolution:** field 13 removed before it shipped and marked `reserved 13`. `local_target`
> moves to Phase 8 (see 8.0). `TestACLRelevantUpdate`'s `local_target only → false` case is the
> guard that fails if it reappears.
> **Rule for later phases:** justify each new wire field by asking *"who receives this message,
> and does each recipient need it?"* — never by how the data is grouped in the DB. Build, vet,
> `buf breaking` and even mutation tests cannot catch a misplaced field; they only prove that
> what you emit arrives.

**Phase 5 test coverage:** `internal/policy/compiler_fqdn_test.go` (`TestParseResolver` ×8 no-DB,
`TestCompileACLSnapshot_FQDNAddressing` ×3 DB-gated on `PKI_TEST_DATABASE_URL`),
`graph/resolvers/resource_addressing_test.go` (×26), `internal/resource/acl_relevance_test.go` (×12).
**Known gaps, deferred to Gate 2:** `loadResourceByID` is manually verified but has no regression
test; the GraphQL `createResource` path with `hostname` has never been executed end-to-end.

#### Phase 6 — Connector resolver module ✅ DONE (2026-08-08)
> See [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]].
- [x] **6.0** `connector/src/policy/mod.rs` — `ResourceAcl` += `hostname` + `resolver` (populated in
      `resource_acl_from`), plus **`remote_network_id`** (not in the original task list — the resolver's
      cache key cannot exist without it). `ResourceAcl::addressing()` returns
      `Addressing::{Pinned, Named, Invalid}`; a row with BOTH `address` and `hostname` is
      `Invalid("ambiguous_addressing")` — **fail closed, never "address wins"**.
      Exactly-one is enforced *only* at the GraphQL layer (`validateAddressing`); the DB check is
      at-least-one, so any SQL-inserted row — which Phase 5's own solo tip recommends — can carry both.
- [x] **6.1** new `connector/src/resolver.rs` — `dns` + `static`; TTL-aware cache keyed by
      `(remote_network_id, name, family)`; TTL clamp (5s–300s); negative cache; single-flight per key.
      **Dependency decided: `hickory-resolver` 0.26.1**, added as
      `--no-default-features --features tokio,system-config`. `tokio::net::lookup_host` was rejected —
      it exposes no record TTL, which makes the clamp and stale handling unimplementable.
      *Verified: hickory pulls no TLS stack, and the `aws-lc-rs` already in the tree came from
      `reqwest`/`hyper-rustls`, not from this change.*
      ⚠️ **Deviation from spec:** the negative cache covers **NXDOMAIN *and* NODATA**, not NXDOMAIN
      alone. NODATA is equally authoritative and equally likely to be retried per connection, so
      leaving it uncached let a deleted A record re-query on every connection attempt. The reason is
      *stored* alongside the deadline so a suppressed NODATA is never reported as NXDOMAIN.
- [x] **6.2** Typed errors, all non-collapsible: `nxdomain` · `resolver_unavailable` ·
      **`resolver_failure`** (SERVFAIL/REFUSED — added; same policy as unavailable but a different
      system to go look at) · `no_address_record` · `unsupported_resolver` ·
      `invalid_resolver_config`. **No `dial_failed` variant** — resolution succeeding while the dial
      fails is Phase 7's concern, and folding it in here would conflate "DNS is broken" with "the
      resource is down".
      Policy is expressed once, as `ResolveError::may_serve_stale()` / `invalidates_cache()`, which are
      asserted disjoint: **serve stale only for failures that say nothing about the name; discard the
      cached address for answers that say the endpoint is gone** (NXDOMAIN *and* NODATA).
      ⚠️ **Deviation from spec:** implemented as **stale-on-error + a 5s `STALE_RETRY` backoff**, not
      background revalidation. Without the backoff, "serving stale" still made *every* connection during
      an outage wait out a full resolver timeout first. Bounded by `STALE_MAX` (1h) so a permanently
      dead resolver eventually fails closed. True prefetch-before-expiry is a later optimisation; the
      cache shape already supports it.
- [x] **6.3** IPv4-only, structurally: explicit `Family::V4` in the cache key and `RData::A`-only
      extraction — not an accident of taking the first result.
- [x] **Gate:** `cd connector && cargo build && cargo test` — **PASS: 128 unit + 4 integration**
      (baseline was 89 + 4). Zero warnings; zero clippy findings in the new files; `rustfmt` clean.
- [x] 📌 **Known limitations, deliberately not fixed** (see the phase file): the cache never evicts
      (bounded by distinct resource names over process lifetime, controller-supplied, not
      client-influenced); `static` with multiple addresses takes the first valid one (no round-robin);
      unknown `resolver.config` keys are tolerated while `server` is rejected.

#### Phase 7 — Connector delivery branch ✅ DONE (2026-08-10)
> See [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]].
> 📌 **Scope was wider than the File Map said:** `handle_stream` has **three** call sites, so threading
> `Arc<Resolver>` touched `device_tunnel.rs`, `quic_listener.rs:110`, `relay_handler.rs:183`
> **and `main.rs`** (two listener spawns + `RelayHandler::new`) — 4 files, not 1. Plus `resolver.rs`
> for `UnavailableBackend` and `Resolved::cache_hit`.
- [x] **7.0** Cross-check scoped to `!entry.address.is_empty()` at `device_tunnel.rs` — a
      name-addressed resource has no pinned address for a client-supplied `destination` to agree with.
      The check stays **fully strict** for every IP-pinned resource; it was scoped, not deleted.
      ⚠️ **Correction to this plan's earlier claim.** It said authorization *"currently denies every
      name-addressed resource."* **It did not.** The original arm read
      `!req.destination.is_empty() && req.destination != entry.address`, so with an empty `destination`
      the first clause was already false and the arm never fired. 7.0 is therefore **defensive
      hardening plus a Phase 9.4 contract**, not a fix for a live break. Discovered while writing the
      guard test: the first attempt sent `destination: ""` and passed with *and* without the fix — a
      test that guarded nothing. The real guard sends a **non-empty (synthetic) destination**, and was
      verified by reverting the scoping: exactly one test fails
      (`named_resource_is_not_denied_for_a_non_empty_destination`).
      **Phase 9.4 must still send `destination` empty** — that contract is unchanged.
- [x] **7.1** `device_tunnel.rs` — branches on **`route_type` first**: `"shield"` → existing shield
      session (**never falls back to direct**); `"connector"` \| `"direct"` → `Pinned` → dial address,
      `Named` → `resolver.resolve()` → dial, `Invalid` → deny. No `match resolver.type { …, shield }`
      exists. `connect_marked_tcp` (`CONNECTOR_EGRESS_MARK`) preserved on the new dial path, so the
      Gate 1 co-location loop cannot return. **Both** the TCP bridge and `relay_udp` take the resolved
      address — verified by grep that no `req.destination` survives as a dial target anywhere.
- [x] **7.2** Logs carry `resource_id` (identity) + `hostname` + the **resolved** address, never a
      synthetic IP. Resolution latency and cache hit/miss emitted at `debug` via a new
      `Resolved::cache_hit` field — see the Post-Sprint Fix below; this bullet was **missed on the
      first pass** and only caught by a verification round.
      📌 `AccessLogFields` was deliberately **not** extended: typed audit fields would need a
      `ConnectorLog` **proto** change, which is the same "who receives this message?" question that
      caught `local_target` in Phase 5. Structured `tracing` + `legacy_message` instead.
- [x] **Gate:** `cd connector && cargo build && cargo test` — **PASS: 148 unit + 4 integration**
      (baseline 129). Zero warnings; no new clippy findings; `rustfmt` clean;
      `cargo build --manifest-path relay/Cargo.toml` still green (no accidental coupling).
      📌 Phases 6–7 have **no E2E proof available** until Phase 9 — no client can express a
      name-addressed resource before the binding registry exists. Unit tests are the gate, deliberately,
      and the first real exercise of both phases is Phase 9.5's `hosts`-entry test.
- [x] 📌 **19 tests added.** A `#[cfg(test)] mod tests` now exists in `device_tunnel.rs` (there was
      none): a duplex-stream harness that drives the real framed-JSON handshake and asserts on the
      emitted `ConnectorLog`. **Gate 1's outstanding negative cases are now all covered** — see Gate 1
      above. Harness note: a fresh `CrlManager` reports `Unavailable` (fail-closed by construction) and
      would deny before authorization is ever reached, so every test calls the pre-existing
      `#[cfg(test)] install_test_cache(vec![])`.

#### Phase 8 — Shield `local_target` ✅ DONE (2026-08-15, commit `e89f941`)
> See [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]].
> 📌 **Scope was wider than the File Map said: 13 files, not 5.** Three exhaustive proto literals across
> two crates, three `check_port` sites, `control_stream.rs` threading, and a second proto message.
> Same undercount pattern as Phase 7 — worth treating as the norm.
- [x] **8.0** **Deliver `local_target` to the Shield** (moved here from 5.1 — see the design
      correction under Phase 5). `proto/shield/v1/shield.proto` — add `string local_target = 7`
      to `ResourceInstruction` (fields 1–6 in use; never renumber). Then
      `internal/resource/store.go` `BuildShieldSnapshot` selects the column, and
      `internal/connector/control_stream.go` populates it in **all three**
      `ResourceInstruction{}` construction sites (~lines 169, 220, 532).
      `buf generate` + `cargo build --manifest-path shield/Cargo.toml`.
      ⚠️ **This plan's claim that the generation bump "comes for free" was wrong** — `fingerprintDesired`
      is an explicit field list, not a struct hash. Without adding `LocalTarget` there, everything
      compiles, every test passes, and the generation never bumps, so **shields never re-apply**.
      Caught only by the phase file's own "**Verify** it changes" instruction.
      📌 The three construction sites are now one shared converter (`internal/connector/instruction.go`)
      with a reflection test, so the divergence the plan warns about is unrepresentable rather than
      merely discouraged. `controller/gen/` is **gitignored** — `buf generate` is a local step, not a
      commit artifact.
- [x] **8.1** `shield/src/resources.rs` — `validate_host` accepts the resource's `local_target`
      (`127.0.0.1` | own LAN IP). ⚠️ This touches a **non-negotiable rule** (`resource.host ==
      detect_lan_ip()`); the check must stay **equally strict**, only more explicitly sourced.
      ✅ `validate_host`'s body is **byte-identical** to its pre-phase version; a new `dial_target()`
      calls it **twice** and is now its only production caller. `host` is validated even when
      `local_target` is set — otherwise an instruction naming **another shield's LAN IP** would be
      applied here as long as it carried `local_target: "127.0.0.1"`.
      ⚠️ **`check_port` ×3 was not in the task list and the phase ships broken without it** — the health
      probe used `host`, so a loopback-only resource would dial correctly and be reported
      `status: "failed"` forever.
- [x] **8.2** `shield/src/tunnel.rs` — dial `local_target`.
      ⚠️ **Required an unplanned second proto field.** 8.2 demands the target "stored for that resource",
      but `TunnelOpen` carried no resource identity — so `string resource_id = 5` was added, and the
      connector passes `acl_entry.resource_id`. Tuple-matching on `(destination, protocol, port)` was
      rejected as ambiguous by construction. Also threads `resource_state` into
      `shield/src/control_stream.rs`.
      📌 Unknown `resource_id` **falls back with a warning rather than failing closed** — deliberate,
      documented, tighten later (Phase 1 → 3 sequence).
- [x] **Gate:** `cargo build --manifest-path shield/Cargo.toml` — **PASS.** Full cross-component:
      shield **31 + 4 gated**, connector **148 + 4** (unchanged), client **39** (untouched), relay green
      with zero files changed, controller build/vet/test green.
- [x] **Live verification (2026-08-15).** The headline claim is proven on a **real kernel with real
      nftables and a real socket**, inside the F21 namespace plus a `dummy0` LAN IP:
      `without local_target → status=failed` · `with local_target=127.0.0.1 → status=protected` ·
      `TCP connect to 127.0.0.1:42535 OK`. Both halves run together because **the contrast is the
      assertion**. F21 atomicity re-verified in the same namespace (0/89 samples saw the port undefended).
- [x] ✅ **Wire hop VERIFIED on a live stack (2026-08-20).** Controller + connector + shield enrolled on
      one host, service bound **only** to `127.0.0.1:8081` (LAN address confirmed unreachable).
      `localTarget=127.0.0.1` → resource acks **protected**; edited to the shield's own LAN IP → **failed
      (`port not listening`)**; edited back → **protected**. Shield generation `1 → 2 → 3` on each edit;
      a description-only edit left it at `3`. Connector ACL version **unchanged** across all three edits
      (last push `version=5`, seven minutes before the first edit) — the Phase 5 asymmetry holding in
      production. Also closed Phase 5's gap that `createResource`/`updateResource` with the new
      addressing fields had never run E2E.
      ⚠️ NOT claimed: that the pre-Phase-8 binaries reproduced the bug. They were installed and did ack
      `failed`, but the backing service was dead at that moment, so that run proves nothing. The
      conclusion rests on the controlled A/B above, which is stronger.
      ⚠️ **`strings <binary> | grep` is not a valid build check.** At `-O3` LLVM emits short string
      literals as instruction immediates, not `.rodata` — invisible to a byte search. Proven by injecting
      a 16-byte marker into a function whose own 5- and 7-byte literals could not be found. Verify by
      behaviour or by md5 against the artefact you just built.

#### Phase 9 — Client binding registry + synthetic routing ✅ DONE (2026-08-18)
> See [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]].
> **Largest and most security-sensitive phase of Stage 2.**
> 📌 **11 files, not the 5 the File Map listed** — `daemon_tests.rs` (5 call sites of a changed
> signature), `main.rs`, and a **third** silent drop site in `net_stack.rs` were all unlisted.
> 📌 **Client tests 39 → 78, plus 4 gated live tests** that build a real TUN and push real packets.
> 📌 **Four bugs found only by the live test, after the unit suite was green** — including invalid nft
> syntax that would have made every FQDN resource silently unroutable, and a pre-existing smoltcp
> 2-address cap that silently truncated the client's resource list. See the phase file's fixes.
- [x] **9.1** new `client/src/registry.rs` — durable `hostname → synthetic IP → resource_id`;
      stable across restarts; **quarantine before reuse**; collision-aware CIDR selection. Preserve the
      three-state semantics (`Some(with transports)` / `Some(empty)` = fail closed / `None` = unmanaged).
      📌 **Inherited debt — assert the three states here.** Phase 2's verify list still carries two
      unverified client-side items (connector-offline fails closed; unmanaged traffic untunnelled).
      `daemon_tests.rs` covers the map *shape* but not that distinction, and losing it converts a
      fail-closed case into a passthrough — a security regression. Phase 9 rewrites this exact code, so
      it owns the assertion.
- [x] **9.2** `client/src/state_store.rs` — persist the registry (encrypted at rest, per ADR-002); a
      corrupt table rebuilds empty **with everything quarantined**, rather than refusing to start.
- [x] **9.3** `client/src/tun.rs` — route the **synthetic CIDR once**; stop installing per-`/32`
      routes for FQDN resources (per-`/32` does not scale). Pinned IPs keep per-`/32` unchanged.
      ⚠️ **A route alone will not pull traffic into the TUN.** Steering is nft-mark based
      (`ip daddr … tcp dport … meta mark set` → `ip rule fwmark` → table 105). **Decide:** per-`(ip,port)`
      rules as today, or **one port-agnostic `ip daddr <SYN_CIDR>` mark rule** (recommended — constant
      ruleset size; then a non-ACL port on a synthetic IP must refuse cleanly, not hang). The
      `meta mark 0x5b return` rule must stay **first** or the Gate 1 co-location loop returns.
      ⚠️ Also verify the synthetic-CIDR route against **split-tunnelling (ADR-009)**.
- [x] **9.4** `client/src/net_stack.rs` — synthetic IP → `resource_id`; send `destination` **empty** for
      named resources (must match Phase 7 task 7.0); **rewrite the response source to the synthetic IP**
      (the app's socket will drop anything else — and this failure looks exactly like the Gate 1 stall,
      which will send you down the wrong path).
- [x] **9.5** Testable without DNS (client-side; by-name run outstanding) via a `hosts` entry → connect **by name** (this preserves TLS
      SNI/validation; connecting to a **raw** synthetic IP does **not**). ⚠️ Resource must **not** be on
      the client's host — the `local` table beats any TUN route and produces a false pass.
- [x] **Gate:** `cd client && cargo build && cargo test` — **PASS: 78 + 4 gated** (baseline 39)
- [x] 📌 **Regression test (acceptance-critical):** a restart must not remap a synthetic IP to a
      different resource. Automate it; a silent remap makes the client assert the wrong identity.

#### Phase 10 — Admin UI
> See [[Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources]].
- [ ] **10.1** Create/edit an FQDN resource: addressing **mode selector** (IP vs hostname — make
      violating exactly-one unrepresentable) + resolver **type dropdown** (`dns`|`static`) with
      type-specific config serialized to JSON. ⚠️ **Do not offer `shield` as a resolver type.**
      `localTarget` editable only for shield-delivered resources.
- [ ] **10.2** Show delivery type (Protected vs Connector-reachable) + resolver health/last error, with
      Phase 6's failure classes kept distinct. ⚠️ **Scope check:** there is no connector→controller
      transport for resolver health today; inventing one would breach the no-new-RPCs rule. Ship without
      live health and record the gap.
- [ ] **Gate:** `cd admin && npm run codegen && npx tsc --noEmit`
- [ ] **🚩 GATE 2 (E2E, merge point):** an FQDN resource created **through the UI** is reachable by name;
      **changing the backend IP requires no controller action, bumps no ACL version, and restarts no
      tunnel** (verify `acl_snapshot_version` is unchanged across a DNS change). Closes Phase 5's known
      gap that `createResource` with `hostname` had never run end-to-end.

### ── STAGE 3 — Name Access (consider deferring to Sprint 17) ──

#### Phase 11 — Client DNS responder
> See [[Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder]].
- [ ] **11.1** new `client/src/dns.rs` — UDP **and** TCP/53 (a truncated UDP answer is retried over TCP;
      UDP-only produces intermittent failures); managed `A` → synthetic IP; managed `AAAA` →
      **NODATA, not NXDOMAIN** (NXDOMAIN can suppress the `A` lookup entirely); TTL 30–60s; exact-name
      match (no wildcards yet); loopback-bind only — **never an open resolver**.
- [ ] **11.2** Unmanaged names never reach us (per-domain OS config, decision #4); if one does anyway,
      answer **REFUSED** — a forged NXDOMAIN breaks the user's unrelated DNS.
- [ ] **Gate:** `cd client && cargo build && cargo test`

#### Phase 12 — OS DNS integration
> See [[Sprint16/Member3-Go-Rust/Phase12-OS-DNS-Integration]]. **Highest-risk phase in the sprint** —
> the only one that mutates host-wide state outside our own interface.
- [ ] **12.1** new `client/src/os_dns.rs` — **per-domain** DNS config (never hijack all DNS);
      reliable teardown on **every** exit path incl. SIGKILL (persist the prior config; reconcile at
      startup, mirroring `tun.rs::cleanup_policy_routes()`); conflict handling with other VPNs =
      **refuse, never silently overwrite**. **Decide:** no-`systemd-resolved` hosts → back up and
      rewrite `resolv.conf`, or **refuse to enable OS DNS** and document the `hosts` fallback
      (recommended — `resolv.conf` rewrites race with NetworkManager/dhcpcd).
- [ ] **12.2** Interaction with split-tunneling (ADR-009) verified explicitly — pairs with 9.3. DNS makes
      this failure **silent**: the name resolves and the connection then hangs.
- [ ] **🚩 GATE 3 (E2E):** `dig managed.name` → synthetic IP; app connects through the tunnel;
      unmanaged names resolve normally; DNS settings restore cleanly on stop.

## File Map (what you touch, when)

| Phase | Files |
|---|---|
| 1, 3 | `connector/src/policy/mod.rs` (single file), `connector/src/device_tunnel.rs` |
| 2 | `client/src/net_stack.rs`, `client/src/daemon.rs` |
| 4 | `controller/migrations/030_fqdn_resources.sql` **(new)**, `internal/resource/store.go` |
| 5 | `proto/client/v1/client.proto`, `internal/policy/{store,compiler}.go`, `graph/**` |
| 6 | `connector/src/resolver.rs` **(new)**, `connector/src/policy/mod.rs` (6.0), `connector/Cargo.toml` |
| 7 | `connector/src/device_tunnel.rs` |
| 8 | **Actual: 13 files, not the 5 listed here.** `proto/shield/v1/shield.proto` (2 messages), `internal/resource/store.go`, `internal/connector/control_stream.go`, `internal/connector/instruction.go` **(new)** + `instruction_test.go` **(new)**, `internal/resource/snapshot_integration_test.go`, `graph/resolvers/resource.resolvers.go` + `resource_acl_coherence_test.go`, `shield/src/resources.rs`, `shield/src/tunnel.rs`, `shield/src/control_stream.rs`, `connector/src/agent_tunnel.rs`, `connector/src/device_tunnel.rs`, `connector/src/agent_server.rs` |
| 9 | `client/src/registry.rs` **(new)**, `state_store.rs`, `tun.rs`, `net_stack.rs`, `runtime.rs` |
| 10 | `admin/src/pages/Resource*.tsx` + gql |
| 11 | `client/src/dns.rs` **(new)**, `Cargo.toml` |
| 12 | `client/src/os_dns.rs` **(new)**, `daemon.rs`, `main.rs` |

**Do not touch:** `relay/**`, `client/src/relay_pool.rs`, `client/src/transport.rs` — the relay sits
below the identity layer and is deliberately out of scope.

## Final Build Gates

```bash
buf generate
cd controller && go build ./... && go vet ./... && go test ./internal/...
cd connector && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml
cd client && cargo build && cargo test
cd relay && cargo build            # must stay untouched — proves no accidental coupling
cd admin && npm run codegen && npx tsc --noEmit
```

## Acceptance Criteria

- [ ] The handshake carries `resource_id`; the connector **never dials a client-supplied address**.
      Unknown/unauthorized id, or mismatched port/protocol, is denied.
- [ ] Existing IP resources behave **identically** (same effective ACLs).
- [ ] An FQDN resource can be created, appears in the ACL snapshot **with no real backend IP**, and is
      reachable end-to-end.
- [ ] A backend IP change is invisible to the control plane: **no DB write, no ACL version bump, no
      tunnel restart** — verified by watching the ACL version across a DNS change.
- [ ] Protected resources are delivered via the Shield session and **never** fall back to direct dial,
      including when every shield is offline (fails closed).
- [ ] Resolver failures are typed (NXDOMAIN vs timeout vs no-A vs dial-fail) and **do not poison ACL
      state**; last-known-good is served on transient failure.
- [ ] The client binding registry survives a daemon restart with **stable** bindings; a recycled
      synthetic IP is quarantined first. **Regression test: a restart must not remap an IP to a
      different resource.**
- [ ] The response path performs **no** re-authorization and **no** re-resolution, and returns over the
      same transport (direct or the same relay session).
- [ ] The client presents responses as originating from the **synthetic IP**.
- [ ] No relay file changed; `cd relay && cargo build` still green.
- [ ] Stage 3 only: managed names → synthetic IPs, unmanaged names unaffected, OS DNS config fully
      restored on daemon stop.

## Decisions — settled during Phases 4–5

Decisions 1, 2 and 6 were **taken in code** during Phases 4–5; recorded here so they aren't re-litigated.

| # | Decision | Resolution | Where it landed |
|---|---|---|---|
| 1 | **Wildcards** (`*.internal`) — does the wire carry `requested_host`? | ✅ **Exact names only; field reserved.** `requested_host` is **not** on the wire. | `ACLEntry` `reserved 14` for `pattern`. Invariant #4 applies only if it is ever adopted. |
| 2 | `shield_ids[]` — array column or join table? | ✅ **Join table.** Written alongside the singular `shield_id`, which stays authoritative for existing readers. | `resource_shields` (migration 030), Phase 4.3 |
| 3 | **IPv6** | ⏳ Deferred. `AAAA → NODATA` in Stage 3; A-records only in Phase 6. | Phase 6.3, Phase 11.1 |
| 4 | **Unmanaged DNS** — per-domain OS config or full proxy? | ⏳ **Per-domain** (far less invasive) — settle the non-`systemd-resolved` fallback in Phase 12.1. | Phase 12.1 |
| 5 | **Synthetic CIDR** | ✅ A `100.64.0.0/10` subrange, collision-checked at startup (CGNAT is used in the wild). **Steering settled 2026-08-15: one port-agnostic whole-CIDR mark rule** (`ip daddr <SYN_CIDR> meta mark set ZECURITY_MARK`) — constant-size ruleset, which is the scaling problem 9.3 exists to fix. Accepted consequence: any port on a synthetic IP is steered into the TUN, so a port absent from the ACL has no smoltcp listener and **must refuse cleanly, not hang** — verify and record. | Phase 9.1, 9.3 |
| 6 | **Name collisions across remote networks** | ✅ **Rejected at create**, per remote network. | `UNIQUE (tenant_id, remote_network_id, COALESCE(host, hostname), name)` — migration 030 |
| 7 | **Per-principal ACL scoping** | ❌ **Not this sprint** — but it is the blocker for 100k resources / 50k clients. Track separately. | — |

### Still open (must settle before the named phase)

| Decision | Phase | Why it can't wait |
|---|---|---|
| What `destination` carries for named resources | **7.0 + 9.4** | Both halves must agree, or every FQDN resource is denied as `destination_mismatch`. |
| `resolver.config["server"]` — implement per-resource DNS servers, or fix the schema comment? | **10.1** | Phase 6 **rejects** `server` as `invalid_resolver_config` (silently ignoring it would resolve against the connector's own resolver — a different answer than the operator asked for). But [resource.graphqls](../../controller/graph/resource.graphqls) still shows `{"type":"dns","config":{"server":"..."}}` as the example. One of the two must change. |
| ~~How the synthetic CIDR is steered into the TUN (nft mark shape)~~ | ~~**9.3**~~ | ✅ **Settled 2026-08-15 — whole-CIDR mark.** See decision #5 above. |
| Non-`systemd-resolved` hosts: rewrite `resolv.conf` or refuse OS DNS? | **12.1** | Failure mode is the user's DNS left broken after our daemon exits. |

### Settled in Phase 6

| Decision | Resolution |
|---|---|
| Ambiguous addressing — a row with **both** `host` and `hostname` | ✅ **Fail closed.** `Addressing::Invalid("ambiguous_addressing")`; never "address wins". |
| DNS client crate | ✅ **`hickory-resolver` 0.26.1**, `--no-default-features --features tokio,system-config`. `lookup_host` rejected: no record TTL. |
| Does NODATA get negative-cached? | ✅ **Yes**, alongside NXDOMAIN, with its own reason stored. Both are authoritative "no endpoint" answers. |
| Stale-while-revalidate shape | ✅ **Stale-on-error + 5s retry backoff**, bounded by `STALE_MAX`. No background refresh task. |

## Deferred (explicitly out of scope)

- **Per-principal ACL filtering** (scale + least-disclosure) — whole-workspace ACL distribution does
  not scale to the stated targets; separate item.
- **K8s resolver** — interface ships in Stage 2; implementation later.
- **Wildcard matching** — field reserved, matching deferred (unless decision #1 says otherwise).
- **IPv6 / synthetic IPv6.**
- **Shield-as-segment-gateway** (a shield protecting hosts other than itself) — would put a resolver
  *inside* the shield; not now, but do not design against it.
- **Deleting ADR-022's resync** — its routing role becomes vestigial after Stage 2; removal is a later
  cleanup.
- **Connector selection policy** for the client→connector hop (unchanged this sprint).

## Phase Files

| Phase | File | Status |
|---|---|---|
| 1 | [[Sprint16/Member3-Go-Rust/Phase1-Connector-Identity-Handshake]] | ✅ done |
| 2 | [[Sprint16/Member3-Go-Rust/Phase2-Client-Identity-Handshake]] | ✅ done |
| 3 | [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]] | ✅ done (Gate 1 passed; negative tests outstanding) |
| 4 | [[Sprint16/Member3-Go-Rust/Phase4-Migration-030-Resource-Model]] | ✅ done |
| 5 | [[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]] | ✅ done |
| 6 | [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]] | ✅ done (128 + 4 tests green) |
| 7 | [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]] | ✅ done (148 + 4 tests green) |
| 8 | [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]] | ✅ done (31 + 4 gated tests green; wire-hop E2E outstanding) |
| 9 | [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]] | ✅ done (78 + 4 gated; by-name E2E outstanding) |
| 10 | [[Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources]] | 🟨 code complete (pushed `36fb39e`) — **Gate 2 outstanding**; resolver health deliberately not shipped |
| 11 | [[Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder]] | ⬜ Stage 3 — deferral candidate |
| 12 | [[Sprint16/Member3-Go-Rust/Phase12-OS-DNS-Integration]] | ⬜ Stage 3 — deferral candidate |

Bug record: [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]] (P0, **resolved** 2026-08-06).

## Post-Sprint Fixes

Overview only — each fix is documented in full in its phase file.

### Fix: the first synthetic binding collided with the TUN's own address
**Phase 9.4b / live.** `next_fresh = cidr.first() + 1` handed the first name-addressed resource
`100.64.0.1` — the address `zecurity0` itself owns. `ip rule 0 (local)` precedes `rule 49 (fwmark)`, so
that resource was delivered locally and never entered the tunnel: **every workspace's first FQDN resource
was unreachable by construction.** Invisible to 84 unit tests because each module was individually
correct. Fixed by reserving `gateway_addr(cidr)` in `allocate()` and discarding stored bindings on it.
Latent sibling left documented: `tun.rs`/`net_stack.rs` hardcode `100.64.0.1` rather than deriving it.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Fix: the synthetic IP was not discoverable, and `resources` showed a blank address
**Phase 9.5.** Step 9.5 instructs *"add a `hosts` entry `<synthetic IP>  <hostname>`"*, but nothing in
the client ever printed that IP: `RuntimeState` held no registry, and `zecurity-client resources`
rendered the ACL entry's `address`, which is empty for name-addressed resources. Published
`synthetic_bindings` on `RuntimeState` (set/cleared with `tun_handle`) and added a pure, tested
`display_address` with precedence pinned → synthetic → hostname, never a fabricated IP. Does not
pre-empt Phase 11, which replaces the manual `hosts` entry entirely.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Fix: unique index silently disarmed by making `host` nullable
**Phase 4.** Postgres treats NULLs as distinct, so `UNIQUE (tenant_id, remote_network_id, host, name)`
stopped enforcing anything for FQDN rows. Replaced with a unique index on `COALESCE(host, hostname)`.
→ [[Sprint16/Member3-Go-Rust/Phase4-Migration-030-Resource-Model]]

### Fix: `local_target` was briefly an `ACLEntry` field
**Phase 5.** Removed before shipping (`reserved 13`); it belongs on `shield.v1.ResourceInstruction` and
is delivered in Phase 8.0. Establishes the rule: *justify each new wire field by who receives the
message, never by how the data is grouped in the DB.*
→ [[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]]

### Fix: `AclEntry` test literal broke `cargo test`
**Phase 5.2.** The Phase 5 commit contained zero Rust files; the exhaustive `AclEntry` literal in
`connector/src/policy/mod.rs`'s `entry()` helper needed `hostname: String::new(), resolver: None`.
`cargo build` passed (the literal is `#[cfg(test)]`), so the Go-only gate missed it.
→ [[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]]

### Fix: blocking audit-log send on the data-plane critical path
**Phase 3.** `emit_access_log` used `send().await` immediately before `copy_bidirectional`. Switched to
`try_send`. Not the cause of the Gate 1 stall (hypothesis #4), but a real defect.
→ [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]]

### Fix (P0, resolved): tunnel opened but no bytes flowed
**Gate 1.** Client/connector co-location routing loop — the client's nft chain matches
`(daddr, dport)` for every process on the host, so it captured the connector's own egress into the
client's TUN (~10 tunnels per curl). Fixed with `CONNECTOR_EGRESS_MARK = 0x5b` (connector sets
`SO_MARK`; client's chain returns early on it) plus a co-location warning. **Not caused by this sprint.**
→ [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]]

### Fix: stale-on-error charged a resolver timeout to every connection
**Phase 6.2.** `store` returned the last-known-good address but recorded nothing, so `check_cache` had
nothing to hit — during a DNS outage *every* connection re-queried and waited out a full resolver
timeout before receiving an address we already had. "Serving stale" with the outage's latency attached
to each request.

**Fix:** `Entry.retry_after` + `STALE_RETRY` (5s). One query per backoff window; the rest are served
from the fast path. Guard: `stale_is_served_from_the_fast_path_without_requerying` (asserts the query
count does not grow) and `stale_backoff_expires_and_recovery_is_picked_up`.
→ [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]]

### Fix: NODATA re-queried on every connection, and nearly reported the wrong reason
**Phase 6.1.** The spec said "negative cache for NXDOMAIN", so NODATA was left uncached — but a deleted
A record is equally authoritative and gets retried just as often, so it floods DNS identically.

The naive fix was a trap: with `negative_until: Option<Instant>`, `check_cache` returned a hardcoded
`Err(NxDomain)`, which would have reported a suppressed NODATA **as NXDOMAIN** — destroying exactly the
reason fidelity Phase 6.2 exists for.

**Fix:** `negative: Option<(ResolveError, Instant)>` — store the reason, don't assume it.
Guard: `nodata_is_negative_cached_with_its_own_reason`.
→ [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]]

### Fix: the hickory success path had no test coverage
**Phase 6.1.** `classify` had five tests but the `answers() → (Ipv4Addr, ttl)` extraction had none — the
only real logic in the module without coverage, and a bug there would first surface at Phase 7 E2E.

**Fix:** extracted `a_records(&Lookup, now)` and tested it against hand-built `Lookup`s: multi-A
extraction, **CNAME-chain skipping** (intermediate records must not make a resolved alias read as
NODATA), empty answers, and TTL saturation on a past deadline.
→ [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]]

### Fix: task 7.2's metrics bullet was silently skipped
**Phase 7.2.** The logging half shipped; *"emit resolution latency and cache hit/miss so the TTL clamp
can be tuned from data"* did not, and the phase's own gate passed anyway — a green gate proves the
specified tests pass, not that every listed task was done. Only a task-by-task audit caught it.

**Fix:** `Resolved` gains `cache_hit: bool` (set at all five construction sites; always `false` for
`static`, which has no cache), and `device_tunnel.rs` times the resolve and emits `resolved`,
`cache_hit`, `stale`, `resolve_us` at **`debug`** — not `info`, since it fires on every connection to a
named resource. The crate has **no metrics facility** (no `prometheus`, no `metrics` crate), so
structured logs are the only sink that exists today. 3 tests assert the flag is trustworthy, including
the subtle case: the *first* stale answer is `cache_hit: false` (a query was attempted and failed)
while the *backoff-served* one is `true`.
→ [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]]

### Fix: a guard test that guarded nothing
**Phase 7.0.** The first `destination_mismatch` test for named resources sent `destination: ""` and
passed with *and* without the 7.0 scoping — because the original arm was already inert for an empty
destination. Writing it is what disproved this plan's claim that authorization "denies every
name-addressed resource".

**Fix:** the real guard sends a **non-empty (synthetic) destination**, and was validated by reverting
the scoping and confirming exactly one test fails. Lesson worth keeping: **a passing test is not
evidence of a guard — revert the fix and watch it fail.**
→ [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]]

### Fix: leaked navigation hints and an untested legacy alias
**Phase 7.** Two smaller items from the same audit round:
- Comments reading `// … (line ~482)` / `// ~487` — navigation aids for applying a patch by hand —
  were pasted into the source and pointed at wrong lines once the file grew. Removed.
- `route_type == "direct"`, the legacy alias for `"connector"`, reaches the same Phase 7.1 addressing
  code but had **no test**. A future tightening of the route_type check would silently break named
  resources on older ACL snapshots. Added `legacy_direct_route_type_still_resolves`.

### Fix: `fingerprintDesired` does not inherit new fields — this plan said it did
**Phase 8.0.** The plan stated `BuildShieldSnapshot` "hashes whatever `desiredForShield` returns", so a
new column's generation bump "comes for free". It does not: `fingerprintDesired` is an explicit
`%s|%s|%s|%d|%d` format string. Adding `LocalTarget` to the struct and the `SELECT` changed nothing —
compiles clean, all tests pass, generation never bumps, **shields never re-apply**. Caught only by the
phase file's own "**Verify** it changes" step. Rollout note: the hash-format change bumps every shield's
generation once.
→ [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]]

### Fix: `check_port` ×3 — unlisted, and the phase ships broken without it
**Phase 8.1.** All three health-probe sites used `host`. `check_port` is a real
`TcpStream::connect_timeout`, so a service bound only to `127.0.0.1` refuses a connect to the shield's
LAN IP — a `local_target` resource would dial correctly and be reported `status: "failed"` forever,
from the first ack onward. Now probes the resolved `dial_target`. This is the first half of the live
acceptance test, kept as a demonstration rather than deleted once it passed.
→ [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]]

### Fix: 8.2 needed a second proto field (`TunnelOpen.resource_id`)
**Phase 8.2.** The task requires dialing the target "stored for that resource", but `TunnelOpen` carried
no resource identity, so there was no lookup key — the task was unimplementable as written. Tuple
matching on `(destination, protocol, port)` is ambiguous by construction (two resources can share
host+protocol+port and differ only by name). Added `string resource_id = 5`; the connector passes
`acl_entry.resource_id`. Identity from the message, **address** from applied state — Stage 1's split one
layer down.
→ [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]]

### Fix (pre-existing): three defects in `resource.resolvers.go`, found by tests that had never run
**Phase 8.** Setting `RESOURCE_TEST_SHIELD_ID` un-skipped the six `resource_acl_coherence_test.go`
tests — **never executed by anyone** until this phase. Three failed:
(1) `UpdateResource` called `NotifyPolicyChange` unconditionally *and* behind the `ACLRelevantUpdate`
gate, making the gate dead code — so every edit bumped the client-visible ACL version and fired a
fleet-wide `restart_tunnel_if_running`; (2) `ForceDeleteResource` had a duplicated notify block;
(3) `TestProtectResource_DoesNotInvalidate` asserted the **wrong** behaviour — `routeTypeForResource`
derives `route_type` from `status`, so protect flips `connector`→`shield` and suppressing the notify
would be an **enforcement bypass**. Code was right, test was wrong; inverted it. Also added
`PushSnapshotForShield` to `UpdateResource`, without which a `local_target` edit bumps the generation
and delivers to nobody.
→ [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]]

### Known trap: `shield/build.rs` does not track the proto
**Phase 8.** `connector/build.rs` emits three `cargo:rerun-if-changed` lines; `shield/build.rs` emits
none. A proto-only edit leaves the shield compiling against **stale stubs**, and `cargo build`/`cargo
test` come back green having tested nothing new — it reported 15/15 passing against a proto without
`local_target`, and `touch`ing the proto did not help. Workaround:
`cargo clean -p zecurity-shield` before trusting any build after a proto change. One-line fix
recommended but not applied (out of Phase 8 scope).
→ [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]]

### ~~Outstanding (doc-only): stale comment on `ACLRelevantUpdate`~~ ✅ RESOLVED in Phase 8
`internal/resource/store.go` (~454) documented `local_target` as reaching the wire and being part of
what `CompileACLSnapshot` emits into an `ACLEntry` — wrong about exactly the rule the Phase 5 design
correction established. The code was always correct. Rewritten during Phase 8 to state why
`local_target` is *deliberately* absent from the ACL-relevant set and that its delivery path is the
shield snapshot generation instead.

### Fix: invalid nft syntax — a bare `tcp` protocol match (would have broken every FQDN resource)
**Phase 9.3.** The whole-CIDR rule was generated as `ip daddr <cidr> tcp meta mark set 0x5a`. A bare
protocol keyword is only valid when it introduces a field match (`tcp dport 443`); alone nft rejects the
rule with a syntax error, so **no synthetic traffic would ever have been marked**. The unit test
asserted `contains(" tcp ")` — true of a rule nft refuses to load. Fixed to `meta l4proto tcp`, with the
test asserting the exact token sequence. Found only by the live kernel test.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Fix: `handle_up` installed the synthetic routing then immediately destroyed it
**Phase 9.4a.** `configure_allowed_flows` was called twice — once with the CIDR, then again with `None`
— and it begins with `cleanup_policy_routes()`, so the second call tore down the chain and rebuilt it
without the CIDR rule or route. `up` reported success, `route_count` counted the bindings, and
connections hung. **Build and all tests were green**: `handle_up` has no unit coverage, and still
doesn't.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Fix (pre-existing): smoltcp silently supported only 2 resource addresses
**Phase 9.4b.** `net_stack` pushed one `/32` per resource entry plus `100.64.0.1` into `iface.ip_addrs`
while **discarding every `push` failure with `let _ =`**. `IFACE_MAX_ADDR_COUNT` is **2** in this build,
and entries are not deduplicated by IP — so one resource on two ports already overflowed, and beyond
that inbound packets were dropped by smoltcp and the app hung. Replaced with `set_any_ip(true)` plus one
default route, which removes the cap and is the only way a synthetic `/22` can work at all
(`has_ip_addr` is an exact match, so a CIDR entry covers only its base address). Three tests now pin
that smoltcp contract.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Fix: a third silent drop site, and a listener/transport divergence
**Phase 9.4b.** `net_stack` re-derived its listener set from `entry.address` — a third place that
dropped name-addressed resources, and a *different source* from the transports map, so the two could
disagree (route into the TUN, nothing behind it). Listeners now come from `transports.keys()`, making
divergence unrepresentable; `net_stack::run` lost its redundant `allowed_entries` parameter.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Verified: ADR-009's port-scoping invariant is deliberately relaxed inside the synthetic CIDR
**Phase 9.3.** ADR-009 exists to stop non-ACL ports on a resource IP being captured, because the
destination may host an unrelated service. The port-agnostic whole-CIDR rule reintroduces that — **but
only where ADR-009's premise cannot hold**: a synthetic IP is client-local and serves exactly one
resource. Pinned resources keep per-`(ip, port)` rules, proven by a live pinned/unmanaged test pair on
the same address. ADR-009's text is now stale in two doc-only respects (it lists `allowed_entries` as an
input to `net_stack::run`, and the map type predates Phase 2).
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Known: the mark-based steering requires a main-table route to the destination
**Phase 9.5.** `connect()` does a main-table route lookup to choose a source address *before* any packet
exists, and the nft output hook only runs on a packet — so with no main-table route the connect fails
immediately with `ENETUNREACH` and the mark rule never executes. On a real host the default route covers
CGNAT, which is why this works; but **a host with no default route cannot reach name-addressed
resources**. Pre-existing property of the mechanism (applies equally to pinned IPs outside any local
subnet), documented rather than changed.
→ [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]]

### Found by the live E2E: five defects in the enrollment / install path
**2026-08-20, during Phase 8's wire-hop verification.** None are Sprint 16 regressions and **none is
reachable by any test in the repo** — the enrollment/install path has no coverage at all. That absence is
itself the finding; every one of these cost real time to diagnose from a misleading symptom.

1. **Connector runtime requires a URL scheme its own docs call optional.** `ConnectorConfig`
   documents `"host:port"` as *"assumes http://"*, but `main.rs` only prepends a scheme on the
   *fallback* path, so a configured bare `host:port` produces `192.168.1.87:8080/ca.crl?...` →
   reqwest `builder error` → **CRL unavailable → fail-closed → every tunnel denied**, while the log
   talks about revocation rather than a malformed URL. The shield does this correctly
   (`enrollment.rs:298`) — lift that helper into the connector. **Highest severity of the five.**
2. **Shield install script prepends `http://` unconditionally**, so the documented full-URL form
   becomes `http://https://host`. Inverse of (1): the script wants bare, the connector runtime wants a
   scheme. Cosmetic but confusing.
3. **A stale state dir silently adopts another workspace's identity.** With a non-empty
   `/var/lib/zecurity-shield`, the shield logs `already enrolled, resuming` at **INFO** and ignores the
   supplied `ENROLLMENT_TOKEN` — here it resumed a `ws-xiyo` identity while the token said `ws-yoge`,
   and the symptom was `Connection refused` to connectors that were never configured. A token whose
   `workspace_id`/`shield_id` disagree with `state.json` should be a hard error.
4. **`BurnShieldJTI` uses `GETDEL` *before* validation** (`internal/shield/enrollment.go:34`), so any
   Enroll attempt spends the token even when the call then fails — and systemd's restart loop replaces
   the real error with `token expired or already used`. One transient failure permanently burns a
   token. Burn after validation, or make the burn conditional on success.
5. **A token's `interface_addr` can go stale against the DB** and burns itself proving it: the token
   carried `100.64.0.1/32` while the row had been reassigned `100.64.0.2/32`, giving
   `shield interface address mismatch` — after (4) had already spent the JTI. Re-generating a token
   should invalidate prior unspent ones, or enrollment should re-read the row rather than trust the claim.

**Also observed, not defects:** the workspace ACL version advances in steps of **2** across admin
actions (something still double-notifies — *not* `UpdateResource`, which was fixed in Phase 8 and
demonstrably did not bump), and each ACL push is logged twice at the same version, so counting log lines
is not a reliable way to count pushes.

### Outstanding (out of sprint scope): direct-path cooldown with no fallback
`client/src/transport.rs` — `mark_direct_failure()` assumes an alternative transport exists. With
direct-only (no relay provisioned), a single transient timeout becomes a hard outage until the cooldown
expires. Cooldown should be skipped or drastically shortened when there is no fallback. Currently
recorded only inside the resolved bug doc; needs its own item.

## Notes for AI Agents

1. Read this `path.md`, then the phase file for the first unchecked phase — see **Phase Files** above.
2. **Solo sprint — phases are strictly sequential.** Do not start a phase until the previous gate is green.
3. **Stage 1 must be fully green before Stage 2.** It is the contract everything else rests on.
4. `ACLEntry` fields **1–12 are in use**, `13`/`14` are **reserved** — add 15+, **never renumber**
   (standing rule). `shield.v1.ResourceInstruction` fields 1–6 in use; Phase 8 adds 7.
5. **Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.**
6. Every new path must **fail closed**: unknown id, missing ACL, unresolvable endpoint, ambiguous
   addressing → deny.
7. Never log or persist a synthetic IP as identity — log `resource_id`.
8. Stages 1 and 2 are separate merge points. Ship them independently.
9. **Two orthogonal axes — never conflate them.** `route_type` (field 7, `connector`|`shield`) answers
   *who delivers*; `resolver.type` (field 12, `dns`|`static`) answers *how the connector finds the
   endpoint*. **`shield` is not a resolver type.** The connector branches on `route_type` first;
   `resolver` is consulted only inside the `connector` arm. Collapsing them breaks invariant #3.
10. **A proto change is never Go-only.** Any `buf generate` must be followed by
    `cargo build && cargo test` in the connector, client, and shield as applicable — `cargo build`
    alone will not catch broken test-only struct literals.
