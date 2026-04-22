---
type: planning
status: active
sprint: 5
tags:
  - sprint5
  - dependencies
  - execution-path
  - team-coordination
---

# Sprint 5 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

Admin defines a resource (IP + port) on a Shield host → Shield applies nftables rules to make the service invisible on LAN but accessible via `zecurity0` → resource status tracked through `pending → managing → protecting → protected` lifecycle via heartbeat piggyback. No new RPCs — all delivery rides on existing Connector ↔ Controller heartbeat.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **Shield auto-match** | `shield_id` set automatically by Controller: `SELECT id FROM shields WHERE lan_ip = $host` |
| **IP validation** | Shield checks `resource.host == detect_lan_ip()` before applying nftables |
| **Health check** | Shield checks port liveness every 30s via `TcpStream::connect("127.0.0.1:{port}")` |
| **Heartbeat interval** | Resource check = 30s (separate from heartbeat = 60s) |
| **Delivery guarantee** | Controller resends all `managing`/`removing` resources every heartbeat until Shield ACKs |
| **nftables chain** | Separate `chain resource_protect` — flushed + rebuilt atomically each update |
| **LAN block** | `tcp dport {port} iif != {lo, zecurity0} drop` — lo + zecurity0 always allowed |

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Resources page, CreateResourceModal, Protect/Unprotect buttons, GraphQL hooks |
| **M2** | Go (Proto + DB + GraphQL Schema) | proto changes, migration 007, `graph/resource.graphqls` |
| **M3** | Go (Controller + Connector relay) | resource package, resolvers, connector `heartbeat.go` + `agent_server.rs` relay |
| **M4** | Rust (Shield) | `shield/src/resources.rs`, config, heartbeat ack, nftables chain |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/shield/v1/shield.proto` | M2 adds ResourceInstruction/Ack messages | M2 commits first — everyone else waits for buf generate |
| `proto/connector/v1/connector.proto` | M2 adds shield_resources + resource_acks to heartbeat | M2 commits first |
| `controller/internal/connector/heartbeat.go` | M3 adds resource injection + ack processing | M3 only |
| `connector/src/agent_server.rs` | M3 adds resource cache + relay | M3 only |
| `connector/src/heartbeat.rs` | M3 adds resource_acks forwarding | M3 only |
| `shield/src/heartbeat.rs` | M4 adds resource handling | M4 only |

---

## Execution Timeline

### DAY 1 — Unblocking Work (Must land before anyone fans out)

- [x] **M2-D1-A** `proto/shield/v1/shield.proto` — Add `ResourceInstruction` + `ResourceAck` messages; add `resources` to `HeartbeatResponse`; add `resource_acks` to `HeartbeatRequest`
- [x] **M2-D1-B** `proto/connector/v1/connector.proto` — Add `ShieldResourceInstructions` wrapper; add `shield_resources` map to `HeartbeatResponse`; add `resource_acks` to `HeartbeatRequest`
- [x] **M2-D1-C** `controller/migrations/007_resources.sql` — `resources` table with all columns + partial indexes
- [x] **M2-D1-D** `controller/graph/resource.graphqls` — `Resource` type, `CreateResource` mutation, `GetResources` + `GetAllResources` queries, `ProtectResource` + `UnprotectResource` + `DeleteResource` mutations
- [x] **TEAM** Run `buf generate` from repo root → Go stubs updated
- [x] **TEAM** Run `cd controller && go generate ./graph/...` → gqlgen regenerates `generated.go`

> After Day 1 checkboxes are done: M1 can start layout, M3 can start resource package, M4 can start resources.rs scaffold.

---

### PHASE A — M2 Resource Package (Depends on: Day 1 done)

- [x] **M2-A1** `controller/internal/resource/config.go` — `ResourceConfig` struct, `NewConfig()`, duration constants
- [x] **M2-A2** `controller/internal/resource/store.go` — DB helpers: `CreateResource` (auto-match shield by lan_ip), `GetPendingForShield`, `UpdateStatus`, `RecordAck`, `MarkRemoving`, `SoftDelete`

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE B — M3 Resolvers (Depends on: Day 1 done + M2-A done)

- [x] **M3-B1** `controller/graph/resolvers/resource.resolvers.go` — `CreateResource` (auto-match shield), `ProtectResource` (status → managing), `UnprotectResource` (status → removing), `DeleteResource` (soft delete), `GetResources(shieldId)`, `GetAllResources`
- [x] **M3-B2** `controller/graph/resolvers/helpers.go` — Add `toResourceGQL()` mapper

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE C — M3 Connector Heartbeat Relay (Depends on: Day 1 proto done + M3-B done)

- [x] **M3-C1** `controller/internal/connector/heartbeat.go` — MODIFY: after updating connector row, query `GetPendingForShield` for each active shield → inject into `HeartbeatResponse.shield_resources`; process `req.resource_acks` → call `resource.RecordAck()`
- [x] **M3-C2** `connector/src/agent_server.rs` — MODIFY: cache `Vec<ResourceInstruction>` per shield_id from Connector HeartbeatResponse; return cached instructions in Shield `HeartbeatResponse.resources`; collect Shield `ResourceAck`s and store in `ShieldHealth`
- [x] **M3-C3** `connector/src/heartbeat.rs` — MODIFY: collect `resource_acks` from all `ShieldHealth` entries → forward in `HeartbeatRequest.resource_acks` to Controller

> Build check: `cd controller && go build ./...` + `cd connector && cargo build` must pass.

---

### PHASE D — M4 Shield Resources (Depends on: Day 1 proto done)

- [x] **M4-D1** `shield/src/resources.rs` — NEW file:
  - `validate_host(resource_host)` → checks `resource_host == detect_lan_ip()`
  - `check_port(port)` → `TcpStream::connect("127.0.0.1:{port}")` → bool
  - `apply_nftables(resources)` → flush + rebuild `chain resource_protect` atomically
  - `remove_nftables(resource_id)` → remove rule for this resource
  - `run_health_check_loop(interval_secs, shared_state)` → every 30s check all protected ports
- [x] **M4-D2** `shield/src/config.rs` — Add `resource_check_interval_secs: u64` (default 30)
- [x] **M4-D3** `shield/src/heartbeat.rs` — MODIFY: handle `resp.resources` → validate host → apply nftables → build `ResourceAck`; send `resource_acks` in `HeartbeatRequest`
- [x] **M4-D4** `shield/src/main.rs` — MODIFY: `tokio::spawn(resources::run_health_check_loop(cfg, shared_acks))`

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---

### PHASE E — M1 Frontend (Depends on: Day 1 codegen done)

- [ ] **M1-E1** `admin/src/pages/Resources.tsx` — Global resources page at `/resources`; columns: Name, Host IP, Protocol, Port, Shield (auto-matched), Status, Last Active; Protect/Unprotect/Delete buttons per row
- [ ] **M1-E2** `admin/src/components/CreateResourceModal.tsx` — Form: Name, Host IP, Protocol (tcp/udp/any), Port From, Port To; no shield selector (auto-matched)
- [ ] **M1-E3** `admin/src/graphql/queries.graphql` — Add `GetAllResources` + `GetResources(shieldId)`
- [ ] **M1-E4** `admin/src/graphql/mutations.graphql` — Add `CreateResource`, `ProtectResource`, `UnprotectResource`, `DeleteResource`
- [ ] **M1-E5** `admin/src/App.tsx` — Add `/resources` route
- [ ] **M1-E6** `admin/src/components/layout/Sidebar.tsx` — Add "Resources" nav link
- [ ] **M1-E7** Run `cd admin && npm run codegen` — regenerate TypeScript hooks

> Build check: `cd admin && npm run build` must pass.

---

## Integration Checklist (Final Validation)

Run these once all phases are complete:

- [ ] `buf generate` (from repo root) — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd connector && cargo build` — clean (warnings OK)
- [ ] `cargo build --manifest-path shield/Cargo.toml` — clean
- [ ] `cd admin && npm run build` — clean
- [ ] Full DB migration: `007_resources.sql` runs on fresh DB
- [ ] Create resource → auto-matched to shield by lan_ip → status = pending
- [ ] Click Protect → status = managing → next heartbeat delivers to Shield
- [ ] Shield applies nftables → `nft list ruleset` shows `chain resource_protect`
- [ ] Port blocked on LAN: `nc -zv {shield_lan_ip} {port}` from another host → refused
- [ ] Port reachable via zecurity0: `nc -zv {interface_addr} {port}` → success
- [ ] Shield health check: stop the service → status = failed within 90s
- [ ] Restart service → status = protected within 90s
- [ ] Click Unprotect → nftables rule removed → port accessible on LAN again
- [ ] Shield goes offline → UI shows "Shield Offline" on all its resources
- [ ] Host with no Shield installed → Create resource rejected with "no shield on this host"

