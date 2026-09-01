---
type: phase
sprint: 16
stage: 2
phase: 5
title: Proto + ACL Emission + GraphQL
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase4-Migration-030-Resource-Model
status: done
tags: [sprint16, controller, go, proto, acl, graphql, fqdn, wire-contract]
---

# Sprint 16 · Phase 5 — Proto + ACL Emission + GraphQL

> Goal: get `hostname` and `resolver` from the database onto the wire and into the API, so a
> name-addressed resource can be created and appears in the ACL snapshot **with no real backend IP**.
> Depends on **Phase 4** (the columns must exist).

## Tasks

### 5.1 — `proto/client/v1/client.proto`
- [x] `ACLEntry` **fields 1–10 are in use — never renumber.** Added:
      ```proto
      string      hostname = 11;   // what the connector resolves instead of dialing `address`
      ACLResolver resolver = 12;   // how to resolve it; absent when hostname is empty
      reserved 13;                 // was briefly local_target — see the design correction below
      reserved 14;                 // reserved for `pattern` (wildcard hostnames) — deferred
      ```
- [x] New `ACLResolver { string type = 1; map<string, string> config = 2; }`.
      **`type` is `"dns" | "static"`** (`"k8s"` later, same interface).
      ⚠️ **`shield` is NOT a resolver type.** Shield-vs-connector delivery is `route_type` (field 7),
      an orthogonal axis. See "Two axes, never conflate them" below.

### 5.2 — Codegen
- [x] `buf generate` → Go stubs (`controller/gen/go/proto/client/v1/client.pb.go`).
- [x] `cargo build` → Rust stubs via `build.rs` in both the connector and the client.
- [x] ⚠️ **Rust fallout:** `connector/src/policy/mod.rs`'s test helper builds an **exhaustive**
      `AclEntry` literal, so it stopped compiling under `cargo test` when fields 11–12 landed.
      Fixed by adding `hostname: String::new(), resolver: None` — the correct classic-IP shape per the
      proto's own contract. `client/src/daemon_tests.rs` was unaffected because its literals use
      `..Default::default()`.
      *This was missed in the original Phase 5 commit, which contained zero Rust files: `cargo build`
      passed (the literal is under `#[cfg(test)]`) but `cargo test` did not.*

### 5.3 — ACL emission
- [x] `internal/policy/store.go` — `ListEnabledRulesWithResources` selects `hostname` and `resolver`.
- [x] `internal/policy/compiler.go` — emits both into the `ACLEntry`; `parseResolver` converts the
      stored `{"type":…,"config":{…}}` JSON into `ACLResolver`.
- [x] **`parseResolver` returns `nil` on malformed input rather than an error — deliberately.**
      `CompileACLSnapshot` returning an error makes the caller default-deny the *entire workspace*, so
      one bad resolver blob would take every user offline. Returning `nil` confines the blast radius to
      the one resource: the connector treats an absent resolver on a hostname-addressed entry as
      unresolvable and denies just that dial.
- [x] `graph/resolvers/policy_helpers.go` — the **second** resource read path, whose raw `SELECT r.host`
      would hard-fail on a NULL host. Fixed with `COALESCE`.

### 5.4 — GraphQL
- [x] Schema (`graph/resource.graphqls`) + `resource.resolvers.go` create/edit + `helpers.go` presenter.
- [x] New `graph/resolvers/resource_addressing.go` — kept **outside** `*.resolvers.go` on purpose:
      gqlgen owns those files and strips non-resolver functions out on the next regeneration.
      - `blankToNil` — `host: ""` would otherwise insert an empty string, which is `NOT NULL` and so
        satisfies `resources_addressable_check` while being useless to dial.
      - `validateAddressing` — **enforces exactly one addressing mode.** The DB constraint only
        requires at least one; allowing both leaves the connector with an IP and a name that can
        disagree and nothing to say which wins.
      - `validateResolverJSON` — requires a JSON object with a non-empty string `type`; the config
        shape is intentionally *not* checked here (Phase 6 owns it).
- [x] Regenerate with **`make gqlgen`** — `go generate ./graph/...` is a no-op, no directive exists.

## Build gate

```bash
cd controller && go build ./... && go vet ./...     # PASS
```

> ⚠️ The sprint's Final Build Gates also require `cd connector && cargo build && cargo test`. Run it —
> 5.2's Rust fallout is invisible to the Go-only gate. Verified green afterwards: **89 + 4 tests pass.**

## Test coverage

| File | Cases |
|---|---|
| `internal/policy/compiler_fqdn_test.go` | `TestParseResolver` ×8 (no DB); `TestCompileACLSnapshot_FQDNAddressing` ×3 (DB-gated on `PKI_TEST_DATABASE_URL`) |
| `graph/resolvers/resource_addressing_test.go` | ×26 |
| `internal/resource/acl_relevance_test.go` | ×12 — includes the `local_target only → false` guard |

**Known gaps, deferred to Gate 2:** `loadResourceByID` is manually verified but has no regression test;
the GraphQL `createResource` path with `hostname` has never been executed end-to-end.

💡 **Solo tip (done):** hand-insert one FQDN resource row via SQL here — it unblocks Phases 6–9 without
waiting on the UI (Phase 10). Scratch DB `ztna_p5test` built from migrations 001–030.
⚠️ But note this bypasses `validateAddressing`, the only place the exactly-one rule is enforced — see
Phase 6 task `6.0`.

---

## ⚠️ Design correction — `local_target` does **NOT** belong in `ACLEntry`

