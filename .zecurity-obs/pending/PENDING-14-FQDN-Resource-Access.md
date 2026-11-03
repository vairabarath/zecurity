---
type: adr
status: pending
id: PENDING-14
domain: data-plane
priority: P2
created: 2026-07-03
updated: 2026-11-03
related: [ADR-009, ADR-015, ADR-017]
tags: [pending, adr, data-plane, dns, resources]
---

# PENDING-14: DNS name (FQDN) resource access

**Status:** design note, **awaiting approval** (rewritten 2026-11-03; the 2026-07-03 version predated the shield, relay and posture work). Nothing is built.
**Date:** 2026-11-03
**Base:** `main` (`f5bd339f`, which now contains all of `fixed-pendings`).
**Scope:** its own epic.

---

## 1. Problem

Admins want to grant access to a resource by name, for example `db.internal.acme.com:5432`, not by IP. Internal names often resolve only inside the remote network (private or split-horizon DNS), and their IPs change (cloud load balancers, k8s services).

Today this fails **silently**. The console accepts a name and stores it, but no client can ever reach it (§2).

## 2. What exists today (facts, checked against `main` on 2026-11-03)

| Part | Today | Where |
|---|---|---|
| Schema | `resources.host TEXT NOT NULL` with no format check; `protocol IN ('tcp','udp','any')`; `UNIQUE (tenant_id, remote_network_id, host, name)` | `controller/migrations/007_resources.sql:8-10`, `021_connector_resource_routes.sql` |
| Controller | `resource.Create` doesn't parse `host`. `AutoMatchShield` (`lan_ip = host`) misses and the resource is stored unprotected. `Update` rejects only an empty host. | `controller/internal/resource/store.go:111-160`, `:466` |
| GraphQL / console | `host: String!`; the comment and the form say "Host IP"; the only check is non-empty | `controller/graph/resource.graphqls:22`, `admin/src/components/CreateResourceModal.tsx:77` |
| ACL compiler | Copies `host` verbatim into `ACLEntry.address` (a `string`) | `controller/internal/policy/store.go:457`, `compiler.go:252`, `proto/client/v1/client.proto:273` |
| **Client** | **Drops any non-IPv4 address**: `routable_key` and `build_transports_by_resource` skip entries that don't parse as IPv4. Capture is nft `ip daddr X tcp dport P` → mark `0x5a` → table 105 → TUN `zecurity0` (`100.64.0.1/32`) → smoltcp (AnyIP). The client never touches DNS. | `client/src/net_stack.rs:1081`, `client/src/daemon.rs:5978`, `client/src/tun.rs:29`, `:207` |
| Client privileges | Runs as the enrolling user with `CAP_NET_ADMIN` only (`CapabilityBoundingSet=CAP_NET_ADMIN`) | `client/zecurity-client.service` |
| smoltcp | `socket-udp` is already enabled but unused | `client/Cargo.toml:62` |
| Connector | The tunnel request is JSON `{destination: String, port, protocol}`. Authorization is an exact string match on address, port and protocol, then the SPIFFE ID (`resolve_resource`). The dial is `TcpStream::connect("dest:port")`, which **already resolves names** with the connector's system resolver, without filtering. | `connector/src/device_tunnel.rs:42`, `:214`, `:537`; `connector/src/policy/mod.rs:157` |
| Shield | Protects only its own LAN IP (`validate_host`); `allows_tunnel` parses the destination as an IP | `shield/src/resources.rs:157`, `:213` |
| systemd-resolved (lab host) | systemd 255, polkit 124. `org.freedesktop.resolve1.set-dns-servers` and `set-domains` are **`auth_admin` for every non-root caller** (`allow_any`/`allow_inactive` `auth_admin`, `allow_active` `auth_admin_keep`). | `/usr/share/polkit-1/actions/org.freedesktop.resolve1.policy` |

So the connector half largely works already. The missing half is the client: it never captures traffic for a name.

## 3. Decisions taken (2026-11-03, Sathiya)

- **F-1 Scope of v1:** exact names only, TCP only, connector-routed only.
- **F-2 Client DNS:** Linux through systemd-resolved; the client answers resource names with fake addresses from a private pool.
- **F-3 Resolution:** the connector resolves the name inside the remote network, and refuses loopback and link-local results.
- **F-4 No wildcards.** `*.domain` resources are **not** in v1 and **not planned** as a follow-up.

## 4. Design (v1)

