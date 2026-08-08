---
type: planning
status: in-progress
sprint: 16
progress: Stage 1 complete (Gate 1 passed 2026-08-06) · Stage 2 phases 4–5 complete · next = Phase 6
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
      - [ ] **Negative cases — outstanding, now unblocked.** `missing_resource_id` ·
            `unknown_resource` · `destination_mismatch` · `unauthorized_spiffe` · shields-all-offline
            fails closed. These were deferred while the data plane was broken; it works now, so they are
            cheap. **Write `destination_mismatch` before Phase 7** — Phase 7 task 7.0 modifies exactly
            that check, and this test is the guard that fails if it is deleted rather than scoped.

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

#### Phase 6 — Connector resolver module
> See [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]]. **← next unchecked phase.**
- [ ] **6.0** `connector/src/policy/mod.rs` — `ResourceAcl` += `hostname` + `resolver` (populate in
      `resource_acl_from`; the resolver has no input without them). **And define the precedence for a
      row with BOTH `address` and `hostname` — make it `reason=ambiguous_addressing`, fail closed.**
      Exactly-one is enforced *only* at the GraphQL layer (`validateAddressing`); the DB check is
      at-least-one, so any SQL-inserted row — which Phase 5's own solo tip recommends — can carry both.
- [ ] **6.1** new `connector/src/resolver.rs` — `dns` + `static`; TTL-aware cache keyed by
      `(remote_network_id, name, family)`; TTL clamp (min ~5s, max ~300s); brief negative cache;
      single-flight per key. ⚠️ **Dependency decision:** `tokio::net::lookup_host` does **not** expose
      record TTLs, so the TTL clamp and stale-while-revalidate are unimplementable with it —
      `hickory-resolver` (or an explicit "fixed cache duration" downgrade) must be chosen deliberately.
- [ ] **6.2** Typed errors: NXDOMAIN · timeout/resolver-down · no-A-record · dial-failure (distinct
      from DNS failure). **Stale-while-revalidate** on resolver failure (serve last-known-good) — but
      **never on NXDOMAIN**, and with a bounded stale window so a dead resolver eventually fails closed.
- [ ] **6.3** IPv4-only this sprint — explicit, not accidental.
- [ ] **Gate:** `cd connector && cargo build && cargo test` (baseline 89 + 4)

#### Phase 7 — Connector delivery branch
> See [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]].
- [ ] **7.0** ⚠️ **Scope amendment — do this first.** Authorization currently denies **every**
      name-addressed resource *before* the `route_type` branch is reached:
      `device_tunnel.rs:225` compares `req.destination != entry.address`, and `entry.address` is empty
      for a named resource. Scope the check to `!entry.address.is_empty()` — **do not delete it**, it
      stays fully strict for every IP resource. Phase 9.4 must agree by sending an **empty**
      `destination` for named resources; if either half ships alone, every FQDN resource is denied.
      *(No live breakage today: clients drop these entries at `daemon.rs:657`.)*
- [ ] **7.1** `device_tunnel.rs` — branch on **`route_type` first**: `"shield"` → existing shield
      session (**never fall back to direct**); `"connector"` → `Pinned` → dial address, `Named` →
      `resolver.resolve()` → dial, `Invalid` → deny. ⚠️ **Never `match resolver.type { …, shield }`** —
      delivery (field 7) and resolution (field 12) are orthogonal axes; collapsing them breaks
      invariant #3. Keep `connect_marked_tcp` (`CONNECTOR_EGRESS_MARK`) on the new dial path or the
      Gate 1 co-location loop returns.
- [ ] **7.2** Preserve `resource_id` + hostname + resolved address in logs/metrics (never the synthetic
      IP). Emit resolution latency + cache hit/miss so the TTL clamp can be tuned from data.
- [ ] **Gate:** `cd connector && cargo build && cargo test`
      📌 Phases 6–7 have **no E2E proof available** until Phase 9 — no client can express a
      name-addressed resource before the binding registry exists. Unit tests are the gate, deliberately.

#### Phase 8 — Shield `local_target`
> See [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]].
- [ ] **8.0** **Deliver `local_target` to the Shield** (moved here from 5.1 — see the design
      correction under Phase 5). `proto/shield/v1/shield.proto` — add `string local_target = 7`
      to `ResourceInstruction` (fields 1–6 in use; never renumber). Then
      `internal/resource/store.go` `BuildShieldSnapshot` selects the column, and
      `internal/connector/control_stream.go` populates it in **all three**
      `ResourceInstruction{}` construction sites (~lines 169, 220, 532).
      `buf generate` + `cargo build --manifest-path shield/Cargo.toml`.
- [ ] **8.1** `shield/src/resources.rs` — `validate_host` accepts the resource's `local_target`
      (`127.0.0.1` | own LAN IP). ⚠️ This touches a **non-negotiable rule** (`resource.host ==
      detect_lan_ip()`); the check must stay **equally strict**, only more explicitly sourced.
- [ ] **8.2** `shield/src/tunnel.rs` — dial `local_target`.
- [ ] **Gate:** `cargo build --manifest-path shield/Cargo.toml`