---

## Dependency Graph (Visual)

```
       M2-D1-A (shield.proto ResourceInstruction/Ack)
       M2-D1-B (connector.proto shield_resources/resource_acks)    Day 1 — FIRST
       M2-D1-C (007_resources.sql)
       M2-D1-D (graph/resource.graphqls)
              │
              ▼
       buf generate + go generate
              │
      ┌───────┼────────────┬──────────────┐
      ▼       ▼            ▼              ▼
    M2-A    M3-B         M4-D           M1-E (layout)
  (resource (resolvers)  (resources.rs   (can start
   package)              + config)        immediately)
      │       │               │
      ▼       ▼               ▼
    M2-A2   M3-C1          M4-D3
  (store.go) (heartbeat     (heartbeat.rs
             relay)          ack handling)
              │
              ▼
            M3-C2/C3
          (agent_server
           + connector
           heartbeat)
```

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Before touching any file, confirm dependency checkboxes are checked.
2. **Proto field numbers are permanent.** Never reuse or renumber. Check existing max field number before adding.
3. **Shield only protects its own host.** Validate `resource.host == detect_lan_ip()` before applying nftables.
4. **nftables chain is atomic.** Always flush + rebuild `chain resource_protect` — never append incrementally.
5. **Heartbeat piggyback only.** No new RPCs. Resource instructions ride on existing HeartbeatResponse.
6. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.

See individual member phase files for detailed specs:
- [[Sprint5/Member1-Frontend/Phase1-Resources-Page]]
- [[Sprint5/Member2-Go-Proto-DB/Phase1-Proto-Migration-Schema]]
- [[Sprint5/Member2-Go-Proto-DB/Phase2-Resource-Package]]
- [[Sprint5/Member3-Go-Controller/Phase1-Resolvers]]
- [[Sprint5/Member3-Go-Controller/Phase2-Heartbeat-Relay]]
- [[Sprint5/Member4-Rust-Shield/Phase1-Resources-Module]]
- [[Sprint5/Member4-Rust-Shield/Phase2-Heartbeat-Ack]]
