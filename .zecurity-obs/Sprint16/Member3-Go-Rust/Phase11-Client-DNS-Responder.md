---
type: phase
sprint: 16
stage: 3
phase: 11
title: Client DNS Responder
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources
status: done — implemented & verified live 2026-08-27
tags: [sprint16, sprint17-candidate, client, rust, dns, stage3, deferred]
---

# Sprint 16 · Phase 11 — Client DNS Responder

> Goal: answer DNS queries for **managed** names with their synthetic IPs, so an app can connect by name
> instead of needing a `hosts` entry.
> Depends on **Phase 10** (Gate 2 must be closed first).

## ⚠️ Consider deferring this whole stage to Sprint 17

path.md's recommendation, restated because it is easy to lose momentum into: **Stage 3 is the only stage
with genuinely new, OS-specific subsystems, and it buys UX rather than capability.** Stopping after
Stage 2 leaves a coherent, useful system — backend IPs can already move freely; only the "type the
hostname" convenience is missing.

Treat this phase as *planned but not committed*. It should not delay merging Stage 2.

## Why a responder and not just `hosts`

Phase 9.5 uses a `hosts` entry, which works but is per-machine manual configuration that the daemon does
not own, cannot update when resources change, and cannot clean up. A responder makes the mapping live and
tied to the daemon's lifetime.

## Tasks

### 11.1 — `client/src/dns.rs` (new)
- [x] Listen on **UDP *and* TCP port 53** (loopback only). ⚠️ TCP/53 is not optional: a resolver that
      gets a truncated UDP answer retries over TCP, and a responder that only speaks UDP produces
      intermittent, hard-to-diagnose failures.
- [x] Managed name → `A` record with its synthetic IP, read from Phase 9's registry (the registry stays
      the single source of truth — this phase adds a *protocol*, not a second mapping).
