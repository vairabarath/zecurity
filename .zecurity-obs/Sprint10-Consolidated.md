---
type: planning
status: in-progress
sprint: 10
tags:
  - sprint10
  - relay
  - pki
  - acl
  - split-tunneling
  - consolidated
date: 2026-06-23
---

# Sprint 10 — Relay: Consolidated Reference

> This file merges Sprint10, Sprint10.1, Sprint10.2, and Sprint10.3 into a
> single reference. It documents what was planned, what was actually
> implemented in the codebase, and where the remaining work sits.

---

## Sprint Goal

**Relay** — a transparent QUIC-based proxy for clients that cannot reach a
Connector directly (NAT, firewall, different network). When direct connection
to `:9092` fails, the client falls back to the Relay.

The Relay is a **dumb pipe** — it validates SPIFFE identities on both sides and
bridges two QUIC streams together. It cannot decrypt traffic: end-to-end mTLS
is maintained between Client and Connector.

```
Same LAN (Sprint 9 path, unchanged):
  Client → Connector :9092   (direct QUIC)

Different networks (Sprint 10 adds):
  Client → Relay :9093 → Connector
  End-to-end mTLS — Relay sees only ciphertext
```

---

## What Was Actually Implemented

### 1. ACL Snapshot — Remote Network Scoped Routing

**Planned (Sprint 10 / Sprint 10.2 M2):** Add relay fields to `ACLSnapshot`.

**Implemented (on `relay-preparation` branch, committed):**

The original plan was to add flat `relay_addr`, `connector_id`,
`connector_spiffe` fields at the snapshot level. This was superseded by a
richer routing model:

```
Resource → remote_network_id → ACLRemoteNetwork → connectors[]
```

**Proto changes (`proto/client/v1/client.proto`):**

- Added `ACLConnector` message: `connector_id`, `connector_tunnel_addr`,
  `connector_spiffe`
- Added `ACLRemoteNetwork` message: `remote_network_id`, `name`,
  `repeated ACLConnector connectors`
- Added `remote_network_id = 9` to `ACLEntry`
- Added `repeated ACLRemoteNetwork remote_networks = 5` to `ACLSnapshot`
- Added `relay_addr = 6`, `relay_spiffe_id = 7` to `ACLSnapshot`
- Reserved old snapshot-level connector fields

**Controller changes:**

| File | Change |
|------|--------|
| `controller/internal/policy/store.go` | `CompilerResourceRow` gains `RemoteNetworkID`, `RemoteNetworkName`, `Status`; SQL joins `remote_networks`; new `GetConnectorsForRemoteNetworks()` batch query |
| `controller/internal/policy/compiler.go` | `CompileACLSnapshot` builds `ACLRemoteNetwork` entries seeded from rules (every referenced RN always present, even with no active connector); populates `connectors[]` from batch query; relay discovery via `store.GetActiveRelay()` |
| `controller/internal/policy/compiler_relay_integration_test.go` | Multi-RN routing tests, no-connector RN partial availability test, relay presence test |
| `controller/internal/connector/control_stream.go` | `pushACLSnapshot` uses `h.PolicyCache.GetOrCompile(...)` with epoch CAS |
| `controller/internal/client/service.go` | `GetACLSnapshot` uses `s.policyCache.GetOrCompile(...)` with epoch CAS |

**Key invariant:** A Remote Network with no active connector appears in
`remote_networks` with `connectors = []`. Client treats its resources as
temporarily unavailable (fail closed, not bypass).

---

### 2. Relay Certificate Provisioning

**Planned (Sprint 10.1 M2 Phase 2):** Relay host generates its own key,
submits CSR to Controller, Controller signs with Platform Intermediate CA.

**Implemented:**

- `proto/relay/v1/relay.proto`: `Provision(ProvisionRequest)` and
  `Heartbeat(HeartbeatRequest)` RPCs defined and generated
- `controller/internal/relay/provision.go`: `Provision` handler validates
  Relay ID (canonical lowercase UUID), DER CSR (self-signature, SPIFFE URI,
  SAN allowlist, EKU, key algorithm), then calls `pki.SignRelayCert`
