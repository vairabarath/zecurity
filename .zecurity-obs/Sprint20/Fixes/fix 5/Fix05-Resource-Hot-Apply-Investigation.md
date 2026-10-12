---
type: fix-investigation
sprint: 20
fix: 5
title: Resource Hot-Apply Investigation
status: investigation
date: 2026-10-07
component: client
related: [fix-01-phase-2a, fix-05]
tags:
  - client
  - tunnel
  - net-stack
  - fix-05
---

# Fix 05 — Resource Hot-Apply Investigation

## Status

**investigation**

Nothing here is implemented, accepted or fixed. No production code was changed. Fix 01 Phase 2-A is
treated as frozen reference behaviour and is not modified by anything below.

Branch `feat/sprint20-m1-phase2` @ `86cda74`. Line numbers were re-grepped on this commit.

Labels: **[src]** = proven by reading source in this repo or a pinned crate; **[live]** = proven by an
earlier live run (cited); **[open]** = needs a decision; **Unknown / requires live validation** =
can't be proven from the repository.

---

## 1. Problem Statement

Any resource change that reaches the client causes a full VPN/TUN restart. That restart deletes
`zecurity0`, flushes the nft table, ip rule and table 105, aborts the `net_stack` task and therefore
resets **every** open flow, including flows to resources that did not change.

Why, from the code:

1. **The data plane is frozen at `handle_up`. [src]** `handle_up` (`client/src/daemon.rs:559-770`)
   computes the allowed entries once (`allowed_entries_for`, `:631`). From those entries it builds:
   - the nft marks, the fwmark rule and the table-105 /32 routes (`TunManager::configure_allowed_flows`, `:725`);
   - the smoltcp `/32` interface addresses and one listener per `(ip, port)` (`net_stack::run`, `net_stack.rs:699-725`);
   - the transport map (`:665-685`).

   `allowed_entries` is passed **by value** into `net_stack::run(dev, allowed_entries, transport_rx, relay_resync)`
   (`:749`). After that the running task has exactly one input channel: the Phase 2-A
   `watch::Receiver<Arc<TransportMap>>`, which is read only at flow accept (`net_stack.rs:568`, `transports_for` `:649-654`).
   There is no path that adds or removes a listener or a smoltcp address.
2. **The kernel side has no delta API. [src]** `TunManager::configure_allowed_flows` (`tun.rs:62-141`)
   starts with `cleanup_policy_routes()` (`tun.rs:195-234`). That deletes the whole nft table, the
   fwmark rule and flushes table 105, then rebuilds everything. Calling it on a live tunnel would briefly
   un-route every flow, so it isn't usable for hot-apply as it stands.
3. **The classifier sends every resource-key change to Structural. [src]** `classify_applied`
   (`daemon.rs:867-875`) compares `structural_view`s (`:847-864`). These blank only
   `preferred_connector_id` and `coords`. Any difference in `(address, port, protocol, remote_network_id)`,
   or in the multiset of entries, therefore lands in `Structural` and goes to `down_up()` (`:1078-1081`).

So the restart isn't just a classifier choice. Today it is the **only** mechanism that can change
listeners, smoltcp addresses, nft marks and routes.

---

## 2. Existing Fix 01 Boundary

Reference: `Fixes/fix 1/Phase2A-Connector-Topology-Hot-Apply.md` (status `live-accepted`), and
`Phase2A-Live-Acceptance-2026-10-05.md` Row D (a resource change still restarts; ifindex 12 → 13; hold reset).

### 2.1 The three-way decision (confirmed in source)

| Class | Decided by | Action | Code |
|---|---|---|---|
| `NoChange` | `candidate == applied` | keep the tunnel, log `effective config unchanged, keeping tunnel` | `daemon.rs:868-869`, `:1074-1077` |
| `TransportOnly` | `structural_view(candidate) == structural_view(applied)` | `hot_apply_transport_map`: build the full map off to the side → re-check `Arc::ptr_eq(tun_handle)` + `effective_config == candidate` under `state.write()` → `transport_tx.send` → replace `TunHandle` with the new `applied` | `:870-871`, `:936-1016`, `:1082-1103` |
| `Structural` | anything else, or `applied`/`acl`/`device` missing | `down_up()` = `handle_down` + `handle_up` | `:872-873`, `:886-888`, `:1078-1081` |

The decision is preceded by a dead-task check (`abort.is_finished()` → `down_up`, `:1052-1071`). It runs
inside `perform_tunnel_restart` (`:1112-1150`), which `TunnelRestartCoordinator` serialises
(`register_restart_request`, `:1235`). That covers all 5 callers: IPC Sync `:290`, IPC Resources
`:325`, PostLoginState `:522`, early transport resync `:3375` and the 60 s tick `:3441`.

### 2.2 What `AppliedConfig` contains

`runtime.rs:28-58`:

- identity: `spiffe_id`, `certificate_pem`, `private_key_pem`, `tpm_key_material`, `ca_cert_pem`;
- `entries: Vec<AppliedEntry>`, where each entry is `address, port, protocol, remote_network_id, preferred_connector_id, coords: Vec<AppliedCoords>`.

`effective_config` (`daemon.rs:3944-4033`) builds it from the **SPIFFE-filtered** ACL entries. It
resolves coords through `resolve_entry_coords` (`:3924-3942`, transport plane first, ACL fallback per
RN), puts the preferred connector first, sorts the rest, and sorts the entries.

**Not in `AppliedEntry`, so already invisible to the decision. [src]** `resource_id`, `name`,
`route_type`, `shield_id`, entries not allowed for this device, `version`, `generated_at`, and the
top-level `relay_addr`. The test `effective_config_metadata_differences_equal`
(`daemon_tests.rs:517`) pins this.

### 2.3 Resource-related changes currently Structural (pinned by tests in `daemon.rs::restart_decision_tests`)

| Change | Test | Line |
|---|---|---|
| entry added (new allowed resource) | `restart_decision_entry_added_restarts` | `:2017` |
| this device loses access (`allowed_spiffe_ids` cleared) | `restart_decision_resource_access_removed_restarts` | `:2038` |
| IP change | `resource_ip_change_is_structural` | `:2047` |
| port change | `resource_port_change_is_structural` | `:2055` |
| protocol change | `resource_protocol_change_is_structural` | `:2062` |
| `remote_network_id` change (D3) | `resource_remote_network_change_is_structural` | `:2072` |
| resource + connector change together | `resource_and_connector_change_together_is_structural` | `:2092` |
| unknown field difference | `classify_non_connector_field_differences_are_structural` | `:2168` |

Each of them asserts **1 down/up, 0 builds, no swap** (`assert_structural`, `:2009-2014`).

### 2.4 Is Structural broader than necessary?

**Yes, in three places [src]:**

1. **Entries that produce no data-plane state.** The client data plane covers only IPv4-literal, TCP
   (or empty-protocol) entries:
   - `handle_up` filters `protocol == "tcp" || ""` and parses as `IpAddr::V4` (`daemon.rs:702-714`);
   - `net_stack::run` does the same (`net_stack.rs:700-710`);
   - `build_transports_by_resource_with_crl` skips non-IPv4 (`daemon.rs:4085-4088`).

   `effective_config` keeps **all** allowed entries, though. So adding, removing or editing a
   `udp`/`any`, hostname or IPv6 resource is classified Structural and restarts the VPN, but the
   restarted tunnel has exactly the same listeners, addresses, routes and nft rules.
2. **`remote_network_id` (D3).** Every client artefact derived from an entry's RN is the coords list, so
   the transport-map slot. Listeners, addresses, nft and routes depend only on `(ip, port)`. D3 was a
   deliberate fail-closed choice in 2-A, not something the code requires (see §5.4).
3. **Unrelated flows.** Even a genuine add or remove of one `(ip, port)` key only needs that key's
   listener, address, route and nft rule changed. The restart kills every other flow as a side effect.

---

## 3. Resource Lifecycle Trace

### 3.1 Controller

