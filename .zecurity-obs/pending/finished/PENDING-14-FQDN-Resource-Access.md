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

**Status:** **approved 2026-11-03** (Sathiya: "start the work", after F0 was approved). **F0 built** (PR #173). **F1 built** on `feat/pending14-f1-names` (§10). **F2 built** (§11). **F3 built** (§12). **F4 live run PASS 2026-11-03** (§13), with three fixes from the run. Ready for one PR. F1–F3 ship together as one PR at F4: F1 alone would let a name save that no client can reach yet.
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

- **F-1 Scope of v1:** exact names only, TCP only.
- **F-2 Client DNS:** Linux through systemd-resolved; the client answers resource names with fake addresses from a private pool.
- **F-3 Resolution:** the connector resolves the name inside the remote network, and refuses loopback and link-local results.
- **F-4 No wildcards.** `*.domain` resources are **not** in v1 and **not planned** as a follow-up.
- **F-5 Shield-protected names (amends F-1).** A name is an alias for a host's IP, so a name resource can be shield-protected. The connector resolves the name and routes the tunnel to the bound shield, which checks the IP as it does today, if the client has access.
- **F-6 Collisions (was O-3).** The same name and port in two remote networks of one workspace is refused when the second is saved.
- **F-7 Fake-address range (was O-2).** Names get addresses from `198.18.0.0/16` (RFC 2544); the DNS responder is `198.19.255.53`; the client refuses to activate name resources on an overlap (§4.7).
- **F-8 Permission for resolved (was O-1).** The client installers ship a polkit rule granting the four resolved link actions only to the installed client user **and only to a process in `zecurity-client.service`**, checked through the caller's cgroup (§13). Lab-verified 2026-11-03.
- **F-9 Fake addresses survive restarts (2026-11-03, Sathiya).** A name's fake address is derived from the name (FNV-1a 64 into the pool), not allocated in sequence, so it is the same after a daemon restart, an upgrade or a reboot (§4.3, §14).

## 4. Design (v1)

### 4.1 Model

A resource's `host` is either an **IPv4 literal** (unchanged behaviour) or a **DNS name**. A name resource:

- is TCP only (`protocol = 'tcp'`);
- is either **unprotected** (connector route: the connector resolves and dials) or **shield-protected** (shield route: the connector resolves and hands the IP to the bound shield, §4.4);
- uses the normal ACL, posture, geofence and session-revocation path, keyed on `resource_id` as today. Nothing there changes.

**Name rules (controller, on create and update):**

- Lowercase the name and strip one trailing dot.
- It must be a valid hostname: LDH labels of 1–63 characters, 253 characters in total, at least two labels, no `*`, and not an IP literal.
- Store the normalised form. The client and the connector compare that exact string (§4.3, §4.4).

### 4.2 Controller and console

- **Fix the silent failure (step F0, built 2026-11-03, §9):** refuse a `host` that isn't an IPv4 literal, with a user-safe error (`apperr.UserError`; the package has messages, not codes) and tests. **F1** widens it to "an IPv4 literal or a valid name" and adds the TCP-only rule for names, each with its own sentinel error and test.
- **Collision rule (F-6):** two resources with the same name and port in **different** remote networks of a workspace would be ambiguous for a client that may access both. The second is refused at save time with its own sentinel error (one name:port per workspace). Same name with a different port is fine.
- **Protecting a name resource (F-5).** The controller can't resolve the name, because it isn't in the remote network, so `AutoMatchShield` (`lan_ip = host`) can't find the shield. Instead:
  - `protectResource` takes an optional `shieldId`. It is **required** for a name resource and must be an active shield in the resource's remote network.
  - The resource stores `host` = the name and `shield_id` = the chosen shield. IP resources keep today's auto-match and `lan_ip = host` rule unchanged.
  - **Instructions to the shield carry the shield's LAN IP, never the name.** `buildSnapshotMsg`, `PushInstruction` (`controller/internal/connector/control_stream.go:392`, `:459`) and the signed resource set (`controller/internal/connector/resource_set.go:70`) take, for a name resource, `host` = the bound shield's current `lan_ip`. The shield sees an ordinary IP resource, so `validate_host`, nftables and `allows_tunnel` need no change.
  - If the shield's `lan_ip` changes, the next snapshot carries the new IP. The name binding stays valid; only DNS has to follow.
- **Console:** relabel the field "Host (IP or DNS name)", add client-side validation that mirrors the server, offer a shield picker in Protect for name resources (listing the network's shields with their LAN IPs), and show the route as today (connector or shield).
- **Schema:** no change needed (`host TEXT`). A DB `CHECK` on the format is optional defence in depth; if we add one, it's a new numbered migration `[schema: reset DB]`.
- **Discovery:** unchanged (`promoteDiscoveredService` always creates IP resources).

### 4.3 Client

1. **Name map.** On every ACL apply, the client gives each admitted name resource a fake address from a reserved pool, `198.18.0.0/16` (F-7).
   - The address is **derived from the name** (F-9): FNV-1a 64 of the name, mod 65534, from `198.18.0.1`. It is the same after a daemon restart, an upgrade or a reboot. The hash must never change; a test pins known values.
   - If two names hash to the same address, the second takes the next free one. For such a pair, which name keeps the hashed address depends on the order the daemon first sees them (the ACL order).
   - Within one daemon run, a name removed from the ACL keeps its address reserved, so the address is never reused for a different name while an old connection might still exist.
2. **Capture.** The fake address goes through the existing path unchanged: an nft mark rule `ip daddr <fake> tcp dport <port>`, a table 105 route, and the smoltcp listener keyed `(fake, port)`.
3. **Tunnel request.** For a connection to `(fake, port)`, the client sends the **name** (the ACL entry's address string) as `destination`, never the fake IP. The connector authorizes and resolves on that string.
4. **DNS responder.** A small authoritative responder runs inside the client's smoltcp stack on a second address, `198.19.255.53:53/udp` (outside the name pool), routed into `zecurity0`. Because smoltcp owns it, the daemon needs no `CAP_NET_BIND_SERVICE` and opens no kernel port 53.
   - `A <name>` for an admitted name → the fake address, TTL 30 s.
   - `AAAA` or any other type for an admitted name → NODATA at once (so apps don't wait on IPv6).
   - Any other name under a routing domain → NXDOMAIN (see 4.3.5).
   - UDP only. Answers are small, so we never set TC and never need TCP DNS.
5. **systemd-resolved wiring.** The client sets per-link DNS on `zecurity0` to `198.19.255.53`, routing domains `~<name>` for each admitted name, and `default-route=false`. Only these names go to the client; all other DNS is untouched.
   - Consequence: resolved routes **subdomains** of a resource name to us too (`~db.acme.com` also covers `x.db.acme.com`), and we answer them NXDOMAIN. That's a known limit (§4.7).
6. **Without systemd-resolved,** or if wiring fails: name resources are not activated, IP resources work as today, the daemon logs it once per change, and `zecurity-client status` reports "DNS name resources unavailable: <reason>". The client never falls back to `/etc/hosts` or `resolv.conf` edits.

### 4.4 Connector

- **Authorization:** unchanged (`resolve_resource` exact string match, then the SPIFFE ID).
- **Resolution:** for a name destination, resolve explicitly (`tokio::net::lookup_host`). Drop loopback, link-local (`169.254.0.0/16`, which covers cloud metadata), unspecified, multicast and broadcast addresses, and use IPv4 results only in v1. Connect to **the checked IP**, never to `"name:port"`, so the name can't be re-resolved to something else between the check and the connect.
- **No usable address:** the tunnel fails with a distinct error ("name did not resolve to an allowed address"), and the connector log records it. Today's `destination` field already carries the name.
- **Shield route (F-5):** the same resolve-and-filter, then `open_relay_session(shield_id, <resolved IP>, port, protocol)` (`connector/src/device_tunnel.rs:375`) with the **IP**, not the name. The shield stays the authority: `allows_tunnel` opens the tunnel only if that IP equals a resource host it protects (its LAN IP). If DNS points somewhere else, the shield refuses. The connector logs it as a distinct error ("name resolved to an address the shield does not protect"), so the admin can fix DNS. A connector, hostile or not, still can't use the shield to reach anything else (D21-R D unchanged).
- **IP destinations:** unchanged.

### 4.5 What does not change

- **No proto change.** `ACLEntry.address` is already a string, and the client-to-connector tunnel request is JSON with a string `destination`.
- No relay or shield change. A shield-protected name reaches the shield as an ordinary IP resource (§4.2).
- No schema change: `host` holds the name, and `shield_id` already exists.
- No ACL, posture, geofence or transport-plane change. Name and IP resources compile the same way (`route_type` and `shield_id` as today).
- Split tunnelling (ADR-009) is unchanged: only resource traffic enters the TUN.

### 4.6 Privilege for resolved (decided as F-8; lab-verified 2026-11-03)

On the lab host, resolved's link-DNS actions require `auth_admin` for any non-root caller. The client daemon runs as the enrolling user with only `CAP_NET_ADMIN`, so these calls will most likely be refused. Proposed fix: `client/scripts/client-install.sh` (already run as root) installs a polkit rule, `/etc/polkit-1/rules.d/50-zecurity-client.rules`. The rule grants **only** the installed client user the actions `set-dns-servers`, `set-domains`, `set-default-route` and `revert`. The uninstall path removes it.

**Lab check (2026-11-03, `inkyank-01-computer`, systemd 255.4-1ubuntu8.18, polkit 124).** A throwaway dummy link was used. Each call was run as the user through `systemd-run --uid=<user>` with `CAP_NET_ADMIN` ambient and bounding and `NoNewPrivileges`, the same way `zecurity-client.service` runs. Results:

| Case | `dns` / `domain` / `default-route` / `revert` |
|---|---|
| A: no rule, with `CAP_NET_ADMIN` | all **refused** ("Interactive authentication required") |
| B: no rule, no capability | all refused (same) |
| C: with the rule, with `CAP_NET_ADMIN` | all **allowed**; the link showed `DNS Servers: 198.19.255.53`, `DNS Domain: ~db.fqdn-test.example`, `-DefaultRoute` |
| D: with the rule, as user `nobody` | refused (the rule is scoped to one user) |
| E: with the rule, no capability | allowed (the rule alone grants it; `CAP_NET_ADMIN` plays no part for resolved) |
| F: with the rule, `revert` | allowed |

So without the rule the v1 client design can't work, and with it it can. Still to check in F3: the dummy link showed `Current Scopes: none` (no address, no carrier), so it remains to confirm that on the real `zecurity0`, with its address, resolved actually sends queries for `~<name>` to `198.19.255.53` and the smoltcp responder.

### 4.7 Known limits (state them in the console and docs)

- Linux with systemd-resolved only.
- A shield-protected name must resolve, **on the connector**, to the bound shield's LAN IP. If it doesn't, connections are refused at the shield (and logged); nothing is reached unprotected.
- Subdomains of a resource name answer NXDOMAIN while the client is connected.
- The fake pool can collide with a network that really uses `198.18.0.0/15` (benchmark labs, some test networks), or with another tool that uses the same range for the same trick. If a real resource IP in the ACL falls inside it, the client turns name resources off and the status says so. Once that resource is changed or removed, restarting the tunnel (`zecurity-client down && zecurity-client up`) turns them back on; no daemon restart is needed.
- Two names that hash to the same fake address (about a 2% chance with 50 names) keep their addresses across a restart only if the ACL lists them in the same order.
- Apps that bypass the system resolver (their own DoH, or a hardcoded `8.8.8.8`) won't see the fake address.
- IPv4 only, at both ends.
- An app that cached a name's real IP before the client connected keeps using that IP until its own cache expires.

## 5. Not in scope

- Wildcard or domain resources (F-4; not planned).
- UDP or "any"-protocol name resources.
- The controller checking DNS at protect time (it can't see the remote network's DNS).
- IPv6 (fake AAAA, IPv6 at the connector).
- Non-Linux clients, and Linux without systemd-resolved.

## 6. Decisions still open

- **O-1 (decided as F-8, lab-verified 2026-11-03):** the installer polkit rule (§4.6).
- **O-2 (decided as F-7, 2026-11-03): the fake pool and the responder address.** Candidates considered:

  | Range | For | Against |
  |---|---|---|
  | **`198.18.0.0/15`** (RFC 2544, recommended) | IANA special-purpose, never routed on the internet, almost never used on real LANs; already the usual "fake-IP" range for proxy tools (e.g. Clash/mihomo default `198.18.0.1/16`) | a benchmark lab or another fake-IP tool on the same device can overlap |
  | `100.64.0.0/10` sub-range (CGNAT) | next to the TUN's `100.64.0.1` | Tailscale uses the whole `/10` (and `100.100.100.100` for its DNS); carrier CGNAT uses it too |
  | `10.x` / `172.16.x` / `192.168.x` | none | collides with real company LANs and real resource IPs |
  | `240.0.0.0/4` (reserved) | unused anywhere | some OS stacks and apps refuse it; avoid |

  Chosen: names from `198.18.0.0/16`, responder `198.19.255.53`, refuse on overlap (§4.7). A config override can come later if a customer needs it.
- **O-4 (decided 2026-11-03):** ship F0 now, on its own (§9).

## 7. Tests (build gate)

- **Controller:** name normalisation and validation (table test: case, trailing dot, IP literal, `*`, label and total lengths); a name resource with `udp`/`any` is refused; the name:port collision is refused; `protectResource` on a name without `shieldId`, or with a shield from another network or an inactive shield, is refused; a protected name's shield snapshot, pushed instruction and signed resource set carry the shield's `lan_ip`, never the name, and follow a `lan_ip` change; ACL output carries the normalised name with `route_type = shield` and the `shield_id`. DB-backed tests run, not skip.
- **Client:** fake-pool allocation is stable across ACL updates and never reuses an address; nft batch and routes for name entries; the DNS responder answers A, AAAA→NODATA, other types, NXDOMAIN, and malformed packets dropped; the tunnel request carries the name, not the fake IP; resolved wiring builds the right calls and degrades cleanly when resolved is missing.
- **Connector:** the resolve-and-filter function (loopback, link-local, metadata, unspecified and multicast are refused; mixed results keep only allowed ones; IPv6 is ignored); it connects to the checked IP; a name that resolves to nothing allowed is refused with its error; on a shield route the resolved IP (not the name) goes to `open_relay_session`.
- **Live (lab):** a real client reaches `db.<lab-domain>` that resolves only on the connector's network; access follows grant and revoke and posture like an IP resource; removing the resource removes the DNS answer; a name resolving to `127.0.0.1` is refused at the connector; a shield-protected name that resolves to the shield's LAN IP works through the shield, and the same name pointed (in DNS) at another host is refused by the shield; with resolved stopped, IP resources keep working and status explains why names don't.

## 8. Phases

| Phase | What | Depends on |
|---|---|---|
| **F0** ✅ | Controller: refuse an invalid host (non-IPv4 for now), with tests; console validation and label | built 2026-11-03 (§9) |
| **F1** ✅ | Controller and console: name validation and normalisation, TCP-only rule, collision rule, protect with an explicit shield, shield instructions carry `lan_ip` | built 2026-11-03 (§10) |
| **F2** ✅ | Connector: resolve, filter, connect to the checked IP; shield route hands the resolved IP to the shield | built 2026-11-03 (§11) |
| **F3** ✅ | Client: fake pool, capture, DNS responder, resolved wiring, status; installer polkit rule (F-8); confirm resolved routes `~<name>` queries to the responder on the real `zecurity0` (→ F4) | built 2026-11-03 (§12) |
| **F4** ✅ | Live lab run (§7), then one PR | PASS 2026-11-03 (§13) |

## 9. F0 record (2026-11-03)

**What changed**
- `controller/internal/resource/store.go`: new `NormalizeHost` and `ErrInvalidHost` (an `apperr.UserError`, so the console shows the message). `Create` and `Update` call it first.
  - Accepted: an IPv4 literal; surrounding spaces are trimmed and the canonical form is stored.
  - Refused: empty input, names, IPv6 (including IPv4-mapped and zone suffixes), ports or CIDRs, leading zeros, and `0.0.0.0`, `255.255.255.255` and multicast.
  - `127.0.0.1` stays allowed, since a shield can protect a loopback-bound service.
  - This replaces `Update`'s old "host must not be empty" check. That was a plain error, so the console masked it as a generic failure.
  - All paths are covered, including `promoteDiscoveredService`: discovery can report a real IPv6 bound address, which now gets a clear refusal where it used to create an unreachable resource.
- `controller/graph/resource.graphqls`: comments only (they aren't schema descriptions, so no codegen).
- `admin/src/lib/resourceHost.ts`: a client-side mirror, used by the Create and Edit resource modals. The label is now "Host IP (IPv4 address)", and the create hint no longer says a shield is required.
- **Consequence:** an existing resource whose stored host is invalid can't be edited until its host is fixed, because the console sends every field. Such a resource never worked anyway, and the project is pre-production.

**Gates**
- `go build ./...` and `go vet ./...` clean.
- `go test -count=1 ./...` with every `*_TEST_DATABASE_URL` plus `AUTH_TEST_VALKEY_URL=redis://localhost:6379/15`: 1655 pass, 0 fail, 2 expected skips (`TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`). The first run without the Valkey URL skipped 12 more; they were re-run with it and pass.
- `scripts/resource-test-fixture.sh`: resolvers and resource ok.
- New tests: `TestNormalizeHost` (table) and `TestCreateUpdateRefuseInvalidHost` (DB-backed: nothing written on a refused create, nothing changed on a refused update, trimmed host stored).
- Mutants: removing the check from `Create` or from `Update` each fails the new test.
- Admin: 30 files / 242 tests (new `resourceHost.test.ts`, the same cases as the Go table), build ok. Lint is unchanged at 17; the one finding in a touched file (`CreateResourceModal.tsx` `as any`) is pre-existing on `main`.

## 10. F1 record (2026-11-03)

**Controller (`internal/resource/store.go`)**
- **`NormalizeHost`** now returns the host and its kind (`HostIPv4` or `HostName`). A name is lowercased and loses one trailing dot. It must have at most 253 characters, at least two labels, LDH labels of 1–63 characters with no leading or trailing hyphen, and a letter in the last label. No wildcards.
- **`IsNameHost`** and the SQL pattern `ipv4HostPattern` tell the two kinds apart. A test checks that Go and SQL agree on the same hosts.
- **New user-safe errors:** `ErrNameNeedsTCP`, `ErrNameCollision`, `ErrNameProtectedMove`, `ErrProtectNoShield` and `ErrProtectNameNeedsShield`. Protect's old error was a plain error, which the console masked.
- **`shieldHostSQL` / `Row.ShieldHost`:** for a name, the bound shield's `lan_ip`; for an IPv4 host, the host. It's used at every shield-facing site:
  - `desiredForShield` (snapshot, fingerprint, reconciler);
  - `GetPendingForShield` (removes on reconnect);
  - the signed set's removes (`connector/resource_set.go`);
  - `PushInstruction` (via `instructionForRow`);
  - the `RecordAck` host comparison and `logIgnoredAck`.
- **`Create`:**
  - A name must use TCP.
  - A name is never auto-matched to a shield.
  - It runs in a transaction. For a name it takes `pg_advisory_xact_lock(workspace, name)` and refuses an overlapping port range on the same name in another remote network (F-6).
  - IPv4 hosts aren't checked for collisions: they legitimately repeat across networks.
- **`Update`:**
  - The final host and protocol must satisfy the TCP rule, including a protocol-only edit.
  - The collision check runs under the same lock.
  - A protected name can't move to another network (`ErrNameProtectedMove`).
  - A new **name** host doesn't count as a shield change, so name→name and IPv4→name keep the bound shield, stay `protected` and push nothing.
  - Name→IPv4 uses the existing LAN-IP match.
- **`MarkProtecting(…, shieldID)`:**
  - IPv4: unchanged auto-match. A given `shieldID` must be the shield on that IP.
  - Name: `shieldID` is required and must be an active shield in the same workspace and remote network. A foreign shield answers like a missing one.
  - It is still one atomic `UPDATE`.
- **GraphQL:** `protectResource(id: ID!, shieldId: ID)`. The audit entry records `shield_id`. gqlgen's import reorder in `resource.resolvers.go` was reverted.
- **A shield `lan_ip` change:** the heartbeat re-sync leaves names alone, and `handleShieldStatus` pushes a snapshot, which carries the new LAN IP for names (checked in the code; covered by a test on the desired set).

**Console (`admin/`)**
- `lib/resourceHost.ts`: `resourceHostKind` mirrors the server, with the same test table, and `isNameHost` is added.
- The Create and Edit modals: label "Host (IPv4 address or DNS name)", and the TCP rule for names.
- `lib/protectShield.ts` `nameProtectTarget`, with tests.
- `ResourceDetail`: for a name, a shield picker (the network's active shields, "name (LAN IP)") and an explanation that the name must resolve, on the connector, to that shield's LAN IP. All three Protect buttons pass `shieldId` for names.
- `ProtectResource` mutation: `$shieldId`; codegen regenerated.

**Tests and gates**
- **New:** `TestDNSNameResources`, 12 DB-backed subtests:
  - Go/SQL agreement;
  - TCP rule on create, on a protocol-only edit, and on UDP IPv4→name;
  - no auto-match;
  - collision with edges, the same network, IPv4 exempt, update and self-edit;
  - a race with 8 concurrent saves through a test seam that pauses between check and insert;
  - protect with no, foreign, offline, other-network, bad or valid shield;
  - IPv4 protect with a wrong shield;
  - acks (the name ignored, the LAN IP counted);
  - the unprotect remove;
  - updates to a protected name;
  - IPv4→name;
  - a `lan_ip` change.

  Also new: `TestShieldInstructionsCarryLANIPForNames` (pushed instruction, snapshot, signed resources and removes). `TestNormalizeHost` gained names.
- **Mutants:** 11, all killed:
  - shield host uses the name;
  - ack compares the name;
  - no TCP rule on update;
  - no collision check;
  - no lock (needed the seam);
  - a new name host counts as a shield change;
  - protect accepts any shield for a name;
  - no last-label letter rule;
  - signed removes use the name;
  - pushed instruction uses the name;
  - plus F0's create and update mutants.
- **Go:** `go build`, `go vet`, and `go test -count=1 ./...` with every DB variable and `AUTH_TEST_VALKEY_URL`: 1669 pass, 0 fail, 2 expected skips. `scripts/resource-test-fixture.sh` ok.
- **Admin:** 30 files / 268 tests, `tsc` clean, build ok. Lint is unchanged at 17; the changed files lint clean.

## 11. F2 record (2026-11-03)

**Connector**
- New module `connector/src/name_target.rs`:
  - `is_name`;
  - `allowed_resolved`: IPv4 only; never loopback, link-local (incl. 169.254.169.254), unspecified, multicast or broadcast;
  - `pick_allowed`: the first allowed result, in resolver order;
  - `resolve_target`: an IP literal is used as given (unchanged behaviour), and a name goes through `tokio::net::lookup_host` plus the filter;
  - `ERR_NAME_UNRESOLVED` (`"NAME_UNRESOLVED"`).
- `device_tunnel::handle_stream`, after authorization (unchanged: exact match on the name) and the gate re-checks:
  - A name destination is resolved **once**.
  - The direct route dials `"<checked IP>:port"`.
  - The shield route calls `open_relay_session(shield_id, <checked IP>, …)`, so the shield's own `allows_tunnel` check (its LAN IP only) stays the authority.
  - A name over UDP is refused before any lookup.
  - On failure the client gets `ok:false, error:"NAME_UNRESOLVED"`, and the controller gets an `error` access log with `error = "name_unresolved: <reason>"` and the name as `destination`.
- No proto change: the tunnel request is JSON with a string `destination`.

**Tests and gates**
- **Unit tests** (`name_target`, 7):
  - the allowed and refused address table;
  - order and skipping in `pick_allowed`;
  - nothing allowed;
  - IP literals untouched (incl. 127.0.0.1 for IP resources);
  - `localhost` refused (loopback);
  - `.invalid` is a lookup error.
- **Handler** (`acl_diff_session_tests`, the production `handle_stream`):
  - A name resource resolving only to loopback is refused with `NAME_UNRESOLVED` and an error access log, although an echo server listens on that loopback port.
  - A name over UDP is refused.
- **Mutants:** "no address filter" and "names not resolved" are each killed.
  - **Not covered by an automated test:** the shield route passing the resolved IP. It needs a live shield session, so it's covered by the F4 lab run.
- **Gates:** `cargo build`; `cargo test` 288 + 4 pass, 0 fail; clippy has no findings in the changed code; `name_target.rs` rustfmt-formatted. Existing files were not reformatted, because `cargo fmt` would format the whole crate.

## 12. F3 record (2026-11-03)

**Client**
- **`client/src/name_map.rs`** (process-wide, behind a mutex):
  - Names get fake addresses from 198.18.0.1 to 198.18.255.254, sequentially. A name keeps its address for the daemon's lifetime, and an address is never reused.
  - `is_resource_name` mirrors the controller's rule for stored names.
  - `resource_ipv4(address)`: the IPv4 literal, or the fake address for a name. It also records any real resource IP inside `198.18.0.0/15`; while one exists, names are disabled (§4.7).
  - `destination_for(ip)`: the name for a fake address.
  - `answer_a`: only **active** names (the currently applied ACL) resolve.
  - `DNS_RESPONDER_ADDR = 198.19.255.53`.
- **The parse points now use it:** `routable_key` (startup preflight, `net_stack::run`, the hot-apply plan) and `build_transports_by_resource_with_crl`. The rest of the capture path (nft marks, table-105 routes, AnyIP listeners, the transport map) is unchanged and works on the fake address.
- **`net_stack` `FlowTable::service`:** the tunnel request `destination` is `name_map::destination_for(ip)`, so the connector gets the name.
- **`client/src/dns.rs`:** a pure authoritative answerer.
  - A query for an active name gets the fake address, TTL 30 s, AA.
  - Other types get NODATA; other names get NXDOMAIN; other opcodes get NOTIMP.
  - Malformed packets, responses, more than one question, or a compression pointer in QNAME are dropped.
  - An EDNS OPT in the query is ignored, and the reply carries no additional records.
- **`net_stack`:** a smoltcp UDP socket bound to 198.19.255.53:53 (`new_dns_socket`), served every loop iteration (`serve_dns`).
- **`client/src/dns_link.rs`:**
  - `names_in(applied)` lists the TCP name entries.
  - `sync` runs `ip route replace 198.19.255.53/32 dev zecurity0` (main table), then `resolvectl dns zecurity0 198.19.255.53`, `resolvectl domain zecurity0 ~<name>…` and `resolvectl default-route zecurity0 false`. With no names it runs `resolvectl revert zecurity0`.
  - It never fails the caller. On failure the names are inactive and the status says why.
  - It is called from `handle_up` (forced: a new link has no resolved config), from the resource hot-apply commit, and `clear()` from `handle_down`.
- **Status:** `IpcResponse.dns_names`, printed as `DNS names:  active (N names)` or `unavailable: <reason>`. Absent when there are no names.
- **`client/scripts/client-install.sh`:** installs `/etc/polkit-1/rules.d/50-zecurity-client.rules`, granting only the installing user the four resolved link actions. It's removed on `-f` reinstall, and skipped with a warning if there's no rules.d directory or the user name is unusual.

**Tests and gates**
- **New unit tests:**
  - `dns` (6): A answer, case, NODATA, NXDOMAIN, dropped packets, NOTIMP.
  - `name_map` (6): the name rule, stable and distinct addresses, never reused, pool end, reserved-range conflict, destination mapping.
  - `dns_link` (3): name selection, the exact commands, and sync with status, failure and revert.
- **End to end through the production capture interface** (`a_resource_name_resolves_to_its_fake_address_and_tunnels_by_name`): a DNS query packet to 198.19.255.53:53 is answered from that address with the fake IP. A TCP connection to the fake IP is tunnelled with `destination` set to the name.
- **Changed test:** `routable_key_is_ipv4_tcp_or_empty_protocol` (a name is now routable over TCP; `localhost` and UDP are not).
- **Polkit rule:** rendered from the installer and evaluated under Node with a stub `polkit` (7 cases: the four actions for the user; other actions and other users refused).
- **Mutants**, all killed:
  - "tunnel sends the fake IP" (2 tests fail);
  - "inactive names still resolve" (2 tests fail).
- **Gates:** `cargo test` 334 pass, 0 fail; clippy has no findings in the new code (the remaining warnings were already there); the new files are rustfmt-formatted. `cargo build --release` needs `ZECURITY_PLATFORM_ROOT_PEM_FILE` by design, so dev builds use `client/scripts/dev-build.sh`.

**Still to verify live (F4):**
- resolved routes `~<name>` queries to 198.19.255.53 on the real `zecurity0` (the O-1 dummy link showed `Current Scopes: none`);
- the shield route receives the resolved IP (F2);
- a real application resolves and connects;
- hot-add and hot-remove of a name;
- a name resolving to loopback is refused at the connector;
- resolved stopped → status explains it, and IP resources keep working.

## 13. F4 live run (2026-11-03)

**Topology**
- **Controller:** branch build, user unit `fqdn-controller` on `inkyank-01-computer` (.164).
- **Client:** this machine's real `zecurity-client.service`, branch build (`93acaa0f…`) installed with `scripts/client-local-install.sh` (incl. the polkit rule), user `r3-geo1`, workspace `rbac-r3`.
- **Connector and shield:** branch builds on **luffy** (.48, Arch), installed with the local installers. The connector couldn't stay on the client's host; see finding 1.
- **Test names:** public wildcard DNS (`*.nip.io`), so names resolve to real lab addresses without editing any DNS.
- **No DB reset:** F0–F3 add no migration.

**Results (all PASS)**

| Check | Result |
|---|---|
| Client status with the rule | `DNS names: active (2 names)` |
| resolved on the real `zecurity0` (open item from O-1) | `Current Scopes: DNS`, `DNS Servers: 198.19.255.53`, routing domains = exactly the resource names; `resolvectl query` answers `198.18.0.1` via `zecurity0` in 14.6 ms; `example.com` unaffected |
| Connector route by name | `curl http://192-168-1-164.nip.io:18080` → 200; the server saw the request from luffy (.48), i.e. luffy resolved and dialled |
| Name resolving to loopback | refused at the connector; controller access log `error name_unresolved: … not allowed: [127.0.0.1]` |
| API refusals | UDP name → "must use protocol tcp"; `*.example.com` → invalid host |
| Protect a name | without `shieldId` → "needs a shield chosen"; with the luffy shield → `protecting` → `protected` in seconds (the shield got and acked its LAN IP) |
| Shield route by name (the F2 path without an automated test) | `curl http://192-168-1-48.nip.io:18082` → 200, `route_type=shield`; direct `192.168.1.48:18082` from the client host → timeout (the shield enforces) |
| Shield-protected name whose DNS points at another host | the shield refuses; the wrong host got **0** requests; the controller logs `name_not_on_shield: 192-168-1-164.nip.io resolved to 192.168.1.164, which is not the shield's LAN IP …` (after fix 3) |
| Hot add (tunnel up) | the new name is resolvable and routed without a restart |
| Hot remove | the routing domain is gone within 2 s; the name resolves through normal DNS again |
| Polkit rule removed, tunnel restarted | `DNS names: unavailable: systemd-resolved configuration failed: … resolvectl dns zecurity0 … exit status: 1`; no DNS scope; the IPv4 control resource still 200; the name falls back to its real IP, and the shield blocks the direct path |
| Rule restored, tunnel restarted | `active (2 names)` again; both names 200; fake addresses unchanged (`.1`, `.3`) |

**Findings, all fixed on the branch**
1. **The connector looped when a client ran on its host** (`942b7998`).
   - The connector's resolver goes through systemd-resolved, which routed the names to that client's answerer. The connector got `198.18.0.1`, dialled it, and the tunnel looped back into the client.
   - Fix: the connector refuses `198.18.0.0/15` like loopback, giving `NAME_UNRESOLVED`. Verified live in the co-located setup ("not allowed: [198.18.0.1]").
2. **The polkit rule was user-wide** (`cc421d6a`; automated security review).
   - Lab probes (systemd 255, polkit 124): resolved passes polkit **no link details**, and `subject.system_unit` is **null**.
   - The rule now also requires the caller's cgroup `0::/system.slice/zecurity-client.service`, read via `polkit.spawn(cat /proc/<pid>/cgroup)`.
   - Lab-checked: the service unit is allowed; another unit of the same user is refused. Node check: 9 cases.
   - Same rule in `scripts/client-local-install.sh`.
3. **Shield refusals were invisible to the controller** (`b7f93369`).
   - The connector logged them only on its host and told the client `INTERNAL`.
   - Every shield-route failure now sends an `error` access log. A name the shield refuses gets the client error `NAME_NOT_ON_SHIELD` and a log naming both the name and the address.

**Observations, not caused by this branch**
- On this desktop, a process in the user's **graphical session** could set resolved link DNS **without** any of our rules, while `pkcheck` reported "authentication required". This is a local polkit/desktop behaviour, not investigated further. Our rule adds nothing for session processes and is scoped to the service unit.
- `deleteResource` with an unknown ID answers `INTERNAL`, not "not found".
- The GraphQL `remoteNetworks { shields { lanIp } }` path returned null while the DB row had the LAN IP.

## 14. Restart-stable fake addresses (2026-11-03, F-9)

**Why:** with sequential allocation, addresses started again at `198.18.0.1` on every daemon restart. An application or resolver that cached `a.example → 198.18.0.1` could then reach `b.example`'s service. That isn't an access bypass (the connector authorizes the name it receives), but it is misdirection, for example credentials sent to the wrong service.

**Change (client only)**
- `name_map.rs`: `home_slot(name)` is FNV-1a 64 mod 65534. `fake_ip_for` takes the hashed address, or probes to the next free one (wrapping); it returns None when the pool is full.
- `clear_conflicts()` runs from `dns_link::clear()` on tunnel down, so a reserved-range clash is re-checked at the next bring-up.
- The status message now says to change or remove the resource and restart the tunnel.

**Tests**
- `stable_addresses_are_pinned`: values computed independently in Python.
- `a_restart_gives_the_same_addresses_whatever_the_order`.
- `a_hash_collision_takes_the_next_free_address`: a real colliding pair.
- `probing_wraps_and_the_pool_can_fill`.
- `clearing_conflicts_re_enables_names`.
- Mutants killed: "hash changed" and "sequential allocation".
- `cargo test`: 339 pass. Clippy has no findings in the changed files.

**Live check**
- On the F4 lab (client on this PC, connector and shield on luffy), with this build installed, the names resolved to their pinned addresses: `192-168-1-164.nip.io → 198.18.130.157` and `192-168-1-48.nip.io → 198.18.46.98`.
- After `systemctl restart zecurity-client` (new PID), the tunnel came back by itself, the addresses were unchanged, and both names still answered HTTP 200.

