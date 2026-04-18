---
type: planning
status: active
tags:
  - roadmap
  - planning
---

# Roadmap

---

## Current State (2026-04-16)

Three sprints complete. The connector is fully operational with automatic cert renewal. Sprint 4 is now planned and underway — deploying the Shield agent on resource hosts with SPIFFE identity, zecurity0 interface, and nftables-based network protection.

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

### 🚧 Sprint 4 — Shield Deployment (In Progress)

**Goal:** Deploy a Shield on any resource host, see it appear ACTIVE in the dashboard, watch it go DISCONNECTED if the server goes offline, and have its `zecurity0` interface + base nftables table set up automatically on enrollment.

**Team split:**
- **M1 (Frontend):** Shields page, NetworkHealth indicator, Sidebar nav, GraphQL operations
- **M2 (Go — Proto + Shield + PKI):** shield.proto, appmeta constants, `internal/shield/` package, `SignShieldCert`, main.go wiring
- **M3 (Go — DB + GraphQL + Connector):** DB migration, GraphQL schema + resolvers, Connector Goodbye RPC, ShieldHealth processing, `agent_server.rs`
- **M4 (Rust — Shield + CI):** `shield/` crate, enrollment, heartbeat, `network.rs` (zecurity0 + nftables), systemd, CI

**Key decisions locked:**
- Shield binary: `zecurity-shield`, service: `zecurity-shield.service`
- Shield SPIFFE: `spiffe://ws-<slug>.zecurity.in/shield/<id>`
- Shield cert TTL: 7 days, renewal window: 48h
- Shield heartbeats to Connector `:9091` (NOT Controller directly)
- Connector gets a new Shield-facing gRPC server on `:9091`
- Interface: `zecurity0` (TUN, CGNAT 100.64.0.0/10)
- Shield network setup uses `rtnetlink` for interface configuration and typed nftables ruleset generation in Rust
- Connector gets `Goodbye` RPC for clean shutdown → immediate DISCONNECTED

**Tracking:** [[Sprint4/path.md]] — full dependency map with checkboxes

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

---

## Sprint 5 — Resource Discovery (Planned)

After Shield is deployed and active:
- RDE: Connector scans network, reports reachable services
- Admin adds resource definitions (IP/port/protocol)
- Per-resource nftables DROP rules delivered via Shield
- "Protect" button in admin UI triggers rule push

---

## Future Sprints (Rough Order)

| Sprint | Feature |
|--------|---------|
| Sprint 6 | Client enrollment (end-user device cert + SPIFFE identity) |
| Sprint 7 | Access policies (workspace ACLs: who can reach what resource) |
| Sprint 8 | Policy enforcement at Shield (nftables rules from ACL) |
| Sprint 9 | Traffic proxying (WireGuard or tun, full packet path) |

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
