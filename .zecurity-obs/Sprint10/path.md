---
type: planning
status: planned
sprint: 10
tags:
  - sprint10
  - relay
  - dependencies
  - execution-path
  - team-coordination
---

# Sprint 10 — Relay: Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

**Relay** — a transparent QUIC-based proxy for clients that cannot reach a Connector directly (different networks, NAT, firewall). When direct connection to `:9092` fails, the client falls back to the Relay.

The Relay is a dumb pipe — it validates SPIFFE identities on both sides and bridges two QUIC streams together. It **cannot decrypt traffic**: end-to-end mTLS is maintained between Client and Connector. The Relay only sees ciphertext.

```
Same LAN (Sprint 9 path still works):
  Client → Connector :9092 (direct QUIC)

Different Networks (Sprint 10 adds):
  Client → Relay → Connector
  End-to-end mTLS — Relay cannot decrypt
```

> **Prerequisite:** Sprint 9 RDE dataplane must be complete. `ConnectorTunnelAddr` in `ACLSnapshot` is the existing direct address. Sprint 10 extends this with `relay_addr`, `connector_id`, and `connector_spiffe`.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **Relay transport** | QUIC on `:9093` (separate port from Connector `:9092`). ALPN `ztna-relay-v1`. |
| **Relay as dumb pipe** | Relay validates both sides' SPIFFE certificates, then bridges streams bidirectionally — never terminates mTLS |
| **Relay wire protocol** | Binary QUIC streams: first message = `RegisterMsg` (connector) or `LookupMsg` (client) as length-prefixed JSON; subsequent data = raw bytes passed through |
| **Connector registration** | Connector opens a persistent QUIC connection to Relay on startup and sends `RegisterMsg{connector_id, spiffe_id}`. Relay stores this. |
| **Client lookup** | Client sends `LookupMsg{connector_id}`. Relay validates client SPIFFE, finds connector connection, opens a new QUIC stream to the connector, then pipes both streams. |
| **Fallback logic** | Client tries direct QUIC first (`connector_tunnel_addr` from ACL snapshot). If connection fails within 2s, falls back to `relay_addr`. |
| **SPIFFE validation** | Relay validates exact SPIFFE URI (not prefix). Connector: `spiffe://<domain>/connector/<id>`. Client: `spiffe://<domain>/client_device/<id>`. Mismatch = reject. |
| **Relay state** | In-memory only — `connector_id → (QUIC connection, QUIC stream sender)`. No persistence. Connector re-registers on reconnect. |
| **Workspace isolation** | Relay derives workspace from SPIFFE trust domain — connectors and clients from different workspaces cannot be bridged |
| **ACL snapshot fields** | Add `relay_addr` (string), `connector_id` (string), `connector_spiffe` (string) to `ACLSnapshot` proto |
| **Controller relay config** | `RELAY_ADDR` env var in controller; stored in ACL snapshot so client knows where to connect |

---

## Team Assignments

This is a **two-member sprint** (M2 + M3 only).

| Member | Role | Area |
|--------|------|------|
| **M2** | Go — Control Plane | Proto changes, ACL snapshot fields, controller relay config, SPIFFE validation design |
| **M3** | Rust — Data Plane | New `relay/` crate, Connector `relay_client.rs`, Client `relay_pool.rs`, fallback in `tunnel_pool.rs` |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `proto/client/v1/client.proto` | M2 adds `relay_addr`, `connector_id`, `connector_spiffe` to `ACLSnapshot` | M2 commits first — everyone waits for codegen |
| `controller/internal/policy/compiler.go` | M2 adds relay fields to snapshot | M2 only |
| `relay/` | M3 — new Rust crate | M3 only |
| `connector/src/relay_client.rs` | M3 — new file | M3 only |
| `connector/src/main.rs` | M3 wires relay_client startup | M3 only |
| `client/src/relay_pool.rs` | M3 — new file | M3 only |
| `client/src/tunnel_pool.rs` | M3 adds fallback path | M3 only |

---

## Execution Timeline

### DAY 1 — Unblocking Work (M2 must land before M3 fans out)

- [ ] **M2-D1** `proto/client/v1/client.proto` — Add three fields to `ACLSnapshot`:
  ```proto
  string relay_addr        = 6;  // relay QUIC address e.g. "relay.example.com:9093" — empty means no relay
  string connector_id      = 7;  // connector UUID — used in relay LookupMsg
  string connector_spiffe  = 8;  // connector SPIFFE URI — used by client to validate relay-bridged connection
  ```
- [ ] **TEAM** Run `buf generate` from repo root → Go stubs updated
- [ ] **TEAM** Run `cd controller && go build ./...` — passes
- [ ] **TEAM** Run `cd admin && npm run codegen`

> After Day 1: M2 continues with controller changes. M3 starts `relay/` scaffold and connector relay client.

---

### PHASE A — M2: ACL Snapshot + Controller Relay Config

> **Depends on:** Day 1 proto done.
> See `Sprint10/Member2-Go/Phase1-ACL-Relay-Fields.md`.

- [ ] **M2-A1** `controller/internal/policy/compiler.go` — Extend `CompileACLSnapshot` to populate `relay_addr`, `connector_id`, `connector_spiffe`
- [ ] **M2-A2** Controller config — Add `RelayAddr string` field; read from `RELAY_ADDR` env var
- [ ] **M2-A3** Build check: `cd controller && go build ./...` passes

---

