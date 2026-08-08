---
type: phase
sprint: 16
stage: 2
phase: 6
title: Connector Resolver Module
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL
status: not-started
tags: [sprint16, connector, rust, dns, resolver, cache, fqdn]
---

# Sprint 16 · Phase 6 — Connector Resolver Module

> Goal: a **standalone** module that turns `(hostname, resolver)` into a current IP, cached by TTL.
> No wiring into the handshake — that is Phase 7. This phase is pure library code plus unit tests.
> Depends on **Phase 5** (the fields must be on the wire).

## Why the resolution point is the connector

Resolving in the control plane would be catastrophic, and not for an obvious reason: writing a resolved
IP to the DB bumps the ACL version, and an ACL version change triggers `restart_tunnel_if_running`
([client/src/daemon.rs:242](../../../client/src/daemon.rs#L242), and 4 more call sites). So a backend that
re-resolves every 60s would restart every tunnel in the fleet every 60s.

**Resolution must therefore be:** at the connector · per connection · TTL-cached · and it must never
touch controller state. That constraint is the whole reason this module exists rather than a column.

## Current state (verified)

- `connector/src/policy/mod.rs` — `ResourceAcl` carries `resource_id`, `address`,
  `allowed_spiffe_ids`, `route_type`, `shield_id`. It does **not** yet carry `hostname` or `resolver`;
  `resource_acl_from` must be extended (task 6.0).
- `connector/src/` is a flat module list (`policy/` and `tls/` and `discovery/` are the only
  directories). `resolver.rs` as a single file matches the existing style.
- **The connector has no DNS dependency today.** See the decision below.

## Decision required before writing code — DNS client

| Option | TTL available? | Verdict |
|---|---|---|
| `tokio::net::lookup_host` (getaddrinfo on a blocking threadpool) | ❌ **No** — the OS resolver does not expose the record TTL | Cannot satisfy "TTL-aware cache". Rejected. |
| `hickory-resolver` (formerly `trust-dns-resolver`) | ✅ Yes — `Lookup::valid_until()` | **Recommended.** Pure-Rust, async-native, reads `/etc/resolv.conf` by default. Adds one dependency tree. |

The plan's TTL clamp and stale-while-revalidate behaviour are both **unimplementable** without record
TTLs, so this is not a free choice. If the dependency is unacceptable, the fallback is a fixed cache
duration — but then say so explicitly in the ADR rather than pretending TTLs are honoured.

## Tasks

### 6.0 — Extend `ResourceAcl`, and fail closed on ambiguous addressing
`connector/src/policy/mod.rs`

- [ ] `ResourceAcl` += `pub hostname: String` and `pub resolver: Option<AclResolver>`; populate both in
      `resource_acl_from`. Without this the resolver has no input and Phase 7 cannot branch.
- [ ] **Define the precedence for a row with both `address` and `hostname` set — and make it deny.**
      This is not hypothetical:

      | Enforcement point | Rule |
      |---|---|
      | GraphQL `validateAddressing` | **exactly one** ← the intended rule |
      | DB `resources_addressable_check` | `host IS NOT NULL OR hostname IS NOT NULL` — **at least one** |

      Any row inserted by SQL (which Phase 5's own solo tip recommends) bypasses the only real
      enforcement point and can legally carry both. Two source comments also disagree with each other
      about this — see the note in [[Sprint16/Member3-Go-Rust/Phase4-Migration-030-Resource-Model]].
      **Fail closed:** an entry with both set is `reason=ambiguous_addressing`, not "address wins".
      Silently preferring one value is exactly the class of bug Stage 1 was written to remove.
- [ ] Add a helper so Phase 7 reads cleanly and the three states are exhaustive. **Shape is
      illustrative, not a contract** — `ResourceAcl` is built by cloning out of the snapshot under a
      lock, so keep this **owned** rather than borrowing (`&'a`) out of it:
      ```rust
      pub enum Addressing {
          Pinned(String),                                       // address set, hostname empty
          Named { hostname: String, resolver: AclResolver },     // hostname + resolver both set
          Invalid(&'static str),                                // both set, neither set, or name w/o resolver
      }
      ```
      A hostname with **no** resolver is `Invalid` — `parseResolver` returns `nil` on malformed JSON
      precisely so the blast radius lands here, on one resource, instead of on the whole workspace.

### 6.1 — `connector/src/resolver.rs` (new)
- [ ] Two resolver types, dispatched on `resolver.type`:
      - `"dns"` — A-record lookup of the name in `resolver.config` (or of `hostname` when config omits
        one; **decide which and document it** — the plan is explicit that the client-facing name and
        the backend spec are *frequently different strings*).
      - `"static"` — a fixed address list from `resolver.config`; no network I/O. Cheap, and it is what
        makes the module testable without a DNS server.
      - Unknown type → `Err(ResolveError::UnsupportedResolver)`, never a default.
- [ ] TTL-aware cache keyed by **`(remote_network_id, name, family)`**.
      ⚠️ `remote_network_id` in the key is load-bearing: the same name can legitimately resolve
      differently in two remote networks. Omitting it cross-contaminates networks — a correctness *and*
      isolation bug.
- [ ] **TTL clamp:** min ~5s, max ~300s. A 1s TTL would make the connector hammer DNS on every
      connection; a 24h TTL defeats the entire point of the sprint.
- [ ] Brief **negative cache** for NXDOMAIN (a few seconds) so a misconfigured resource cannot turn into
      a DNS flood.
- [ ] Single-flight per key: N concurrent connections to one cold name must issue **one** query, not N.

### 6.2 — Typed errors + stale-while-revalidate
- [ ] Distinct, non-collapsible error variants — the whole point is that operators can tell these apart:
      | Variant | Means |
      |---|---|
      | `NxDomain` | the name does not exist — a config error |
      | `ResolverUnavailable` | timeout / resolver down — an infra problem |
      | `NoAddressRecord` | the name exists but has no A record |
      | `DialFailed` | **resolution succeeded**, the TCP connect did not — a resource problem |
      `DialFailed` must never be reported as a DNS failure; conflating them sends operators to the wrong
      system.
- [ ] **Stale-while-revalidate:** on `ResolverUnavailable`, serve the last-known-good address and
      refresh in the background. A DNS blip must not become an outage.
      ⚠️ Serve stale on *transient* failure only. Do **not** serve stale on `NxDomain` — a deleted name
      is a decision, and honouring a removed record is a security-relevant failure to converge. Bound
      the stale window (suggest ~1h) so a permanently-down resolver eventually fails closed.
- [ ] Resolver failures must **not poison ACL state**. The cache is process-local; nothing here writes
      to `PolicyCache`.

### 6.3 — IPv4-only, explicitly
- [ ] A records only. `AAAA` is out of scope for the sprint (decision #3).
- [ ] Make this an **explicit** `family: V4` in the cache key and a documented limitation — not an
      accident of using the first result. Stage 3 returns `NODATA` for `AAAA`; that only makes sense if
      this layer is deliberately v4.

## Build gate

```bash
cd connector && cargo build && cargo test
```

Baseline before this phase: **89 unit + 4 integration tests** (verified 2026-08-08).

## Verify (unit tests — no live DNS needed)

- [ ] `static` resolver returns its configured address; unknown type errors.
- [ ] Cache hit inside TTL issues **no** second query; expiry re-queries.
- [ ] TTL below the floor is clamped up; above the ceiling, clamped down.
- [ ] Same name in two `remote_network_id`s yields two independent entries.
- [ ] `ResolverUnavailable` after a successful lookup serves the stale address; `NxDomain` does **not**.
- [ ] Single-flight: 10 concurrent resolves of one cold key issue 1 query.
- [ ] `Addressing::Invalid` for: both set · neither set · hostname with `resolver: None`.

## Notes

- **Nothing in this phase touches `device_tunnel.rs`.** Keeping it a pure module is what makes it
  unit-testable without a live stack — which matters because no client can express a name-addressed
  resource until Phase 9, so Phase 6 and 7 have **no E2E proof available** until then. Unit tests are
  the gate here, deliberately.
- `k8s` is a later resolver type behind this same interface. Design the trait so it fits; do not
  implement it.
- Do not log or cache anything under a synthetic IP — those are client-local and meaningless here.
  Log `resource_id` and `hostname`.

## Post-Phase Fixes

_(none yet)_
