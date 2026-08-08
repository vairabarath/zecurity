---
type: phase
sprint: 16
stage: 2
phase: 6
title: Connector Resolver Module
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL
status: done
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

## DNS client — ✅ decided: `hickory-resolver` 0.26.1

| Option | TTL available? | Verdict |
|---|---|---|
| `tokio::net::lookup_host` (getaddrinfo on a blocking threadpool) | ❌ **No** — the OS resolver does not expose the record TTL | Cannot satisfy "TTL-aware cache". **Rejected.** |
| `hickory-resolver` | ✅ Yes — `Lookup::valid_until()` | **Chosen.** Pure-Rust, async-native, reads `/etc/resolv.conf`. |

The TTL clamp and stale handling are both **unimplementable** without record TTLs, so this was not a
free choice.

```bash
cargo add hickory-resolver --no-default-features --features tokio,system-config
```

**Crypto-provider check — clean.** The connector pins `rustls` on `ring` with `quinn` on top, so a
second provider would be a real hazard. Verified: hickory pulls **no TLS stack** at these features
(`cargo tree -e normal -p hickory-resolver` shows no rustls/ring/aws-lc), and the `aws-lc-rs` present in
the tree was **already in the committed lockfile** via `reqwest` → `hyper-rustls` — not introduced here.

Two real additions it does bring: `resolv-conf` (expected) and **`moka`** — hickory has its own internal
response cache. Ours still earns its keep: hickory's does not do the TTL clamp, the bounded stale window,
or per-`remote_network_id` keying.

### API surface (0.26.1 — read from the vendored source, not docs)

`TokioResolver::builder_tokio()?.build()?` · `ipv4_lookup()` returns plain **`Lookup`** (0.26 removed
the typed `Ipv4Lookup`) · `Lookup::valid_until()` · `Record::data` is a **public field, not a method**
(the one compile error hit) · `RData::A(A(pub Ipv4Addr))`.

⚠️ **`classify` is the highest-stakes function in the module.** Hickory folds NXDOMAIN, NODATA and
SERVFAIL into one `NoRecordsFound` variant, separated only by `response_code`. Getting it wrong inverts
the stale decision, and the dangerous direction is under-invalidating: NXDOMAIN misread as transient
keeps a **deleted** resource reachable for up to `STALE_MAX`. Unknown errors therefore default to a
stale-eligible, **non-invalidating** class — a cached address is discarded only on an explicit
authoritative signal. Five tests cover it, one per branch.

## Tasks

### 6.0 — Extend `ResourceAcl`, and fail closed on ambiguous addressing
`connector/src/policy/mod.rs`

- [x] `ResourceAcl` += `pub hostname: String` and `pub resolver: Option<AclResolver>`; populate both in
      `resource_acl_from`. Without this the resolver has no input and Phase 7 cannot branch.
- [x] **Define the precedence for a row with both `address` and `hostname` set — and make it deny.**
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
- [x] Add a helper so Phase 7 reads cleanly and the three states are exhaustive. **Shape is
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
- [x] Two resolver types, dispatched on `resolver.type`:
      - `"dns"` — A-record lookup of the name in `resolver.config` (or of `hostname` when config omits
        one; **decide which and document it** — the plan is explicit that the client-facing name and
        the backend spec are *frequently different strings*).
      - `"static"` — a fixed address list from `resolver.config`; no network I/O. Cheap, and it is what
        makes the module testable without a DNS server.
      - Unknown type → `Err(ResolveError::UnsupportedResolver)`, never a default.
- [x] TTL-aware cache keyed by **`(remote_network_id, name, family)`**.
      ⚠️ `remote_network_id` in the key is load-bearing: the same name can legitimately resolve
      differently in two remote networks. Omitting it cross-contaminates networks — a correctness *and*
      isolation bug.
- [x] **TTL clamp:** min ~5s, max ~300s. A 1s TTL would make the connector hammer DNS on every
      connection; a 24h TTL defeats the entire point of the sprint.
- [x] Brief **negative cache** (a few seconds) so a misconfigured resource cannot turn into a DNS flood.
      ⚠️ **Implemented for NXDOMAIN *and* NODATA**, not NXDOMAIN alone: a deleted A record is equally
      authoritative and gets retried just as often. The reason is *stored* with the deadline
      (`negative: Option<(ResolveError, Instant)>`) so a suppressed NODATA is never reported as NXDOMAIN.
- [x] Single-flight per key: N concurrent connections to one cold name must issue **one** query, not N.