5.1 originally listed `local_target` alongside `hostname`/`resolver`, because migration 030 adds all
three columns together. **That grouping is right for the database (a shared store) and wrong for the
wire (point-to-point).**

`ACLSnapshot` reaches only **clients and connectors**, and neither dials a resource's on-host address.
The **Shield** does — and Shields receive `shield.v1.ResourceInstruction`, built by the controller in
`internal/connector/control_stream.go`, and **never see an `ACLEntry`**.

Keeping it there would have:
- leaked an internal loopback address to every client,
- churned the ACL version on every `local_target` edit (→ fleet-wide tunnel restarts),
- and *still* not delivered it to the Shield.

**Resolution:** field 13 removed before it shipped, marked `reserved 13`. `local_target` moves to
**Phase 8** (task `8.0`). `TestACLRelevantUpdate`'s `local_target only → false` case is the guard that
fails if it reappears.

**Rule for later phases:** justify each new wire field by asking *"who receives this message, and does
each recipient need it?"* — never by how the data is grouped in the DB. `go build`, `go vet`,
`buf breaking` and even mutation tests cannot catch a misplaced field; they only prove that what you
emit arrives.

## Two axes, never conflate them

A recurring misreading worth stating explicitly, because getting it wrong breaks invariant #3:

| Axis | Field | Question | Values |
|---|---|---|---|
| Delivery | `ACLEntry.route_type` (7) | **Who** delivers the bytes | `"connector"` \| `"shield"` |
| Resolution | `ACLEntry.resolver.type` (12) | **How** the connector finds the endpoint | `"dns"` \| `"static"` |

The connector branches on `route_type` **first**. `resolver` is only ever consulted on the
`"connector"` branch. A shield-delivered resource is **never resolved** — the Shield *is* the endpoint.

## Post-Phase Fixes

### Fix: moving a resource between remote networks changed the ACL without bumping its version
**Found 2026-09-01** while verifying Phase 10's item 118 (*"editing `hostname` or `resolver` bumps the ACL
version"*) on a live stack.

**Issue.** `ACLRelevantUpdate` (`internal/resource/store.go`) gates
`NotifyPolicyChange` on the update touching a compiler-visible field. It omitted
`RemoteNetworkID` — and both the resolver's comment and the unit test asserted, deliberately, that
`remote_network_id` was *"invisible to the compiler"*.

It is not. Three legs, all verified by reading:

| Leg | Evidence |
|---|---|
| the field is editable | `internal/resource/store.go` — `Update` does `add("remote_network_id", …)` |
| the compiler reads it **from the resource row** | `internal/policy/store.go` rules query selects `r.remote_network_id` |
| the compiler emits it | `internal/policy/compiler.go` — `RemoteNetworkId: rnByKey[key]` on every `ACLEntry` |

`ACLEntry.remote_network_id` is the **routing reference** a client follows to find which connectors serve
a resource. So moving a resource to another remote network changed every affected client's routing while
leaving the ACL version untouched — clients kept the stale id and dialled the *old* network's connectors
until some unrelated edit churned the version.

**Fix.** Add `input.RemoteNetworkID != nil` to `ACLRelevantUpdate`; correct the resolver comment; flip the
test case from `false` to `true`. Revert-verified — dropping the field again fails
`TestACLRelevantUpdate/remote_network_only`.

**The other three exclusions are correct**, and worth recording so nobody "fixes" them too:
`description`, `port_to` and `local_target` are genuinely not selected by the ACL rules query. Only one
port per entry reaches the wire (`ACLEntry.port`), and `local_target` travels to shields via
`PushSnapshotForShield`, which `UpdateResource` calls separately.

**Why the test did not catch it.** The test asserted the buggy behaviour as intended. This is the second
time in Sprint 16 that a test encoded the wrong expectation rather than missing the case — the first was
the renewal prompt, where `TestRenewalPromptRepeatsUntilTheCertChanges` asserted repeat-prompting is
correct and so never asked what happens when renewal *succeeds*.

### Fix: `AclEntry` test literal broke `cargo test`
See 5.2 above. Root cause: exhaustive struct literal in `connector/src/policy/mod.rs`'s `entry()`
helper, versus `..Default::default()` everywhere else.

**Fix applied:**
```rust
// AFTER — in fn entry(), connector/src/policy/mod.rs
route_type: "shield".into(),
shield_id: "shield-test".into(),
// Sprint 16 Stage 2 (fields 11–12): this helper builds classic
// IP-pinned entries, which keep `address` and leave these empty.
hostname: String::new(),
resolver: None,
```
The exhaustive literal is kept deliberately — it forces a test author to consider each new field —
but it will need this edit again on every future `ACLEntry` addition.

### Fix (doc-only, outstanding): stale comment on `ACLRelevantUpdate`
**Issue:** `internal/resource/store.go` (~line 454) documents the ACL-relevant set as
"name, host, hostname, resolver, **local_target**, port_from, protocol, shield_id — everything
`CompileACLSnapshot` emits into an `ACLEntry`", and adds "all three reach the wire: … `local_target` is
what a shield dials."

**Root cause:** written before the design correction above; not updated when field 13 was removed.

**Fix needed:** the *code* is correct (`ACLRelevantUpdate` does **not** test `input.LocalTarget`, and
`acl_relevance_test.go` asserts `local_target only → false`). Only the comment is wrong — and it is
wrong about precisely the rule the correction established, so it will mislead the next reader. Update
the comment to say `local_target` is Shield-only and deliberately absent.
