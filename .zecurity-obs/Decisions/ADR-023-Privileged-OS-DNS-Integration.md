---
type: decision
status: proposed
date: 2026-08-27
related:
  - "[[Decisions/ADR-002-Client-Daemon-Required]]"
  - "[[Decisions/ADR-009-Split-Tunnelling]]"
  - "[[Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder]]"
  - "[[Sprint16/Member3-Go-Rust/Phase12-OS-DNS-Integration]]"
  - "[[pending/PENDING-14-FQDN-Resource-Access]]"
tags:
  - adr
  - client
  - dns
  - privilege
  - security
  - deferred
---

# ADR-023 — Privileged OS DNS Integration for the Client Daemon

## Status

**Proposed — not accepted.** Raised by Sprint 16 Phase 12, which is deferred. This ADR exists so the
decision is made deliberately when Stage 3 is picked up, rather than discovered again from scratch.

## Context

Sprint 16 delivered dynamic-IP resources without ACL churn. The capability is complete and verified
(Gate 2, 5/5, on a two-host stack):

- `resource_id` on the wire — the connector authorizes by identity, not address
- Synthetic-IP routing on the client — addresses allocated locally, never seen by the controller
- Connector-side resolution at dial time — a backend can move with **no** ACL version change

Phase 11 added a DNS responder in the client daemon (`client/src/dns.rs`) that answers managed names with
their synthetic IPs on `127.0.0.1:53` (UDP + TCP). It is complete and verified live, 7/7.

What is **missing** is only that the operating system does not automatically consult that responder. Today
a user must map the name manually (`/etc/hosts`), or point a resolver at `127.0.0.1` explicitly. Phase 12
was to close that gap by configuring per-domain DNS routing.

## The blocker

Per-domain DNS routing on Linux means `systemd-resolved`'s per-link configuration (`SetLinkDNS` /
`SetLinkDomains` over D-Bus, or `resolvectl dns` / `resolvectl domain`). Both are polkit-gated:

```text
$ pkcheck --action-id org.freedesktop.resolve1.set-domains     --process $$
rc=2  polkit.result=auth_admin_keep
$ pkcheck --action-id org.freedesktop.resolve1.set-dns-servers --process $$
rc=2  polkit.result=auth_admin_keep
```

```xml
<action id="org.freedesktop.resolve1.set-domains">
  <allow_any>auth_admin</allow_any>
  <allow_inactive>auth_admin</allow_inactive>
  <allow_active>auth_admin_keep</allow_active>
</action>
```

The client daemon runs as `User=<enrolling user>` with `AmbientCapabilities=CAP_NET_ADMIN
CAP_NET_BIND_SERVICE`. **Capabilities do not help**: polkit authorizes on uid, not on capabilities. A
headless systemd service has no session in which to satisfy an `auth_admin` prompt.

So the daemon can create a TUN, install nftables rules and policy routes — but cannot tell the OS to use
its own resolver. This is a privilege-model gap, not a DNS bug.

## How comparable products solve it

Twingate's Linux client runs the privileged work in a **systemd service** while the CLI is deliberately
unprivileged (their docs advise `twingate start` *without* sudo, so desktop notifications reach the user).
It also declines to manage DNS itself: *"The Linux Client requires either `systemd-resolved` service to be
enabled/running or `NetworkManager` service to be configured and enabled/running as the client DNS
service."* Its resolver listens **inside the CGNAT range** (`100.95.0.251-254`) on its own interface
rather than on loopback.

Tailscale (`tailscaled`), `wg-quick` and NetworkManager are all root. **A privileged daemon is the
industry-standard shape for this**, and our existing CLI-over-IPC split already matches it — only the
daemon's uid differs.

Sources: <https://www.twingate.com/docs/linux>, <https://www.twingate.com/docs/how-dns-works-with-twingate>

## Options

### A — Run the daemon as root (recommended if Stage 3 proceeds)

Matches Twingate/Tailscale/NetworkManager. Root bypasses polkit entirely.

- **Cost:** amends [[Decisions/ADR-002-Client-Daemon-Required]]. The IPC socket
  (`/run/zecurity-client/daemon.sock`) must be regrouped/mode-set so the unprivileged CLI can still reach
  a root-owned daemon — and that socket becomes a privilege boundary, so every IPC request needs to be
  treated as untrusted input rather than as same-uid convenience.
- **Benefit:** no new grants, no new components, and the capability set can then be dropped in favour of
  ordinary root.

### B — Ship a scoped polkit rule

A `/etc/polkit-1/rules.d/` rule granting the daemon's user `set-domains` and `set-dns-servers`.

- **Cost:** the resolve1 actions do **not** reliably carry the link as a polkit detail, so the grant is
  effectively *all links*. The daemon's user could redirect **all** system DNS. This is **broader**
  privilege than option A while appearing narrower, and it adds an installer-managed file outside our
  own tree.
- Rejected unless a way to scope the rule per-link is demonstrated first.

### C — A small privileged helper

A root-run unit (or setuid binary) exposing only "apply/restore per-link DNS for `zecurity0`".

- **Cost:** best scoping, but a new component, a new IPC surface, and new attack surface. Hard to justify
  before A has been shown to be inadequate.

### D — Do nothing automatic; document the manual path (**current state**)

The responder is fully usable with explicit configuration:

- `dig @127.0.0.1 <managed.name>` — works today
- `curl --resolve <name>:<port>:<synthetic-ip> …` — real `Host:` header and TLS SNI, no config change
- an admin may configure per-link routing themselves with sudo:
  `resolvectl dns zecurity0 127.0.0.1` + `resolvectl domain zecurity0 '~<domain>'`
  ⚠️ **Untested by us**, and it will not survive daemon restart or TUN recreation, since the link is
  recreated each time.
- `zecurity-client resources` already prints the synthetic IP and the hosts-entry hint

## Decision

**Defer. Option D stands for Sprint 16.** Sprint 16 is complete without OS DNS integration: the sprint's
capability goal is met and verified, and Stage 3 only ever bought UX.

If Stage 3 is picked up, **option A** is the recommended starting point, and the *first* piece of work is
the IPC-socket privilege boundary — not the DNS code.

## Consequences

- Typing a managed hostname into a browser does not work until this is decided. Phase 11's responder must
  be reached explicitly, or the name mapped in `hosts`. **This is by design, not a defect** — worth
  stating plainly, because it reads as a Phase 11 bug otherwise.
- No `os_dns.rs` module was written. A detect-and-refuse module would have had no caller while Phase 12 is
  deferred, so it would be speculative code; the manual path is documentation, not behaviour.
- Phase 12's other requirements — reliable teardown on every exit path, startup reconciliation, and
  refusing to overwrite another VPN's per-domain claim — remain **unimplemented and still required**. They
  are the parts whose failure mode is *"the user's DNS is broken after our daemon exits"*, which is
  materially worse than needing a hosts entry.
- Phase 12's prerequisite is **already satisfied**: Phase 9.3 closed the synthetic-CIDR vs split-tunnelling
  question (`[x]`, with an explicit ADR-009 verdict). Whoever picks this up does not need to revisit it.
- If option A is taken, `CAP_NET_BIND_SERVICE` (added in Phase 11) becomes redundant, and the
  responder could move off loopback to the TUN address as Twingate does — worth reconsidering together,
  since `systemd-resolved` may refuse a loopback server for a link to avoid resolution loops.
