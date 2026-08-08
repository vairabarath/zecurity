---
type: phase
sprint: 16
stage: 3
phase: 12
title: OS DNS Integration
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder
status: not-started
tags: [sprint16, sprint17-candidate, client, rust, dns, os-integration, adr-009, stage3, deferred, gate3]
---

# Sprint 16 · Phase 12 — OS DNS Integration

> Goal: point the OS at Phase 11's responder for **managed domains only**, and restore the previous DNS
> configuration cleanly on daemon stop.
> Depends on **Phase 11**. **Highest-risk phase in the sprint** — it is the only one that mutates
> host-wide state outside our own interface.

## ⚠️ Deferral candidate (Sprint 17)

Same note as Phase 11: Stage 3 buys UX, not capability. This phase in particular is platform-specific and
its failure mode is *"the user's DNS is broken after our daemon exits"* — which is far worse than "you
need a hosts entry". Do not ship it under time pressure.

## Tasks

### 12.1 — `client/src/os_dns.rs` (new)
- [ ] **Per-domain DNS configuration — never hijack all DNS** (decision #4). Only the managed
      domains/suffixes route to `127.0.0.1`; everything else keeps the user's existing resolvers,
      untouched.
      ⚠️ Taking over the default resolver would mean every DNS query on the machine depends on our
      daemon, and every bug in Phase 11 becomes a total-connectivity bug.
- [ ] Platform mechanism, stated explicitly rather than assumed:
      - `systemd-resolved` present → per-link domain routing (`resolvectl domain`/`dns`) scoped to
        `zecurity0`. This is the clean path and the only one that supports per-domain routing natively.
      - No `systemd-resolved` → decide **now** whether to (a) edit `/etc/resolv.conf` with a backup and
        restore, or (b) **refuse to enable** OS DNS and fall back to the documented `hosts` workflow.
        **Recommend (b).** Rewriting `resolv.conf` races with NetworkManager/dhcpcd and is the classic
        source of "my DNS broke after uninstalling a VPN".
- [ ] **Reliable teardown on every exit path** — `zecurity-client down`, logout, daemon crash, host
      reboot. Not just the happy path.
      - Capture the prior configuration before changing it, and persist it (a crash must not lose the
        thing needed to restore).
      - Restore idempotently: running teardown twice, or on a machine that was never configured, must be
        a no-op.
      - Reconcile at startup: if a previous run left configuration behind, clean it before applying new
        state. `tun.rs::cleanup_policy_routes()` already follows this pattern — mirror it.
- [ ] **Conflict handling with other VPNs.** Detect an existing per-domain claim on the same domain and
      **refuse with a clear error** rather than silently overwriting someone else's DNS configuration.
      Log what was found.

### 12.2 — Verify the split-tunnelling interaction (ADR-009)
- [ ] Explicitly test, don't reason about it: split-tunnel mode changes which traffic enters the TUN, and
      a name that resolves to a synthetic IP whose CIDR is **not** routed into the TUN in that mode
      resolves fine and then blackholes.
- [ ] ⚠️ Pairs with Phase 9.3's open item — the synthetic-CIDR route vs split-tunnelling. If 9.3 left
      that unresolved, resolve it here before enabling OS DNS, because DNS makes the failure silent
      (the name works, the connection just hangs).

## Build gate

```bash
cd client && cargo build && cargo test
```

## 🚩 GATE 3 — E2E (Stage 3)

- [ ] `dig managed.name` (no explicit `@server`) → the synthetic IP.
- [ ] An app connects **by name** through the tunnel, with no `hosts` entry present.
- [ ] TLS/SNI validation succeeds against the real certificate (this is why name access matters and raw
      synthetic-IP access never did).
- [ ] Unmanaged names resolve normally, with unchanged latency — verify against a name that resolved
      before the daemon started.
- [ ] `zecurity-client down` → DNS settings **fully restored**; `resolvectl status` / `resolv.conf`
      byte-identical to the pre-start state.
- [ ] Kill the daemon with `SIGKILL` → the next start reconciles and the user's DNS is not left broken.
- [ ] Another VPN holding the same domain → clear refusal, no silent overwrite.
- [ ] Split-tunnel mode: name access behaves consistently with 12.2's decision.

## Verify (manual, on each supported platform)

- [ ] `systemd-resolved` host: full pass.
- [ ] Non-`systemd-resolved` host: behaves per the 12.1 decision — either a correct backup/restore cycle,
      or a clean refusal with the `hosts` fallback documented.

## Notes

- This phase touches nothing in the data plane. If it starts needing changes in `net_stack.rs`,
  `tun.rs`, or the connector, the boundary has been crossed and the design should be revisited — Phase 11
  already owns the mapping and Phase 9 owns the routing.
- Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.
- Wildcard domains remain **out of scope** (decision #1, `reserved 14`). Per-domain OS routing will
  happily send `*.internal` to us; the responder must still exact-match and `REFUSE` the rest, per
  Phase 11.

## Post-Phase Fixes

_(none yet)_
