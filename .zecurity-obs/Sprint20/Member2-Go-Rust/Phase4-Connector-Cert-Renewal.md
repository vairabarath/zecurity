---
type: phase
member: M2
person: Barath
sprint: 20
phase: 4
execution: G
title: Connector Renewal Trigger + Cert Hot-Swap
status: planned
depends_on: [3, "M1-Phase1"]
tags:
  - go
  - rust
  - connector
  - pki
  - renewal
  - provider-dashboard
---

# Phase 4 (G) — Connector Renewal Trigger + Cert Hot-Swap

> Depends on **M1 Phase 1 (B) merged**: it edits `control_stream.go` (conflict zone) and relies on
> B's revocation guard in `RenewCert`. `depends_on: [3]` orders it after M2's relay work.
> Decision Record prerequisite #3 (Q15; discovery §8).

## Problem (verified)

### Controller never triggers renewal

- The connector renews only when it receives `ReEnroll` on the control stream (`connector/src/control_stream.rs:353-370` → `renewal::renew_cert`).
- No controller code constructs `ConnectorControlMessage_ReEnroll`. The old unary heartbeat computed `renewBy := now + RenewalWindow` and set `ReEnroll`. It was removed in commit `372744d` ("cut over agents to streaming control") and never re-added to the stream.
- `connector.Config.RenewalWindow` (`CONNECTOR_RENEWAL_WINDOW`, 48 h, `main.go:~192`) is read but unused.
- Result: connector certs (`CONNECTOR_CERT_TTL` 7 d) expire, and the connector then fails mTLS everywhere.

### Renewed cert is not used by every TLS consumer

The connector loads its cert/key once at startup (`connector/src/main.rs:88`,
`CertStore::load`). After `renew_cert` writes new files, only the control stream picks them up,
because it reloads on each reconnect (`control_stream.rs:145`). These consumers keep the **old**
cert until process restart:

| # | Consumer | Where | Role |
|---|----------|-------|------|
| 1 | Shield-proxy controller channel (`ShieldRegistry.controller_channel`) | `main.rs:92`, `agent_server.rs:74,723` | client identity to controller (proxies shield `RenewCert`) |
| 2 | Shield-facing gRPC server `:9091` | `agent_server.rs:301-323` (reads `connector.crt` once) | server identity to shields |
| 3 | Device-tunnel TLS listener `:9092` | `device_tunnel::listen`, `tls/server_cfg.rs` `build_device_tunnel_tls` (`with_single_cert`) | server identity to clients |
| 4 | Device-tunnel QUIC listener `:9092` | `quic_listener::listen`, same builder | server identity to clients |
| 5 | Relay stream handler | `relay_handler::RelayHandler::new(&cert_store, …)` (`main.rs:~245`) | inner TLS for relayed tunnels |
| 6 | Relay client / selector identity | `RelaySelectorConfig{cert_pem, key_pem}` (`main.rs:~272`) → `relay_client.rs` | client identity to relays |
| 7 | Control stream | `control_stream.rs:145` | ✔ already reloads |

Also, `renewal.rs` writes `connector.crt` and `workspace_ca.crt` with plain `tokio::fs::write`
(not atomic).

## Goal

Inside `CONNECTOR_RENEWAL_WINDOW` the controller asks the connector to renew. After renewal,
**every** connector TLS path uses the new certificate before the old one expires, without
dropping live tunnels.

## Files

| File | Change |
|------|--------|
| `controller/internal/connector/control_stream.go` | `handleConnectorHealth`: `RETURNING cert_not_after` from the guarded UPDATE; enqueue `ReEnroll` when inside the window; per-stream throttle field on `connectorStreamClient` |
| `controller/internal/connector/control_stream_test.go` | Trigger/throttle tests |
| `connector/src/tls/cert_store.rs` (or new `tls/cert_holder.rs`) | Shared reloadable holder (e.g. `Arc<ArcSwap<CertStore>>` + `tokio::sync::watch` for change notification) |
| `connector/src/renewal.rs` | Atomic writes; publish the new `CertStore` to the holder |
| `connector/src/main.rs` | Construct the holder once; pass it to consumers 1–6 instead of cloned bytes |
| `connector/src/tls/server_cfg.rs` | Server configs resolve the cert through the holder (rustls `ResolvesServerCert`) |
| `connector/src/agent_server.rs` | `:9091` server identity from holder; `controller_channel` rebuilt on cert change |
| `connector/src/relay_handler.rs`, `relay_selector.rs`, `relay_client.rs` | Read identity from holder on each new session / dial |
| Rust tests alongside the above | Holder swap + consumer tests |

## Steps

### G1 — Controller trigger (Go)

In `handleConnectorHealth`, after Phase B's guarded UPDATE succeeds:

```sql
... UPDATE connectors SET ... WHERE id = $5 AND status NOT IN ('revoked','deleted') AND revoked_at IS NULL
    RETURNING id, cert_not_after ...
```