#### Phase 9 — Client binding registry + synthetic routing
> See [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]].
> **Largest and most security-sensitive phase of Stage 2.**
- [ ] **9.1** new `client/src/registry.rs` — durable `hostname → synthetic IP → resource_id`;
      stable across restarts; **quarantine before reuse**; collision-aware CIDR selection. Preserve the
      three-state semantics (`Some(with transports)` / `Some(empty)` = fail closed / `None` = unmanaged).
- [ ] **9.2** `client/src/state_store.rs` — persist the registry (encrypted at rest, per ADR-002); a
      corrupt table rebuilds empty **with everything quarantined**, rather than refusing to start.
- [ ] **9.3** `client/src/tun.rs` — route the **synthetic CIDR once**; stop installing per-`/32`
      routes for FQDN resources (per-`/32` does not scale). Pinned IPs keep per-`/32` unchanged.
      ⚠️ **A route alone will not pull traffic into the TUN.** Steering is nft-mark based
      (`ip daddr … tcp dport … meta mark set` → `ip rule fwmark` → table 105). **Decide:** per-`(ip,port)`
      rules as today, or **one port-agnostic `ip daddr <SYN_CIDR>` mark rule** (recommended — constant
      ruleset size; then a non-ACL port on a synthetic IP must refuse cleanly, not hang). The
      `meta mark 0x5b return` rule must stay **first** or the Gate 1 co-location loop returns.
      ⚠️ Also verify the synthetic-CIDR route against **split-tunnelling (ADR-009)**.
- [ ] **9.4** `client/src/net_stack.rs` — synthetic IP → `resource_id`; send `destination` **empty** for
      named resources (must match Phase 7 task 7.0); **rewrite the response source to the synthetic IP**
      (the app's socket will drop anything else — and this failure looks exactly like the Gate 1 stall,
      which will send you down the wrong path).
- [ ] **9.5** Testable without DNS via a `hosts` entry → connect **by name** (this preserves TLS
      SNI/validation; connecting to a **raw** synthetic IP does **not**). ⚠️ Resource must **not** be on
      the client's host — the `local` table beats any TUN route and produces a false pass.
- [ ] **Gate:** `cd client && cargo build && cargo test` (baseline 39)
- [ ] 📌 **Regression test (acceptance-critical):** a restart must not remap a synthetic IP to a
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
| 8 | `proto/shield/v1/shield.proto`, `internal/resource/store.go`, `internal/connector/control_stream.go`, `shield/src/resources.rs`, `shield/src/tunnel.rs` |
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
| 5 | **Synthetic CIDR** | ⏳ A `100.64.0.0/10` subrange, collision-checked at startup (CGNAT is used in the wild). **Open sub-decision:** how it is *steered* (see Phase 9.3 — a route alone is insufficient). | Phase 9.1, 9.3 |
| 6 | **Name collisions across remote networks** | ✅ **Rejected at create**, per remote network. | `UNIQUE (tenant_id, remote_network_id, COALESCE(host, hostname), name)` — migration 030 |
| 7 | **Per-principal ACL scoping** | ❌ **Not this sprint** — but it is the blocker for 100k resources / 50k clients. Track separately. | — |

### Still open (must settle before the named phase)

| Decision | Phase | Why it can't wait |
|---|---|---|
| Ambiguous addressing: what happens when a row has **both** `host` and `hostname`? | **6.0** | Exactly-one is enforced only at the GraphQL layer; SQL-inserted rows bypass it. **Recommend fail closed.** |
| DNS client crate — `hickory-resolver` vs `lookup_host` | **6.1** | `lookup_host` exposes no record TTL, so the TTL clamp and stale-while-revalidate are unimplementable with it. |
| What `destination` carries for named resources | **7.0 + 9.4** | Both halves must agree, or every FQDN resource is denied as `destination_mismatch`. |
| How the synthetic CIDR is steered into the TUN (nft mark shape) | **9.3** | Routing is mark-driven; a route alone won't capture traffic. |
| Non-`systemd-resolved` hosts: rewrite `resolv.conf` or refuse OS DNS? | **12.1** | Failure mode is the user's DNS left broken after our daemon exits. |

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
| 6 | [[Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module]] | ⬜ **next** |
| 7 | [[Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch]] | ⬜ |
| 8 | [[Sprint16/Member3-Go-Rust/Phase8-Shield-Local-Target]] | ⬜ |
| 9 | [[Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing]] | ⬜ |
| 10 | [[Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources]] | ⬜ — closes **Gate 2** |
| 11 | [[Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder]] | ⬜ Stage 3 — deferral candidate |
| 12 | [[Sprint16/Member3-Go-Rust/Phase12-OS-DNS-Integration]] | ⬜ Stage 3 — deferral candidate |

Bug record: [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]] (P0, **resolved** 2026-08-06).

## Post-Sprint Fixes

Overview only — each fix is documented in full in its phase file.

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

### Outstanding (doc-only): stale comment on `ACLRelevantUpdate`
`internal/resource/store.go` (~454) still documents `local_target` as reaching the wire and being part of
what `CompileACLSnapshot` emits into an `ACLEntry`. The **code is correct** (it is excluded, and
`acl_relevance_test.go` asserts `local_target only → false`); only the comment contradicts the Phase 5
design correction — i.e. it is wrong about exactly the rule the correction established.

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
