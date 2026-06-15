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

> **Security prerequisite:** Complete [[Sprint10.1/path]] before enabling Relay
> Connector registration or Client fallback. Sprint 10.1 defines Relay
> certificate provisioning, multi-workspace chain validation, and inner
> Client-to-Connector mTLS so the Relay cannot inspect tunnel payloads.

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

This is a **two-member sprint** (M2 + M3 only). M1 (Frontend) and M4 (Client/Shield) are not assigned active phases — M4 Client changes are handled by M3 in this sprint.

| Member | Role | Area |
|--------|------|------|
| **M2** | Go — Control Plane | Proto changes, ACL snapshot fields, controller DB (relay columns), SPIFFE validation design |
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
| `client/Cargo.toml` | M3 adds quinn dependency if missing | M3 only |

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
- [ ] **TEAM** Run `cd controller && go build ./...` — passes (new fields default to zero/empty)
- [ ] **TEAM** Run `cd admin && npm run codegen`

> After Day 1: M2 continues with controller/DB changes. M3 starts `relay/` scaffold and connector relay client.

---

### PHASE A — M2: ACL Snapshot + Controller Relay Config

> **Goal:** Controller reads relay address from env, populates new ACL snapshot fields.
> **Depends on:** Day 1 proto done.
> See `Sprint10/Member2-Go/Phase1-ACL-Relay-Fields.md`.

- [ ] **M2-A1** `controller/internal/policy/compiler.go` — Extend `CompileACLSnapshot` to populate `relay_addr`, `connector_id`, `connector_spiffe`:
  - Query `connectors` table for `id`, `spiffe_id` alongside existing `lan_addr` query
  - Read relay address from config/env `RELAY_ADDR` (empty = relay disabled)
  - Set all three fields in the returned `ACLSnapshot`
- [ ] **M2-A2** Controller config — Add `RelayAddr string` field to server config struct; read from `RELAY_ADDR` env var; pass into compiler
- [ ] **M2-A3** Build check: `cd controller && go build ./...` passes

---

### PHASE B — M2: Relay SPIFFE Verification Design

> **Goal:** Document the exact SPIFFE validation rules that the relay must enforce.
> This is design ownership — M3 implements, M2 owns the spec.
> See `Sprint10/Member2-Go/Phase2-SPIFFE-Relay-Spec.md`.

- [ ] **M2-B1** Write `Sprint10/Member2-Go/Phase2-SPIFFE-Relay-Spec.md` specifying:
  - Exact connector SPIFFE format: `spiffe://<trust_domain>/connector/<connector_id>`
  - Exact client device SPIFFE format: `spiffe://<trust_domain>/client_device/<device_id>`
  - Workspace isolation rule: trust domain must match between client and connector SPIFFE
  - Relay should reject if SPIFFE does not parse or trust domain mismatches
- [ ] **M2-B2** Review M3's `relay/src/spiffe.rs` once written — confirm exact-match logic matches the spec

---

### PHASE C — M3: Relay Service (New `relay/` Crate)

> **Goal:** Standalone Rust binary — accepts QUIC connections from connectors and clients, bridges them.
> **Depends on:** Day 1 done (SPIFFE spec helps but relay can scaffold without it).
> See `Sprint10/Member3-Rust/Phase1-Relay-Service.md`.

- [ ] **M3-C1** `relay/Cargo.toml` — New workspace member. Deps: `quinn`, `rustls`, `tokio`, `serde_json`, `tracing`, `anyhow`, `bytes`
- [ ] **M3-C2** `relay/src/main.rs` — QUIC listener on `:9093`, ALPN `ztna-relay-v1`, loads TLS cert from env `RELAY_TLS_CERT` / `RELAY_TLS_KEY`, calls `relay::start()`
- [ ] **M3-C3** `relay/src/listener.rs` — `start()`: creates QUIC endpoint, accept loop, spawns `handle_connection()` per peer
- [ ] **M3-C4** `relay/src/state.rs` — `RelayState`:
  ```rust
  pub struct RelayState {
      connectors: DashMap<String, ConnectorEntry>,  // connector_id → entry
  }
  struct ConnectorEntry {
      connection: quinn::Connection,
      spiffe_id: String,
      trust_domain: String,
  }
  ```
  Methods: `insert_connector()`, `remove_connector()`, `lookup_connector()`
- [ ] **M3-C5** `relay/src/session.rs` — `handle_connection()`:
  - Reads first length-prefixed JSON message to determine role: `RegisterMsg` or `LookupMsg`
  - `RegisterMsg { connector_id, spiffe_id }` → validates SPIFFE, stores in `RelayState`
  - `LookupMsg { connector_id }` → validates client SPIFFE, checks trust domain match, opens new QUIC stream to stored connector, then `pipe_streams()` bidirectionally
- [ ] **M3-C6** `relay/src/spiffe.rs` — `validate_spiffe(cert, expected_prefix)` — extracts URI SAN from peer cert, validates exact connector/client format; workspace isolation via trust domain comparison
- [ ] **M3-C7** Build check: `cd relay && cargo build` passes

---

### PHASE D — M3: Connector Relay Registration

> **Goal:** Connector registers with relay on startup and maintains the connection.
> **Depends on:** M3-C (relay service running, or at least the protocol known).
> See `Sprint10/Member3-Rust/Phase2-Connector-Relay-Client.md`.