### 4.1 Model

A resource's `host` is either an **IPv4 literal** (unchanged behaviour) or a **DNS name**. A name resource:

- is TCP only (`protocol = 'tcp'`);
- is never shield-protected; it routes via a connector of its remote network;
- uses the normal ACL, posture, geofence and session-revocation path, keyed on `resource_id` as today. Nothing there changes.

**Name rules (controller, on create and update):**

- Lowercase the name and strip one trailing dot.
- It must be a valid hostname: LDH labels of 1–63 characters, 253 characters in total, at least two labels, no `*`, and not an IP literal.
- Store the normalised form. The client and the connector compare that exact string (§4.3, §4.4).

### 4.2 Controller and console

- **Fix the silent failure (step F0, can ship on its own):** refuse a `host` that is neither an IPv4 literal nor a valid name. Refuse a name resource whose protocol isn't `tcp`. Refuse `Protect` on a name resource with a clear error ("name resources can't be shield-protected"), not the current `lan_ip` miss. Each refusal has its own `apperr` code and test.
- **Collision rule:** two resources with the same name and port in **different** remote networks of a workspace would be ambiguous for a client that may access both. Refuse the second at save time (one name:port per workspace). Same name with a different port is fine.
- **Console:** relabel the field "Host (IP or DNS name)", add client-side validation that mirrors the server, show the Protect action as unavailable for name resources, and show "via connector" in the route column.
- **Schema:** no change needed (`host TEXT`). A DB `CHECK` on the format is optional defence in depth; if we add one, it's a new numbered migration `[schema: reset DB]`.
- **Discovery:** unchanged (`promoteDiscoveredService` always creates IP resources).

### 4.3 Client

