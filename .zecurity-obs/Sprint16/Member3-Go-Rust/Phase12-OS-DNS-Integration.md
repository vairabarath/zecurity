---
type: phase
sprint: 16
stage: 3
phase: 12
title: OS DNS Integration
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase11-Client-DNS-Responder
status: done (with stated gaps) — Gate 3 closed 2026-08-28: 7 pass, 1 design-incompatible (rival-domain refusal). NOT tested: non-systemd-resolved host, teardown on logout/reboot. See [[Decisions/ADR-023-Privileged-OS-DNS-Integration]]
tags: [sprint16, sprint17-candidate, client, rust, dns, os-integration, adr-009, stage3, deferred, gate3]
---

# Sprint 16 · Phase 12 — OS DNS Integration

> Goal: point the OS at Phase 11's responder for **managed domains only**, and restore the previous DNS
> configuration cleanly on daemon stop.
> Depends on **Phase 11**. **Highest-risk phase in the sprint** — it is the only one that mutates
> host-wide state outside our own interface.

## ✅ DONE — GATE 3 CLOSED 2026-08-28

Reversed the 2026-08-27 deferral after ADR-023's Task 1 came back positive and the privilege question was
settled (option C, a minimal privileged helper). **Managed names now resolve through the OS with no
`hosts` entry.** Verified on a real two-host stack — controller/connector on `Archer`, client on
`brucewayne`.

### The result

```text
Current DNS Server: 127.0.0.1
       DNS Servers: 127.0.0.1
        DNS Domain: ~fqdn-test.internal
     Default Route: no
fqdn-test.internal: 100.64.0.2
$ curl http://fqdn-test.internal:5174/
BACKEND-A (172.20.0.1)
```

Connector side, same request:

```text
access allowed spiffe_id=…/client/0c9f025d-…  resource_id=86f515d9-…
  hostname=fqdn-test.internal  dest=172.20.0.1  stale=false  route="connector"
tunnel_opened ok dest=172.20.0.1:5174
```

The whole chain: OS resolver → helper-configured per-link DNS → Phase 11's responder → synthetic IP →
tunnel → connector → dial-time resolution → backend.

### Gate 3 — 6 of 8

| Item | Result |
|------|--------|
| `resolvectl query <managed>` (no explicit server) → synthetic IP | ✅ |
| App connects **by name**, no `hosts` entry | ✅ |
| Unmanaged names resolve normally | ✅ `github.com` via `enp2s0` throughout |
| `down` → DNS fully restored | ✅ link gone, managed name stops resolving, unmanaged unaffected |
| `SIGKILL` → next start reconciles | ✅ DNS never broken; config reapplied on the next `up` |
| Rival claim on the same domain | ⚠️ **design-incompatible — see below** |
| TLS/SNI validation against a real certificate | ✅ |
| Split-tunnel consistency | ✅ — no mode toggle exists; both planes verified together |

### TLS/SNI — the strongest form of the claim

A second resource, `tls-test` (`hostname=tls-test.internal`, `tcp/5443`), pointed at a self-signed
certificate whose only SAN is `DNS:tls-test.internal`:

```text
$ curl --cacert ca.pem https://tls-test.internal:5443/
TLS-BACKEND (172.20.0.1:5443, SNI-validated)

$ curl --cacert ca.pem https://fqdn-test.internal:5443/      # negative control
curl: (60) SSL: no alternative certificate subject name matches target hostname
```

The negative control is what makes the positive meaningful: validation is genuinely enforced, so the
success requires **SNI, the `Host:` header and the certificate SAN to all agree** — which is only possible
if the name survives the entire path. A raw synthetic IP could never demonstrate this, which is exactly
why the task singled it out.

### Two managed names at once, and the `static` resolver

The same run covered two paths nothing had exercised:

```text
Resources (3):
  fqdn-test    100.64.0.2   fqdn-test.internal   5174   ← resolver: dns
  ip-control   172.20.0.1   —                    8085   ← pinned IP
  tls-test     100.64.0.3   tls-test.internal    5443   ← resolver: static

DNS Domain: ~fqdn-test.internal ~tls-test.internal
```

- **`resolver.type = "static"`** end to end for the first time — every earlier test used `dns`. The
  connector dialled from the ACL with no DNS lookup at all:
  `hostname=tls-test.internal dest=172.20.0.1 stale=false route="connector"`.
- **Two managed names simultaneously** — two entries in the helper's domain list, two synthetic IPs from
  the registry, each routed **individually** (invariant 4 in practice, not just in the whitelist).
- All three addressing shapes coexisting in one workspace: `dns`, `static`, and pinned-IP.

### Split-tunnel consistency

12.2 asks this be tested rather than reasoned about, and warns of a name that resolves to a synthetic IP
whose CIDR is **not** routed into the TUN — resolving fine and then blackholing.

**There is no full-tunnel mode in this client.** Only ACL'd destinations are ever routed, so
split-tunnelling is not a mode to toggle — it is the only behaviour, and the warned-of failure cannot
arise. Both planes were verified simultaneously throughout: `resolvectl query github.com` answered
`-- link: enp2s0` (unmanaged, straight out the LAN) while managed names resolved to synthetic IPs and
carried traffic over `zecurity0`. Phase 9.3 had already closed the synthetic-CIDR-vs-ADR-009 question with
an explicit verdict.

`dig` was unavailable on the client host; `resolvectl query` was used instead, which is arguably better —
it goes through the system resolver *and* names the link that answered.

### ⚠️ "Conflict handling" is not what 12.1 asked for

12.1 wanted the helper to **detect a rival per-domain claim and refuse**. It cannot, and not by oversight:
invariants 2–3 mean the helper only ever touches `zecurity0`, so it has no visibility of another link's
configuration. What was actually tested is **systemd-resolved's precedence**:

```text
rival0 (dummy) configured with 1.1.1.1 + ~fqdn-test.internal
fqdn-test.internal: 100.64.0.2      ← our answer still won
```

So the safety property holds — a rival claim does not hijack a managed name, and unmanaged DNS survives —
but **"clear refusal on conflict" is not implemented and is incompatible with the current design.** Record
it as a limitation, not a pass. Closing it properly would mean reading other links' configuration, which
widens the helper's remit considerably.

### Incidental confirmation

`resolvectl` **refuses `127.0.0.53`** as a per-link server ("Invalid DNS server address" — its own stub
address, loop protection) while accepting `127.0.0.1`. Phase 11's choice of `127.0.0.1` was correct for a
reason that had not been verified until now.

---

