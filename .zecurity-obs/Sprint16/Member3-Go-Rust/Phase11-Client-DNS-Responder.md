---
type: phase
sprint: 16
stage: 3
phase: 11
title: Client DNS Responder
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources
status: not-started
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
- [ ] Listen on **UDP *and* TCP port 53** (loopback only). ⚠️ TCP/53 is not optional: a resolver that
      gets a truncated UDP answer retries over TCP, and a responder that only speaks UDP produces
      intermittent, hard-to-diagnose failures.
- [ ] Managed name → `A` record with its synthetic IP, read from Phase 9's registry (the registry stays
      the single source of truth — this phase adds a *protocol*, not a second mapping).
- [ ] Managed name → **`AAAA` = NODATA** (empty answer, `NOERROR`), **not** `NXDOMAIN`. Returning
      NXDOMAIN for `AAAA` tells the client the *name* doesn't exist and can suppress the `A` lookup
      entirely on some stacks. This is the client-side half of the sprint's explicit IPv4-only stance
      (decision #3) and only makes sense because Phase 6 is deliberately v4.
- [ ] TTL **30–60s**. Short enough that a binding change propagates, long enough to avoid a query per
      connection. It does **not** need to track the backend's DNS TTL — the synthetic IP is stable; the
      backend TTL is the connector's concern.
- [ ] **Exact-name match only.** No wildcards this sprint (decision #1; `pattern` is `reserved 14` on
      `ACLEntry`, matching deferred). Do not add prefix/suffix matching "while we're here" — invariant #4
      requires pattern validation on the wire before any wildcard is honoured, and that field does not
      exist yet.
- [ ] Case-insensitive matching, and echo the queried name's case back in the answer.
- [ ] Refuse recursion (`RA=0`) and ignore query types the responder does not serve; never forge answers
      for names it does not manage.

### 11.2 — Unmanaged names pass through
- [ ] Per decision #4, unmanaged names are **not** proxied by us — they are handled by per-domain OS DNS
      configuration in Phase 12, so they never reach this responder at all.
- [ ] Defensive behaviour if one arrives anyway (misconfiguration, or a client pointing at us
      directly): **REFUSED**, not a forged NXDOMAIN. A forged negative answer for a name we don't manage
      breaks the user's unrelated DNS and is very hard to attribute.
- [ ] ⚠️ **Never become an open resolver.** Bind to loopback, and drop queries from off-host sources.

## Build gate

```bash
cd client && cargo build && cargo test
```

## Verify

- [ ] `dig @127.0.0.1 <managed.name> A` → the synthetic IP.
- [ ] `dig @127.0.0.1 <managed.name> AAAA` → `NOERROR` with **no** answer records (not NXDOMAIN).
- [ ] `dig +tcp @127.0.0.1 <managed.name> A` → same answer as UDP.
- [ ] An unmanaged name → `REFUSED`.
- [ ] A query from another host on the LAN → dropped.
- [ ] Case variations of a managed name all resolve.
- [ ] The answer matches Phase 9's registry after a resource is added and after one is deleted.

## Notes

- Adding a DNS library is a new dependency; note which one and why in the phase's fixes section when it
  lands, since Phase 6 already introduces a resolver crate and the two should share it if possible.
- Nothing here binds to the OS's DNS configuration — that is Phase 12 entirely. This phase is testable
  in full with `dig` pointed explicitly at `127.0.0.1`, which is deliberate: it keeps the risky,
  platform-specific work isolated in one phase.

## Post-Phase Fixes

_(none yet)_