| Step | File : function | Type | Data | Notes |
|---|---|---|---|---|
| Resource row | `controller/migrations/007_resources.sql`; soft-delete dropped by `017_resources_drop_soft_delete.sql` | `resources` table | `host TEXT`, `protocol IN ('tcp','udp','any')`, `port_from`, `port_to`, `status`, `shield_id`, `remote_network_id` | Hard delete only, so there is **no "restore" and no resource enable/disable**. `host` is free text (a hostname is possible). |
| Create | `graph/resolvers/resource.resolvers.go:20` `CreateResource` → `internal/resource/store.go:122` `Create` | `CreateInput` | host, protocol, ports, RN; `AutoMatchShield` | `NotifyPolicyChange` |
| Update | `resource.resolvers.go:43` `UpdateResource` → `store.go:406` `Update` | `UpdateInput` (`store.go:396-403`) | RN, name, description, protocol, port_from, port_to | **`host` is not editable [src]**, so an address change is delete + create (new `resource_id`). Notifies unconditionally, then again if `ACLRelevantUpdate` (`store.go:459-461`), which omits `remote_network_id` even though the compiler reads it. Both are known Fix 02 territory; not part of this work. |
| Delete | `resource.resolvers.go:114` `DeleteResource`; `:157` `ForceDeleteResource` | — | pending/unprotected → `DeleteRow`; protected/failed → `MarkDeleting` (tombstone, excluded by the compiler's `status != 'deleting'`) | `NotifyPolicyChange` |
| Access | `internal/policy/store.go:199` `AssignResourceToGroup`, `:228` `SetRuleEnabled`, `:149/:161` group member add/remove; SCIM `internal/scim/groups.go`, `directory_service.go` | `access_rules`, `group_members` | — | `NotifyPolicyChange` |
| Posture | `internal/policy/compiler.go:310` `applyPosture`; `cache.go:72-96` expiry → `notifyExpiryEvent` → `cmd/server/main.go:254` → `NotifyPolicyChange` | `CompiledACL.ValidUntil` | allowed SPIFFEs gated per resource | Posture expiry is a real access-removal path. |
| Compile | `policy/store.go:305` `ListEnabledRulesWithResources` (`ar.enabled = TRUE AND r.status != 'deleting'`) → `policy/compiler.go:26` `CompileACLSnapshot` | `clientv1.ACLSnapshot` / `ACLEntry` | entry key `(resource_id, host, port_from, protocol)` (`compiler.go:40-45`); `allowed_spiffe_ids`, `route_type`, `shield_id`, `remote_network_id`, `preferred_connector_id = shields.connector_id` | `port_to` is ignored, so only `port_from` reaches clients. RNs come with **active** connectors (`store.go:429-450`, ordered by `last_heartbeat_at`). |
| Version | `policy/notifier.go:57` `NotifyPolicyChange` | per-workspace `atomic.Uint64` | bump + `cache.Invalidate` + push hook | Connector status changes bump it too (`internal/connector/control_stream.go`, `disconnect_watcher.go`). |
| Serve (client) | `internal/client/service.go:714` `GetACLSnapshot` | `GetACLSnapshotResponse` | `up_to_date` iff `known_version == snap.Version` | Workspace-wide snapshot; the client filters by SPIFFE. |
| Serve (connector) | `internal/connector/acl_push.go:123`, `control_stream.go:833` | same `ACLSnapshot` | pushed proactively on notify | The connector gets the **same workspace snapshot**. |
| Transport plane | `internal/transport/compiler.go` `CompileTransportSnapshot` / `assembleTransportSnapshot` | `TransportSnapshot` | active connectors per RN plus present-but-empty active RNs | **Resource changes never touch it [src]** (no resource input). |

### 3.2 Client

| Step | File : function | Type | Consumes | Restart needed? |
|---|---|---|---|---|
| Poll | `daemon.rs:3076` `run_acl_sync_scheduler` (60 s, `ACL_REFRESH_TTL_SECS` `:38`) → `:3397` `sync_and_restart_if_changed` | — | — | — |
| ACL fetch/store | `:3766` `sync_acl_now_with` | `AclSyncResult{changed}` | `changed = old_version != new_version`; the **old snapshot is overwritten** (`:3810-3812`) | n/a |
| Transport fetch | `:3648` `fetch_and_store_transport` | — | separate version | n/a |
| Decision | `:1040` `run_restart_decision` → `:880` `classify_delta` | `ConfigDelta` | `TunHandle.applied` vs `effective_config(current acl, transport, device)` | the decision |
| Effective config | `:3944` `effective_config` | `AppliedConfig` | SPIFFE filter + coords | pure |
| Allowed entries | `:905` `allowed_entries_for` | `Vec<AclEntry>` | SPIFFE filter | pure |
| Kernel routing | `tun.rs:62` `configure_allowed_flows` | `AllowedFlow{ip, port}` | TCP + IPv4 entries → nft `ip daddr X tcp dport P meta mark set 0x5a`, `ip rule fwmark 0x5a lookup 105 prio 49`, `ip route replace X/32 dev zecurity0 table 105` | **today yes** (flush-first). **Could be live**: nft rules and table-105 routes are per key/IP; the fwmark rule and the TUN link are resource-independent. |
| TUN link | `tun.rs:29` `TunManager::create` | `zecurity0` `100.64.0.1/32` | nothing resource-derived | **no**: resource-independent |
| smoltcp addrs | `net_stack.rs:713-719` `iface.update_ip_addrs` | `heapless::Vec<IpCidr, IFACE_MAX_ADDR_COUNT>` | one /32 per resource IP, plus `100.64.0.1` | **today yes** (no channel). **Could be live**: `update_ip_addrs` is callable at any time on the owned `iface` (`smoltcp-0.11.0/src/iface/interface/mod.rs:337`). |
| Listeners | `net_stack.rs:523-540` `FlowTable::new`, `:827` `new_listen_socket` | `listen_handles: HashMap<(Ipv4Addr,u16), SocketHandle>` | one listener per key; re-armed per accept (`:556-563`) | **today yes**. **Could be live**: `FlowTable` already inserts and replaces listeners every accept. |
| Transport map | `daemon.rs:4062` `build_transports_by_resource_with_crl`; `net_stack.rs:639` `TransportMap`, `:649` `transports_for` | `HashMap<(Ipv4Addr,u16), Option<Vec<Arc<ClientTransport>>>>` | read once at accept (`:568`) | **no**: already hot-swappable (2-A) |
| Accept / fail-closed | `net_stack.rs:568-617` | — | `Some(Some)` → spawn relay; `Some(None)`/`None` → `abort()` (RST) | — |
| Relay | `net_stack.rs:844` `relay_tcp_to_quic` | `TunnelRequest{destination, port, "tcp"}` | candidates in order; the first `ok` wins | — |
| Existing flow | `ActiveRelay` (`net_stack.rs:232-249`) keyed by `SocketHandle` in `FlowTable.active_relays` | — | the smoltcp socket keeps its 4-tuple; the relay task keeps only the selected stream | `handle_down` aborts the task → all flows reset |
| Connector authz | `connector/src/device_tunnel.rs:186-258` `handle_stream` | `ResourceAcl` | `resolve_resource(dest, port, proto)` + `allowed_spiffe_ids`; registers `(spiffe, resource_id)` in `SessionRegistry`; post-registration `is_allowed` recheck | — |
| Connector revocation | `connector/src/control_stream.rs:635-648` → `policy/mod.rs:40-46` `update_and_revoked` | `HashSet<(spiffe, resource_id)>` | cancels every live session whose `(spiffe, resource_id)` left the allow set | independent of the client |

---

## 4. Resource Change Matrix

Columns:

| Col | Question |
|---|---|
| wire | Changes the wire/routing behaviour of an existing tunnel? |
| ex | Affects existing connections? |
| new | Affects new connections only? |
| TUN | Needs the TUN interface rebuilt? |
| krt | Needs kernel routes / nft changed? |
| DNS | Needs DNS state changed? |
| tx | Needs connector transport state changed? |
| hot? | Can be hot-applied safely? |

Key: `K` is the client data-plane key `(IPv4, tcp port)`. "No-DP" means the entry produces no
data-plane artefact (non-TCP, non-IPv4-literal).

**DNS: the client has no DNS state at all [src].** Nothing in `client/src` resolves resource
hostnames, and the only DNS use is connector-address resolution in `build_transport_from_coords`
(`daemon.rs:4133-4139`). The DNS column is therefore **no** for every row and is left out below.

| # | Category | Change | Client effect (code path) | wire | ex | new | TUN | krt | tx | hot? |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | Lifecycle | resource added (TCP/IPv4, allowed for this device) | new K: listener + smoltcp addr (if new IP) + nft rule + table-105 route (if new IP) + map slot | no (other K) | no | yes | no | yes (add) | yes (new slot) | **yes**. If the desired address set exceeds the current smoltcp capacity, that is an apply failure / unsupported desired state (§8.F, §12), **not** a restart boundary |
| 2 | Lifecycle | resource added (No-DP) | entry in `AppliedConfig` only | no | no | no | no | no | no | **yes**: no data-plane change at all (today: needless restart) |
| 3 | Lifecycle | resource removed (TCP/IPv4) | K goes: listener, addr (if IP unused), nft rule, route (if IP unused), map slot; flows on K must end (§5.2) | yes for K | yes, K flows only | yes | no | yes (del) | yes | **yes**, with in-loop close of K flows |
| 4 | Lifecycle | resource removed (No-DP) | none | no | no | no | no | no | no | **yes** |
| 5 | Lifecycle | restored | not a state: soft delete was removed (mig 017); a "restore" is a create, so row 1 | — | — | — | — | — | — | as row 1 |
| 6 | Lifecycle | resource disabled/enabled | no resource-level flag; the nearest is `access_rules.enabled` (`SetRuleEnabled`), so row 15 | — | — | — | — | — | — | as row 15 |
| 7 | Identity | `resource_id` changes (delete + recreate with the same host/port/proto/RN) | `resource_id` isn't in `AppliedEntry`, so **already NoChange** on the client. The connector revokes `(spiffe, old_id)` and cancels those sessions (`control_stream.rs:638-641`). | no | yes, via the **connector** | no | no | no | no | already no restart |
| 8 | Identity | name change | not in `AppliedEntry`, so already NoChange | no | no | no | no | no | no | already |
| 9 | Identity | UUID | same as `resource_id` (row 7) | | | | | | | |
| 10 | Address | IP change | impossible in place (`UpdateInput` has no `host`); arrives as remove K_old + add K_new (rows 3 + 1) | yes | yes, K_old flows | yes | no | yes | yes | **yes** (as remove + add) |
| 11 | Address | hostname change / hostname resource | No-DP (no client DNS; `parse::<IpAddr>` fails) | no | no | no | no | no | no | **yes** (no-op) |
| 12 | Address | port change (`UpdateResource.port_from`, same `resource_id`) | remove `(ip, old)` + add `(ip, new)`; the IP stays, so addr/route stay | yes for `(ip, old)` | yes, old-port flows | yes | no | yes (nft rules only) | yes | **yes**, with in-loop close of old-port flows (§5.3; the connector does **not** cancel them) |
| 13 | Address | multiple addresses/ports in one snapshot | set difference of K | per K | per K | per K | no | yes | yes | **yes**, applied as one ordered batch |
| 14 | Address | protocol change tcp ↔ udp/any | TCP→other = remove K; other→TCP = add K | per K | per K | per K | no | yes | yes | **yes** |
| 15 | Policy | ACL / group / rule-enabled / posture change that adds **this device** to an entry | = row 1 or 2 for that entry | | | | | | | **yes** |
| 16 | Policy | same, removing **this device** | = row 3 or 4 | | | | | | | **yes**, with close (§9) |
| 17 | Policy | ACL / group change affecting **other** devices only | the SPIFFE filter drops it, so **already NoChange** | no | no | no | no | no | no | already |
| 18 | Network | `remote_network_id` change, same K | only `AppliedEntry.remote_network_id` + coords change, so the map slot changes; no TUN/nft/addr/listener change | no for existing flows | no (see §5.4) | yes | no | no | yes | **technically TransportOnly-shaped**; recommendation: keep Structural for now (D3, §5.4) [open] |
| 19 | Network | RN becomes unreachable (0 active connectors) | transport RN present-but-empty → slot `None` → new flows fail closed. That is connector topology, already TransportOnly (2-A). | — | — | — | — | — | — | already hot-applied |
| 20 | Network | RN reachable again | TransportOnly (2-A) | | | | | | | already |
| 21 | Connector | connector set / preferred connector / topology for a resource | `preferred_connector_id`/`coords` only, so TransportOnly (2-A) | | | | | | | already (frozen 2-A) |
| 22 | Connector | resource `route_type` connector ↔ shield, `shield_id` change | not in `AppliedEntry`; the client sends only `{dest, port, "tcp"}` and the connector routes. The preferred connector change that comes with it is TransportOnly. | no | no | no | no | no | maybe | already |
| 23 | Metadata | description, `port_to`, tags | not compiled into `ACLEntry` (`compiler.go:246-257`), so never reaches the client | no | no | no | no | no | no | already |
| 24 | Edge | last allowed TCP resource removed | `handle_up` refuses an empty set (`daemon.rs:633-640`, `:716-723`), so today a restart **ends with the VPN down** | yes | all | — | — | — | — | **not a restart boundary**: no immutable state needs rebuilding (§6.2). The user-visible outcome is a behaviour decision [open, Q3] |
| 25 | Edge | two allowed entries with the same K (different `resource_id`) | map and listeners collapse to one key (`daemon.rs:4114`, `net_stack.rs:533`) | — | — | — | — | — | — | the hot-apply diff must be keyed by K, not by entry |
| 26 | Identity | device identity (cert/key/CA) | Phase 1/2-A Structural (D4) | | | | | | | **stays Structural** (out of scope) |

---

## 5. Existing-Flow Semantics

### 5.0 What the architecture guarantees today [src]

- The client has **no per-flow authorisation state**. A flow is a smoltcp socket plus an `ActiveRelay`
  (`net_stack.rs:232-249`), keyed by `SocketHandle`. Its destination is recoverable from the
  socket's `local_endpoint()` (smoltcp keeps the 4-tuple for the life of the connection), but it isn't
  recorded separately.
- On a Structural change every flow is reset by `handle_down` (task abort → sockets dropped → relay
  tasks see `quic_to_tcp_tx.closed()` and stop, `net_stack.rs:1022`).
- **Authoritative per-flow revocation lives on the connector [src].** Each tunnel registers
  `(client_spiffe, resource_id)` in `SessionRegistry` (`device_tunnel.rs:240-250`). Every ACL push runs
  `update_and_revoked`, and every revoked pair is cancelled (`control_stream.rs:635-648`). There is a
  post-registration recheck for the scan race (`device_tunnel.rs:252-258`). The connector learns of a
  change by **push**; the client learns by a **60 s poll**.

### 5.1 Resource added

- Existing flows: unaffected. New K state is additive: a new listener and nft rule for a key no live
  flow uses, and a new map slot.
- New flows: once the ordered add completes (§8.C) a SYN to K is marked, routed to `zecurity0`,
  accepted by the new listener, finds `Some(Some)` in the map and relays. Before that point, packets to K
  take the normal kernel route (same as today before the restart finishes).
- Edge: an **already open non-tunnelled** connection to K (it went out the normal route before K was
  managed) gets marked by the new nft rule. smoltcp has no socket for its 4-tuple and answers RST. That
  is identical to today's restart. **Unknown / requires live validation** for the exact app-visible
  timing.

### 5.2 Resource removed (or this device loses access)

Current architecture and security semantics require:

- **New connections must be refused.** The listener is removed (or the map slot is `None`), so smoltcp
  RSTs or the accept fails closed, and the nft mark is then removed.
- **Existing connections on K must end.** Three reasons, from the code:
  1. Once the nft rule for K is gone, the flow's next packets leave via the normal kernel route with
     the smoltcp-side sequence state. The flow is broken anyway, so leaving it "open" only means a hang
     instead of a clean RST.
  2. That matches today's client-side semantics (restart = RST).
  3. The connector cancels revoked `(spiffe, resource_id)` sessions on its own, so the remote side is
     torn down regardless (§9).
- Therefore: **abort the K sockets inside the smoltcp loop** before removing the nft rule. Per the
  Fix 05-A contract, close from inside the loop: `socket.abort()`, mark `terminated`, let `drive_relay`
  reap it. Dropping the `ActiveRelay` closes `quic_to_tcp_rx`, so the relay task exits via
  `quic_to_tcp_tx.closed()` (`net_stack.rs:1022`).
- Flows on other keys: untouched. That's the point of the change.

### 5.3 Resource address/port changed

- **IP change**: arrives as remove + add with a new `resource_id` (§3.1), so 5.2 for the old K
  (the connector also cancels, because the old `resource_id` disappears) and 5.1 for the new K.
- **Port change (same `resource_id`)**: the connector does **not** cancel. `allow_set` is keyed by
  `(spiffe, resource_id)` (`policy/mod.rs:139-152`), which is still allowed, and the established
  connector→resource TCP connection doesn't depend on the ACL port. So **only the client** ends the
  old-port flow today (by restarting). Hot-apply must abort `(ip, old_port)` flows explicitly, or this
  silently becomes "old-port flows survive". The old address/port must **not** stay valid for new
  connections: `resolve_resource` at the connector would deny them anyway (`policy/mod.rs:104-124`).
- New connections use the new K immediately after publication.

### 5.4 Resource `remote_network_id` changed

From the implementation:

- **Client [src].** The only RN-derived state is the coords list. `resolve_entry_coords` keys on
  `entry.remote_network_id` (`daemon.rs:3929`). Listeners, smoltcp addresses, nft and routes are
  unaffected. A new map with the new RN's connectors is exactly what 2-A already builds and publishes.
  So the client does **not** require a TUN restart for an RN move.
- **Existing flow [src].** It is bound to a stream on an old-RN connector. The connector doesn't cancel
  it: `(spiffe, resource_id)` is still allowed, and connectors receive the whole workspace ACL
  (`acl_push.go:123`), so the old-RN connector still has the entry. The flow keeps reaching whatever
  the old connector reaches at `host:port`. If the same IP in the new RN is a *different host*, the
  existing flow keeps talking to the old host. A full restart prevents that, and a map-only hot-apply
  would not.
- **Shield-routed [src, plus the Phase 2-A live finding].** `UpdateResource` does not re-run
  `AutoMatchShield` (`store.go:406-450`), so `shield_id`/`preferred_connector_id` may still point to the
  old RN's shield and connector. The client would then put a connector in `preferred_connector_id` that
  isn't in the new RN's list (dropped by `ordered_transport_connectors_for_entry`). Controller-side
  consistency of an RN move for shield resources is **Unknown / requires live validation**.
- **Conclusion.** It doesn't *have* to stay Structural for client-mechanical reasons. It **should stay
  Structural in the first slice** because:
  1. hot-applying it changes existing-flow semantics (old-network flows survive);
  2. the controller side of an RN move for shield resources is unproven;
  3. it is a rare admin action;
  4. D3 is part of frozen 2-A and its test pins it.

  Revisit only together with "close K flows on RN change" [open, Q2].

---

## 6. Restart-Required Boundaries

> Revised 2026-10-07 (design correction). Only changes **proven** to need a rebuild of the tunnel's
> immutable identity or state are restart boundaries. Implementation limits and error handling are
> **not** classified as Structural.

### 6.1 Mandatory restart boundary (Phase 1 of resource hot-apply)

| Change | Why a restart is retained |
|---|---|
| `remote_network_id` change on an entry whose K is unchanged | D3 (frozen 2-A test `resource_remote_network_change_is_structural`, `daemon.rs:2072`). Client-mechanically it is only a map-slot change, but without a client close an existing flow survives on the old-RN connector: the connector doesn't cancel, because `resource_id` is unchanged (§5.4, §15 Q6). The controller can also leave a moved shield resource on its old-RN shield (§15 Q6, S3). Retained for Phase 1; a later slice can hot-apply it with a close of that key's flows plus a controller fix. |
| Device identity change (cert / key / TPM / CA / SPIFFE) | D4 (Phase 1/2-A, frozen). The identity is part of `AppliedConfig` (`runtime.rs:52-56`) and is baked into every `TunnelPool`/`RelayPool` (`build_transport_from_coords`, `daemon.rs:4141-4161`). Unchanged. |

Code analysis found **no other** resource-related change that requires reconstruction of immutable
tunnel state. The TUN link, the fwmark rule and `relay_crl` are resource-independent (§3.2). Listeners,
smoltcp addresses, table-105 routes, nft rules and the transport map all have a hot update path in the
§8 design.

### 6.2 Explicitly NOT restart boundaries

| Situation | Classification | Handling |
|---|---|---|
| Desired smoltcp address set exceeds `IFACE_MAX_ADDR_COUNT` | **Apply failure / unsupported desired state.** Not Structural. | Validated **before mutation** (§8.C step 1b). Never silently dropped: today's `let _ = addrs.push(...)` (`net_stack.rs:716`) must not be reproduced. The rest of the delta is still applied, and the unapplied additions are reported and retried (§8.F, Q8). The eventual capacity solution belongs to the separate 2-IP investigation (§12). |
| Resource change and connector change in the same snapshot | **One combined `ResourceDelta`** (§8.A). Not Structural. | Applied in one ordered pass with one transport-map publish (§8.C). The 2-A test `resource_and_connector_change_together_is_structural` (`daemon.rs:2092`) changes meaning, like the 2-A precedent for `…connector_absent…`. |
| The result has zero allowed IPv4/TCP entries | **Behaviour decision** [open, Q3]. Not Structural. | No immutable state needs rebuilding: `net_stack` runs with any listener count. `handle_up` refusing to *start* with zero entries (`daemon.rs:633-640`, `:716-723`) is start-time policy. Today a restart ends with the VPN **down**. **Resolved in §17: keep the VPN/TUN up with zero routable resources** (a valid runtime state; ordinary hot-apply removal). |
| Any failure **after** the first data-plane mutation | **Failure handling** (§8.F), not classification. | Full restart, because the partially changed state can't be asserted equal to either `applied` or the candidate. |
| Dead `net_stack` task; missing ACL/device; unclassifiable field difference | **Inherited Phase 1/2-A fail-closed fallbacks** (frozen). Not resource categories. | Unchanged: full restart. |

Everything else that today restarts because of resources is a hot-apply candidate (§7) or needs no
data-plane change at all.

---

## 7. Hot-Apply Candidate Boundaries

### 7.1 Required table

| Change | Current behaviour | Can hot-apply? | Existing flows | New flows | Reason |
|---|---|---|---|---|---|
| add resource (IPv4/TCP) | Structural → full restart (`restart_decision_entry_added_restarts`) | **Yes**. Over smoltcp capacity = apply failure for that addition, not a restart (§6.2) | all unaffected | new K usable after the ordered add | additive per-K state (listener, addr, nft rule, route, map slot) |
| remove resource (IPv4/TCP) | Structural → full restart | **Yes**, with in-loop close | K flows aborted (RST); others unaffected | K refused (no listener → RST; no mark) | per-K state; revocation kept by client abort plus connector cancel |
| change resource IP | remove + add (`host` not editable) → restart | **Yes** (as remove + add) | old-K flows aborted; the connector also cancels (new `resource_id`) | new K immediately | §5.3 |
| change resource port | Structural → restart | **Yes**, with in-loop close of `(ip, old)` | old-port flows aborted by the **client** (the connector does not cancel) | new port immediately; old port refused | §5.3 |
| change resource hostname / add a hostname resource | Structural → restart (no data-plane effect) | **Yes**, no-op | unaffected | unchanged | No-DP entry; the client has no DNS |
| change protocol tcp ↔ udp/any | Structural → restart | **Yes** (= remove or add K) | per §5.2 | per §5.1 | the client data plane is TCP-only |
| change `remote_network_id` | Structural (D3) | **Not in slice 1** (technically TransportOnly-shaped) | would survive on the old connector; a restart kills them | new RN coords | §5.4 |
| change connector association / preferred | TransportOnly (2-A) | already | unaffected | new map | frozen 2-A |
| resource change + connector change in the same snapshot | Structural (2-A `resource_and_connector_change_together_is_structural`) | **Yes**, one combined apply (§8.C) | per the resource part; connector part leaves flows untouched (2-A) | new map with both changes | §6.2 |
| resource ACL change (this device gains/loses) | Structural | **Yes** (= add/remove K) | loss: K flows aborted; the connector cancels | per add/remove | §9 |
| resource group/policy change, other devices only | NoChange (SPIFFE filter) | already | unaffected | unaffected | `effective_config` filter, `daemon.rs:3950-3953` |
| metadata-only (name, description, tags, `port_to`, `route_type`/`shield_id`) | NoChange (not in `AppliedEntry` / not compiled) | already | unaffected | unaffected | `daemon_tests.rs:517` |
| result = zero allowed TCP entries | restart → `handle_up` fails → VPN down | **Yes, mechanically**; the outcome is a behaviour decision (Q3) | removed-K flows reset | per Q3 | §6.2 |
| last allowed resource added back while the VPN is down | n/a (no running tunnel) | n/a | — | — | `perform_tunnel_restart` returns early when `tun_slot` is `None` (`:1117-1119`) |

### 7.2 Classification conditions

A delta is a resource hot-apply candidate iff **all** of the following hold:

1. identity is equal;
2. for every K present in both, the entry's `remote_network_id` is equal;
3. every other difference is explainable as K added, K removed, a No-DP entry added/removed/changed,
   or a connector-field change, **in any combination**.

Address capacity and the zero-K result are **not** classification inputs. Capacity is validated
before mutation as part of the apply (§8.C step 1b, §8.F). Zero-K is handled per Q3 (§6.2).

---

## 8. Proposed Architecture

**Not implemented. Design only, and only for state the code shows exists.**

### A. Classification

Extend `ConfigDelta`, don't replace it. The fail-closed construction of 2-A (prove equality of
everything not explicitly classified) is kept.