### 6.2 — Typed errors + stale-while-revalidate
- [x] Distinct, non-collapsible error variants — the whole point is that operators can tell these apart.
      **As shipped** (`ResolveError`, each with a stable `reason()` token):
      | Variant | `reason()` | Means | Stale? | Invalidates? |
      |---|---|---|---|---|
      | `NxDomain` | `nxdomain` | the name does not exist — an **answer**, a config error | ❌ | ✅ |
      | `NoAddressRecord` | `no_address_record` | NODATA: name exists, A record gone — also an **answer** | ❌ | ✅ |
      | `ResolverUnavailable` | `resolver_unavailable` | nothing answered: timeout, no route, refused conn | ✅ | ❌ |
      | `ResolverFailure` | `resolver_failure` | the resolver answered *that it failed* — SERVFAIL/REFUSED, incl. DNSSEC validation failure | ✅ | ❌ |
      | `UnsupportedResolver` | `unsupported_resolver` | `resolver.type` not implemented | ❌ | ❌ |
      | `InvalidResolverConfig` | `invalid_resolver_config` | config missing/malformed for its type | ❌ | ❌ |

      ⚠️ **`DialFailed` is deliberately NOT a variant** (the original task list had it). A failed TCP
      connect means resolution *succeeded*; putting it in this enum would conflate "DNS is broken" with
      "the resource is down" — the exact thing typed errors exist to prevent. Phase 7 owns `dial_failed`
      at its own call site.
      ⚠️ **`ResolverFailure` was added.** Same *policy* as `ResolverUnavailable` (stale-eligible), but a
      different *diagnosis*: the resolver/zone rather than the network path to it. Collapsing them would
      defeat the purpose of this task.
      The two policy columns above are the methods `may_serve_stale()` and `invalidates_cache()`, asserted
      disjoint by `stale_and_invalidate_policies_are_disjoint`. The rule stated once: **serve stale only
      for failures that say nothing about the name; discard the cached address for answers that say the
      endpoint is gone.**
- [x] **Stale-on-error** (not background revalidation): on `ResolverUnavailable`/`ResolverFailure`,
      serve the last-known-good address. A DNS blip must not become an outage.
      ⚠️ Plus a **5s `STALE_RETRY` backoff** — without it, "serving stale" still made *every* connection
      during an outage wait out a full resolver timeout first. True prefetch-before-expiry is a later
      optimisation; the cache shape supports it.
      ⚠️ Serve stale on *transient* failure only. Do **not** serve stale on `NxDomain` — a deleted name
      is a decision, and honouring a removed record is a security-relevant failure to converge. Bound
      the stale window (suggest ~1h) so a permanently-down resolver eventually fails closed.
- [x] Resolver failures must **not poison ACL state**. The cache is process-local; nothing here writes
      to `PolicyCache`.