- `controller/internal/pki/`: `SignRelayCert` issues leaf signed by Platform
  Intermediate CA with `ServerAuth + ClientAuth` EKU and exact
  `spiffe://<global-trust-domain>/relay/<relay-id>` SAN
- `relay/src/provision.rs`: Relay fetches `/ca.crt`, verifies
  `RELAY_CA_FINGERPRINT`, generates `relay.key` (EC P-384), builds CSR, calls
  `Provision`, validates returned leaf SPIFFE URI and Intermediate CA
  fingerprint, stores `relay.key`, `relay.crt`, `intermediate-ca.crt`

**Current limitation:** `provisioning_token` is reserved in the proto but not
yet validated. Any caller that can reach the gRPC endpoint can provision a
Relay certificate (Sprint 10.3 M2 Phase 1 closes this).

---

### 3. Relay Heartbeat

**Planned (Sprint 10.3 M2 Phase 1):** mTLS heartbeat from Relay to Controller
persisting health and observed address for ACL relay discovery.

**Implemented:**

- `relay.v1.RelayService.Heartbeat` defined and registered
- `relay/src/heartbeat.rs`: periodic task using `relay.crt`/`relay.key` as
  mTLS client identity, trusts Platform Intermediate CA
- `controller/internal/relay/heartbeat.go`: verifies Relay cert against
  Intermediate CA, derives Relay ID from authenticated SPIFFE identity (never
  from request field), records `version`, `hostname`, `last_heartbeat_at`,
  `cert_serial`, `cert_expires_at`
- Valkey liveness keys written on every heartbeat
- Postgres writes throttled by `RELAY_HEARTBEAT_DB_WRITE_INTERVAL`
- `controller/migrations/020_relay_update_table.sql`: adds `public_addr`,
  `observed_ip`, `observed_port`, `address_scope` columns

**Address classification logic:**

| Observed IP | `address_scope` | `public_addr` set? |
|---|---|---|
| Public/global | `public` | Yes — `<observed_ip>:9093` |
| Private RFC1918 | `private` | No |
| Loopback | `loopback` | No |
| Link-local | `link_local` | No |

**`GetActiveRelay()` in compiler:** Reads from the `relays` table; if
`public_addr` is set it uses that directly; otherwise falls back to
`observed_ip:9093` when `address_scope = public`. This is how `relay_addr`
gets into the ACL snapshot.

---

### 4. Client SPIFFE-Filtered Split Tunneling

**Planned (plan file / ADR-009):** Filter ACL entries by device SPIFFE ID;
implement true port-based split tunneling via nftables.

**Implemented (committed to `relay-preparation`, also on
`merge-relay-preparation-into-main`):**

**SPIFFE filtering (`client/src/daemon.rs`):**

```rust
let allowed_entries: Vec<AclEntry> = acl.entries.iter()
    .filter(|e| e.allowed_spiffe_ids.iter().any(|id| id == my_spiffe.as_str()))
    .cloned()
    .collect();
```

`allowed_entries` flows to:
1. `configure_allowed_flows()` — kernel policy
2. `build_transports_by_resource()` — transport map
3. `net_stack::run()` — smoltcp listeners

**Three-way transport map (`daemon.rs`):**

| Value | Meaning | Client action |
|---|---|---|
| `Some(Some(transport))` | Connector online | Tunnel via QUIC |
| `Some(None)` | Connector offline | Fail closed (RST) |
| absent | Should not occur (smoltcp only listens on allowed ports) | Fail closed |

**Port-based split tunneling (`client/src/tun.rs`):**

Replaced `/32` main-table routes with:

1. **nftables OUTPUT chain** — marks only `(daddr, dport)` pairs from
   `allowed_entries` with `SO_MARK 0x5a`
2. **Route table 105** — `/32` host routes for resource IPs pointing at
   `zecurity0`; reachable only by marked packets via `ip rule priority 49`