```
NoChange          candidate == applied                       (unchanged)
TransportOnly     structural_view equal                      (unchanged, frozen 2-A)
ResourceDelta     NEW: identity equal AND the conditions in §7.2 hold
Structural        everything else                            (unchanged fallback)
```

- A new **data-plane view** `dp_view(cfg) -> (identity, BTreeSet<K>, rn_by_K)` is derived from
  `AppliedConfig`. Here K = `(Ipv4Addr, u16)` from IPv4-literal entries with protocol `tcp`/empty,
  the same filter `handle_up`/`net_stack::run` use. **Extract that filter into one helper used by all
  three sites**, so the classifier can't drift from what the data plane actually builds (the same lesson
  as 2-A's `allowed_entries_for`).
- `ResourceDelta` carries `{added: BTreeSet<K>, removed: BTreeSet<K>, addr_add: BTreeSet<Ipv4Addr>,
  addr_del: BTreeSet<Ipv4Addr>}` (IPs whose last K went or whose first K arrived).
- "No-DP only" differences (added/removed/edited non-TCP or hostname entries, and nothing else) produce
  `ResourceDelta` with empty sets. The apply then only updates `applied` (no kernel or loop work).
  This alone removes a class of needless restarts.
- A delta with resource changes **and** connector-field changes is **one** `ResourceDelta`. The
  transport map is built once for the full candidate, so it carries both the connector changes and the
  added/removed K slots, and it is published once in the ordered apply (§8.C). Two change categories
  occurring together is **not** a reason for Structural.
  - A pure connector delta stays `TransportOnly` and keeps using the frozen 2-A
    `hot_apply_transport_map`, unchanged.
  - The 2-A test `resource_and_connector_change_together_is_structural` (`daemon.rs:2092`) changes
    meaning and must be rewritten to assert one combined apply. That is a deliberate, documented change
    to a 2-A test, as 2-A did with `…connector_absent…`.
- Structural only for the §6.1 boundary: RN change for an existing K (D3) and identity change. Plus the
  inherited Phase 1/2-A fail-closed fallbacks (missing inputs, unclassifiable fields).
- Capacity and zero-K are **not** classifier outcomes (§6.2). They are handled in the apply (§8.C step 1b,
  §8.F, Q3, Q8).

### B. Resource state (only what exists)

| Derived state | Owner today | Proposed owner / update path |
|---|---|---|
| allowed K set | `allowed_entries` moved into `net_stack::run` | stays derived from `applied` (`dp_view`) |
| smoltcp `/32` addrs | `iface` local in `run` | in-loop, on command |
| listeners | `FlowTable.listen_handles` | in-loop: `FlowTable::add_listener(K)` / `remove_listener(K)` |
| K-flow close | none (no flow→K index) | in-loop: iterate `active_relays`, match `sockets.get(handle).local_endpoint()` to K, `abort()` + `terminated` |
| transport map | `watch` (2-A) | unchanged channel; a new map built for the candidate |
| nft marks per K | `TunManager` in `tun_slot` | new incremental `TunManager` methods (below) |
| table-105 routes per IP | `TunManager` | `ip route replace` / `ip route del` per IP |
| fwmark rule, TUN link | `TunManager` | **unchanged**: resource-independent |
| `route_count` in `TunHandle` | `TunHandle` | updated with `applied` |

No ACL/policy state exists on the client beyond the SPIFFE filter. Authorisation stays on the connector.
So there is **no "PolicyOnly"** class: a policy change matters to the client only as "this device's K set
changed", which is `ResourceDelta`.

**New net_stack input.** `run(.., resource_rx: mpsc::Receiver<ResourceCmd>)`, where
`ResourceCmd { add: Vec<K>, add_addrs, remove: Vec<K>, remove_addrs, ack: oneshot::Sender<Result<()>> }`.
The loop drains it with `try_recv` once per pass, before `flows.service`. Never await on it in the loop.
`mpsc` + ack (not `watch`), because the daemon must **sequence** kernel steps around loop steps (§C).
The sender lives in `TunHandle` next to `transport_tx`. `FlowTable` gains the add/remove/close
methods, so they are unit-testable with the existing Fix 05-A `Lab` harness (`net_stack.rs:1749+`).

**New `TunManager` API (tun.rs).** `apply_flow_delta(add: &[AllowedFlow], remove: &[AllowedFlow])`:

- **nft**: one `nft -f -` transaction that flushes and refills `chain inet zecurity_client output` with
  the new rule set. nft applies a single batch atomically, so there is no window with no rules.
  Alternatively, per-rule delete by handle (needs handles captured at add). The atomic batch is simpler
  and doesn't need handle tracking [open, Q5].
- **routes**: `ip route replace X/32 dev zecurity0 table 105` for `addr_add`; `ip route del` for `addr_del`.
- **never** call `cleanup_policy_routes()`; the fwmark rule is untouched.
- `TunManager` is reachable only through `tun_slot`. Holding the `tun_slot` lock across the apply
  serialises against `handle_down`, which takes `tun_slot.lock()` (`daemon.rs:778`).

### C. Atomic publication (ordered, every intermediate state fail-closed)

A kernel + smoltcp change can't be one atomic switch. The order is chosen so that **no intermediate
state routes a K that is no longer allowed, and no SYN for a newly allowed K meets a missing listener
while marked**. The same sequence carries the **combined** resource + connector delta.

