---
type: phase
sprint: 16
stage: 2
phase: 8
title: Shield `local_target`
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch
status: done
completed: 2026-08-15
commit: e89f941
tags: [sprint16, shield, connector, go, rust, proto, nftables, security]
---

# Sprint 16 · Phase 8 — Shield `local_target`

> Goal: let a shield-protected resource declare **which local address the shield dials**
> (`127.0.0.1` vs its LAN IP) instead of always dialing the address the connector passed down.
> Depends on **Phase 7**.

## Why

Today the connector calls `open_relay_session(&shield_id, &acl_entry.address, port, protocol)` and the
shield dials that address ([shield/src/tunnel.rs:58](../../../shield/src/tunnel.rs#L58)). Since a shield
protects the host it runs on, that address is the shield's own LAN IP. A service bound **only to
loopback** — the common hardening posture — is therefore unreachable, even though the shield is running
on the very host that could reach it.

`local_target` makes the shield-side dial target an explicit property of the resource.

## ⚠️ 8.0 comes from Phase 5, and it is the interesting part

`local_target` was originally planned as `ACLEntry` field 13 because migration 030 adds all three
addressing columns together. **That grouping was wrong for the wire** and the field was removed before
it shipped (`reserved 13`). Full reasoning:
[[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]] § *Design correction*.

The short version: `ACLSnapshot` reaches only clients and connectors, neither of which dials a
resource's on-host address. The **Shield** does — and it receives
`shield.v1.ResourceInstruction`, never an `ACLEntry`. So the delivery vehicle is different, and this
phase builds it.

A useful contrast, and the reason this is the *right* home:

| | Bumps on a `local_target` edit? | Consequence |
|---|---|---|
| ACL snapshot version | **No** (`ACLRelevantUpdate` excludes it; asserted by `acl_relevance_test.go`) | no fleet-wide `restart_tunnel_if_running` |
| Shield snapshot generation | **Yes** — it is a content fingerprint over `desiredForShield`'s rows | the shield re-applies, which is exactly what should happen |

That asymmetry is the design working: the recipient that needs the change gets it; the fleet that does
not, is not disturbed.

## Tasks

### 8.0 — Deliver `local_target` to the Shield ✅
- [x] `proto/shield/v1/shield.proto` — `ResourceInstruction` **fields 1–6 are in use; never renumber.**
      Add `string local_target = 7;`
      **Also added `string resource_id = 5` to `TunnelOpen`** — not in the original task list; 8.2 was
      unimplementable without it. See the design correction below.
- [x] `internal/resource/store.go`:
      - `PendingRow` += `LocalTarget string`
      - `desiredForShield`'s `SELECT` += `COALESCE(local_target, '')` and the matching `Scan` target.
        ⚠️ Note the existing `COALESCE(host, '')` on the same query (added in Phase 4) — the same
        nullable-column trap applies to the new column.
      - ~~`BuildShieldSnapshot` inherits it automatically~~ — **this claim was wrong.** `fingerprintDesired`
        is an explicit field list, not a struct hash. See the first Post-Phase Fix below; without that
        change the generation never bumps and shields never re-apply, while everything still compiles
        and every test passes. The "**Verify** it changes" instruction is what caught it.
      - Also added `GetPendingForShield` (the reconnect remove-batch query) for consistency; the value
        is unused there, since a remove dials nothing.
- [x] `internal/connector/control_stream.go` — populate it in **all three** `ResourceInstruction{}`
      construction sites: **~169** (snapshot), **~220** (single push), **~532** (batch).
      ⚠️ Three sites is the whole risk of this task. A missed site means `local_target` is silently `""`
      on one delivery path, and the shield falls back to its LAN IP — a resource that works after a full
      resync but not after an incremental push, which is a miserable bug to chase.
      **Resolved structurally:** all three now route through one converter,
      `internal/connector/instruction.go`, so the divergence is unrepresentable rather than merely
      discouraged. A reflection test asserts every exported wire field is populated, so the *next*
      proto field forces a decision here too.
- [x] Codegen: `buf generate` **and** `cargo build --manifest-path shield/Cargo.toml`.
      ⚠️ Expect the same class of fallout as Phase 5.2: any exhaustive `ResourceInstruction` literal in
      Rust tests stops compiling. Prefer `..Default::default()` in new test helpers.
      *Confirmed twice:* `connector/src/agent_server.rs:775` (`ResourceInstruction`) and
      `connector/src/agent_tunnel.rs:131` (`TunnelOpen`). The second is production code, so the
      connector does not build until 8.2's connector half lands.
      📌 `controller/gen/` is **gitignored** — the Go stubs are not a commit artifact. `buf generate` is
      a required local step for anyone on this branch.

### 8.1 — `shield/src/resources.rs` — `validate_host` accepts `local_target`
> ⚠️ **This touches a non-negotiable project rule** (`resource.host == detect_lan_ip()`). The check must
> stay **equally strict** — only more explicitly sourced. Read this task twice before editing.

Current implementation ([shield/src/resources.rs:94](../../../shield/src/resources.rs#L94)):
```rust
pub fn validate_host(resource_host: &str) -> bool {
    if resource_host == "127.0.0.1" { return true; }
    match util::detect_lan_ip() {
        Some(my_ip) => my_ip == resource_host,
        None => false,
    }
}
```

- [x] The **allowed set is unchanged**: `{ "127.0.0.1", detect_lan_ip() }`. `local_target` selects *from*
      that set; it does not extend it. An instruction naming anything else is rejected exactly as today.
      **`validate_host`'s body is byte-identical to its pre-Phase-8 version** (verified by `diff` against
      HEAD~1) — the non-negotiable rule was not edited, only more explicitly sourced.
- [x] Empty `local_target` → current behaviour (validate `instruction.host`). Backward compatible with
      an un-upgraded controller.
- [x] `None` from `detect_lan_ip()` still returns `false` — **fail closed**, unchanged.
- [x] Do **not** accept a hostname here. This is a shield-local dial target, not a resolvable name; the
      shield is not a resolver. Accepting names would put a resolver inside the shield, which the sprint
      explicitly defers (*Shield-as-segment-gateway*).
- [x] The rejection log line must say which value failed and where it came from — a `local_target`
      typo currently surfaces as the misleading "resource host does not match this shield's LAN IP".
      Implemented as `RejectedTarget { field, value }`, carried into both the `warn!` and the
      `ResourceAck.error`.

**How it landed.** A new `dial_target(host, local_target) -> Result<&str, RejectedTarget>` calls
`validate_host` **twice** and is now the only production caller of it — no path can bypass the check.
The resolved address is stored on `ActiveResource.dial_target`, validated once at apply time, so the
health and tunnel paths cannot re-derive it differently.

⚠️ **`host` is validated even when `local_target` is set.** `host` is what binds a resource to *this*
shield; skipping it would let an instruction naming **another shield's LAN IP** be applied here as long
as it carried `local_target: "127.0.0.1"`. Nothing in the type system enforces this — the guard is
`host_is_still_validated_when_local_target_is_set`, and removing the check fails exactly 3 tests.

⚠️ **Unlisted but mandatory: `check_port` ×3.** See the second Post-Phase Fix — without it a
loopback-only resource dials correctly and is reported `failed` forever.

### 8.2 — `shield/src/tunnel.rs` — dial `local_target` ✅
- [x] `handle_tunnel_open_tcp` / `handle_tunnel_open_udp` dial the resource's validated `local_target`
      when set, else the current `destination`.
      **Resolved once in the dispatcher** (`handle_tunnel_open`), so both handlers receive an
      already-decided address and keep their signatures. Grep confirms no other value reaches
      `TcpStream::connect` or `socket.connect`.
- [x] The `local_target` used must be the one **stored for that resource** in the shield's active state
      — not one read out of the tunnel-open message. The tunnel-open path is per-connection and
      connector-driven; taking a dial target from it would hand the connector free-form dialing inside
      the shield, which is the same confused-deputy shape Stage 1 removed one layer up.
      Implemented as `SharedResourceState::dial_target_for(resource_id)`.
      ⚠️ **This required a proto change the task list did not anticipate** — see the design correction
      below. `TunnelOpen` carried no resource identity, so there was nothing to look up.
- [x] `check_port` already treats an unparseable address as "not listening" rather than panicking; keep
      that property for `local_target`. Preserved — `check_port` is byte-identical apart from `rustfmt`
      re-wrapping its `warn!`, and `dial_target` only ever returns an already-validated IP.
- [x] `shield/src/control_stream.rs` — thread `resource_state` into `handle_tunnel_open` (~line 262).
      Not in the File Map; one argument, already in scope.

> **⚠️ Design correction — 8.2 needed `TunnelOpen.resource_id`.**
> The task says the dial target must be "the one stored for that resource", but `TunnelOpen`
> (fields 1–4: `connection_id`, `destination`, `port`, `protocol`) carried **no resource identity**, so
> there was no lookup key. Matching on `(destination, protocol, port)` was rejected: the unique index is
> `(tenant_id, remote_network_id, COALESCE(host, hostname), name)`, so two resources can share
> host+protocol+port range and differ only by name — a tuple match is ambiguous by construction, and
> "first match wins" is exactly the silent-preference bug this sprint keeps removing.
> **Resolution:** `string resource_id = 5` on `TunnelOpen`; the connector passes `acl_entry.resource_id`
> (never the client's). Identity comes from the message, the **address** comes from applied state —
> Stage 1's split applied one layer down. Touches `connector/src/agent_tunnel.rs` (a `resource_id`
> param on `open_relay_session`) and `connector/src/device_tunnel.rs` (its single call site).

📌 **Deliberate residual: unknown `resource_id` falls back, it does not fail closed.** When the connector
asserts an id this shield has not applied (snapshot in flight, or genuine divergence), we dial the
message's `destination` and `warn!`. That is exactly what the shield did before this phase, so it is not
a *new* hole — but it is the one place a connector can still steer the dial. Tightening it to a denial is
a follow-up, once every connector is known to send `resource_id`; the same tolerant-then-require sequence
as Phase 1 → Phase 3.

## Build gates

```bash
buf generate
cd controller && go build ./... && go vet ./...
cargo build --manifest-path shield/Cargo.toml
cd connector && cargo build && cargo test     # proto change is shared — re-verify
```

## Verify

- [x] A protected resource with no `local_target` behaves **identically** to before (regression).
      Unit (`empty_local_target_falls_back_to_host`, `resource_without_local_target_dials_its_host`)
      + live (half 1 of the acceptance test).
- [x] `local_target = "127.0.0.1"` → the shield dials loopback; a loopback-only service becomes
      reachable. **Proven live on a real kernel** — see *Live verification* below.
- [x] `local_target` = the shield's LAN IP → accepted (`own_lan_ip_is_accepted_as_either_field`).
- [x] `local_target` = any **other** IP → rejected, with `status: "failed"` and a log naming the field.
      Live: `live_phase8_foreign_local_target_is_refused` also asserts the resource never enters the
      applied set, so the tunnel path has nothing to dial for it.
- [x] `local_target` = a hostname → rejected (unit + live).
- [x] Editing `local_target` bumps the **shield generation** — `TestBuildShieldSnapshotGeneration`,
      extended with a set → assert → clear-to-NULL → assert cycle.
      ⬜ *"…and the shield re-applies"* is **inferred** from the shield's `generation <= last` gate,
      not observed. Needs the full-stack run below.
- [x] …and does **not** bump the **ACL version**. `TestACLRelevantUpdate` plus the six
      `resource_acl_coherence_test.go` tests — which had **never executed** before this phase; see the
      fourth Post-Phase Fix.
- [x] nftables `chain resource_protect` is still flushed + rebuilt atomically in one transaction.
      Re-ran the F21 live tests in the same namespace: `89 samples across 50 rebuilds; 0 saw the port
      undefended`, and a failed apply still rolls back.

### Live verification (2026-08-15)

The headline claim cannot be shown by a pure unit test — it needs real nftables, a real socket, and a
LAN IP that is **not** the address the service is bound to. All three are reproducible inside the same
throwaway namespace the F21 tests use, because `dummy` is not in `util::VIRTUAL_PREFIXES`, so
`detect_lan_ip()` picks up a dummy interface:

```bash
cargo test --manifest-path shield/Cargo.toml --no-run     # note the test binary path
unshare -rn sh -c '
  ip link set lo up
  ip link add dummy0 type dummy; ip addr add 10.99.0.1/24 dev dummy0; ip link set dummy0 up
  exec <test-binary> live_phase8 --ignored --nocapture --test-threads=1'
```

```text
[phase8] lan_ip=10.99.0.1  loopback-only service on 127.0.0.1:42535
[phase8] without local_target → status=failed     port_reachable=false
[phase8] with local_target=127.0.0.1 → status=protected  port_reachable=true
[phase8] TCP connect to 127.0.0.1:42535 OK — loopback-only service reachable
```

Both halves run together on purpose: **the contrast is the assertion.** The first half reproduces the
bug being fixed; without it a passing second half proves nothing. The test asserts `lan_ip != 127.0.0.1`
up front so it cannot silently degrade into a tautology.

### ✅ The wire hop — VERIFIED on a live stack (2026-08-20)

Controller + connector + shield all enrolled on one host (`192.168.1.87`, workspace `ws-yoge`), with a
service bound **only** to `127.0.0.1:8081` and `192.168.1.87:8081` confirmed unreachable.

Resource: `host = 192.168.1.87`, `localTarget = 127.0.0.1`, tcp/8081, shield-delivered.

| `local_target` | Resource ack | Shield generation |
|---|---|---|
| `127.0.0.1` | **protected**, `error_message` null | 0 → 1 |
| edited to `192.168.1.87` | **failed** — `port not listening` | 1 → **2** |
| edited back to `127.0.0.1` | **protected** | 2 → **3** |
| description-only edit | protected (unchanged) | **3 → 3** — no churn |

**Why this proves the hop.** The service is loopback-only, so `protected` is achievable *only* if the
shield probed `127.0.0.1` — an address it can learn from nowhere except
`ResourceInstruction.local_target` arriving over gRPC. And the A/B is controlled: the service stayed up
and unchanged while the field was flipped, and the outcome flipped with it, in both directions, on
demand.

The generation column is the first Post-Phase Fix working in production: without adding `LocalTarget` to
`fingerprintDesired`'s explicit format string, the generation would have stayed at 1 and the shield
would never have re-applied.

**ACL version: unchanged.** The connector's last `ACL snapshot stored` was `version=5` at 12:17:21 UTC;
the three `localTarget` edits ran 12:24–12:25 UTC and produced **no** further pushes. That is the
asymmetry the Phase 5 design correction exists for — the shield gets the change, the fleet is not
disturbed, no `restart_tunnel_if_running`.

Also closed as a side effect: **Phase 5's known gap** that `createResource` / `updateResource` with the
new addressing fields had never executed end-to-end. Both ran, four times total.

⚠️ **A claim NOT established:** it is tempting to say "the pre-Phase-8 binaries reproduced the bug".
They were installed and did ack `failed`, but the service happened to be dead at that moment, so that
run proves nothing. The evidence above stands on the controlled A/B alone, which is stronger anyway.

⚠️ **Verification-method warning, learned the hard way:** `strings <binary> | grep -c local_target` is
**not** a valid way to check whether a build contains a change. At `-O3`, LLVM emits short string
literals as immediate operands inside instructions rather than `.rodata` entries, so they are invisible
to a byte search — proven by injecting a 16-byte marker into a function whose own 5- and 7-byte literals
could not be found. Verify by behaviour (a log line the new code emits) or by md5 against the artefact
you just built.

## Build gates — all green (2026-08-15)

| Gate | Result |
|---|---|
| `buf generate` | clean |
| `cd controller && go build ./... && go vet ./...` | clean |
| `cd controller && go test ./internal/... ./graph/...` | green (DB-gated; see below) |
| `cargo build --manifest-path shield/Cargo.toml` | clean |
| `cargo test --manifest-path shield/Cargo.toml` | **31 passed, 4 ignored** (baseline 15 + 2) |
| shield live tests under `unshare -rn` | **4 passed** (2 Phase 8 + 2 F21) |
| `cd connector && cargo build && cargo test` | **148 + 4** — unchanged, as expected |
| `cd client && cargo test` | **39** — untouched |
| `cd relay && cargo build` | clean, **zero files changed** |

Only pre-existing failure: `internal/auth::TestNewValkeyClient_Success`, where the local Valkey rejects
`CLIENT TRACKING`. Environmental, unrelated.

📌 **DB-gated tests need two env vars, not one.** `RESOURCE_TEST_DATABASE_URL` alone leaves
`TestBuildShieldSnapshotGeneration` and all six coherence tests skipping; they also need
`RESOURCE_TEST_SHIELD_ID` pointing at a real `shields` row whose `lan_ip` matches the host the fixture
seeds (`MarkProtecting` joins on `shields.lan_ip = resources.host`).

## Notes

- Shields heartbeat to the Connector `:9091` only, never directly to the Controller. Nothing in this
  phase changes delivery topology — `local_target` rides the existing `ResourceInstruction` piggyback.
  **No new RPCs.**
- This phase is independent of the resolver work: a shield-delivered resource is **never** resolved.
  If you find yourself calling Phase 6's resolver from shield code, stop — the two axes have been
  conflated (see Phase 7's warning).

## Post-Phase Fixes

### Fix: `fingerprintDesired` does NOT inherit new fields — the plan said it did
**Issue:** 8.0 stated *"`BuildShieldSnapshot` inherits it automatically (it hashes whatever
`desiredForShield` returns), so the generation bump comes for free."*

**Root cause:** `fingerprintDesired` is an explicit format string, not a struct hash:
```go
fmt.Fprintf(h, "%s|%s|%s|%d|%d\n", r.ID, r.Host, r.Protocol, r.PortFrom, r.PortTo)
```
Adding `LocalTarget` to `PendingRow` and to the `SELECT` changes nothing here. The code compiles, every
test passes, and **the generation never bumps on a `local_target` change — so shields never re-apply.**

**Fix applied (`internal/resource/store.go` ~375):**
```go
// BEFORE
fmt.Fprintf(h, "%s|%s|%s|%d|%d\n", r.ID, r.Host, r.Protocol, r.PortFrom, r.PortTo)
// AFTER
fmt.Fprintf(h, "%s|%s|%s|%d|%d|%s\n",
    r.ID, r.Host, r.Protocol, r.PortFrom, r.PortTo, r.LocalTarget)
```
Plus a comment stating the function is an explicit field list, because the next person will assume
otherwise for exactly the same reason.

**Deployment note:** changing the hash format changes every shield's stored fingerprint, so the first
`BuildShieldSnapshot` after deploy bumps the generation for **every** shield and each re-applies once.
Harmless — re-apply is idempotent and the chain rebuild is atomic — but it will show up as a fleet-wide
re-apply on rollout.

**Guard:** `TestBuildShieldSnapshotGeneration`'s new `local_target` case. Reverting this one line fails
it with `local_target change did not bump generation: 16 → 16`.

---

### Fix: `check_port` ×3 — unlisted, and the phase ships broken without it
**Issue:** The task list never mentions the health probe. All three `check_port` call sites
(`run_health_check_loop`, `handle_apply`, `handle_snapshot`) probed `host`.

**Root cause:** `check_port` is a real `TcpStream::connect_timeout`. A service bound **only** to
`127.0.0.1` refuses a connect to the shield's LAN IP.

**Consequence had it shipped:** a `local_target = "127.0.0.1"` resource would dial correctly and be
reported `status: "failed"`, `port_reachable: false`, `"port not listening"` — starting with the very
first ack and re-asserted every health interval. The controller would mark a working resource
permanently broken. Arguably worse than the original bug, because it reports confidently.

**Fix applied:** all three probe `res.dial_target` / the resolved `target`. This is the first half of
the live acceptance test, kept as a demonstration rather than deleted once it passed.

---

### Fix: `existing.dial_target` — the assignment no compiler catches
**Issue:** `handle_apply`'s update-or-insert branch mutates an existing `ActiveResource` in place.
The `else` arm's struct literal is a hard compile error if a field is missing (`E0063`, which did fire
during implementation). **The `if` arm's `existing.dial_target = …` assignment is not.**

**Consequence:** omitting it means a `local_target` change delivered as an *incremental push* keeps the
stale target, while `handle_snapshot` — which rebuilds its Vec from scratch — picks the new value up.
That is the phase file's own "works after a full resync but not after an incremental push" failure,
reproduced one layer down inside the shield.

**Fix applied (`shield/src/resources.rs`, `handle_apply`):** `existing.dial_target = target.clone();`
alongside the other four field assignments.

---

### Fix (pre-existing, found by this phase): three defects in `resource.resolvers.go`
Setting `RESOURCE_TEST_SHIELD_ID` to run 8.0's generation check also un-skipped
`graph/resolvers/resource_acl_coherence_test.go` — **six tests that had never executed**, because the
var had never been set by anyone. Three of them failed immediately:

1. **`UpdateResource` double-notify.** `NotifyPolicyChange` was called **unconditionally**, then again
   behind the `ACLRelevantUpdate` gate two lines below — making the gate dead code. Since
   `CompileACLSnapshot` uses `notifier.Version(workspaceID)` verbatim as the snapshot version (it is
   **not** content-derived), *every* resource edit — a description, a `port_to`, a `local_target` —
   bumped the client-visible ACL version and fired a fleet-wide `restart_tunnel_if_running`.
   **Fix:** removed the unconditional call. `push hook fired 2 times, want 1` → passes.
2. **`ForceDeleteResource`** had a literally duplicated `NotifyPolicyChange` block. Two version bumps,
   two fleet-wide restarts, for one delete. **Fix:** removed the duplicate.
3. **`TestProtectResource_DoesNotInvalidate` asserted the wrong behaviour.** Its premise — protect
   *"changes only status/pending_action, which the compiler never reads"* — is false: `policy/store.go`
   selects `r.status` and `routeTypeForResource` derives `route_type` from it
   (`unprotected` → `"connector"`, `protecting` → `"shield"`). Protect **flips `route_type`**, and the
   connector branches on it first (Phase 7.1). Suppressing that notification would leave connectors
   direct-dialing a now-shield-protected resource — an **enforcement bypass**.
   **Fix: the code was right; the test was wrong.** Inverted to `TestProtectResource_Invalidates`.
   The asymmetry with unprotect is real and correct: unprotect goes `protected` → `protecting`, both of
   which map to `"shield"`, so the ACL is unchanged at that moment.

**Also added:** `UpdateResource` now calls `PushSnapshotForShield`. Without it a `local_target` edit
bumps the generation and delivers to nobody until an unrelated protect or reconnect. Deliberately
unconditional rather than gated on a new "does the shield care?" predicate — the snapshot fingerprint
already *is* that test, and a second predicate would be one more thing to keep in step with the wire
format, which is the drift that caused the first fix above.

---

### Fix: three `ResourceInstruction` construction sites → one converter
**Issue:** The phase file calls the three sites "the whole risk of this task" but offers no structural
remedy, and no test covered the `PendingRow → ResourceInstruction` mapping at all — it was reachable
only through a live snapshot build.

**Fix applied:** new `controller/internal/connector/instruction.go` with `instructionFor` (+
`instructionForRow`, which flattens `resource.Row`'s nullable `*string`). All three sites call it.
`instruction_test.go` adds 5 no-DB tests, including a reflection guard that fails if **any** exported
wire field is left zero — so the next proto field forces a decision here rather than shipping empty on
all three paths. Deleting `LocalTarget` from the converter fails 4 tests.

---

### Known trap: `shield/build.rs` does not track the proto
`connector/build.rs` emits three `cargo:rerun-if-changed` lines; `shield/build.rs` emits **none**. A
proto-only edit therefore leaves the shield compiling against **stale generated stubs**, and
`cargo build` / `cargo test` come back green having tested nothing new. This bit during implementation:
the shield reported 15/15 passing against a proto that did not yet have `local_target`, and `touch`ing
the proto did not help.

**Workaround used:** `cargo clean -p zecurity-shield --manifest-path shield/Cargo.toml` before trusting
any build that follows a proto change.

**Recommended fix (not applied — out of Phase 8 scope):** add to `shield/build.rs`
```rust
println!("cargo:rerun-if-changed=../proto/shield/v1/shield.proto");
```

---

### Note: `controller/gen/` is gitignored
`.gitignore:33` ignores `controller/gen/`, and `git ls-files` shows only the *client* and *relay* stubs
are tracked (committed before that rule landed). `shield.pb.go` is **not** a commit artifact, so
`buf generate` is a required local build step on this branch, and an empty `git status` after running it
is expected rather than evidence that codegen did nothing.