- [x] **M3-D1** `connector/src/relay_client.rs` — NEW:
  - `RelayClient::new(relay_addr, connector_id, tls_config)` 
  - `connect_and_register()` — opens QUIC connection to relay, sends `RegisterMsg`, keeps connection alive
  - `maintain_registration()` — reconnect loop: on disconnect, wait 5s, reconnect
  - Spawned as a background task; does not block connector startup
- [x] **M3-D2** `connector/src/main.rs` — After existing listeners are up, if `relay_addr` is set in config, spawn `relay_client::maintain_registration()`
- [x] **M3-D3** `connector/src/config.rs` — Add `relay_addr: Option<String>` field; read from `RELAY_ADDR` env var
- [x] **M3-D4** Build check: `cd connector && cargo build` passes

---

### PHASE E — M3: Client Relay Pool + Fallback

> **Goal:** Client tries direct QUIC first; on failure falls back to relay.
> **Depends on:** M2-D1 (ACL snapshot has `relay_addr` and `connector_id`).
> See `Sprint10/Member3-Rust/Phase3-Client-Relay-Fallback.md`.

- [ ] **M3-E1** `client/src/relay_pool.rs` — NEW:
  - `RelayPool` — per-relay QUIC connection (analogous to `tunnel_pool.rs` but for relay)
  - `connect_relay(relay_addr)` — opens QUIC connection to relay using device mTLS cert
  - `open_relay_stream(connector_id)` — opens QUIC stream, sends `LookupMsg`, returns `(SendStream, RecvStream)` ready for tunnel traffic
- [ ] **M3-E2** `client/src/tunnel_pool.rs` — MODIFY: In `open_stream()` (or equivalent), wrap direct QUIC connect in a 2s timeout:
  ```rust
  match tokio::time::timeout(Duration::from_secs(2), connect_direct(&addr)).await {
      Ok(Ok(stream)) => return Ok(stream),
      _ => {
          // fall back to relay
          relay_pool.open_relay_stream(&connector_id).await
      }
  }
  ```
- [ ] **M3-E3** `client/src/daemon.rs` — Pass `relay_addr` and `connector_id` from ACL snapshot through to tunnel pool when starting smoltcp loop
- [ ] **M3-E4** Build check: `cd client && cargo build` passes

---

## Final Build Gates (all must pass)

- [ ] `buf generate` (repo root) — clean
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd relay && cargo build` — clean
- [ ] `cd connector && cargo build` — clean
- [ ] `cd client && cargo build` — clean
- [ ] `cd admin && npm run build` — clean

## Acceptance Criteria

- [ ] Connector on server A registers with relay — `RelayState` shows connector entry
- [ ] Client on server B (different network, no direct path to A) connects via relay — traffic flows
- [ ] End-to-end mTLS intact — Wireshark on relay shows ciphertext only
- [ ] Client on same LAN as connector takes direct path — relay not used
- [ ] Connector disconnects → `RelayState` removes entry → client lookup returns error
- [ ] Client with wrong trust domain (different workspace) → relay rejects with SPIFFE mismatch
- [ ] `RELAY_ADDR` unset → `relay_addr` in ACL snapshot is empty → client never attempts relay fallback

---

## Dependency Graph

```
Sprint 9 (ACLSnapshot, TunnelPool, device mTLS)
              │
              ▼
       M2-D1: proto/client/v1/client.proto
       (relay_addr, connector_id, connector_spiffe fields)
              │
       buf generate → go build → npm codegen
              │
      ┌───────┴────────────────────┐
      ▼                            ▼
   M2-A                          M3-C
  (Controller:                 (relay/ new crate:
   compiler.go +                listener, state,
   RELAY_ADDR config)            session, spiffe)
      │                            │
      │                            ▼
      │                          M3-D
      │                       (connector/src/
      │                        relay_client.rs)
      │                            │
      └───────────┬────────────────┘
                  ▼
               M3-E
           (client/src/
            relay_pool.rs +
            tunnel_pool.rs fallback)
                  │
                  ▼
           Integration test
```

---

## Notes for AI Agents

1. **Relay is a new workspace member.** Add `relay` to the root `Cargo.toml` workspace members list.
2. **Proto field numbers 6–8 on ACLSnapshot are now reserved for Sprint 10.** Never reuse them.
3. **Relay never calls the controller.** All identity validation is from mTLS peer certificates only.
4. **Relay has no persistent state.** Connector must re-register after relay restart. This is acceptable — connector `maintain_registration()` reconnects automatically.
5. **Workspace isolation is enforced by SPIFFE trust domain.** The trust domain in `spiffe://<trust_domain>/...` must be identical for client and connector to be bridged.
6. **Direct path takes precedence.** The 2s timeout in tunnel_pool fallback is the only trigger for relay use — do not add logic that prefers relay.
7. **Build gates are not optional.** Each phase has a build check. Do not proceed until it passes.

See individual member phase files for detailed specs:
- [[Sprint10/Member2-Go/Phase1-ACL-Relay-Fields]] — compiler.go + controller config (M2)
- [[Sprint10/Member2-Go/Phase2-SPIFFE-Relay-Spec]] — SPIFFE validation spec (M2)
- [[Sprint10/Member3-Rust/Phase1-Relay-Service]] — relay/ new crate (M3)
- [[Sprint10/Member3-Rust/Phase2-Connector-Relay-Client]] — relay_client.rs (M3)
- [[Sprint10/Member3-Rust/Phase3-Client-Relay-Fallback]] — relay_pool.rs + tunnel_pool fallback (M3)

---

## Post-Sprint Fixes

*(Empty — add fixes here as they are discovered during testing)*
