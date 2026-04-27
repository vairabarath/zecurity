---
type: planning
status: active
sprint: 4
tags:
  - sprint4
  - dependencies
  - execution-path
  - team-coordination
---

# Sprint 4 — Execution Path & Dependency Map

> **Note:** This sprint predates the control_stream refactor (commit 722995c). All `heartbeat.rs` references here reflect the state of Sprint 4 when it completed. The old heartbeat modules in both Connector and Shield were replaced by persistent bidirectional `control_stream.rs` in a later sprint.

---

## Sprint Goal

Deploy a Shield on any resource host → appears ACTIVE in dashboard → goes DISCONNECTED if offline → `zecurity0` interface + base nftables table set up automatically on enrollment. Shield heartbeats through Connector (:9091), never directly to Controller.

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | React + GraphQL operations + Admin UI |
| **M2** | Go (Proto + Shield + PKI) | proto files, appmeta, `internal/shield/`, `internal/pki/`, `cmd/server/main.go` |
| **M3** | Go (DB + GraphQL + Connector improvements) | migrations, graph schemas, resolvers, connector goodbye + heartbeat, Rust `agent_server.rs` |
| **M4** | Rust (Shield binary + CI) | `shield/` crate (new), `connector/src/main.rs`, GitHub Actions |

---

## Critical Rule: Conflict Zones