```
nft add table inet zecurity_client
nft add chain inet zecurity_client output { type route hook output priority mangle; }
# per allowed (ip, port):
nft add rule inet zecurity_client output ip daddr <IP> tcp dport <PORT> meta mark set 0x5a
ip rule add fwmark 0x5a lookup 105 priority 49
ip route replace <IP>/32 dev zecurity0 table 105
```

Non-ACL ports on a resource IP are never marked → use kernel main table →
bypass TUN entirely. See ADR-009.

---

## Remaining Work

### Sprint 10.3 M2 Phase 1 — Authenticated Provisioning (not yet done)

- Wire `provisioning_token` verification into `Provision` handler
- Token must be: signed, non-expired, correct purpose, bound to exact Relay ID
- JTI must be atomically burned (reject replays)
- Map auth failures to correct gRPC status codes without leaking internals

### Sprint 10.3 Shared Phase 1 — Integration & Security Gates (not yet done)

- Full integration tests: valid token issues once, replay fails, concurrent
  replay only one succeeds
- Heartbeat rejection tests: wrong-role cert, mismatched Relay ID
- Load tests: connection/stream limits, incomplete handshake timeouts
- Security regression gates: cross-workspace Lookup denied, inner mTLS intact

### PKI Chain Audit (Sprint 10.1 M2 Phase 1 — not verified)

- Tests for `Root MaxPathLen=2`, `Intermediate MaxPathLen=1`,
  `Workspace MaxPathLen=0`
- Cross-workspace and unknown CA chain failure tests
- Startup fatal error if stored CA constraints make full-chain validation
  impossible

---

## SPIFFE Identity Map (Relay-related)

| Role | SPIFFE URI |
|---|---|
| Relay | `spiffe://<global-trust-domain>/relay/<relay-id>` |
| Connector | `spiffe://<workspace-trust-domain>/connector/<connector-id>` |
| Client device | `spiffe://<workspace-trust-domain>/client/<device-id>` |
| Shield | `spiffe://<workspace-trust-domain>/shield/<shield-id>` |

---

## Port Reference

| Port | Component | Protocol |
|---|---|---|
| `:9091` | Shield → Connector control stream | gRPC/TLS |
| `:9092` | Client → Connector tunnel (direct) | QUIC |
| `:9093` | Client → Relay (fallback) | QUIC, ALPN `ztna-relay-v1` |
| `:9094` | Connector → Relay (register) | QUIC, ALPN `ztna-relay-v1` |

---

## Key Files Changed (this sprint, across all sub-sprints)

| File | What changed |
|---|---|
| `proto/client/v1/client.proto` | `ACLConnector`, `ACLRemoteNetwork`, `remote_network_id` on `ACLEntry`, `remote_networks` + `relay_addr` + `relay_spiffe_id` on `ACLSnapshot` |
| `proto/relay/v1/relay.proto` | `Provision`, `Heartbeat` RPCs |
| `controller/internal/policy/store.go` | `CompilerResourceRow` + `GetConnectorsForRemoteNetworks` |
| `controller/internal/policy/compiler.go` | RN-scoped ACL compilation, `GetActiveRelay` for relay discovery |
| `controller/internal/relay/heartbeat.go` | mTLS Relay heartbeat handler, address classification, Valkey + Postgres writes |
| `controller/internal/relay/provision.go` | Relay CSR validation and signing |
| `controller/internal/relay/store.go` | `RecordHeartbeat` with address metadata |
| `controller/migrations/020_relay_update_table.sql` | `public_addr`, `observed_ip`, `observed_port`, `address_scope` |
| `client/src/tun.rs` | nftables port-based split tunneling, route table 105 |
| `client/src/daemon.rs` | SPIFFE-filtered `allowed_entries`, `AllowedFlow`, three-way transport map |
| `client/src/net_stack.rs` | `run()` takes `Vec<AclEntry>` instead of `Arc<AclSnapshot>` |
| `relay/src/provision.rs` | Key generation, CSR, `Provision` RPC client, cert storage |
| `relay/src/heartbeat.rs` | Periodic mTLS heartbeat task |

---

## ADRs Written This Sprint

| ADR | Decision |
|---|---|
| ADR-009 | Client port-based split tunneling via nftables + route table 105 |