```go
if certNotAfter != nil && time.Until(*certNotAfter) < h.Cfg.RenewalWindow &&
    client.reEnrollDue(now, reEnrollResendInterval) {
    client.send(&pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_ReEnroll{ReEnroll: &shieldpb.ReEnrollSignal{}}})
}
```

- `reEnrollResendInterval`: send at most once per stream per e.g. 10 min, so a connector whose renewal keeps failing is re-asked but not every 15 s. A successful renewal reconnects the stream (connector side) and updates `cert_not_after`, which clears the condition.
- A revoked connector never reaches this code (the guarded UPDATE returns no row and the stream closes).
- `send` failures (full mailbox) are logged, not fatal; the next health report retries.

### G2 — Holder (Rust)

- One holder constructed in `main.rs` from the initial `CertStore`.
- `renewal::renew_cert` writes files **atomically** (temp + fsync + rename, for both `connector.crt` and `workspace_ca.crt`), then `holder.store(new_cert_store)`, which notifies watchers.
- The key never changes (existing renewal keeps the key pair).

### G3 — Consumers (Rust)

- **Servers (#2 `:9091`, #3/#4 `:9092`, #5 relay inner TLS):** build rustls `ServerConfig` with a resolver that returns the holder's current `CertifiedKey`. New handshakes use the new cert; existing sessions continue. For tonic (`:9091`), use a rustls-based acceptor with the same resolver, or rebuild the server's TLS config on change without dropping established streams. Pick whichever keeps live shield streams up, and record the choice in Post-Phase Fixes.
- **Clients (#1 controller channel, #6 relay dials):** read identity from the holder when creating a channel/connection. On a holder change, rebuild `controller_channel` (swap behind an `ArcSwap` in `ShieldRegistry`). Relay sessions pick up the new identity on the next dial or migration, and must dial with the new identity before the old cert expires. Worst case the selector re-dials once after a swap; active tunnels are drained, not cut.

## Invariants

- **I1** A connector inside `CONNECTOR_RENEWAL_WINDOW` receives `ReEnroll` on the next health report after entering the window, and no more often than the resend interval.
- **I2** Revoked connectors never receive `ReEnroll` and cannot renew (Phase B guard in `RenewCert`).
- **I3** After a successful renewal, all seven consumers present the new cert before the old cert's `NotAfter`.
- **I4** Renewal never drops established device tunnels or shield control streams.
- **I5** Cert files on disk are always complete (atomic writes).
- **I6** Shield renewal is unchanged (connector-driven, 48 h, `agent_server.rs:624-630`). The shield-proxy path (#1) must keep working after the connector's renewal.
- **I7** No proto change: `ReEnrollSignal` (field 2 of `ConnectorControlMessage`) and `RenewCert` already exist.
- **I8** `CONNECTOR_RENEWAL_WINDOW` default (48 h) and `CONNECTOR_CERT_TTL` default (7 d) are unchanged.

## Tests

Go (`control_stream_test.go`):
- Health report, `cert_not_after` inside the window → exactly one `ReEnroll` enqueued.
- Second report within the resend interval → none; after the interval → one more.
- `cert_not_after` outside the window → none.
- `cert_not_after` NULL (legacy row) → none, no panic.
- Revoked connector → stream closes, no `ReEnroll`.

Rust (`cargo test` in `connector/`):
- Holder: `store` then `load` returns the new cert; watchers are notified.
- Server resolver: a handshake after the swap presents the new serial (device-tunnel config).
- `renew_cert` atomic write: a simulated failure before rename leaves the old files intact.
- `ShieldRegistry` rebuilds `controller_channel` on a holder change (unit-level, with a fake channel factory if needed).
- Relay selector config reads the identity from the holder (new session after swap uses the new cert).

## Acceptance Criteria

- [ ] Dev stack with `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`: within ~5 min the controller logs a `ReEnroll`, the connector logs a renewal, and `connectors.cert_serial` changes.
- [ ] After the **original** cert's `NotAfter`, without restarting the connector: a client opens a direct tunnel (`:9092`) successfully; a relayed tunnel works; a shield completes its own renewal through the connector.
- [ ] No established tunnel is dropped at the swap (watch session logs).
- [ ] Revoked connector: no `ReEnroll`, `RenewCert` denied.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/connector/...
cd connector && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml    # unchanged; must still build
```

## Implementation Checklist

- [ ] **M2-G1** Controller: `ReEnroll` trigger in `handleConnectorHealth` with per-stream throttle
- [ ] **M2-G2** Connector: shared cert holder; atomic renewal writes; publish on renew
- [ ] **M2-G3** Connector: consumers #1–#6 read from the holder (servers via resolver; clients rebuilt)
- [ ] **M2-G4** Tests (Go trigger/throttle; Rust holder + consumers)
- [ ] **Build gate:** `cd controller && go build ./... && cd ../connector && cargo build`

## Post-Phase Fixes

_None yet._