## 🛑 Original deferral (2026-08-27) — superseded by the above

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
- [x] **Per-domain DNS configuration — never hijack all DNS** (decision #4). Only the managed **✅ verified** — only `~fqdn` entries; `github.com` answered via `enp2s0` throughout.
      domains/suffixes route to `127.0.0.1`; everything else keeps the user's existing resolvers,
      untouched.
      ⚠️ Taking over the default resolver would mean every DNS query on the machine depends on our
      daemon, and every bug in Phase 11 becomes a total-connectivity bug.
- [x] Platform mechanism, stated explicitly rather than assumed: **✅ decided (b)** — `resolvectl` per-link when resolved is present, refuse otherwise. The refusal path has NOT been exercised on a real non-`systemd-resolved` host.
      - `systemd-resolved` present → per-link domain routing (`resolvectl domain`/`dns`) scoped to
        `zecurity0`. This is the clean path and the only one that supports per-domain routing natively.
      - No `systemd-resolved` → decide **now** whether to (a) edit `/etc/resolv.conf` with a backup and
        restore, or (b) **refuse to enable** OS DNS and fall back to the documented `hosts` workflow.
        **Recommend (b).** Rewriting `resolv.conf` races with NetworkManager/dhcpcd and is the classic
        source of "my DNS broke after uninstalling a VPN".
- [x] **Reliable teardown on every exit path** — `zecurity-client down`, logout, daemon crash, host **⚠️ partial** — `down` and `SIGKILL`+restart both verified. Logout and host reboot were NOT tested.
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

      **❌ NOT IMPLEMENTED — design-incompatible with invariants 2–3.** See "⚠️ 'Conflict handling' is not what 12.1 asked for" above.
### 12.2 — Verify the split-tunnelling interaction (ADR-009)
- [x] Explicitly test, don't reason about it: split-tunnel mode changes which traffic enters the TUN, and **✅ verified** — no full-tunnel mode exists, so the warned-of failure cannot arise; both planes checked in the same run.
      a name that resolves to a synthetic IP whose CIDR is **not** routed into the TUN in that mode
      resolves fine and then blackholes.
- [x] ⚠️ Pairs with Phase 9.3's open item — the synthetic-CIDR route vs split-tunnelling. If 9.3 left **✅** — Phase 9.3 closed the synthetic-CIDR/ADR-009 question with an explicit verdict.
      that unresolved, resolve it here before enabling OS DNS, because DNS makes the failure silent
      (the name works, the connection just hangs).

## Build gate

```bash
cd client && cargo build && cargo test
```

## 🚩 GATE 3 — E2E (Stage 3)

- [x] `dig managed.name` (no explicit `@server`) → the synthetic IP. **✅** via `resolvectl query` (`dig` absent on the client host).
- [x] An app connects **by name** through the tunnel, with no `hosts` entry present. **✅**
- [x] TLS/SNI validation succeeds against the real certificate (this is why name access matters and raw **✅** — `tls-test.internal` SAN validated, with a negative control.
      synthetic-IP access never did).
- [x] Unmanaged names resolve normally, with unchanged latency — verify against a name that resolved **✅**
      before the daemon started.
- [x] `zecurity-client down` → DNS settings **fully restored**; `resolvectl status` / `resolv.conf` **✅**
      byte-identical to the pre-start state.
- [x] Kill the daemon with `SIGKILL` → the next start reconciles and the user's DNS is not left broken. **✅**
- [ ] Another VPN holding the same domain → clear refusal, no silent overwrite. **❌ NOT IMPLEMENTED — design-incompatible.** Invariants 2–3 keep the helper blind to other links. Tested instead: resolved's precedence (our answer wins, unmanaged DNS survives), so the safety property holds without the refusal.
- [x] Split-tunnel mode: name access behaves consistently with 12.2's decision. **✅**

## Verify (manual, on each supported platform)

- [x] `systemd-resolved` host: full pass. **✅ 7 of 8** — the rival-claim item is design-incompatible, recorded as a limitation.
- [ ] Non-`systemd-resolved` host: behaves per the 12.1 decision — either a correct backup/restore cycle,
      **❌ NOT TESTED.** No non-`systemd-resolved` host was available. The code path (refuse + `hosts` fallback) exists and is unit-tested, but has never run on such a host.
      or a clean refusal with the `hosts` fallback documented.

## Notes

- This phase touches nothing in the data plane. If it starts needing changes in `net_stack.rs`,
  `tun.rs`, or the connector, the boundary has been crossed and the design should be revisited — Phase 11
  already owns the mapping and Phase 9 owns the routing.
- Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.
- Wildcard domains remain **out of scope** (decision #1, `reserved 14`). Per-domain OS routing will
  happily send `*.internal` to us; the responder must still exact-match and `REFUSE` the rest, per
  Phase 11.

## 🔴 Controller defects found by running Gate 3 (NOT DNS bugs)

Gate 3 spent most of its time blocked by three pre-existing defects that had nothing to do with DNS. They
are the most valuable output of this phase.

### 1. One inconsistent resource row fails the ENTIRE workspace ACL compile

The blocker that cost the most. Every resource became unreachable with `reason="unknown_resource"`, and the
cause was a single row:

```text
push ACL snapshot to connector cf39b80a-…:
  compile ACL snapshot: compile acl: resource ada3a5a5-…: status "protected" requires a shield
```

`CompileACLSnapshot` aborts on the first bad resource, so **no snapshot is produced at all** and every
*other* resource in the workspace loses access. Failing closed for that one resource is right; failing the
workspace is a blast radius nobody would choose. Skipping the offending entry and logging it would have
left the other two resources working and made the symptom legible instead of universal.

**The row was created by the system itself** — see defect 2.

### 2. Revoking a connector cascade-deletes its shields without reconciling their resources

Revoking connector `e2061d99` deleted shield `s1`, but left `prot-test` with `status='protected'` and a
`shield_id` pointing at a shield that no longer existed. That state is unreachable by any UI action and is
exactly what defect 1 treats as fatal. A shield-bound resource must be demoted when its shield disappears.

Together these two turn a routine revoke into a total workspace outage, with a diagnostic that names the
wrong thing (`unknown_resource` on a resource that is perfectly fine).

### 3. Certificate expiry is unrecoverable without manual re-enrolment

The connector's cert expired 24h before Gate 3 and it could not recover:

```text
controller SPIFFE identity verified — opening Control stream
received frame=Reset { stream_id: 1, error_code: NO_ERROR }
ERROR failed to open Control stream  backoff_secs=60      (424 restarts)
```

`renewal.rs` calls the **`RenewCert` RPC**, which needs the authenticated channel that expiry removes. So
renewal is only possible *before* expiry — and this connector spent that window crash-looping through
reboots, a downed controller and three DHCP address changes.

**This is the second instance of the same pattern.** The orphaned-shield finding (Gate 2) was identical in
shape: *the only path to recovery runs through the thing that is broken.* Two independent instances make it
a design gap rather than two accidents, and it deserves an ADR of its own.

---

## Post-Phase Fixes

### Fix: `RestrictAddressFamilies=AF_UNIX` broke every helper call
**12.1.** The service unit restricted socket families to `AF_UNIX` alone. `resolvectl` resolves an
interface *name* to an ifindex via `if_nametoindex()`, which needs a netlink or inet socket — and a blocked
family makes `socket()` return `EAFNOSUPPORT`. Every `apply` and `revert` failed at interface lookup:

```text
apply FAILED iface=zecurity0: resolvectl dns zecurity0 127.0.0.1 failed:
  Failed to resolve interface "zecurity0": Address family not supported by protocol
```

The helper looked broken; the **sandbox** was. What settled it in one command was running the identical
`resolvectl` by hand as root — `rc=0`. Same command, same interface, same resolved; only the sandbox
differed. **Fix:** `RestrictAddressFamilies=AF_UNIX AF_NETLINK AF_INET AF_INET6`.

**Lesson worth keeping:** sandbox hardening needs the same revert-test discipline as code. Six
`Protect*`/`Restrict*` directives were added without exercising the code path through them, and this one
fails only at runtime, in a subprocess, with an errno that looks like a bug in something else.

### Fix: a socket-activated service keeps its old unit until it exits
**12.1 / installer.** After correcting the unit above, `daemon-reload` + reinstall still failed
*identically*, because the running helper was still serving with the previous sandbox. `daemon-reload` does
not restart a running service. The installer had the same gap — it enabled the socket but never stopped a
running instance, so an **upgrade** would silently keep the old unit. Now it stops the service first.

Same family as this sprint's stale-binary findings: the artifact changed, the running thing did not.

### Note: group membership does not reach an existing shell
**Installer.** `usermod -aG zecurity <user>` is picked up by *new* processes only. The daemon gets it via
the restart the installer performs, but an interactive shell does not until re-login — so poking the socket
by hand fails with `EACCES` and looks like a permissions bug in the helper. The installer now says so.

_(none yet)_