The following files are touched by **multiple members**. Coordinate before committing:

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/connector/v1/connector.proto` | M2 writes, M3+M4 consume | M2 commits first — everyone else waits for buf generate |
| `graph/connector.graphqls` | M3 writes NetworkHealth + shields, M1 consumes | M3 commits Day 1, M1 waits for codegen |
| `connector/src/main.rs` | M4 adds agent_server start | M4 only, after M3's `agent_server.rs` exists |
| `connector/src/heartbeat.rs` | M3 adds ShieldHealth processing | M3 only — M4's shield binary sends to M3's server (historical — now control_stream.rs) |
| `cmd/server/main.go` | M2 adds ShieldConfig + ShieldService registration | M2 only, after all M2 services are done |

---

## Execution Timeline

### DAY 1 — Unblocking Work (Must land before anyone fans out)

These must be committed to the shared branch **before** anyone else starts their implementation work.

- [x] **M2-D1-A** `proto/shield/v1/shield.proto` — NEW proto file (unblocks M3 buf generate + M4 crate)
- [x] **M2-D1-B** `proto/connector/v1/connector.proto` — Add `Goodbye` RPC + `ShieldHealth` message + `shields` field in `HeartbeatRequest`
- [x] **M2-D1-C** `controller/internal/appmeta/identity.go` — Add `SPIFFERoleShield`, `PKIShieldCNPrefix`, `ShieldInterfaceName`, `ShieldInterfaceCIDR`, `ShieldSPIFFEID()` (unblocks M3 resolvers + M4 appmeta)
- [x] **M3-D1-A** `controller/migrations/003_shield_schema.sql` — Shield table, indexes, unique interface_addr constraint (unblocks M2 token.go DB calls)
- [x] **M3-D1-B** `controller/graph/shield.graphqls` — Shield type, ShieldToken, Mutation + Query extensions (unblocks M1 codegen)
- [x] **M3-D1-C** `controller/graph/connector.graphqls` — Add `networkHealth`, `shields` field to `RemoteNetwork` type; add `NetworkHealth` enum (unblocks M1 codegen)
- [x] **TEAM** Run `buf generate` from repo root → Go stubs generated under `controller/gen/go/proto/shield/v1/` and connector stubs updated
- [x] **TEAM** Run `cd controller && go generate ./graph/...` → gqlgen regenerates `generated.go`

> After Day 1 checkboxes are done: M1 can start Shields page layout, M4 can scaffold the crate.

---

### PHASE A — M2 Core (No external dependencies after Day 1)

- [x] **M2-A1** `controller/internal/shield/config.go` — `ShieldConfig` struct with all duration fields
- [x] **M2-A2** `controller/internal/shield/token.go` — JWT generation, Redis JTI burn, connector selection (least-loaded), interface_addr assignment from 100.64.0.0/10
- [x] **M2-A3** `controller/internal/shield/enrollment.go` — `Enroll` gRPC handler (12-step flow: verify JWT → burn JTI → verify workspace → verify connector → parse+verify CSR → SignShieldCert → update DB → return response)
- [x] **M2-A4** `controller/internal/shield/heartbeat.go` — Disconnect watcher goroutine only (Controller does NOT receive Shield heartbeats directly)
- [x] **M2-A5** `controller/internal/shield/spiffe.go` — Thin wrapper reusing connector SPIFFE logic

> Build check after M2-A5: `cd controller && go build ./...` must pass.

---

### PHASE B — M2 PKI (Depends on: appmeta Day 1)

- [x] **M2-B1** `controller/internal/pki/workspace.go` — Add `SignShieldCert()` and `RenewShieldCert()` alongside existing `SignConnectorCert`/`RenewConnectorCert`

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE C — M2 Wiring (Depends on: M2-A done + M2-B done + M3-D1-A done)

- [x] **M2-C1** `controller/cmd/server/main.go` — Wire `ShieldConfig`, `shield.NewService()`, register `shieldpb.RegisterShieldServiceServer()`, start `RunDisconnectWatcher()`
- [x] **M2-C2** Add `SHIELD_CERT_TTL`, `SHIELD_RENEWAL_WINDOW`, `SHIELD_ENROLLMENT_TOKEN_TTL`, `SHIELD_DISCONNECT_THRESHOLD` to `controller/.env` and `.env.example`

> Final build check: `cd controller && go build ./...` must pass.

---

### PHASE D — M3 Resolvers (Depends on: Day 1 done + buf generate done)

- [x] **M3-D1** `controller/graph/resolvers/shield.resolvers.go` — `GenerateShieldToken`, `RevokeShield`, `DeleteShield` mutations; `Shields`, `Shield` queries
- [x] **M3-D2** `controller/graph/resolvers/connector.resolvers.go` — Add `NetworkHealth` computation (ONLINE / DEGRADED / OFFLINE based on active connector count)

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE E — M3 Connector Improvements (Depends on: Day 1 connector.proto done + buf generate)

- [x] **M3-E1** `controller/internal/connector/goodbye.go` — NEW file: `Goodbye` RPC handler; marks Connector DISCONNECTED immediately on clean shutdown
- [x] **M3-E2** `controller/internal/connector/heartbeat.go` — MODIFY: process `req.Shields` list → call `shieldSvc.UpdateShieldHealth()` for each entry after updating connector row

> Build check: `cd controller && go build ./...` must pass.

---

### PHASE F — M3 Rust Agent Server (Depends on: M2-D1-A proto done + M4 has NOT committed agent_server.rs yet)

- [x] **M3-F1** `connector/src/agent_server.rs` — NEW: Shield-facing gRPC server on :9091. Implements `ShieldService`: `Heartbeat` (update local shields map, check cert expiry), `RenewCert` (proxy to Controller), `Goodbye` (remove from map), `Enroll` (returns UNIMPLEMENTED — Shield enrolls with Controller directly)

> Coordination: M4 writes `connector/src/main.rs` to START the server — M3 writes the server itself. Agree on the public API (`ShieldServer::new()` signature) before M3 starts F1.

> Build check: `cd connector && cargo build` must pass (warnings OK, errors not).

---

### PHASE G — M4 Crate Scaffold (Depends on: M2-D1-A proto landed + buf generate done)

- [x] **M4-G1** `shield/Cargo.toml` — Full dependency list (tokio, tonic, prost, rcgen, rustls, figment, rtnetlink, nftables, etc.)
- [x] **M4-G2** `shield/build.rs` — `tonic_build::compile_protos("../proto/shield/v1/shield.proto")`
- [x] **M4-G3** `shield/Cross.toml` — pre-build apt-get protobuf-compiler
- [x] **M4-G4** `shield/Dockerfile` — mirrors connector Dockerfile

> Build check: `cargo build --manifest-path shield/Cargo.toml` must compile (even if main is empty).

---

### PHASE H — M4 Core Modules (Depends on: M4-G done)

- [x] **M4-H1** `shield/src/appmeta.rs` — Mirror `connector/src/appmeta.rs` + Shield constants (`SPIFFE_ROLE_SHIELD`, `PKI_SHIELD_CN_PREFIX`, `SHIELD_INTERFACE_NAME`, `SHIELD_INTERFACE_CIDR_RANGE`)
- [x] **M4-H2** `shield/src/config.rs` — figment config: `CONTROLLER_ADDR`, `CONTROLLER_HTTP_ADDR`, `ENROLLMENT_TOKEN`, `AUTO_UPDATE_ENABLED`, `LOG_LEVEL`, `SHIELD_HEARTBEAT_INTERVAL_SECS`; state dir `/var/lib/zecurity-shield/`
- [x] **M4-H3** `shield/src/main.rs` — Startup: init tracing → load config → check state.json → enrollment or heartbeat loop → SIGTERM handler calls Goodbye
- [x] **M4-H4** `shield/src/crypto.rs` — EC P-384 keygen, CSR builder, PEM/DER helpers (mirror `connector/src/crypto.rs`)
- [x] **M4-H5** `shield/src/tls.rs` — `verify_connector_spiffe()`: verifies Connector's SPIFFE ID during mTLS handshake (checks full URI: `spiffe://<trust_domain>/connector/<connector_id>`)
- [x] **M4-H6** `shield/src/util.rs` — hostname reader, public IP helper (mirror connector)