```
PRE-MUTATION (failure here: keep the working VPN, map and `applied`; retry)
0.  snapshot (handle, acl, transport, device, relay_crl); candidate; delta = ResourceDelta
    {removed, added, addr_del, addr_add, connector-field changes}   (inside the coordinator pass, as 2-A)
1.  build the new transport map for the FULL candidate, off to the side   (no locks held)
    - contains the connector changes for unchanged K, slots for added K, no slots for removed K
1b. capacity pre-check (H1, approved 2026-10-08): free slots = IFACE_MAX_ADDR_COUNT − 1 (100.64.0.1)
    − |current IPs − addr_del|. Removals free capacity first. Additions on an IP that already has an
    address always apply. New IPs are admitted in ascending IPv4 order until the slots are full. The
    rest are UNAPPLIED (pending): they are excluded from this apply's target and from the map built in
    step 1 (build the map for the reduced target), and logged explicitly, never silently dropped.
    Removals and connector changes are never blocked by capacity.
2.  lock tun_slot; re-check Arc::ptr_eq(state.tun_handle, handle) (else TunnelGone)

MUTATION (failure from here on: full restart, §8.F)
    REMOVE side first:
3.  loop cmd: remove listeners for `removed`, abort live K sockets for `removed`
              → ack only after every aborted socket is reset AND reaped (FlowTable/iface.poll
                semantics); 2 s timeout → Err → recovery restart (H4, approved 2026-10-08)
4.  tun: nft batch #1 = desired rules minus `removed` (C5.1–C5.2)
5.  loop cmd: remove smoltcp addrs for addr_del → ack
5b. tun: `ip route del` for addr_del            (approved order H2: nft → addresses → routes)
    CONNECTOR + ADD side:
6.  transport_tx.send(new_map)   — ONE publish carrying both connector changes and the new K slots
7.  loop cmd: add smoltcp addrs for addr_add (already capacity-checked; push error = unexpected failure)
              + listeners for `added` → ack
8.  tun: `ip route replace` for addr_add, then nft batch #2 = final desired rules (nft is ALWAYS last)
COMMIT
9.  state.write(): tun_handle = Arc::new(TunHandle { same abort/channels, applied: target, route_count })
    where target = candidate minus any UNAPPLIED additions (so they remain a visible pending delta)
```

Ordering rationale for the combined delta:

- **Connector changes need no flow handling.** Existing flows hold only their selected stream
  (2-A; `established_flow_survives_map_swap_that_removes_its_connector`, `net_stack.rs:1265`). So the
  connector part is fully expressed by the single map publish at step 6.
- **Removed-K flows are closed at step 3 regardless of connector changes.** Their keys have no slot in
  the new map, so publish order can't resurrect them.
- **Step 6 after the remove side, before the add side.** Removed K have no listener when the map loses
  their slots. Added K have a slot before their listener exists (6 before 7), which avoids a spurious
  fail-closed RST.
- **Step 3 before 4.** Aborting while still marked gives the app a clean RST from smoltcp instead of
  its packets leaking to the normal route mid-connection.
- **Addresses before nft (7 before 8).** This is mandatory (I7.1, §15). nft goes last because it is the
  only step that changes the path of application packets.
- **`applied` is set only at step 9.** It is set to what was actually applied (`target`), not re-read.
  If the snapshot moved during the apply, the next coordinator pass classifies `applied` vs the newer
  snapshot and applies the next delta (see §10). This deliberately differs from 2-A's "config changed
  during build → Structural": a resource apply mutates the kernel, so "discard" is impossible after
  step 3.

Other notes:

- Steps 3–5 and 7 can be one command each. Splitting remove and add keeps the ordering explicit.
- Lock order: never hold `state` (RwLock) while awaiting `tun_slot` or a loop ack. Mirror `handle_down`'s
  order (state, released, then `tun_slot`).

### D. Existing flows

- K ∉ removed: no code touches their sockets, `ActiveRelay`, relay task or QUIC stream. Same reasoning
  as 2-A: the relay task owns only its `AuthenticatedStream` after selection (`net_stack.rs:858-935`).
  The map swap in step 6 doesn't affect them (proven by
  `established_flow_survives_map_swap_that_removes_its_connector`, `net_stack.rs:1265`).
- K ∈ removed: aborted in-loop at step 3 (RST to the app), relay task ends via `quic_to_tcp_tx.closed()`.
  The connector independently cancels if `(spiffe, resource_id)` was revoked.
- K whose RN changed: N/A in slice 1 (Structural).

### E. New flows

- After step 6 every accept reads the new map. After step 8, SYNs to added K are marked and reach the
  new listener.
- Removed K: from step 3 no listener (a smoltcp RST if still marked), from step 4 unmarked (normal
  kernel route, which is the pre-Zecurity behaviour for an unmanaged destination).

### F. Failure behaviour

| Failure point | Behaviour | Consistent with the existing architecture? |
|---|---|---|
| step 1 map build fails | keep the VPN, map and `applied`; return `Err`; retry on the 60 s tick (generalise `transport_apply_pending` to "pending non-Structural delta") | yes: 2-A D2 (`:1090-1093`, `:3431-3444`) |
| `tun_handle` replaced / `tun_slot` None at step 2 | `TunnelGone`: no apply, no restart | yes: 2-A `TunnelGone` |
| capacity pre-check fails (step 1b) | **no restart**. The overflowing additions are not applied and are logged as an unsupported desired state, never silently dropped. Removals, connector changes and fitting additions proceed. `applied` excludes the unapplied additions, so they stay a pending delta retried each tick until the desired state fits (Q8). | yes: pre-mutation, the same as 2-A D2 "keep the working VPN". Not a classification. |
| any loop ack `Err` or channel closed, any `tun` command fails, an unexpected address-push error (steps 3–8) | **full restart** (`down_up`) | yes: Phase 2 umbrella says "if an incremental route change or apply fails, do a full restart (fail-closed)" (`Phase2-Full-Hot-Apply.md`). "Retain previous state, no restart" is **not** safe here, because a partially changed kernel/smoltcp state can't be asserted equal to either `applied` or the candidate. |
| dead task | full restart (unchanged) | yes |

So the requested "retain previous state, don't restart, retry next cycle" applies **only before
mutation** (step 1/2). After mutation starts, the existing architecture's only proven-consistent
recovery is the full rebuild.

---

## 9. Security / Authorization Analysis

| Topic | Current guarantee [src] | Effect of hot-apply |
|---|---|---|
| Revocation of **new** connections | Enforced by the connector at tunnel open: `resolve_resource` + `allowed_spiffe_ids` (`device_tunnel.rs:186-237`), plus the registration recheck. Client-side it is enforced only after the next poll + restart. | Same or better: removal happens after the same poll, with no TUN teardown delay. The connector check is unchanged. |
| Revocation of **existing** connections | The connector cancels `(spiffe, resource_id)` on ACL push (`control_stream.rs:638-641`), so this is already independent of the client and faster (push vs 60 s poll). The client additionally resets everything on restart. | Must keep the client-side reset for removed K (§5.2, step 3), or behaviour is weakened for the port-change case, which the connector does not cancel (§5.3). |
| Answer: *"If a user loses access while a connection is active, does it continue?"* | **No, on the connector side**: the session is cancelled when the ACL push lands at the connector ("session cancelled — authorization revoked mid-session", `device_tunnel.rs:353-362`, `:481-487`, `:555-561`). Client side: reset at the next 60 s poll by the restart. | Unchanged on the connector. On the client, the reset becomes K-scoped instead of global. Hot-apply doesn't change the security model, provided step 3 exists. |
| Adding access | The connector allows only after its ACL contains the pair. The client can't route before its own poll. | Unchanged. The client can't open what the connector would deny. |
| Changing destination (IP) | New `resource_id` → connector cancels old sessions | Unchanged; the client also aborts old K. |
| Changing port | Connector **does not** cancel (same `resource_id`) | The client abort of `(ip, old_port)` is the only mechanism, so it is **required** in the design. |
| Changing remote network | The connector does not cancel; the old-RN connector still holds the entry (workspace-wide ACL) | Keeping Structural (§5.4) preserves today's "flows reset". Hot-applying would let old-network flows survive. Unresolved policy [open, Q2]. |
| Stale client ACL | Up to 60 s + early-resync windows; fail-open on staleness by design (`daemon.rs:3410-3421` keeps the cache on fetch error) | Unchanged. Hot-apply doesn't add staleness: same classification inputs and the same coordinator. |
| Stale applied state after a failed apply | n/a today | Bounded: pre-mutation failure keeps the **old** state (the connector still enforces); post-mutation failure → full restart. |
| Device revocation / re-enroll | `react_to_device_directive` → `handle_down` outside the coordinator (`daemon.rs:2558-2605`) | Must stay as is. `tun_slot` serialises it against an in-progress apply; the `ptr_eq` check prevents publishing into a downed tunnel. |
| Client as an enforcement point | The client is a **router**, not the policy point; the connector is authoritative | Unchanged. |

**Flag [open]:** whether a removed-K flow should be closed by the client immediately, or left to the
connector's cancellation. The code says the client should close it (§5.2, reason 1: an unmarked flow is
broken anyway), and the port-change case requires it. Recommended default: **close**.

---

## 10. Failure and Concurrency Model

- **Serialisation.** The apply runs inside `run_restart_decision`, i.e. inside a
  `TunnelRestartCoordinator` pass, so it can't race another restart or apply [src, `:1162-1280`].
  `handle_down` from IPC Down / a device directive is outside the coordinator; `tun_slot` (held from
  step 2 to step 8) and the `ptr_eq` re-check cover it.
- **Stale snapshot / concurrent update.** ACL and transport are fetched sequentially with independent
  versions, and the stored snapshot is overwritten before the decision (`sync_acl_now_with`
  `:3810-3812`). The design compares only `applied` vs a candidate computed from one consistent
  read (step 0). If the snapshot changes during the apply, `applied := candidate` (what was really
  applied) and the next pass applies the remainder. Any request that arrived mid-pass is already
  re-queued by the coordinator (`request_arriving_mid_pass_is_answered_by_next_pass`, `:1405`).
- **Two planes lagging.** A resource-only ACL bump with an old transport snapshot is fine: the resource
  K set comes only from the ACL. Coords for a new K may come from the transport plane or fall back to
  the ACL per RN (`resolve_entry_coords`). If neither has the RN, the slot is `None` and the K fails
  closed.
- **Rebuild failure.** See §8.F. The retry trigger must cover a pending `ResourceDelta`: today
  `transport_apply_pending` (`:1023-1034`) checks only TransportOnly, and a sync without a version bump
  never re-enters the decision (2-A deviation note).
- **Atomic publication.** The transport map is atomic (`watch::send`). The kernel nft change is atomic
  per batch. smoltcp changes happen between `iface.poll` calls of the single loop thread, so they are
  atomic w.r.t. packet processing. Cross-component, it is ordered, not atomic, with every intermediate
  state fail-closed (§8.C).
- **Dead task.** Still checked first. A closed command channel at any step means the data plane is dead,
  so full restart.
- **Device cert renewal.** Still reaches pools only via restart/rebuild. A `ResourceDelta` apply builds
  a fresh map with the current `state.device`, as 2-A does. Identity changes themselves stay Structural.

---

## 11. Test Plan

### 11.1 Unit (pure classifier and harness, `daemon.rs::restart_decision_tests` + `daemon_tests.rs`)

Use the 2-A harness (`harness_with`, `:1704`). Extend it with a fake resource-apply sink that records
commands, can fail at step N, and counts down/up. **Every case asserts** restart count, build count,
resource-command count and map swap (`Arc::ptr_eq` / `rx.has_changed()`).

| # | Case | Expected |
|---|---|---|
| U1 | no change (version/timestamp bump) | NoChange; 0 restart, 0 build, 0 cmds |
| U2 | TCP/IPv4 resource added | ResourceDelta{added=[K]}; 0 restart; 1 build; map swapped; cmds remove=∅, add=[K]; `applied` updated |
| U3 | resource removed | ResourceDelta{removed=[K]}; 0 restart; abort-K cmd issued before the nft remove (order asserted) |
| U4 | IP change (remove + add) | removed=[old], added=[new], addr_del/addr_add computed |
| U5 | port change, same IP | removed=[(ip,a)], added=[(ip,b)], addr sets empty |
| U6 | RN change for the same K | **Structural**; 0 build; 0 cmds (D3 kept) |
| U7 | this device gains / loses access via `allowed_spiffe_ids` | = U2 / U3 |
| U8 | other device's access changes | NoChange |
| U9 | metadata (name, `resource_id`, `route_type`, `shield_id`) | NoChange |
| U10 | udp / any / hostname / IPv6 entry added or removed only | ResourceDelta with empty sets; 0 restart; 0 kernel cmds; `applied` updated |
| U11 | resource + connector change in one snapshot | single ResourceDelta; **0 restart**; exactly one map publish carrying both changes; remove-side cmds before the publish, add-side cmds after it (order asserted) |
| U12 | identity + resource change | Structural |
| U13 | result has zero TCP K | per Q3 decision (not Structural); asserts 0 `handle_up` |
| U14 | add exceeding the address capacity (with a removal + connector change in the same delta) | **0 restart**; overflowing additions not applied and an error logged; removal + connector change applied; `applied` excludes the unapplied additions; the next tick retries without mutation while it still doesn't fit |
| U15 | map build fails (step 1) | 0 restart; 0 cmds; `applied` unchanged; returns Err; the next-tick pending retry succeeds |
| U16 | loop ack Err / channel closed at step 3, 5 or 7 | full restart exactly once |
| U17 | `tun` command fails at step 4 or 8 | full restart exactly once |
| U18 | `tun_handle` replaced / `tun_slot` None during the apply | TunnelGone; 0 restart; no publish |
| U19 | snapshot changes during the apply | `applied == candidate actually applied`; a second pass applies the remainder |
| U20 | dead task + ResourceDelta | full restart |
| U21 | the shared TCP/IPv4 filter helper gives the same set for `handle_up`, `net_stack` and the classifier | equality |
| — | Rewritten: `restart_decision_entry_added_restarts`, `restart_decision_resource_access_removed_restarts`, `resource_{ip,port,protocol}_change_is_structural`, `resource_and_connector_change_together_is_structural` | **existing tests change meaning**; list them in the phase doc as 2-A did |