1. **Name map.** On every ACL apply, the client gives each admitted name resource a fake address from a reserved pool (proposed `100.127.0.0/16`, inside the CGNAT range the TUN already uses). The mapping is **stable for the daemon's lifetime**: the same name keeps the same address across ACL updates. A name removed from the ACL keeps its address reserved until the daemon restarts, so it is never reused for a different name while an old connection might still exist.
2. **Capture.** The fake address goes through the existing path unchanged: an nft mark rule `ip daddr <fake> tcp dport <port>`, a table 105 route, and the smoltcp listener keyed `(fake, port)`.
3. **Tunnel request.** For a connection to `(fake, port)`, the client sends the **name** (the ACL entry's address string) as `destination`, never the fake IP. The connector authorizes and resolves on that string.
4. **DNS responder.** A small authoritative responder runs inside the client's smoltcp stack on a second address, proposed `100.127.255.53:53/udp`, routed into `zecurity0`. Because smoltcp owns it, the daemon needs no `CAP_NET_BIND_SERVICE` and opens no kernel port 53.
   - `A <name>` for an admitted name → the fake address, TTL 30 s.
   - `AAAA` or any other type for an admitted name → NODATA at once (so apps don't wait on IPv6).
   - Any other name under a routing domain → NXDOMAIN (see 4.3.5).
   - UDP only. Answers are small, so we never set TC and never need TCP DNS.
5. **systemd-resolved wiring.** The client sets per-link DNS on `zecurity0` to `100.127.255.53`, routing domains `~<name>` for each admitted name, and `default-route=false`. Only these names go to the client; all other DNS is untouched.
   - Consequence: resolved routes **subdomains** of a resource name to us too (`~db.acme.com` also covers `x.db.acme.com`), and we answer them NXDOMAIN. That's a known limit (§4.7).
6. **Without systemd-resolved,** or if wiring fails: name resources are not activated, IP resources work as today, the daemon logs it once per change, and `zecurity-client status` reports "DNS name resources unavailable: <reason>". The client never falls back to `/etc/hosts` or `resolv.conf` edits.

### 4.4 Connector

- **Authorization:** unchanged (`resolve_resource` exact string match, then the SPIFFE ID).
- **Resolution:** for a name destination, resolve explicitly (`tokio::net::lookup_host`). Drop loopback, link-local (`169.254.0.0/16`, which covers cloud metadata), unspecified, multicast and broadcast addresses, and use IPv4 results only in v1. Connect to **the checked IP**, never to `"name:port"`, so the name can't be re-resolved to something else between the check and the connect.
- **No usable address:** the tunnel fails with a distinct error ("name did not resolve to an allowed address"), and the connector log records it. Today's `destination` field already carries the name.
- **IP destinations:** unchanged.

### 4.5 What does not change

- **No proto change.** `ACLEntry.address` is already a string, and the client-to-connector tunnel request is JSON with a string `destination`.
- No relay or shield change.
- No ACL, posture, geofence or transport-plane change. Name and IP resources compile the same way.
- Split tunnelling (ADR-009) is unchanged: only resource traffic enters the TUN.

### 4.6 Privilege for resolved (**blocking open decision, §6 O-1**)

On the lab host, resolved's link-DNS actions require `auth_admin` for any non-root caller. The client daemon runs as the enrolling user with only `CAP_NET_ADMIN`, so these calls will most likely be refused. Proposed fix: `client/scripts/client-install.sh` (already run as root) installs a polkit rule, `/etc/polkit-1/rules.d/50-zecurity-client.rules`. The rule grants **only** the installed client user the actions `set-dns-servers`, `set-domains`, `set-default-route` and `revert`. The uninstall path removes it.

### 4.7 Known limits (state them in the console and docs)

- Linux with systemd-resolved only.
- Subdomains of a resource name answer NXDOMAIN while the client is connected.
- The fake pool can collide with another CGNAT user on the device (Tailscale uses `100.64.0.0/10`; so do some carriers). If the pool overlaps an existing route, the client refuses to activate name resources and reports it.
- Apps that bypass the system resolver (their own DoH, or a hardcoded `8.8.8.8`) won't see the fake address.
- IPv4 only, at both ends.
- An app that cached a name's real IP before the client connected keeps using that IP until its own cache expires.

## 5. Not in scope

- Wildcard or domain resources (F-4; not planned).
- UDP or "any"-protocol name resources.
- Shield-protected name resources.
- IPv6 (fake AAAA, IPv6 at the connector).
- Non-Linux clients, and Linux without systemd-resolved.

## 6. Decisions still open

- **O-1 (blocking): how the client gets permission to configure resolved.** Options:
  - (a) an installer polkit rule (recommended, §4.6);
  - (b) run the daemon as root;
  - (c) a small root helper.
  First verify on the lab that the call really is refused without the rule and allowed with it. If (a) fails, the v1 client design changes.
- **O-2: the fake pool and the responder address.** `100.127.0.0/16` and `100.127.255.53` are proposals. Choose them, and choose what happens on overlap (refuse, as proposed, or make the pool configurable).
- **O-3: the collision rule.** Refuse the same name:port across a workspace's remote networks (proposed), or allow it and let the client pick by remote network.
- **O-4: step F0 timing.** Ship the "refuse non-IP host" fix now, before the rest, so today's silent failure ends. It becomes "refuse an invalid host" when names land.

## 7. Tests (build gate)

- **Controller:** name normalisation and validation (table test: case, trailing dot, IP literal, `*`, label and total lengths); a name resource with `udp`/`any` is refused; `Protect` on a name is refused with its code; the name:port collision is refused; ACL output carries the normalised name. DB-backed tests run, not skip.
- **Client:** fake-pool allocation is stable across ACL updates and never reuses an address; nft batch and routes for name entries; the DNS responder answers A, AAAA→NODATA, other types, NXDOMAIN, and malformed packets dropped; the tunnel request carries the name, not the fake IP; resolved wiring builds the right calls and degrades cleanly when resolved is missing.
- **Connector:** the resolve-and-filter function (loopback, link-local, metadata, unspecified and multicast are refused; mixed results keep only allowed ones; IPv6 is ignored); it connects to the checked IP; a name that resolves to nothing allowed is refused with its error.
- **Live (lab):** a real client reaches `db.<lab-domain>` that resolves only on the connector's network; access follows grant and revoke and posture like an IP resource; removing the resource removes the DNS answer; a name resolving to `127.0.0.1` is refused at the connector; with resolved stopped, IP resources keep working and status explains why names don't.

## 8. Phases

| Phase | What | Depends on |
|---|---|---|
| **F0** | Controller: refuse an invalid host (non-IP for now), with codes and tests; console label | approval of O-4 |
| **F1** | Controller and console: name validation and normalisation, TCP-only rule, Protect refusal, collision rule | approval |
| **F2** | Connector: resolve, filter, connect to the checked IP | F1 |
| **F3** | Client: fake pool, capture, DNS responder, resolved wiring, status; installer polkit rule (O-1) | F1, O-1 verified |
| **F4** | Live lab run (§7), then one PR | F2, F3 |