> Build check: `cargo build --manifest-path shield/Cargo.toml` must pass.

---

### PHASE I — M4 Enrollment (Depends on: M2-A3 Enroll handler live in dev env)

- [x] **M4-I1** `shield/src/enrollment.rs` — Full enrollment flow: parse JWT → fetch + verify CA fingerprint → keygen → build CSR (SPIFFE SAN: `spiffe://ws-<slug>.zecurity.in/shield/<id>`) → call Controller Enroll RPC → save certs + state.json → call `network::setup()`

> Integration check: Run enrollment against dev controller. Shield should appear in DB with `status='active'`.

---

### PHASE J — M4 Heartbeat + Renewal (Depends on: M3-F1 agent_server.rs live)

- [x] **M4-J1** `shield/src/heartbeat.rs` — mTLS heartbeat loop to Connector :9091; interval `SHIELD_HEARTBEAT_INTERVAL_SECS`; exponential backoff on failure; calls `renewal::renew_cert()` when `re_enroll=true`
*(historical — now `shield/src/control_stream.rs` in subsequent sprints)*
- [x] **M4-J2** `shield/src/renewal.rs` — RenewCert flow: read shield.key → build CSR → call RenewCert on Connector :9091 → save new shield.crt → update state.json

> Integration check: Heartbeat appears in Connector logs. Shield shows ACTIVE in dashboard within 30s.

---

### PHASE K — M4 Network Setup (Independent — no external dependencies)

- [x] **M4-K1** `shield/src/network.rs` — `setup(interface_addr, connector_addr)`: creates `zecurity0` TUN interface via rtnetlink, assigns interface_addr (/32), brings UP; writes nftables table `inet zecurity` with chain `input` (ACCEPT lo, ACCEPT connector_ip, DROP on zecurity0)

> Test check: After enrollment, `ip link show zecurity0` shows interface. `nft list ruleset` shows `table inet zecurity`.

---

### PHASE L — M4 Updater + Systemd + Install Script (Depends on: M4-H done)

- [x] **M4-L1** `shield/src/updater.rs` — Mirror connector updater; check `shield-v*` releases; replace `/usr/local/bin/zecurity-shield`
- [x] **M4-L2** `shield/systemd/zecurity-shield.service` — Service unit with `CAP_NET_ADMIN` + `CAP_NET_RAW` capabilities
- [x] **M4-L3** `shield/systemd/zecurity-shield-update.service` — Update service unit
- [x] **M4-L4** `shield/systemd/zecurity-shield-update.timer` — Weekly update timer
- [x] **M4-L5** `shield/scripts/shield-install.sh` — One-line install script (mirrors connector-install.sh)

---

### PHASE M — M4 CI + Connector Main (Depends on: M3-F1 done)

- [x] **M4-M1** `.github/workflows/shield-release.yml` — CI: triggers on `shield-v*` tags; cross builds amd64 + arm64 musl; uploads binaries + checksums + install script + systemd units
- [x] **M4-M2** `connector/src/main.rs` — MODIFY: instantiate `ShieldServer::new(controller_channel, trust_domain)` and `tokio::spawn` it on :9091

> Build check: `cd connector && cargo build` must pass. Connector starts and binds :9091.

---

### PHASE N — M1 Frontend Wire-up (Depends on: Day 1 codegen done)