### 6.3 — IPv4-only, explicitly
- [x] A records only. `AAAA` is out of scope for the sprint (decision #3).
- [x] Make this an **explicit** `family: V4` in the cache key and a documented limitation — not an
      accident of using the first result. Stage 3 returns `NODATA` for `AAAA`; that only makes sense if
      this layer is deliberately v4.

## Build gate

```bash
cd connector && cargo build && cargo test
```

Baseline before this phase: **89 unit + 4 integration tests**.
**Result: 128 unit + 4 integration, 0 failed.** Zero build warnings; zero clippy findings in
`resolver.rs` / `policy/mod.rs`; `rustfmt` clean. *(The crate has 3 pre-existing `rustfmt` diffs in
`device_tunnel.rs` / `relay_client.rs`, deliberately left alone — out of this phase's scope.)*

## Verify (unit tests — no live DNS needed)

- [x] `static` resolver returns its configured address; unknown type errors.
- [x] Cache hit inside TTL issues **no** second query; expiry re-queries.
- [x] TTL below the floor is clamped up; above the ceiling, clamped down.
- [x] Same name in two `remote_network_id`s yields two independent entries.
- [x] `ResolverUnavailable` after a successful lookup serves the stale address; `NxDomain` does **not**.
- [x] Single-flight: 10 concurrent resolves of one cold key issue 1 query.
- [x] `Addressing::Invalid` for: both set · neither set · hostname with `resolver: None`.

Added beyond the original list: stale served from the **fast path** without re-querying · stale backoff
expiry + recovery · NODATA negative-cached **with its own reason** · SERVFAIL stale-eligible · the two
policy predicates disjoint · `reason()` tokens distinct · `a_records` extraction ×4 (multi-A, CNAME-chain
skipping, empty answers, TTL saturation) · `classify` ×5 (one per branch, incl. unknown-error default).

## Decisions taken during implementation

| Question the task list left open | Resolution |
|---|---|
| Which name does `"dns"` query — `config["name"]` or `hostname`? | `config["name"]` when present, else `hostname`. The client-facing name and the backend spec are frequently different strings. |
| `resolver.config["server"]` | **Rejected** as `invalid_resolver_config`. Silently ignoring it would resolve against the connector's *own* resolver — a different answer than the operator asked for, and the same "silently prefer one value" antipattern as `ambiguous_addressing`. ⚠️ This contradicts the example in `graph/resource.graphqls`; tracked as an open decision for Phase 10.1. |
| Unknown `resolver.config` keys | Tolerated (forward compat) — asymmetric with `server` above, and a typo like `nameserver` is therefore silently ignored. |
| `static` with several addresses | First valid one wins. No round-robin or failover, despite the plural `addresses` key. |

## Known limitations (deliberate, not defects)

- **The cache never evicts.** Bounded by distinct resource names over the process lifetime, so it grows
  with resource churn and never shrinks. Not client-influenced — names come from the controller-issued
  ACL. An eviction policy needs a deliberate decision rather than a silently-chosen LRU size.
- **`resolve()`, the public wrapper over `resolve_at()`, has no test** — every test calls `resolve_at` to
  inject `now`. It is two lines, but it is the entry point Phase 7 will actually use.
- **`HickoryBackend::from_system()` is never called in tests** (it needs a real resolver). Its extraction
  and error-classification logic *are* covered; the constructor itself is first exercised in Phase 7.

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

All three were found by a verification pass **after** the phase's own gate was already green — the gate
proved the code compiled and the specified tests passed, not that the behaviour was right.

### Fix: stale-on-error charged a resolver timeout to every connection
**Issue:** during a DNS outage, *every* connection re-queried and waited out a full resolver timeout
before receiving the stale address.

**Root cause:** `store` returned the last-known-good address but recorded nothing about the failure, so
`check_cache` had nothing to hit on the next request. Technically "serving stale", with the outage's
latency attached to each request — the opposite of the intent ("a blip must not become an outage").

**Fix applied:** `Entry.retry_after` + `STALE_RETRY` (5s).
```rust
// store(), stale-eligible arm — AFTER:
Some(good) if now < good.expires_at + STALE_MAX => {
    entry.retry_after = Some(now + STALE_RETRY);   // ← added
    Ok(Resolved { address: good.address, stale: true })
}
```
`check_cache` now serves the stale address directly while the backoff is active and within `STALE_MAX`.
**Guards:** `stale_is_served_from_the_fast_path_without_requerying` (asserts the query count does not
grow across three connections — it was 3 before, 2 after) and
`stale_backoff_expires_and_recovery_is_picked_up`.

### Fix: NODATA re-queried on every connection, and nearly reported the wrong reason
**Issue:** a resource whose A record was deleted re-queried DNS on every connection attempt.

**Root cause:** followed the task text literally ("negative cache for NXDOMAIN"). But NODATA is equally
authoritative and gets retried just as often, so it floods identically.

The obvious fix was a trap: with `negative_until: Option<Instant>`, `check_cache` returned a hardcoded
`Err(NxDomain)` — so a suppressed NODATA would have been reported **as NXDOMAIN**, destroying exactly the
reason fidelity task 6.2 exists for.

**Fix applied:** store the reason instead of assuming it.
```rust
// BEFORE: negative_until: Option<Instant>
// AFTER:  negative: Option<(ResolveError, Instant)>
```
**Guard:** `nodata_is_negative_cached_with_its_own_reason`.

### Fix: the hickory success path had no test coverage
**Issue:** `classify` had five tests, but the `answers() → (Ipv4Addr, ttl)` extraction had none — the only
real logic in the module without coverage, and a bug there would first surface at Phase 7 E2E.

**Fix applied:** extracted `a_records(&Lookup, now) -> Vec<(Ipv4Addr, Duration)>` from `lookup_a` and
tested it against hand-built `Lookup`s (`Lookup::new_with_max_ttl` + `Record::from_rdata`): multi-A
extraction, **CNAME-chain skipping** (intermediate records in the answer set must not make a resolved
alias read as NODATA), empty answers → NODATA, and TTL saturation on an already-past deadline.

**Related files:** all three fixes are confined to `connector/src/resolver.rs`.
