---
type: decision
status: proposed — design approved 2026-08-27, implementation not started
date: 2026-08-27
amended: 2026-08-27
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

**Design approved 2026-08-27; implementation not started.** Raised by Sprint 16 Phase 12, which ships
deferred. The **approved direction is option C** (a minimal privileged helper) — see the Amendment, which
supersedes the recommendation in the Options section below. Option A is withdrawn, option B rejected.

⚠️ **Read the Amendment before the Options.** The Options section records the analysis as it stood *before*
Task 1 was run; Task 1's result changed the conclusion.

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

### A — Run the daemon as root — ⛔ WITHDRAWN (see Amendment)

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
- ⛔ **REJECTED.** Not to be revisited unless per-link scoping of the resolve1 actions is demonstrated first.

### C — A small privileged helper — ✅ APPROVED (see Amendment)

A root-run, socket-activated unit exposing only `apply`/`revert` per-link DNS for the Zecurity TUN.

- **Cost:** a new component and a new IPC surface — but that surface is two verbs behind a validating
  whitelist, which is a far smaller thing to get right than option A's retrofit of an authorization model
  onto the whole existing IPC surface (including `GetToken`, which hands out an access token).
- **Benefit:** the main daemon stays unprivileged, so the CLI→daemon boundary is unchanged.

### D — Do nothing automatic; document the manual path (**current state**)

The responder is fully usable with explicit configuration:

- `dig @127.0.0.1 <managed.name>` — works today
- `curl --resolve <name>:<port>:<synthetic-ip> …` — real `Host:` header and TLS SNI, no config change
- an admin may configure per-link routing themselves with sudo:
  `resolvectl dns zecurity0 127.0.0.1` + `resolvectl domain zecurity0 '~<domain>'`
  ⚠️ **Untested by us**, and it will not survive daemon restart or TUN recreation, since the link is
  recreated each time.
- `zecurity-client resources` already prints the synthetic IP and the hosts-entry hint

## Amendment — approved design (2026-08-27)

Superseded the original "defer, option D only" position after Task 1 came back positive. **Option C (a
minimal privileged helper) is the approved direction; option A (root daemon) is withdrawn.** Rationale:
keeping the daemon unprivileged means the existing CLI→daemon boundary (`RuntimeDirectoryMode=0700` +
`check_same_user`) is untouched, so the only new privilege surface is a two-verb helper API. That is a far
smaller thing to get right than retrofitting an authorization model onto the whole IPC surface.

### Task 1 result — `systemd-resolved` accepts a loopback per-link server

Tested on a throwaway `dummy0` link with a marker responder on `127.0.0.1:53`:

```text
resolvectl dns    dummy0 127.0.0.1            ACCEPTED
resolvectl domain dummy0 ~phase12-test.internal  ACCEPTED
  Current DNS Server: 127.0.0.1
  DNS Domain: ~phase12-test.internal
  Default Route: no
dig @127.0.0.53 test.phase12-test.internal  ->  10.77.77.77   (our marker)
```

**Phase 11's `BIND_ADDR = 127.0.0.1:53` therefore stands** — no need to follow Twingate onto an in-CIDR
address, and no change to already-shipped code.

Two further findings from the same test, both load-bearing:

1. **`~domain` captures the entire subtree.** The marker was returned for `test.phase12-test.internal`, a
   *subdomain* of the routed name. Routing a shared suffix (e.g. `~internal`) would send every sibling
   name to our responder, which exact-matches and returns `REFUSED` — and because the domain is
   routing-only for that link, resolved has nowhere else to try. That would **break resolution of
   legitimate names we do not manage.** Hence invariant 4.
2. **The teardown risk largely evaporates.** We only ever configure the per-link entry of a TUN we create
   and destroy ourselves; deleting the link takes its resolved configuration with it (the test relied on
   exactly this, and global config was verified untouched). So there is **nothing of the user's to back
   up**, which removes the capture/persist/restore machinery Phase 12.1 called for, and with it the
   crash-safety problem. A link stranded by `SIGKILL` is handled by the existing
   `tun.rs::cleanup_policy_routes()` reconcile pattern.

### Architectural invariants (approved)

1. **Never modify global DNS.**
2. **Never modify another interface's DNS.**
3. **Only configure the Zecurity TUN interface.**
4. **Route managed FQDNs individually (`~fqdn`), never parent domains.**
5. **The resolver keeps exact-match + `REFUSED` behaviour** (Phase 11, unchanged).

Invariants 1–3 are what make finding 2 true; 4 is what makes finding 1 safe; 5 is what makes 4
necessary. They are a set, not a list — relaxing any one re-introduces a failure the others were chosen
to prevent.

### Implementation decisions (approved)

- A **separate `zecurity-dns-helper` binary** — not a subcommand of the client. Keeps the root footprint
  to the helper's own code rather than the whole client, at the cost of a second release artifact.
- **systemd socket activation** — root runs only while a DNS change is being applied.
- **The main daemon stays unprivileged.**
- **API restricted to two verbs:**

  ```text
  apply  { iface, server, domains[] }
  revert { iface }
  ```

- **The helper validates rather than trusts.** Everything else is rejected and logged:
  - `iface` is a Zecurity TUN (exists, is a TUN, name matches ours)
  - `server` is loopback **or** inside the synthetic CIDR (`100.64.0.0/10`)
  - every entry in `domains` is routing-only (`~`-prefixed)

### Additions I am recording rather than assuming

These were not in the approved list but follow from it, and are cheaper to decide now:

- **Peer authentication must be `SO_PEERCRED`, not just socket mode.** A `root:zecurity` `0660` socket
  authorizes *any* member of that group. The helper should read the peer's credentials and require the
  daemon's uid. Socket mode is defence in depth, not the check.
- **`apply` replaces the full domain list; it never appends.** Otherwise deleting a resource leaves its
  route behind, and the list drifts from the registry. Both verbs must be idempotent, and `revert` on an
  interface that does not exist must be a no-op — the same contract `cleanup_policy_routes()` already
  follows.
- **Resolve the interface to an ifindex at validation time and use that index for the D-Bus call**, so a
  link cannot be swapped between the check and the call.
- **Install-path consequence:** the daemon's user must be added to the helper's group. The install script
  currently only substitutes `User=`; this is one more step it must perform, and — per Sprint 16's
  findings — an *existing* install will not have it, so upgrading requires more than replacing a binary.

### Explicitly still out of scope

Wildcards (`ACLEntry.pattern` is `reserved 14`) and IPv6 (`AAAA` answers NODATA, matching the
connector's v4-only resolver). Per-domain OS routing will happily hand us subdomains regardless; invariant
5 is what keeps that safe.

## Decision

**Sprint 16 ships without OS DNS integration** (option D remains the shipped state): the capability goal
is met and verified, and Stage 3 only ever bought UX.

**When Stage 3 is picked up, option C is approved** with the invariants and constraints in the amendment
above. Option A (root daemon) is **withdrawn** — it required retrofitting an authorization model onto the
entire IPC surface, including `GetToken`, which hands out an access token. Option B (broad polkit rule) is
**rejected**: the resolve1 actions do not carry the link as a detail, so the grant is effectively all
links — broader privilege while appearing narrower.

**Implementation has not started.** The first work item is the helper's validation and peer-authentication
model, not the DNS calls.

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
- `CAP_NET_BIND_SERVICE` (added in Phase 11) **stays required** — the daemon remains unprivileged under
  option C and still binds `:53` itself. And Task 1 settled the loopback question: `systemd-resolved` does
  accept `127.0.0.1` as a per-link server, so the responder does **not** need to move to the TUN address.
