---
type: planning
status: planned
sprint: 16
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
- [ ] **1.1** `connector/src/policy/mod.rs` — **`ResourceAcl` += `address`** (its absence is the root
      cause: the lookup result carries no address, so the handler must use the client's string);
      add `resolve_by_resource_id(resource_id, port, protocol)` reusing the **existing** private
      `find_entry_by_id`; populate `address` in `resolve_resource` too.
- [ ] **1.2** `connector/src/device_tunnel.rs` — `TunnelRequest` (line 41) gains **optional**
      `resource_id`; reorder `handle_stream` (line 133) to lookup → authorize → destination-cross-check
      → branch; dial **`acl.address`**, not `req.destination` (audit every `req.destination` use).
- [ ] **1.3** Keep the legacy destination path working (tolerant reader) so the client can roll after.
- [ ] **Gate:** `cd connector && cargo build && cargo test`
- [ ] 📌 **Already exists — do not rebuild:** `is_allowed(resource_id, client_spiffe_id)`
      (`policy/mod.rs:42`) already performs identity-based SPIFFE authorization, and
      `find_entry_by_id` (`:109`) already does the id lookup. **Real scope: ~1 new function +
      1 struct field + 1 handler reorder.**

#### Phase 2 — Client sends `resource_id`
> See [[Sprint16/Member3-Go-Rust/Phase2-Client-Identity-Handshake]]. Depends on Phase 1.
- [ ] **2.1** `client/src/net_stack.rs` — `TunnelRequest` += `resource_id`; populate it.
- [ ] **2.2** `client/src/daemon.rs` — thread `resource_id` through `build_transports_by_resource`
      (it is already on the ACL entry — pass it along, don't re-derive).
- [ ] **Gate:** `cd client && cargo build && cargo test`

#### Phase 3 — Connector requires `resource_id`
> Depends on Phase 2 being deployed everywhere.
- [ ] **3.1** `device_tunnel.rs` — reject a handshake with no `resource_id` (default-deny).
- [ ] **3.2** Distinct denial reasons: unknown id · port/proto mismatch · unauthorized SPIFFE ·
      destination mismatch.
- [ ] **Gate:** `cd connector && cargo build`
- [x] **🚩 GATE 1 (E2E, merge point) — AUTHORIZATION PASSED (2026-08-05).** Verified on a live stack
      (controller + connector + client, one LAN, no relay): the client asserts `resource_id`, the
      connector logs `auth_path="resource_id"` → `access allowed` → `tunnel_opened ok`, and dials
      **its own ACL's address**. Phases 1–3 confirmed working end-to-end.
      - [ ] **BLOCKED — byte transfer:** no data flows after the tunnel opens. Root-caused as far as
            "the client's relay `select!` stalls", **not caused by this sprint** (the relay loop and
            accept path are untouched). Filed as
            [[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]] (**P0**, 4 hypotheses disproved, next step
            = `tcpdump -i zecurity0`). Stage 2 does not depend on the data plane and may proceed.
      - [ ] Negative cases (`unknown_resource`, `destination_mismatch`, `missing_resource_id`) —
            deferred until the data plane is usable.

### ── STAGE 2 — Delivery Split + Connector Resolution ──

#### Phase 4 — Migration 030 + resource model
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
- [ ] **6.1** new `connector/src/resolver.rs` — `dns` + `static`; TTL-aware cache keyed by
      `(remote_network_id, name, family)`; TTL clamp (min ~5s, max ~300s); brief negative cache.
- [ ] **6.2** Typed errors: NXDOMAIN · timeout/resolver-down · no-A-record · dial-failure (distinct
      from DNS failure). **Stale-while-revalidate** on resolver failure (serve last-known-good).
- [ ] **6.3** IPv4-only this sprint — explicit, not accidental.
- [ ] **Gate:** `cd connector && cargo build && cargo test`

#### Phase 7 — Connector delivery branch
- [ ] **7.1** `device_tunnel.rs` — after authorization: `route_type == "shield"` → existing shield
      session (**never fall back to direct**); else → `resolver.resolve()` → dial.
- [ ] **7.2** Preserve `resource_id` + hostname in logs/metrics (never the synthetic IP).
- [ ] **Gate:** `cd connector && cargo build`

#### Phase 8 — Shield `local_target`
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
- [ ] **9.1** new `client/src/registry.rs` — durable `hostname → synthetic IP → resource_id`;
      stable across restarts; **quarantine before reuse**; collision-aware CIDR selection.
- [ ] **9.2** `client/src/state_store.rs` — persist the registry (encrypted at rest, per ADR-002).
- [ ] **9.3** `client/src/tun.rs` — route the **synthetic CIDR once**; stop installing per-`/32`
      routes for FQDN resources (per-`/32` does not scale).
- [ ] **9.4** `client/src/net_stack.rs` — synthetic IP → `resource_id`; **rewrite the response source
      to the synthetic IP** (the app's socket will drop anything else).
- [ ] **9.5** Testable without DNS via a `hosts` entry → synthetic IP (this preserves TLS
      SNI/validation; connecting to a **raw** synthetic IP does **not**).
- [ ] **Gate:** `cd client && cargo build && cargo test`

#### Phase 10 — Admin UI
- [ ] **10.1** Create/edit an FQDN resource: hostname + resolver type/config.
- [ ] **10.2** Show delivery type (Protected vs Connector-reachable) + resolver health/last error.
- [ ] **Gate:** `cd admin && npm run codegen && npx tsc --noEmit`
- [ ] **🚩 GATE 2 (E2E, merge point):** an FQDN resource is reachable via its synthetic IP; **changing
      the backend IP requires no controller action, bumps no ACL version, and restarts no tunnel.**

### ── STAGE 3 — Name Access (consider deferring to Sprint 17) ──

#### Phase 11 — Client DNS responder
- [ ] **11.1** new `client/src/dns.rs` — UDP **and** TCP/53; managed `A` → synthetic IP;
      managed `AAAA` → **NODATA**; TTL 30–60s; exact-name match (no wildcards yet).
- [ ] **11.2** Unmanaged names: passthrough per decision #4.
- [ ] **Gate:** `cd client && cargo build && cargo test`

#### Phase 12 — OS DNS integration
- [ ] **12.1** new `client/src/os_dns.rs` — **per-domain** DNS config (never hijack all DNS);
      reliable teardown on daemon stop/logout; conflict handling with other VPNs.
- [ ] **12.2** Interaction with split-tunneling (ADR-009) verified explicitly.
- [ ] **🚩 GATE 3 (E2E):** `dig managed.name` → synthetic IP; app connects through the tunnel;
      unmanaged names resolve normally; DNS settings restore cleanly on stop.

## File Map (what you touch, when)

| Phase | Files |
|---|---|
| 1, 3 | `connector/src/policy/mod.rs` (single file), `connector/src/device_tunnel.rs` |
| 2 | `client/src/net_stack.rs`, `client/src/daemon.rs` |
| 4 | `controller/migrations/030_fqdn_resources.sql` **(new)**, `internal/resource/store.go` |
| 5 | `proto/client/v1/client.proto`, `internal/policy/{store,compiler}.go`, `graph/**` |
| 6 | `connector/src/resolver.rs` **(new)** |
| 7 | `connector/src/device_tunnel.rs` |
| 8 | `shield/src/resources.rs`, `shield/src/tunnel.rs` |
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

## Decisions Required (settle before Phase 4)

| # | Decision | Recommendation |
|---|---|---|
| 1 | **Wildcards** (`*.internal`) — does the wire carry `requested_host`? | **Decide now**; it shapes the resource model. Exact names first, but reserve the field. |
| 2 | `shield_ids[]` — array column or join table? | Join table (HA is expensive to retrofit). |
| 3 | **IPv6** | Later. `AAAA → NODATA` in Stage 3. |
| 4 | **Unmanaged DNS** — per-domain OS config or full proxy? | Per-domain (far less invasive). |
| 5 | **Synthetic CIDR** | A `100.64.0.0/10` subrange, collision-checked at startup (CGNAT is used in the wild). |
| 6 | **Name collisions across remote networks** | Per-network DNS suffix, or reject duplicates at create. |
| 7 | **Per-principal ACL scoping** | **Not this sprint** — but it is the blocker for 100k resources / 50k clients. Track separately. |

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

## Notes for AI Agents

1. Read this `path.md`, then the phase file for the first unchecked phase.
2. **Solo sprint — phases are strictly sequential.** Do not start a phase until the previous gate is green.
3. **Stage 1 must be fully green before Stage 2.** It is the contract everything else rests on.
4. `ACLEntry` fields **1–10 are in use** — add 11+, **never renumber** (standing rule).
5. **Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.**
6. Every new path must **fail closed**: unknown id, missing ACL, unresolvable endpoint → deny.
7. Never log or persist a synthetic IP as identity — log `resource_id`.
8. Stages 1 and 2 are separate merge points. Ship them independently.
