---
type: phase
sprint: 16
stage: 3
phase: 12
title: OS DNS Integration
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder
status: deferred — blocked on a privilege decision, see [[Decisions/ADR-023-Privileged-OS-DNS-Integration]]
tags: [sprint16, sprint17-candidate, client, rust, dns, os-integration, adr-009, stage3, deferred, gate3]
---

# Sprint 16 · Phase 12 — OS DNS Integration

> Goal: point the OS at Phase 11's responder for **managed domains only**, and restore the previous DNS
> configuration cleanly on daemon stop.
> Depends on **Phase 11**. **Highest-risk phase in the sprint** — it is the only one that mutates
> host-wide state outside our own interface.

## 🛑 DEFERRED 2026-08-27 — blocked on privilege, not on DNS

**Decision: deferred. Sprint 16 is complete without this phase.** The sprint's capability goal —
dynamic-IP resources without ACL churn — is delivered and verified (Gate 2, 5/5): `resource_id` on the
wire, synthetic-IP routing, connector-side resolution at dial time. This phase only ever bought UX.

### The blocker

Per-domain routing means `systemd-resolved`'s per-link API, and it is polkit-gated:

```text
$ pkcheck --action-id org.freedesktop.resolve1.set-domains     --process $$
rc=2  polkit.result=auth_admin_keep
$ pkcheck --action-id org.freedesktop.resolve1.set-dns-servers --process $$
rc=2  polkit.result=auth_admin_keep
```

The daemon runs as `User=<enrolling user>` with `CAP_NET_ADMIN CAP_NET_BIND_SERVICE`.
**Capabilities do not help — polkit authorizes on uid, not capabilities**, and a headless service has no
session in which to answer an `auth_admin` prompt. So the daemon can build a TUN, nftables rules and
policy routes, but cannot tell the OS to use its own resolver.

This is why the task list below could not simply be executed: 12.1's first bullet assumes
`resolvectl domain`/`dns` is callable, and for a non-root daemon it is not.

### ✅ Design approved 2026-08-27 — option C, implementation not started

Task 1 (does `systemd-resolved` accept a loopback per-link server?) came back **positive**, so the design
was settled rather than left open. Full detail in
[[Decisions/ADR-023-Privileged-OS-DNS-Integration]]; the short version:

- **`zecurity-dns-helper`** — a separate root binary, **socket-activated**, main daemon stays unprivileged
- **API is two verbs**: `apply { iface, server, domains[] }` and `revert { iface }`
- **The helper validates**: `iface` is a Zecurity TUN · `server` is loopback or inside `100.64.0.0/10` ·
  every domain is routing-only (`~`-prefixed). Everything else is rejected and logged.
- **Invariants**: never touch global DNS · never touch another interface · only configure our own TUN ·
  route managed FQDNs **individually** (`~fqdn`), never parent domains · the resolver keeps exact-match +
  `REFUSED`
- Phase 11's `BIND_ADDR = 127.0.0.1:53` **stands** — no change to shipped code

Two Task 1 findings that shaped it: `~domain` captures the whole **subtree** (so routing a shared suffix
would break sibling names we do not manage), and because we only ever configure a TUN we create and
destroy, **there is nothing of the user's to back up** — which removes most of 12.1's capture/restore
machinery and the crash-safety risk with it.

### Options as first analysed (superseded — full analysis in ADR-023)

| | |
|---|---|
| **A — root daemon** | Twingate/Tailscale/NetworkManager model. Recommended. Amends ADR-002; the IPC socket becomes a privilege boundary and that is the first work item, not the DNS code. |
| **B — scoped polkit rule** | resolve1 actions do not reliably carry the link, so the grant is effectively *all links* — **broader** privilege than A while looking narrower. Rejected unless per-link scoping is demonstrated. |
| **C — privileged helper** | Best scoping, new component and new attack surface. Not justified before A is shown inadequate. |
| **D — manual path (current)** | `dig @127.0.0.1`, `curl --resolve`, or a `hosts` entry. Zero risk, ships today, no UX gain. |

### What "manual onboarding" means concretely

**No `os_dns.rs` was written.** A detect-and-refuse module would have had no caller while this phase is
deferred — speculative code with a maintenance cost and no behaviour. The manual path is documentation,
and it is already surfaced by the client: `zecurity-client resources` prints the synthetic IP together
with *"Map the hostname to it locally, e.g. in /etc/hosts."* (added by the Phase 9.5 fix).

An admin who wants the full UX today can configure the link themselves with sudo —
`resolvectl dns zecurity0 127.0.0.1` and `resolvectl domain zecurity0 '~<domain>'` — but ⚠️ **we have not
tested this**, and it will not survive a daemon restart or TUN recreation.

### ✅ The prerequisite is already satisfied

12.2 warns that this phase pairs with Phase 9.3's open split-tunnelling item. **That item is closed**
(`[x]` in the Phase 9 file, with an explicit ADR-009 verdict: the whole-CIDR rule relaxes ADR-009's
invariant deliberately and only inside the synthetic CIDR, proven live in both directions). Whoever picks
this up does not need to revisit it.

### Still required whenever this is picked up

The parts of 12.1 that are **not** about privilege remain unimplemented and still matter — reliable
teardown on every exit path, startup reconciliation, and refusing to overwrite another VPN's per-domain
claim. Their failure mode is *"the user's DNS is broken after our daemon exits"*, which is materially
worse than needing a hosts entry. Do not implement the happy path without them.

---

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