`net_stack.rs` (Fix 05-A `Lab` harness, `:1749+`; fake clock, in-memory pipe):

| # | Case |
|---|---|
| N1 | add listener + addr at runtime → app connects to the new K, the relay is spawned |
| N2 | remove K with a live flow → the app sees RST, the socket is removed, the other K flow keeps passing bytes |
| N3 | removed K, new SYN → RST (no listener), no relay spawned |
| N4 | address push beyond capacity returns an error (not silently ignored) |
| N5 | the relay task for an aborted K ends via `quic_to_tcp_tx.closed()` |

`tun.rs`: the nft batch text builder is pure, so unit-test the generated `nft -f` script and the
route add/del lists. Actual kernel effect is live only (needs root).

### 11.2 Integration (root lab VM or live)

| # | Scenario | Pass criteria |
|---|---|---|
| I1 | resource added while the VPN runs, with a hold flow on another resource | 0 `structural … restarting VPN`; new resource curl OK; hold 0 FAIL |
| I2 | resource removed while a flow to it and a hold to another run | the removed flow gets RST within one poll + apply; the hold survives; new connects to the removed K fail |
| I3 | IP change (delete + create) | old K refused, new K OK, hold survives |
| I4 | port change | old-port flow reset by the client; new port OK; hold survives |
| I5 | multiple resource changes in one snapshot | one apply; all deltas present |
| I6 | resource + connector change in the same snapshot | one apply, 0 restarts; hold on an unaffected resource survives; new flows use the new connector order and the new K |
| I7 | ACL change (group removal) together with a resource change | this device's K removed; flows on it reset (client) and cancelled (connector log) |
| I8 | No-DP resource add (udp/hostname) | 0 restarts, nft hash unchanged |
| I9 | removing the last TCP resource | outcome matches the Q3 decision; 0 `handle_up` |
| I10 | resource add beyond smoltcp capacity | 0 restarts; explicit error log; no partial address; other parts of the delta applied |

### 11.3 Live acceptance (reuse the 2-A method, `Phase2A-Live-Acceptance-2026-10-05.md`)

Instruments: `fc_hold.py` long-lived socket, `p1-newconn.sh`, the client journal, connector journals,
`ip -o link show zecurity0` every 2 s, `nft list table inet zecurity_client | sha256sum`,
`ip route show table 105`, and `p1-mon.sh`/`fc-mon.sh` counters (they must also count the new
resource-apply log lines; update them in the same change).

| ID | Check | Measure |
|---|---|---|
| L1 | VPN restart count | `structural configuration change, restarting VPN` = 0 for rows 1–6; = 1 for the control rows |
| L2 | TUN identity | `zecurity0` ifindex unchanged across every hot-applied row |
| L3 | Routing state | table 105 and the nft ruleset equal the expected set after each row (diff vs expected); the fwmark rule present once |
| L4 | Existing-flow survival | a hold on unaffected resource A: 0 FAIL across every row |
| L5 | New connection success | a curl to an added/kept K succeeds within one poll + apply (record seconds after `ACL snapshot synced`) |
| L6 | Removal enforcement | a hold on the removed K → reset within one apply; a new connect fails; connector `session cancelled — authorization revoked` when the `resource_id` was revoked |
| L7 | Addition availability | the added K reachable without a restart |
| L8 | Address migration | delete + create with a new IP: old refused, new OK, hold on A survives |
| L9 | Port change | old-port hold reset by the client (no connector cancel line expected), new port OK |
| L10 | Connector failover (2-A regression) | Rows A/B/C of 2-A re-run: still hot-applied, 0 restarts |
| L11 | Controls | RN change → restart (D3); identity change → restart (D4) |

---

## 12. Separate 2-IP Limit Issue

**Separate issue — investigated independently.**

The only facts recorded here are the ones that constrain the hot-apply design:

- The smoltcp interface address list is a fixed-capacity `heapless::Vec<IpCidr, IFACE_MAX_ADDR_COUNT>`.
  The default is `IFACE_MAX_ADDR_COUNT = 2` (`smoltcp-0.11.0/build.rs:9`), and `client/Cargo.toml:59`
  sets no `iface-max-addr-count-*` feature. `net_stack.rs:713-719` ignores the `push` result
  (`let _ = addrs.push(...)`). **[src]** That's a cardinality limit inside the resource/address
  representation.
- Effect on this design: the apply must validate capacity **before mutation** and treat overflow as an
  apply failure / unsupported desired state. It is never silently dropped, and it is **not** a restart
  boundary (§6.2, §8.C step 1b, §8.F, U14, N4). The eventual capacity solution belongs to the separate
  2-IP investigation; the design does not depend on it.
- Also observed, recorded for the separate investigation only: listeners bind **port only**
  (`new_listen_socket` → `socket.listen(port)`, `net_stack.rs:831`). smoltcp then accepts any local
  address on that port (`smoltcp-0.11.0/src/socket/tcp.rs:1356-1361`), while `FlowTable` takes the
  destination from the map key, not from `socket.local_endpoint()` (`net_stack.rs:557-565`). With two
  resource IPs on the same port this can select the wrong key. Proven from source; **live behaviour
  unknown / requires live validation**. A resource hot-apply that adds listeners inherits this. Fix it
  in the separate issue, not here.

---

## 13. Open Questions

Only what the code can't answer:

| Q | Question | Recommended default |
|---|---|---|
| Q1 | When this device loses a K, close existing flows on the client immediately, or rely on the connector? | **Close on the client** (step 3). Required for the port-change case; an unmarked flow is broken anyway. |
| Q2 | RN move: keep Structural, or hot-apply plus close K flows? Does the product want old-network flows to survive? | **Keep Structural** (D3) in slice 1. |
| Q3 | Removing the last TCP resource: bring the tunnel down (today's user-visible outcome, via `handle_down` with no `handle_up`), or keep an empty tunnel up? Not a restart boundary (§6.2). **Resolved (§17): KEEP VPN/TUN UP WITH ZERO ROUTABLE RESOURCES.** (Earlier default "bring the tunnel down" withdrawn: it can never auto-reconnect, `daemon.rs:1117-1119`.) |
| Q4 | ~~Resource + connector change in one snapshot~~ **Resolved (design correction 2026-10-07):** one combined apply (§8.A, §8.C). Not Structural. | — |
| Q5 | nft update: atomic chain-flush batch (`nft -f`) or per-rule delete by handle? | **Atomic batch**. Atomicity of a single nft transaction on the lab kernel is **Unknown / requires live validation**. |
| Q6 | Controller consistency of a shield resource after an RN move (`AutoMatchShield` isn't re-run in `Update`) | Out of scope for the client; **Unknown / requires live validation**. |
| Q7 | App-visible behaviour when a new nft rule marks a pre-existing non-tunnelled connection | **Unknown / requires live validation** (expected RST, as with a restart today). |
| Q8 | When the desired address set exceeds smoltcp capacity, which additions stay unapplied? | **Resolved (H1, approved 2026-10-08):** admit new-IP additions in ascending IPv4 order until the capacity is full; additions on IPs that already have an address always apply; removals free capacity first; removals and connector changes are never blocked; the rest stay pending (not in `AppliedConfig`) and are retried each tick. Never silently dropped. The capacity fix itself stays with the 2-IP investigation. (The earlier "all new-IP additions as a group" default is withdrawn.) |

---

## 14. Recommendation

**Partially hot-apply.**

Implement a resource hot-apply slice, call it Phase 2-B, that covers:

- **add and remove** of IPv4/TCP keys (which also covers IP change = remove + add, port change,
  protocol change, and this device gaining/losing access through ACL/group/posture);
- **No-DP-only deltas** (udp/any/hostname/IPv6 entries) that need no data-plane change;
- **resource + connector changes in the same snapshot**, as one combined apply.

It should use the ordered, fail-closed publication in §8.C, in-loop close of removed-K flows, and full
restart on any post-mutation failure (failure handling, not classification).

**Keep Structural (mandatory restart boundary, §6.1):**

- RN change of an existing K (D3);
- identity changes (D4).

**Not restart boundaries (§6.2):**

- address-capacity overflow → pre-mutation apply failure / unsupported desired state (Q8);
- a zero-resource result → valid runtime state; the tunnel stays up with nothing captured (Q3, §17).

Why:

1. **Feasible [src].** The listener, address, route and nft state is per key. smoltcp supports runtime
   address and listener changes, and `FlowTable` already re-arms listeners every accept. The kernel
   side needs only a new incremental `TunManager` API. The 2-A pattern (classify → build off to the side
   → re-check → publish → update `applied`) carries over, with an ordered multi-step publish because
   kernel state can't be swapped atomically.
2. **Valuable.** Today every resource add or remove resets every unrelated long-lived flow
   (live-proven, 2-A Row D), and No-DP edits restart for nothing.
3. **Doesn't weaken authorisation.** The connector stays the authoritative enforcement point for new and
   existing sessions. The client keeps its existing behaviour for affected flows (reset), now scoped to
   the affected key.
4. **Cost and risk are real and larger than 2-A.** It needs:
   - a new command channel into `net_stack`;
   - in-loop selective close;
   - a new `tun.rs` delta API (root-only, live-tested);
   - the generalised pending retry;
   - several existing Structural tests rewritten.

   The 2-IP capacity limit and the port-only listener binding are pre-existing hazards that this work
   must not mask. They are tracked separately.

Not recommended: hot-applying RN moves, or treating "can't rebuild after mutation" as "retain old state".

---

## Files inspected

Client:

- `client/src/daemon.rs`: `handle_request`, `handle_up`, `handle_down`, `restart_tunnel_if_running`, `ConfigDelta`, `structural_view`, `classify_applied`, `classify_delta`, `allowed_entries_for`, `hot_apply_transport_map`, `transport_apply_pending`, `run_restart_decision`, `perform_tunnel_restart`, `react_to_device_directive`, `run_acl_sync_scheduler`, `run_transport_recovery`, `sync_and_restart_if_changed`, `sync_acl_now_with`, `resolve_entry_coords`, `effective_config`, `build_transports_by_resource_with_crl`, `build_transport_from_coords`, `restart_decision_tests`
- `client/src/runtime.rs`, `client/src/tun.rs`
- `client/src/net_stack.rs`: `ActiveRelay`, `drive_relay`, `RelaySpawn`, `FlowTable`, `TransportMap`, `transports_for`, `run`, `new_listen_socket`, `relay_tcp_to_quic`, test list
- `client/src/daemon_tests.rs` (test list), `client/Cargo.toml`, `client/Cargo.lock`
- `~/.cargo/registry/.../smoltcp-0.11.0/{build.rs, Cargo.toml, src/iface/interface/mod.rs, src/socket/tcp.rs}`

Controller:

- `internal/policy/{compiler.go, cache.go, notifier.go, store.go}`
- `internal/transport/{compiler.go, store.go}`
- `internal/resource/store.go`
- `internal/client/service.go` (`GetACLSnapshot`)
- `graph/resolvers/resource.resolvers.go`, `graph/resource.graphqls`
- `cmd/server/main.go` (expiry notifier)
- `migrations/007_resources.sql`, `017_resources_drop_soft_delete.sql`
- grep of `NotifyPolicyChange` call sites, `internal/connector/acl_push.go`

Connector:

- `connector/src/device_tunnel.rs` (`handle_stream`)
- `connector/src/policy/mod.rs`
- `connector/src/control_stream.rs` (ACL arm)
- `connector/src/session_registry.rs`

Proto: `proto/client/v1/*.proto` (ACL/Transport messages).

Docs:

- `Fixes/fix 1/Phase2A-Connector-Topology-Hot-Apply.md`, `Phase2-Full-Hot-Apply.md`
- `Fixes/fix 5/Fix05-Client-Dataplane-Reliability.md` (scope lines)
- `Fixes/Fix02-Resource-ACL-Double-Notify.md` (header)

Production code modified: **none**. Commits created: **none**.

---

## 15. Pre-Implementation Verification — Q5/Q6/Q7

Date: 2026-10-07. Verification only. No production code, tests, firewall, routes, tunnel or remote
host state was changed. §1–§14 are left as written. This section adds conditions; it does not change
the architecture.

Constraints in force:

- **No root.** `sudo -n` fails, so the live `inet zecurity_client` table can't be read.
- **No namespace.** Unprivileged user namespaces are blocked: `kernel.apparmor_restrict_unprivileged_userns = 1`, and `unshare -rn` fails.
- **No experiments.** No packet or nft experiment was run on any host.

Known environment (client host `inkyank-01-computer`):

| Item | Value |
|---|---|
| Kernel | `7.0.0-34-generic` |
| nftables | `v1.0.9 (Old Doc Yak #3)` |
| iproute2 | `6.1.0` |
| `zecurity0` | up |
| `ip rule` | `49: from all fwmark 0x5a lookup 105` |
| table 105 | `10.255.255.1` and `192.168.1.75` via `zecurity0` |
| `rp_filter` | `2` (loose) on `all`, `default` and `zecurity0` (read from `/proc/sys`) |

### Q5 — nftables atomicity

**Question.** Is a single nftables batch atomic enough for the Resource Hot-Apply failure model?

**Evidence.**

| # | Evidence | Source | What it shows |
|---|---|---|---|
| E1 | `tun.rs:62-141` `configure_allowed_flows` runs **one `nft`/`ip` process per command** via `run_command` (`tun.rs:236-245`): `nft add table`, `nft add chain`, one `nft add rule` per flow, then `ip rule add`, then `ip route replace` per IP. Cleanup (`tun.rs:195-234`) is likewise one process per command, with errors ignored. | repo [src] | Today's code is **not batched**. Each `nft` invocation is its own transaction, so the current implementation offers **no** stronger evidence of batch atomicity. It is the non-atomic "shell script" pattern. |
| E2 | `shield/src/resources.rs:118-197` `build_protect_ruleset` / `apply_nftables`: "ONE transaction … commits as a swap … `add table`/`add chain` are idempotent … `flush chain` clears the old rules in the same commit while the chain object … stays installed throughout". Uses the `nftables` crate and has unit tests for the command order. | repo [src] | In-repo precedent for the exact shape the client needs (idempotent add table/chain + flush chain + add rules in one transaction). It is a design rationale, not a kernel proof. |
| E3 | `/usr/src/linux-headers-7.0.0-34-generic/include/uapi/linux/netfilter/nfnetlink.h:67-80` defines `NFNL_MSG_BATCH_BEGIN`/`NFNL_MSG_BATCH_END` and `NFNL_BATCH_GENID`. | local headers for the **running** kernel [src] | The running kernel's UAPI has the batch protocol. Headers don't contain the commit/abort logic. |
| E4 | Upstream `net/netfilter/nfnetlink.c` (**v6.12**, fetched for reference) `nfnetlink_rcv_batch` (`:371`). Any per-message error sets `NFNL_BATCH_FAILURE` (`:441-467`, `:550-558`). `ss->commit()` runs only when `status == NFNL_BATCH_DONE` (`:573-574`). Otherwise, or if commit fails, `ss->abort()` runs (`:579`, `:588-593`). | upstream kernel source, **not** the Ubuntu 7.0.0-34 build [doc/src-upstream] | Batch semantics are all-or-nothing: an error anywhere in the batch aborts the whole batch, and nothing is committed. |
| E5 | nftables wiki *Atomic rule replacement*: "`nft -f` … in one atomic operation it swaps the old config for the new one", and "The kernel handles the rule commands in the file in one single transaction, so … the flushing and the load of the new rules happens in one single shot." | upstream doc [doc] | `nft -f <file>` submits the whole file as one batch, and `flush` inside the file is part of the same commit. |
| E6 | `nft(8)` on this host: `-c, --check  Check commands validity without actually applying the changes.` | local man page [doc] | A pre-check exists. It still needs `CAP_NET_ADMIN`, so it couldn't be run here. |

**Finding.** The three layers have different guarantees:

| Layer | Atomic? | Basis |
|---|---|---|
| **nftables transaction** (one `nft -f` input) | **Yes**, per E4/E5: all-or-nothing, and a failing command leaves earlier commands in the same batch **unapplied**. | Proven from upstream source and docs. **Not** proven on the exact 7.0.0-34 build (E3 shows the protocol only). |
| **Route mutation** (`ip route replace`/`del`, `ip rule`) | **No.** Each `ip` invocation is a separate rtnetlink request (E1 pattern). They aren't part of the nft transaction, and table-105 route changes can't join it. | [src] |
| **smoltcp address/listener changes** | Atomic only with respect to packet processing (single loop thread). Separate from both of the above. | [src] |
| **Complete Resource Hot-Apply** | **Not atomic.** It is an ordered sequence of at least three independently committed steps. nft transactionality **doesn't** make the whole apply atomic. | [src] |

So a single nft batch gives the design two things:

1. Firewall-rule replacement has **no gap** (old rules stay in force until the swap).
2. An nft failure leaves the nft ruleset **unchanged**.

It does **not** give a "nothing happened" guarantee for the whole apply. Route and smoltcp steps may
already have run before or after it.

**Result: YES, WITH CONDITIONS.**

**Conditions (the implementation must satisfy them):**

- **C5.1** All nft changes for one apply go in **one** `nft -f` input (or one `nftables`-crate batch, as in E2). Never use multiple `nft` invocations like today's `tun.rs`.
- **C5.2** Build the batch as **desired state**: `add table inet zecurity_client` + `add chain … { type route hook output priority mangle; policy accept; }` + `flush chain inet zecurity_client output` + one `add rule` per allowed `(ip, tcp port)`. No `delete rule handle N`, because a stale handle would fail and abort the whole batch. This mirrors E2.
- **C5.3** The batch never touches the `ip rule` / table 105. Those are separate, non-atomic steps and are ordered explicitly (see the ordering cross-check below).
- **C5.4** The failure model is **unchanged** (§8.F). Every step after the first data-plane mutation, including a failed nft batch, still leads to a full restart, because route and smoltcp steps may already be applied. The model is correct **without** relying on nft atomicity. Atomicity only removes the firewall gap.

**Impact on design.** No architectural change. C5.1–C5.4 make §8.B "one nft batch" precise.

**Confidence.** High that the design is correct regardless (C5.4). Medium that the running kernel
behaves exactly like upstream v6.12 (not verified on 7.0.0-34).

**Live validation requirement.** Not blocking for design approval. Before acceptance, as a controlled
root run on the lab client with the VPN **down** or in a root-created namespace (needs your approval
and root):

1. Submit a batch whose last rule is invalid, and assert the `zecurity_client` ruleset hash is unchanged.
2. Submit a valid flush + refill under a continuous `curl` probe to an unchanged resource, and assert 0 failures.

### Q6 — `remote_network_id` / shield consistency

**Question.** Is "`remote_network_id` change → Structural/restart" (D3) required by the architecture?

**Source evidence.**

| # | Evidence | What it shows |
|---|---|---|
| S1 | `remote_network_id` is used by the client only in `resolve_entry_coords` (`daemon.rs:3924-3942`), to pick that RN's connector list, and is carried in `AppliedEntry` (`runtime.rs:42`). Listeners, smoltcp addresses, nft and routes are keyed only by `(ip, tcp port)` (`daemon.rs:702-714`, `net_stack.rs:700-725`). | On the client, an RN change only changes the transport-map slot for that key. |
| S2 | Controller: `resources.remote_network_id` and `resources.shield_id` are independent FKs (`007_resources.sql:4-5`; `021_connector_resource_routes.sql` makes `shield_id` `ON DELETE SET NULL`). `shields.remote_network_id` is a separate column (`003_shield_schema.sql:6`). **No constraint ties a resource's RN to its shield's RN.** | A resource and its shield can disagree on RN. |
| S3 | `AutoMatchShield` (`internal/resource/store.go:100-118`) matches `lan_ip + tenant + remote_network_id + active`, but it runs **only in `Create`** (`:122-123`). `Update` (`:406-450`) sets `remote_network_id` with **no** shield re-match and **no** status guard. | After an RN move a protected resource keeps its **old-RN shield**. |
| S4 | Compiler: `preferred_connector_id = shields.connector_id` (`policy/store.go:309`, `compiler.go:65`), `route_type = shield` for protected/failed (`compiler.go:395-406`). | After the move, the ACL entry has the new `remote_network_id` but the old shield and an **old-RN preferred connector**. |
| S5 | Client: `ordered_transport_connectors_for_entry` (`daemon.rs:3900-3917`) puts the preferred connector first only if it is **in the entry's RN list**. An old-RN preferred connector is silently dropped, so candidates are the new-RN connectors only. | The client cannot reach the old-RN connector for **new** flows. |
| S6 | Connector: a shield route is served only through the connector the shield's control stream is attached to. Others answer `SHIELD_NOT_ATTACHED` (`connector/src/device_tunnel.rs:371-374`; skill fact from 2-A live Row C). | **New** flows to a moved **shield-routed** resource fail closed: `SHIELD_NOT_ATTACHED` → next candidate → RST. A restart doesn't fix that either. It is a controller consistency gap, not a client lifecycle issue. |
| S7 | Shield: `ResourceInstruction` = `{resource_id, host, protocol, port_from, port_to, action}` (`proto/shield/v1`, `shield/src/resources.rs`). **The shield has no remote-network concept** (`grep remote_network shield/src` → none). It protects by `host`/port and validates `host == own LAN IP` (`validate_host`, `resources.rs:94-102`). | The shield's state doesn't change on an RN move. It keeps protecting the resource for its own connector. |
| S8 | Connector revocation is keyed `(spiffe, resource_id)` (`connector/src/policy/mod.rs:139-152`; `control_stream.rs:638-641`). An RN move keeps `resource_id` and `allowed_spiffe_ids`, and every connector receives the whole workspace ACL (`internal/connector/acl_push.go:123`). | The connector does **not** cancel an existing session on an RN move. The old connector's ACL still authorises it. |

**nika read-only evidence.**

- **No read-only inspection of nika was possible.** The saved Orca handle `T_NIKA` (`~/s20-run/terms.env`)
  no longer has an SSH session to nika: `orca-ide terminal list` shows its title as `~/zecurity`, i.e. a
  local shell.
- **The one probe I sent landed on that local zsh.** It contained only read-only commands (`hostname`,
  `ip`, `ss`, `systemctl is-active`, `journalctl | grep`). zsh rejected the marker line
  (`zsh: =Q6NIKA1== not found`), and nothing was changed.
- **Its scrollback shows an earlier session.** The host was `luffy` at `192.168.1.39`, with `zecurity-shield`
  `active` at that time.
- **No retry.** I did not open a new SSH session; that needs your password.

Substitute read-only evidence: a `BEGIN READ ONLY` SELECT on the local controller DB.

| Resource | Host:port | RN | Shield | Shield RN | Shield status |
|---|---|---|---|---|---|
| `s20f5-nika-web` (protected) | `192.168.1.39:51711` | `palace` | `s20f5-shield-nika` | `palace` | **disconnected** |
| `s20f5-rn2-web` (unprotected) | `192.168.1.75:51712` | `s20f5-rn2` | — | — | — |

The live data is consistent today (resource RN = shield RN, as `AutoMatchShield` set it at create).
The playbook topology (`s20fc-shield-nika`, `.38`, TCP 5173) is stale relative to the DB.

**Finding.**

1. **What `remote_network_id` controls.** Which connector list the client dials (S1) and which RN's connectors are offered. It is not the authorisation key, and the shield doesn't see it (S7).
2. **Does it change the destination?** It changes the *connector* destination for new flows. For shield-routed resources the *shield* stays the old one (S3/S4), so new flows fail closed (S6).
3. **Can the controller become inconsistent?** **Yes**, persistently, not just temporarily: resource RN ≠ shield RN after `UpdateResource` (S2/S3).
4. **Can the client hot-apply it?** **Mechanically, yes**: it is a map-slot change (S1).
5. **What happens to an existing flow after the move?** Under hot-apply without a close, it continues on the old connector (S8). Under restart, the client resets it.
6. **Does the connector cancel or migrate the session?** Neither (S8).
7. **Can a move leave the client using an old connector for an existing flow?** **Yes**, if it isn't closed on the client.
8. **Is hot-apply a security problem?** Authorisation is not weakened (`resource_id` and SPIFFE are unchanged, S8). It **is** a correctness gap: an existing flow keeps reaching `host:port` through the old network, which can be a different machine when IP spaces overlap. Closing that key's flows on the client (the same in-loop close the design already has for removed keys) restores today's semantics.
9. **Is a restart genuinely required?** **No.** "Close K flows + swap map" is restart-equivalent for the client. Restart is the safest first-slice boundary because:
   - D3 is part of the frozen 2-A design and its test pins it;
   - the controller-side shield inconsistency (S3/S6) is unresolved and unverified live;
   - nika couldn't be inspected.

**Result: CAN HOT-APPLY BUT SHOULD REMAIN RESTART FOR PHASE 1.**

**Impact on design.** None for slice 1 (D3 kept). For a later slice, hot-applying an RN move must:

- close existing flows on that key (treat it as remove + add of K for flow purposes);
- be gated on the controller fixing S3, i.e. re-running `AutoMatchShield` or refusing an RN change on a shield-bound resource (a controller change, outside the client).

**Confidence.** High from source (S1–S8). nika live state wasn't inspected.

**Live validation requirement.** None for Phase 1, since the restart is kept. For a future RN
hot-apply, run a controlled move of an **unprotected** resource between `palace` and `s20f5-rn2`:

- an existing flow is reset by the client, and new flows use new-RN connectors;
- a protected (shield) resource move reproduces `SHIELD_NOT_ATTACHED`, which confirms S3/S6.

Both need your approval (they mutate lab DB state through the admin API).

### Q7 — existing connection + new capture rule

**Question.** What happens to an already-open, untunnelled connection when a new capture rule for its
`(ip, port)` is installed?

**Source evidence (proven) [src].**

| # | Evidence | What it shows |
|---|---|---|
| P1 | The rule installed is `ip daddr X tcp dport P meta mark set 0x5a` in chain `type route hook output priority mangle` (`tun.rs:72-106`). **No `ct state`/conntrack match** appears anywhere in the client ruleset. | The rule matches every locally generated packet to X:P, whatever its conntrack state, including packets of an already-established socket. |
| P2 | `ip rule … fwmark 0x5a lookup 105` (`tun.rs:109-121`) + `ip route replace X/32 dev zecurity0 table 105` (`tun.rs:123-137`). | Marked packets resolve to `zecurity0` if table 105 has X, else fall through to the next rule (main). |
| P3 | **Identical rule text is installed by today's full restart** (`handle_up` → `configure_allowed_flows`). | A hot-apply that adds the same rule creates **no new behaviour** for already-open connections compared with today's restart. |
| P4 | smoltcp drops any IPv4 packet whose dst isn't an interface address (`smoltcp-0.11.0/src/iface/interface/ipv4.rs:107-121`, `any_ip` off). | If X isn't yet (or can't be, capacity) a smoltcp address, captured packets are **silently dropped**, and the app stalls until its own timeout. |
| P5 | A non-SYN segment never matches a `Listen` socket (`socket/tcp.rs:1345-1347`). With no matching socket, `process_tcp` replies with `rst_reply` (`iface/interface/mod.rs:1205-1217`). `rst_reply` sets `seq = segment.ack_number` (`tcp.rs:1248-1262`), exactly the receiver's next expected sequence. | A captured mid-stream segment of an existing connection gets an RST from smoltcp that the app's kernel TCP will accept (exact-match sequence). |
| P6 | Even if the listener for X:P exists, it can't adopt the existing connection (P5). `FlowTable` promotes only sockets that left `Listen` (`net_stack.rs:556-563`). No resource lookup, connector or relay is ever reached. | The existing connection is **never tunnelled** and never mis-attributed to a resource. |
| P7 | RST return path: smoltcp emits the RST to `zecurity0` with src = X, dst = app's source address. The kernel receives it on `zecurity0`. `rp_filter = 2` (loose) on `all`/`zecurity0` (read). Normal tunnelled replies take the identical path and work live (2-A acceptance). | The RST is delivered to the app socket. Confirmed the same way as all tunnel return traffic. |

**General kernel/nft behaviour (documented, not verified on 7.0.0-34) [doc].**

- nft wiki: a `route` chain "is used to reroute packets if any relevant IP header field or the packet mark is modified".
- Upstream v6.12 `nft_chain_route.c` `nf_route_table_hook4`: if `skb->mark` changed during the chain, call `ip_route_me_harder()` for **that packet**.

This happens per packet, independent of conntrack and of the socket's cached route. So once the rule
exists, every later outbound segment of the existing connection is re-routed to `zecurity0`.
Inbound packets from the real peer still arrive on the physical NIC (the output hook doesn't affect
them).

**Finding — packet path for an already-open connection after the rule is added:**

1. app sends the next segment;
2. output route chain marks it 0x5a → re-routed via table 105 → `zecurity0`;
3. smoltcp: dst X is an iface address (otherwise P4: drop);
4. no socket accepts a non-SYN segment → RST with exact seq;
5. RST back via `zecurity0` → app socket is reset (`ECONNRESET`).

The real peer gets nothing and ages its half out on its own timeout. An idle connection is reset only
when it next sends. Answers to the detailed questions:

1. The rule applies to established-connection packets (P1).
2. Conntrack doesn't affect it: there is no ct match, and the route chain re-routes per packet.
3. The socket is captured (rerouted).
4. The established state doesn't change capture (P1).
5. Zecurity never determines a resource for it, because no socket accepts it (P5/P6).
6. The listener model handles it safely by RST (P5/P6), with the P4 caveat.
7. There's no *new* race versus today (P3).
8. The ordering stays valid, with one invariant made explicit (below).

**Result: SAFE — EXISTING CONNECTION BECOMES CAPTURED IN A DEFINED WAY.** The connection is reset by a
smoltcp RST on its next outbound segment, provided X is already a smoltcp interface address. It is
never tunnelled or mis-routed to a resource, and the behaviour is identical to today's restart.

**Impact on design.** No change. One invariant is now **mandatory**, not just preferred:

- **I7.1** In the add path, X must be a smoltcp address before the nft rule exists. Otherwise captured packets are silently dropped (stall) instead of reset. §8.C already orders "addrs + listeners (step 7) before nft (step 8)". The capacity pre-check (§8.C step 1b, pre-mutation) must exclude an overflowing addition before any mutation; it is an apply failure for that addition, not a restart (§6.2).

**Confidence.** High for the client/smoltcp side (P1–P7, all in-repo or pinned crate source). Medium
for the kernel re-route step (upstream v6.12 source and nft docs, not the 7.0.0-34 build).

**Live validation requirement.** Recommended before acceptance (not before design approval; P3 means
there's no regression versus today). It needs root and your approval:

- open a long-lived TCP connection to an unmanaged `X:P`;
- add X:P as a resource;
- expect the app to see `ECONNRESET` on its next send, and **no** `new TCP connection` log for it in the client journal;
- run as part of live row L7/L11 (§11.3).

### Cross-check: ordering and failure boundary

**Removal** (§8.C steps 3–5): close affected flows → remove nft rules → remove addresses/listeners
(listeners are removed together with the flow close in step 3). **Remains correct.**

- **Clarification (from P5/P7).** Step 3's acknowledgement should be sent only after the aborted sockets' RSTs have been dispatched (one `iface.poll` after `abort()`). Then step 5's address removal can't suppress them.
- **Why closing before un-marking is right.** It guarantees a clean RST. If the nft rule went first, the app's next segment would leave via the physical route, the real peer would answer for a connection it doesn't know, and the result would depend on the peer.
- **Routes.** `ip route del` for IPs with no remaining K goes after the nft removal and after the smoltcp address removal (approved order H2). With no mark, nothing can reach table 105 for that IP.

**Addition** (§8.C steps 6–8): publish resource map → add smoltcp addresses + listeners → add routes →
add nft rules. **Remains correct**, with I7.1 mandatory.

- Routes before nft is safe either way (P2: a marked packet with no table-105 route falls through to main).
- nft must be **last**, because it is the only step that changes the path of existing and new app packets.

**Failure boundary.** **Unchanged**, with two clarifications:

1. "Mutation begins" = the first command that changes data-plane state: the first loop command, the map publish or the first kernel command, whichever runs first.
2. A failed nft batch leaves nft unchanged (Q5) but doesn't make the apply "pre-mutation", because earlier route/smoltcp/map steps may have run. It is still a full restart (C5.4).

The pre-mutation phase stays: snapshot, classify, full off-side map build and capacity check → on
failure keep the working VPN and retry.

---

## 16. Pre-Implementation Status

**READY WITH CONDITIONS**

The §8 partial hot-apply design stands. Its architecture is unchanged. Conditions for implementation:

1. **Batched nft.** All nft changes go in one `nft -f` (or `nftables`-crate) batch, built as desired state (add table/chain, flush chain, add rules), with no handle-based deletes (C5.1–C5.3).
2. **Atomicity scope.** Atomicity is claimed only for the nft layer. The complete apply stays an ordered, non-atomic sequence with the unchanged failure boundary (C5.4, §15 cross-check).
3. **Address before rule.** In the add path, a resource IP must be a smoltcp address before its nft rule is installed (I7.1). Capacity is validated before mutation (§8.C step 1b); an overflow is an apply failure for the overflowing additions, not a restart (§6.2, Q8).
4. **RST before address removal.** In the remove path, step 3 is acknowledged only after the aborted sockets' RSTs have been dispatched.
5. **D3 stays.** An RN change remains Structural for Phase 1 (Q6). A later RN hot-apply requires a client close of that key's flows **and** a controller fix for the resource/shield RN inconsistency (S3).

**Live validation still outstanding.** Not blocking design approval; all of it needs root and/or your
approval:

- Q5: nft batch abort + no-gap on 7.0.0-34;
- Q7: an existing connection is reset when its key is added;
- Q6 (future RN hot-apply only): a controlled RN move;
- re-establish a read-only nika SSH session when nika state is needed (`T_NIKA` currently points to a local shell).

**Design correction (2026-10-07, after §15).**

- Address-capacity overflow and resource + connector change in the same snapshot were removed from the
  restart boundary.
- The mandatory restart boundary is now only `remote_network_id` change (D3) and device identity change
  (D4) (§6.1).
- §6, §7, §8.A, §8.C, §8.F, §11, §12, §13 (Q3, Q4 resolved, Q8 added) and §14 were updated. The zero-K
  result was also moved out of the boundary, because no code analysis shows it needs reconstruction of
  immutable state (§6.2, Q3).
- Status remains **READY WITH CONDITIONS**, pending approval of this corrected design.

Not implemented. Not accepted. Production code modified: **none**. Tests modified: **none**.
Commits created: **none**.

---

## 17. Q3 — Last Routable Resource Removal

Date: 2026-10-08. Clarification only: the architecture in §8 is unchanged, and Q5/Q6/Q7 and the 2-IP
issue are not reopened. "Routable" means an allowed IPv4-literal TCP (or empty-protocol) key K, the
same filter as `handle_up`/`net_stack` (§8.A).

### Current startup behavior

`handle_up` (`client/src/daemon.rs:559-770`) has exactly **three** emptiness checks. Each one returns
an `IpcResponse { ok: false }` with a user-facing message:

| # | Line | Condition | Message | State already created when it fires |
|---|---|---|---|---|
| 1 | `:605-612` | `acl.entries.is_empty()` (the whole workspace snapshot, before the SPIFFE filter) | "ACL snapshot has no entries — no resources to route" | none (ACL refresh only) |
| 2 | `:633-640` | `allowed_entries.is_empty()` (SPIFFE-filtered for this device) | "no accessible resources for this device — check group membership" | `effective_config` computed (pure) |
| 3 | `:716-723` | `allowed_flows.is_empty()` (TCP + IPv4 only) | "no TCP resources available for this device" | `relay_crl` initialised plus its refresh task (`:642-663`; `spawn_refresh` is once per process via `refresh_started`, `crl.rs:90-92`); transport map built and the `watch` channel created (`:665-685`); **TUN `zecurity0` created** (`TunManager::create`, `:688`). Then `mgr` is dropped on return. `tun.rs` never calls `persist()` (tun 0.6.1), so the non-persistent TUN goes away when its fd closes. No nft, ip rule or route has been installed yet: `configure_allowed_flows` runs only after the check, at `:725`. |

What the checks are about:

- **Not a technical requirement of any component.** `TunManager::configure_allowed_flows` explicitly
  accepts an empty set (`tun.rs:64-66`: flush, then `return Ok(())`).
  - `net_stack::run` works with zero resources: `FlowTable::new` with an empty slice means no listeners;
    `update_ip_addrs` adds only `100.64.0.1`; the loop runs.
  - `build_transports_by_resource_with_crl` returns an empty map.
- **The checks refuse to start a useless VPN.** The messages are diagnostic, telling the user why
  nothing would be routed.
- **They are startup-only.** They live only in `handle_up`, and runtime reaches them only through
  `down_up()` (`perform_tunnel_restart`, `:1141`).

### Runtime behavior

Today (Phase 1/2-A code), when the last routable resource disappears while the VPN is up:

1. **Controller to client.** The controller deletes the resource or rule → `NotifyPolicyChange` → the
   client's 60 s tick (or IPC Sync/Resources) stores the new ACL with `changed = true`.
2. **Classifier.** `run_restart_decision` → `classify_delta` sees the entry gone (§2.3,
   `restart_decision_resource_access_removed_restarts`) → **Structural** → `down_up()`.
3. **`handle_down` succeeds.** The task is aborted, which resets all flows. Cleanup removes the nft
   table, the ip rule and table 105, and deletes the TUN. `tun_handle` and `tun_slot` are now `None`.
4. **`handle_up` fails** at check 1, 2 or 3 → `down_up` returns `Err` → `run_restart_decision` returns
   `Err` → the caller only logs a warning (`"background sync: tunnel restart failed"`, `:3441-3443`).
5. **Result: the VPN is down.**
6. **A later resource addition does NOT bring it back [src].** `perform_tunnel_restart` returns
   `Ok(())` immediately when `tun_slot` is `None` (`:1117-1119`). Only IPC `Up` (`:553`) or the login
   path (`:506`, only if not running) call `handle_up`. After removing the last resource, the user stays
   disconnected until they manually run `up`, even when resources come back.

**Runtime invariant.** There is **no** runtime code that enforces "at least one routable resource must
exist".

- A grep of `is_empty()` across `daemon.rs`, `net_stack.rs` and `tun.rs` finds only the three
  `handle_up` checks.
- `hot_apply_transport_map`, `transport_apply_pending`, `net_stack` and `TunManager` have no such
  check.
- The current "VPN goes down" outcome is an emergent side effect of routing every resource change
  through `down_up()` into the startup checks. It is not a designed runtime invariant.

### Existing-flow behavior

For the flow to the final resource, apply the §8.C remove side unchanged (being the last resource
doesn't matter):

1. **Should the flow be explicitly closed?** Yes, at step 3, as for any removed K (§5.2, Q1 default).
   Once the nft rule is gone the flow's packets bypass `zecurity0` and the flow is broken anyway; a
   clean RST is the defined outcome.
2. **Does the connector already end it?** When the resource is deleted or this device's access is
   revoked, yes. `(spiffe, resource_id)` leaves the allow set, so the connector cancels the session
   (`connector/src/control_stream.rs:638-641`). That doesn't happen for a port-only or protocol change
   (same `resource_id`, §5.3), so the client close is required.
3. **Could the client keep a flow after the resource disappears?** Only if step 3 were skipped. The
   smoltcp socket and `ActiveRelay` would live until the app or relay ends them. Step 3 removes that
   possibility, because the listener and all sockets whose `local_endpoint()` matches K are aborted.
4. **Wait for resets before removing the address?** Yes. The step-3 ack is sent only after the aborted
   sockets' RSTs have been dispatched (one `iface.poll`, §15 cross-check). Removing the smoltcp address
   first would drop them.

**Ordering for the final resource: unchanged and still correct.**

1. Close flows and remove the listener.
2. Run nft batch #1 with the desired rules = **none**: `add table` + `add chain` + `flush chain`, i.e.
   an empty route-hook chain with `policy accept`, so nothing is marked.
3. Remove the smoltcp address. Only `100.64.0.1` remains.
4. Delete the table-105 route for that IP. Table 105 is now empty.

### Zero-resource state

**Zero routable resources is a valid runtime state. [src]**

- No component requires a non-empty set (see above).
- The only place it is rejected is the startup UX checks.
- The desired state "this device may reach no TCP resource" is a legitimate policy outcome, e.g. access
  revoked.

So it must be modelled as a **valid desired state**, distinct from the other cases:

| Case | Meaning | `AppliedConfig` after the apply |
|---|---|---|
| Valid zero state | the candidate has no K (it may still hold No-DP entries) | `entries` = the candidate's entries (zero K). This is a successful apply. |
| Invalid / unclassifiable input | ACL or device missing, identity changed, RN change | unchanged. Handled by the existing Structural / fail-closed rules (§6.1, §6.2), not by this question. |
| Pending (unapplied) additions | capacity overflow (Q8) | `applied` = candidate minus the unapplied additions. They stay a visible pending delta. |
| Failed application | pre-mutation failure → `applied` unchanged, retry; post-mutation failure → error-recovery restart (§8.F) | per §8.F |

Zero K must **never** be reported as an apply failure or trigger the fallback. `route_count` becomes 0.
`TunHandle` keeps the same `abort`, `transport_tx` and command channel.

### Future resource addition

Sequence: A is the only K → remove A → zero K → add B. This **can be hot-applied without a restart**,
because everything the add path needs still exists in the zero state:

| Needed for the add | Present in the zero state? | Evidence |
|---|---|---|
| TUN `zecurity0`, `net_stack` task, smoltcp `iface`, `FlowTable` | yes (never torn down by a removal) | §8.C touches only listeners and addresses |
| Command channel + `transport_tx` | yes (held by `TunHandle`) | §8.B |
| smoltcp address slot for B | yes: 1 (`100.64.0.1`) + 1 ≤ 2 | §12 (capacity is Q8's concern, not a blocker here) |
| nft table/chain | yes (empty chain kept). Even if absent, batch #2 is idempotent `add table` + `add chain` (C5.2). | §15 Q5 |
| `ip rule fwmark 0x5a lookup 105` | **only if the incremental API leaves it in place.** Today it is installed only by `configure_allowed_flows` (`tun.rs:109-121`) and removed only by `cleanup_policy_routes` (`:195-234`). | implication I-Q3.2 |
| table-105 route for B | added at step 8 | §8.C |
| transport map slot for B | the step-6 publish | §8.C |

**No architectural blocker.** The add path from zero is the ordinary §8.C add path with
`removed = ∅`, `addr_add = {B.ip}`.

### Security behavior

The target property: **no resource means no resource traffic is captured or routed through a stale
resource configuration.** After the §8.C remove side completes for the final K:

| Question | Answer | Why |
|---|---|---|
| Can stale nft rules remain? | **No** | Desired-state batch #1 flushes the chain and re-adds zero rules, all-or-nothing (Q5). A failed batch is a post-mutation failure → error-recovery restart → `handle_down` deletes the whole table. |
| Can stale routes remain? | **No**, provided `ip route del` for every `addr_del` succeeds | A failure → error-recovery restart (§8.F) → `cleanup_policy_routes` flushes table 105. |
| Can stale smoltcp addresses remain? | **No** | Step 5 removes them; a failed ack → error recovery. |
| Can a removed resource still receive traffic through the tunnel? | **No** | Nothing is marked (empty chain). Even if a packet reached `zecurity0`, smoltcp drops it (dst isn't an interface address, `ipv4.rs:107-121`), or RSTs it if the address still existed (no listener, `mod.rs:1205-1217`). The map has no slot either, so it fails closed at accept. |
| Can traffic "bypass the VPN"? | Only in the defined split-tunnel sense: traffic to destinations that aren't resources leaves via the normal route | Same as for any unmanaged destination today (`daemon.rs:4042-4045`, "None (absent) — unmanaged traffic … no tunnel route"). Zecurity never captures non-resource traffic, so zero resources = zero capture. That is the same for Model 1 and Model 2. Reachability of the resource host outside the tunnel is the shield's/network's concern. |
| Is fail-closed preserved? | **Yes** for every *managed* destination. There are none in this state; a stray packet into `zecurity0` is dropped or RST. | as above |
| Can a later addition safely recreate the capture path? | **Yes** | The §8.C add order (map → address + listener → route → nft last, I7.1) is unchanged. |
| The remaining `ip rule fwmark 0x5a lookup 105` | inert | Nothing sets mark `0x5a` (empty chain), and table 105 is empty. A marked packet with no route falls through to `main` (§15 P2). |

The removal sequence guarantees the target property **on success**. On any post-mutation failure the
existing error-recovery restart applies.

**Edge (implication I-Q3.3).** With zero desired K, `handle_up` refuses to start. So an error-recovery
restart (failed final removal, dead task) ends with the VPN **down**, which is today's outcome.
That is still fail-closed (nothing captured). But the VPN then won't come back on a later addition,
because of the `tun_slot = None` early return.

### Recommendation

**Q3 result: KEEP VPN/TUN UP WITH ZERO ROUTABLE RESOURCES** (Model 1).

Why Model 1 and not Model 2:

- The architecture permits it: no component needs a non-empty set, and the zero state is exactly what
  the remove side already produces.
- It is fail-closed: nothing is captured and nothing stale remains (table above).
- It is the only model in which a later resource addition is restored automatically.
- **Model 2 (take the VPN down) has a proven defect.** `perform_tunnel_restart` no-ops when `tun_slot`
  is `None` (`:1117-1119`), so a resource added later is never reconnected without a manual `up`. To
  make Model 2 work, a new auto-up path would have to be invented. Its only effect would be to make a
  revoke → re-grant cycle require user action.
- The user explicitly ran `up` (or was auto-connected at login). A policy change that temporarily
  leaves nothing to route is not a request to disconnect.
- **Model 3 (dormant state) is Model 1 described precisely.** It is not a distinct option. What stays:
  - TUN `zecurity0` + `100.64.0.1/32`;
  - the `net_stack` task (empty `FlowTable`);
  - `TunHandle` (abort handle, `transport_tx`, the resource command channel, `applied` with zero K);
  - an empty `inet zecurity_client` table/chain;
  - `ip rule fwmark 0x5a lookup 105` (inert);
  - an empty table 105;
  - the transport map (empty, or only `None` slots);
  - `relay_crl`.

  What is removed: every resource listener, resource smoltcp address, table-105 route, nft mark rule
  and map slot, plus every flow to those resources.

**Startup behaviour is unchanged.** `handle_up` keeps refusing to *start* with zero resources (UX
checks; not touched by this design). The asymmetry ("won't start empty, may run empty") is deliberate
and harmless.

### Implementation implications

Implications only. Nothing is implemented.

- **I-Q3.1 Classifier.** A candidate with zero K is an ordinary `ResourceDelta` (`removed = {last K}`).
  It is never Structural and never an apply failure. U13 and I9 (§11) assert 0 `handle_up`, 0
  restarts, the same ifindex, an empty table 105 and an nft chain with zero rules.
- **I-Q3.2 `tun.rs` delta API.** It must **not** remove the `fwmark 0x5a` ip rule when the K set becomes
  empty, and must ensure exactly one such rule exists before the first route/nft add. A plain `ip rule
  add` duplicates rules, so check first. The desired-state nft batch with zero rules keeps the table
  and chain, and only the full `cleanup_policy_routes` (`handle_down`) deletes them.
- **I-Q3.3 Error recovery at zero K.** Because `handle_up` refuses an empty set, an error-recovery
  restart at zero desired K leaves the VPN down, and later additions don't reconnect it (the
  `tun_slot = None` early return). That is fail-closed and the same as today. Whether error recovery
  should instead bring up an empty tunnel (a recovery-only variant of `handle_up` without the three
  UX checks) is a **design decision**, and it is outside this Q3 clarification. Recommended default:
  leave it as is for Phase 1 and record it as a follow-up.
- **I-Q3.4 Observability.** Log the transition: e.g. `no routable resources; tunnel kept up, nothing
  captured`, and the reverse on the first addition. Add these lines to the live-lab monitor scripts in
  the same change.
- **I-Q3.5 Live acceptance (later; needs approval).** Run hold on A → remove A (last) → assert:
  - the A flow is reset;
  - 0 restarts;
  - the same `zecurity0` ifindex;
  - table 105 is empty;
  - the nft chain has 0 rules;
  - `ip rule` has exactly one fwmark rule.

  Then add B → B is reachable with 0 restarts and the same ifindex.

### Q3 status

**RESOLVED (design): KEEP VPN/TUN UP WITH ZERO ROUTABLE RESOURCES.** Removing the final routable TCP
resource is an ordinary hot-apply removal. The client enters the Model 1 zero state described above.

Open follow-up (not blocking): I-Q3.3, error-recovery behaviour at zero desired K, as a design
decision. Live confirmation per I-Q3.5 at acceptance.

Overall status (§16): **READY WITH CONDITIONS**, unchanged. Not implemented. Not accepted.
Production code modified: **none**. Tests modified: **none**. Commits created: **none**.

---

## 18. Approved Implementation Decisions (2026-10-08)

The implementation plan was approved with the decisions below. The full plan is in
[[Phase5C-Resource-Hot-Apply]] (`Fixes/fix 5/Phase5C-Resource-Hot-Apply.md`).

| ID | Decision |
|---|---|
| H1 | Capacity selection: removals free capacity first. Additions on IPs that already have a smoltcp address always apply. New IPs are admitted in **ascending IPv4 order** until capacity is full. The remainder stays pending: it is not in `AppliedConfig`, it is retried each tick, and it is logged, never silently dropped. Removals and connector changes are never blocked. Capacity overflow is not Structural and not a restart. The capacity fix stays with the 2-IP investigation. (§8.C step 1b and Q8 updated.) |
| H2 | Removal order: close flows + remove listeners → ack after reset/reap → nft desired-state batch → remove smoltcp addresses → delete routes. The final-resource case is identical. (§8.C and §15 cross-check aligned.) |
| H3 | Error-recovery restart while zero resources are desired: `handle_up` may still refuse the empty startup state, so the VPN stays down (fail-closed). Not solved in Phase 1. **Follow-up FU-1** (= I-Q3.3). |
| H4 | R1 ack timeout = **2 s**. The ack succeeds only once every affected flow is reset and reaped (FlowTable/iface.poll semantics). On timeout it returns `Err`, which goes into the full-restart recovery path, because mutation has begun. No partial apply is left. |

Status: **APPROVED FOR IMPLEMENTATION — awaiting explicit authorization to start.** Not implemented.
Production code modified: **none**. Tests modified: **none**. Commits created: **none**.