### PHASE B — M2: Relay SPIFFE Verification Design

> See `Sprint10/Member2-Go/Phase2-SPIFFE-Relay-Spec.md`.

- [ ] **M2-B1** Write SPIFFE validation spec for relay (exact connector + client formats, workspace isolation rule)
- [ ] **M2-B2** Review M3's `relay/src/spiffe.rs` once written

---

### PHASE C — M3: Relay Service (New `relay/` Crate)

> **Depends on:** Day 1 done.
> See `Sprint10/Member3-Rust/Phase1-Relay-Service.md`.

- [ ] **M3-C1** `relay/Cargo.toml` — New workspace member
- [ ] **M3-C2** `relay/src/main.rs` — QUIC listener `:9093`, ALPN `ztna-relay-v1`
- [ ] **M3-C3** `relay/src/listener.rs` — accept loop
- [ ] **M3-C4** `relay/src/state.rs` — `RelayState` with `DashMap<connector_id, ConnectorEntry>`
- [ ] **M3-C5** `relay/src/session.rs` — handle Register vs Lookup, `pipe_streams()`
- [ ] **M3-C6** `relay/src/spiffe.rs` — exact SPIFFE validation per M2 spec
- [ ] **M3-C7** Build check: `cd relay && cargo build` passes

---

### PHASE D — M3: Connector Relay Registration

> **Depends on:** M3-C protocol known.
> See `Sprint10/Member3-Rust/Phase2-Connector-Relay-Client.md`.

- [ ] **M3-D1** `connector/src/relay_client.rs` — `connect_and_register()` + `maintain_registration()` reconnect loop
- [ ] **M3-D2** `connector/src/main.rs` — spawn relay registration task if `RELAY_ADDR` set
- [ ] **M3-D3** `connector/src/config.rs` — Add `relay_addr: Option<String>`
- [ ] **M3-D4** Build check: `cd connector && cargo build` passes

---

### PHASE E — M3: Client Relay Pool + Fallback

> **Depends on:** M2-D1 (ACL snapshot has `relay_addr` and `connector_id`).
> See `Sprint10/Member3-Rust/Phase3-Client-Relay-Fallback.md`.

- [ ] **M3-E1** `client/src/relay_pool.rs` — `RelayPool`, `connect_relay()`, `open_relay_stream()`
- [ ] **M3-E2** `client/src/tunnel_pool.rs` — 2s timeout on direct, fallback to relay
- [ ] **M3-E3** `client/src/daemon.rs` — pass `relay_addr` + `connector_id` from ACL snapshot to net stack
- [ ] **M3-E4** Build check: `cd client && cargo build` passes

---

## Final Build Gates

- [ ] `buf generate` (repo root) — clean
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd relay && cargo build` — clean
- [ ] `cd connector && cargo build` — clean
- [ ] `cd client && cargo build` — clean
- [ ] `cd admin && npm run build` — clean

## Acceptance Criteria

- [ ] Connector registers with relay — `RelayState` shows connector entry
- [ ] Client on different network connects via relay — traffic flows
- [ ] End-to-end mTLS intact — relay sees ciphertext only
- [ ] Client on same LAN takes direct path — relay not used
- [ ] Connector disconnects → relay removes entry → client lookup fails gracefully
- [ ] Client from different workspace → relay rejects with SPIFFE mismatch
- [ ] `RELAY_ADDR` unset → `relay_addr` empty in ACL snapshot → client never attempts relay

---

## Dependency Graph

```
Sprint 9 (ACLSnapshot, TunnelPool, device mTLS)
              │
              ▼
       M2-D1: proto/client/v1/client.proto
       (relay_addr, connector_id, connector_spiffe)
              │
       buf generate → go build → npm codegen
              │
      ┌───────┴────────────────────┐
      ▼                            ▼
   M2-A                          M3-C
  (compiler.go +               (relay/ crate:
   RELAY_ADDR config)            listener, state,
      │                          session, spiffe)
      │                            │
      │                            ▼
      │                          M3-D
      │                       (connector relay_client.rs)
      │                            │
      └───────────┬────────────────┘
                  ▼
               M3-E
           (client relay_pool.rs +
            tunnel_pool fallback)
                  │
                  ▼
           Integration test
```

---

## Notes for AI Agents

1. **Relay is a new workspace member.** Add `relay` to root `Cargo.toml` workspace members.
2. **Proto field numbers 6–8 on ACLSnapshot are reserved for Sprint 10.** Never reuse them.
3. **Relay never calls the controller.** All identity validation is from mTLS peer certificates only.
4. **Relay has no persistent state.** Connector re-registers after relay restart via `maintain_registration()`.
5. **Workspace isolation enforced by SPIFFE trust domain.** Trust domain must match between client and connector.
6. **Direct path takes precedence.** 2s timeout in tunnel_pool is the only trigger for relay fallback.
7. **Build gates are not optional.**

See phase files:
- [[Sprint10/Member2-Go/Phase1-ACL-Relay-Fields]]
- [[Sprint10/Member2-Go/Phase2-SPIFFE-Relay-Spec]]
- [[Sprint10/Member3-Rust/Phase1-Relay-Service]]
- [[Sprint10/Member3-Rust/Phase2-Connector-Relay-Client]]
- [[Sprint10/Member3-Rust/Phase3-Client-Relay-Fallback]]

---

## Post-Sprint Fixes

*(Empty — add fixes here as discovered during testing)*