- [x] **M1-N1** `admin/src/pages/Shields.tsx` — New page at `/remote-networks/<id>/shields`; table with columns: Name, Status, Interface (zecurity0 IP), Via (connector), Last Seen, Version, Hostname; 30s auto-poll; "Add Shield" → `InstallCommandModal`
- [x] **M1-N2** `admin/src/pages/RemoteNetworks.tsx` — Add NetworkHealth indicator (🟢/🟡/🔴) + shield count to each network card
- [x] **M1-N3** `admin/src/components/layout/Sidebar.tsx` — Add "Shields" nav link under "Connectors"
- [x] **M1-N4** `admin/src/graphql/mutations.graphql` — Add `GenerateShieldToken`, `RevokeShield`, `DeleteShield`
- [x] **M1-N5** `admin/src/graphql/queries.graphql` — Add `GetShields`
- [x] **M1-N6** Run `cd admin && npm run codegen` — generates TypeScript hooks from final schema

> Coordination: M1 can build layout + routing immediately. Only N1–N3 wiring needs the generated hooks (N6). Run codegen after Day 1 schema is committed for initial hooks, then re-run after all schema changes are final.

---

## Integration Checklist (Final Validation)

Run these once all phases are complete:

- [ ] `buf generate` (from repo root) — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd connector && cargo build` — clean (warnings OK)
- [ ] `cargo build --manifest-path shield/Cargo.toml` — clean
- [ ] `cd admin && npm run build` — clean
- [ ] Full DB migration: `003_shield_schema.sql` runs on fresh DB
- [ ] End-to-end enrollment: Shield appears ACTIVE within 30s
- [ ] `ip link show zecurity0` visible on resource host after enrollment
- [ ] `nft list ruleset` shows `table inet zecurity` after enrollment
- [ ] Kill Shield process → DISCONNECTED within 120s
- [ ] Restart Shield → ACTIVE on next Connector heartbeat cycle
- [ ] Connector SIGTERM → Connector DISCONNECTED immediately (Goodbye RPC)
- [ ] Shield SIGTERM → Shield removed from Connector's health map

---

## Dependency Graph (Visual)

```
       M2-D1-A (shield.proto)
       M2-D1-B (connector.proto)          Day 1 — Must land FIRST
       M2-D1-C (appmeta)        ─────────────────────────────────
       M3-D1-A (003_shield_schema.sql)
       M3-D1-B (graph/shield.graphqls)
       M3-D1-C (graph/connector.graphqls)
              │
              ▼
       buf generate + go generate
              │
      ┌───────┼───────────┬──────────────┐
      ▼       ▼           ▼              ▼
    M2-A    M3-D/E      M4-G/H         M1-N (layout)
  (shield    (resolvers   (crate         (can start
   package)  + goodbye    scaffold)       immediately)
      │       + heartbeat)     │
      ▼            │           ▼
    M2-B           ▼         M4-I (enrollment)
   (PKI)        M3-F1           requires M2-A3 live
      │       (agent_server.rs)
      ▼            │
    M2-C           ▼
  (main.go)    M4-M2 (connector/main.rs)
  wiring       M4-J (heartbeat to :9091)
                    │
                    ▼
               M4-K (network.rs) ← independent
               M4-L (updater + systemd) ← after M4-H
               M4-M1 (CI workflow) ← independent
               M1-N (full wire-up) ← after codegen
```

---

## Notes for AI Agents Working on This Sprint

1. **Always check this file first.** Before touching any file, find the phase box above and confirm its dependency checkboxes are checked.
2. **Do not modify files owned by other members** without coordination. The conflict zone table above is authoritative.
3. **Build gates are not optional.** Each phase has a build check. Do not proceed to the next phase until it passes.
4. **Proto is the contract.** Never change proto files without updating both consumers (Go stubs via buf generate, Rust stubs via build.rs).
5. **Vault updates:** After completing a phase, check the box in this file and append to [[Planning/Session Log]].

See individual member phase files for detailed specs:
- [[Sprint4/Member1-Frontend/Phase1-Layout-Routing]]
- [[Sprint4/Member2-Go-Proto-Shield/Phase1-Proto-appmeta]]
- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]]
- [[Sprint4/Member4-Rust-Shield-CI/Phase1-Crate-Scaffold]]
