---
type: planning
status: active
tags:
  - roadmap
  - planning
---

# Roadmap

---

## Current State (2026-04-24)

Five sprints complete. Sprint 5 resource protection is done — Admin defines resources, Shield applies nftables rules, lifecycle tracked through `pending → managing → protecting → protected`. Sprint 6 is now active — discovery: Shield scans its own host for listening services and reports via Control stream; Connector executes admin-triggered TCP network scans.

---

## Sprint Status

### ✅ Sprint 1 — Foundation (Complete)
- Google OAuth + JWT auth
- Workspace management (create, lookup)
- Admin UI (React + Apollo)
- PostgreSQL schema (users, workspaces)

### ✅ Sprint 2 — Connector (Complete)
- 3-tier PKI (Root → Intermediate → Workspace CA)
- Connector enrollment (JWT + CSR + SPIFFE cert)
- mTLS heartbeat loop (SPIFFE interceptor)
- Disconnect watcher
- Binary auto-update (GitHub releases)
- One-line install script + systemd units
- Admin UI: connector provisioning + enrollment tokens

### ✅ Sprint 3 — Cert Renewal (Complete)
- `RenewCert` gRPC RPC (mTLS, proof-of-possession CSR)
- Controller heartbeat signals `re_enroll=true` within renewal window
- Connector auto-renews cert, saves new bundle, rebuilds mTLS channel
- Zero downtime, zero admin action
- `CONNECTOR_RENEWAL_WINDOW` env var (default 48h)

### ✅ Sprint 4 — Shield Deployment (Complete)

**Goal:** Deploy a Shield on any resource host, see it appear ACTIVE in the dashboard, watch it go DISCONNECTED if the server goes offline, and have its `zecurity0` interface + base nftables table set up automatically on enrollment.

**Key decisions locked:**
- Shield binary: `zecurity-shield`, service: `zecurity-shield.service`
- Shield SPIFFE: `spiffe://ws-<slug>.zecurity.in/shield/<id>`
- Shield cert TTL: 7 days, renewal window: 48h
- Shield heartbeats to Connector `:9091` (NOT Controller directly)
- Interface: `zecurity0` (TUN, CGNAT 100.64.0.0/10)
- lan_addr (connector) + lan_ip (shield) auto-detected via if_addrs crate

**Tracking:** [[Sprint4/path.md]]

---

### ✅ Sprint 5 — Resource Protection (Complete)

**Goal:** Admin defines a resource (IP + port) on a Shield host → Shield applies nftables rules to make the service invisible on LAN but accessible via `zecurity0` → status tracked through `pending → managing → protecting → protected` lifecycle via Control stream piggyback.

**Key decisions locked:**
- Shield auto-matched by `lan_ip == resource.host` — admin never picks shield manually
- Shield validates host IP before applying nftables (defense in depth)
- Resource check interval: 30s (separate from 60s heartbeat)
- nftables `chain resource_protect` flushed + rebuilt atomically on every change
- No new RPCs — instructions ride on existing Control stream

**Tracking:** [[Sprint5/path.md]] — all phases complete

---

### 🚧 Sprint 6 — Discovery (Active)

**Goal:** Two discovery features: **(1) Shield Discovery** — Shield scans its own host's listening TCP ports via `/proc/net/tcp`, sends differential `DiscoveryReport` messages up the Control stream → Connector relays to Controller → Admin sees discovered services per Shield host and can promote any to a resource in one click. **(2) Connector Network Discovery** — Admin defines a scan scope (CIDR/IP list + ports) → Controller sends `ScanCommand` to Connector → Connector TCP-pings targets → Admin sees live services and can create resources from results.

**Team split:**
- **M1 (Frontend):** Discovered services tab on Shields page, Scan modal on Remote Networks page
- **M2 (Go — Proto + DB + Schema):** `DiscoveryReport` / `ScanCommand` / `ScanReport` proto messages, migration 008, `graph/discovery.graphqls`
- **M3 (Go+Rust — Controller + Connector):** discovery store + resolvers, control stream handlers, connector `discovery/` scan module
- **M4 (Rust — Shield):** `shield/src/discovery.rs`, `/proc/net/tcp` parser, Control stream wiring

**Key decisions locked:**
- Discovery rides existing Control streams — no new RPCs
- `ShieldControlMessage` field 7 = `DiscoveryReport`; `ConnectorControlMessage` fields 8/9/10 = `ShieldDiscoveryBatch` / `ScanReport` / `ScanCommand`
- Shield scans only its own host (reads `/proc/net/tcp`) — no network scanning from Shield side
- Connector scanner: max 512 targets, 16 ports, 32 concurrent probes, 5s per-target timeout
- Scan results TTL: 24h (purged by background goroutine in controller)
- M4 can start `discovery.rs` scaffold immediately on Day 1 — no proto dependency for the core logic

**Tracking:** [[Sprint6/path.md]] — full dependency map with checkboxes

---

## Phase 6 — End-to-End Renewal Test (Pending — run before Sprint 4 merges)

Testing cert renewal with short TTLs:

```env
CONNECTOR_CERT_TTL=3m
CONNECTOR_RENEWAL_WINDOW=2m
CONNECTOR_HEARTBEAT_INTERVAL=5s
```

**Expected timeline:**
```
0:00  Enroll → 3-minute cert issued
1:00  Heartbeat → 2 min left → re_enroll=true
1:00  Connector calls RenewCert → new 3-minute cert
1:00  mTLS channel rebuilt with new cert
3:00  Old cert expires → doesn't matter, already renewed
```

**Binary:** `connector-v0.2.0` (GitHub Actions build via `connector-v*` tag)

---

## Sprint 6 — Connector Fallback (Deferred)

After resource protection is stable:
- Shield can failover to a secondary Connector if primary goes offline
- Join table for M:N shield-connector assignments
- Admin assigns fallback connector per shield

---

## Future Sprints (Rough Order)

| Sprint | Feature |
|--------|---------|
| Sprint 7 | Client enrollment (end-user device cert + SPIFFE identity) |
| Sprint 8 | Access policies (workspace ACLs: who can reach what resource) |
| Sprint 9 | Policy enforcement at Shield (nftables rules from ACL) |
| Sprint 10 | Traffic proxying (WireGuard or tun, full packet path) |

---

## Deferred Features

| Feature | Reason Deferred |
|---------|----------------|
| CRL / OCSP revocation | DB status flag is sufficient for now |
| Shield cert renewal | Sprint 4 — same pattern as Connector cert renewal |
| Client cert renewal | Same pattern, future sprint |
| Renewal failure alerting | Agent retries on next heartbeat |
| Admin notification on renewal | No UI change needed, fully automatic |
| WireGuard integration | Sprint 9 — after ACL enforcement is solid |
| Shield connector failover | After discovery is stable |