- [x] Managed name → **`AAAA` = NODATA** (empty answer, `NOERROR`), **not** `NXDOMAIN`. Returning
      NXDOMAIN for `AAAA` tells the client the *name* doesn't exist and can suppress the `A` lookup
      entirely on some stacks. This is the client-side half of the sprint's explicit IPv4-only stance
      (decision #3) and only makes sense because Phase 6 is deliberately v4.
- [x] TTL **30–60s**. Short enough that a binding change propagates, long enough to avoid a query per
      connection. It does **not** need to track the backend's DNS TTL — the synthetic IP is stable; the
      backend TTL is the connector's concern.
- [x] **Exact-name match only.** No wildcards this sprint (decision #1; `pattern` is `reserved 14` on
      `ACLEntry`, matching deferred). Do not add prefix/suffix matching "while we're here" — invariant #4
      requires pattern validation on the wire before any wildcard is honoured, and that field does not
      exist yet.
- [x] Case-insensitive matching, and echo the queried name's case back in the answer.
- [x] Refuse recursion (`RA=0`) and ignore query types the responder does not serve; never forge answers
      for names it does not manage.

### 11.2 — Unmanaged names pass through
- [x] Per decision #4, unmanaged names are **not** proxied by us — they are handled by per-domain OS DNS
      configuration in Phase 12, so they never reach this responder at all.
- [x] Defensive behaviour if one arrives anyway (misconfiguration, or a client pointing at us
      directly): **REFUSED**, not a forged NXDOMAIN. A forged negative answer for a name we don't manage
      breaks the user's unrelated DNS and is very hard to attribute.
- [x] ⚠️ **Never become an open resolver.** Bind to loopback, and drop queries from off-host sources.

## Build gate

```bash
cd client && cargo build && cargo test
```

## Verify

- [x] `dig @127.0.0.1 <managed.name> A` → the synthetic IP.
- [x] `dig @127.0.0.1 <managed.name> AAAA` → `NOERROR` with **no** answer records (not NXDOMAIN).
- [x] `dig +tcp @127.0.0.1 <managed.name> A` → same answer as UDP.
- [x] An unmanaged name → `REFUSED`.
- [x] A query from another host on the LAN → dropped.
- [x] Case variations of a managed name all resolve.
- [x] The answer matches Phase 9's registry after a resource is added and after one is deleted.
      **Verified live 2026-08-27** by taking the bindings away and putting them back:

      ```text
      --- tunnel DOWN ---
      fqdn-test.internal   A  UDP  REFUSED  aa=1 ra=0  (no answers)
      --- tunnel UP ---
      fqdn-test.internal   A  UDP  NOERROR  aa=1 ra=0  A=100.64.0.2 ttl=30
      ```

      `handle_down` clears `synthetic_bindings`, so the name genuinely stops being managed and is
      REFUSED rather than answered from a stale copy; `handle_up` restores it. Together with
      `bindings()` snapshotting per query, this closes the "registry is the single source of truth"
      requirement — the responder demonstrably has no cache of its own.

## Notes

- Adding a DNS library is a new dependency; note which one and why in the phase's fixes section when it
  lands, since Phase 6 already introduces a resolver crate and the two should share it if possible.
- Nothing here binds to the OS's DNS configuration — that is Phase 12 entirely. This phase is testable
  in full with `dig` pointed explicitly at `127.0.0.1`, which is deliberate: it keeps the risky,
  platform-specific work isolated in one phase.

## Implementation Notes (2026-08-27)

**7 files, 1 new.** `client/src/dns.rs` (new) · `main.rs` (`mod dns;`) · `Cargo.toml` · `daemon.rs`
(spawn) · **both copies** of `zecurity-client.service`. Client suite **88 → 102**; 14 tests in `dns.rs`.
Zero new clippy warnings; `dns.rs` is rustfmt-clean.

### Structure

`respond(req, lookup) -> Message` is **pure** — no sockets, no state — so every rule in 11.1/11.2 is
testable without binding a privileged port. Same reason `nft_rule_plan`, `resolve_dial_target` and
`display_address` are pure. The socket layer around it is thin: a UDP loop and a TCP accept loop, both
delegating to `handle_bytes`.

### Lifetime: the responder is NOT tied to tunnel up/down

It runs for the daemon's whole lifetime and answers from `RuntimeState.synthetic_bindings`, which
`handle_up`/`handle_down` already keep correct (Phase 9.5). Consequences, all deliberate:

- Tunnel down ⇒ no live bindings ⇒ every name is `REFUSED`. Honest, and it needs no extra state.
- **No cache**: `bindings()` snapshots the map per query, so a released binding stops being answered
  immediately. This is what makes verify-item 7 true by construction — there is nothing that can diverge
  from the registry.
- No `runtime.rs` change was needed, which is why this came in at 7 files rather than 8.

### Fail-soft on bind failure

Binding `:53` needs `CAP_NET_BIND_SERVICE`. If it fails the daemon logs loudly and **carries on** — the
tunnel still works and the Phase 9.5 `hosts`-entry path still resolves. A DNS bind failure taking the
tunnel down would be a bad trade, and an older installed unit will not have the capability.

### 🔴 The unit change is a real upgrade hazard

`CAP_NET_BIND_SERVICE` had to be added to `AmbientCapabilities` **and** `CapabilityBoundingSet` — the
bounding set previously allowed only `CAP_NET_ADMIN`, so the ambient grant alone would have been dropped.
**This is not in the task list**, and it means:

> **Upgrading an existing install requires reinstalling the UNIT, not just the binary.**

There are also **two copies** of the unit (`client/zecurity-client.service` and
`client/systemd/zecurity-client.service`), differing only by a comment header, and the installer fetches
it from the GitHub release. Both were updated; a drifted unit is exactly the class of failure that cost a
full session during Gate 2.

### Dependency choice (the phase asked this be recorded)

**`hickory-proto 0.26.1`, `features = ["std"]`** — message parse/serialise only, no server framework.
Same version family as the connector's `hickory-resolver 0.26.1`, per this phase's "share it if possible"
note. `hickory-server` was deliberately **not** taken: a responder this small needs `op::Message`, `rr`
and `serialize`, and nothing else.

Three API traps worth recording for the next reader of this crate:

1. **`Message::query()` is gated on `feature = "std"`.** `default-features = false` silently removes it —
   the first build failed on a missing associated item, not a missing feature.
2. **`Message::queries` / `answers` are public FIELDS, not methods**, in 0.26.
3. **`data()` / `ttl()` belong to `RecordRef`, not `Record`.** `Record` exposes `name` / `ttl` / `data` as
   public fields.

### A test that was wrong before the code was

The case-echo bullet ("echo the queried name's case back") initially appeared to **fail**: the answer came
back `app.internal` for a query of `ApP.InTeRnAl`. The responder was correct; **the test was not.**
`Name::from_str` runs IDNA normalisation, which the crate documents as having *"a side-effect of
lowercasing the name"* — so the test lowercased its own query before it ever reached the wire.
`Name::from_ascii` preserves case, and with it the round-trip passes.

Worth keeping because the naive version of this test is vacuous, and because the property is not
cosmetic: **DNS 0x20 case randomisation** is an anti-spoofing technique (hickory's own `ResolverOpts` has
`case_randomization`), and a resolver using it may reject an answer whose case does not match.

### One latent bug fixed while verifying

`MAX_UDP` was 512 — the classic *non-EDNS* limit. `recv_from` into a short buffer **silently truncates**,
so an EDNS0 query larger than 512 bytes would have arrived unparseable and been dropped with only a
`debug!` line. Raised to **1232**, the widely-adopted EDNS payload size that avoids IP fragmentation on
both v4 and v6.

### ⚠️ `curl http://<name>:port` still fails — by design

Phase 11 does **not** touch OS resolver configuration; that is Phase 12 entirely. `curl` by name returns
`Could not resolve host` until then, and that is **not** a Phase 11 defect. What works today:
`curl http://100.64.0.2:5174/`, or `curl --resolve <name>:<port>:<synthetic-ip> …` to prove the responder
would serve it.

## Live verification (brucewayne 192.168.1.38, client on a separate device)

`dig` was unavailable on the client host, so queries were sent with a dependency-free Python DNS client
against `127.0.0.1:53` explicitly.

```text
DNS responder listening (UDP+TCP) addr="127.0.0.1:53" ttl_secs=30
127.0.0.1:53  UDP + TCP  held by zecurity-client (pid 5708)

fqdn-test.internal      A     UDP  NOERROR  aa=1 ra=0  A=100.64.0.2 ttl=30
fqdn-test.internal      AAAA  UDP  NOERROR  aa=1 ra=0  (no answers)
fqdn-test.internal      A     TCP  NOERROR  aa=1 ra=0  A=100.64.0.2 ttl=30
FQDN-TEST.INTERNAL      A     UDP  NOERROR  aa=1 ra=0  A=100.64.0.2 ttl=30
fqdn-test.internal      TXT   UDP  NOERROR  aa=1 ra=0  (no answers)
example.com             A     UDP  REFUSED  aa=1 ra=0  (no answers)
sub.fqdn-test.internal  A     UDP  REFUSED  aa=1 ra=0  (no answers)
```

`aa=1 ra=0` on every answer is the pair that stops a resolver treating us as recursive.

**Not an open resolver**, probed from the other host (Archer → `192.168.1.38:53`): UDP timed out, TCP
refused. `systemd-resolved` on that machine holds `127.0.0.53`, `127.0.0.54` and `172.17.0.1` — **no
conflict with `127.0.0.1`**, confirming the port choice on a second, independently-configured host.

Reachability via the synthetic IP: `curl http://100.64.0.2:5174/` → `BACKEND-A (172.20.0.1)`.

## Post-Phase Fixes

_(none yet — the three API traps and the `MAX_UDP` truncation were found and fixed before the first
commit, and are recorded under Implementation Notes above.)_
