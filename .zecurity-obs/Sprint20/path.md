---
type: planning
status: planned
sprint: 20
tags:
  - sprint20
  - dependencies
  - execution-path
  - team-coordination
  - provider-dashboard
  - backend-truth
  - relay
  - connector
  - pki
  - lifecycle
---

# Sprint 20 — Provider Dashboard Phase 1: Backend Truth

> **Read this before writing a single line of code.**
> Source of truth for scope: `docs/provider-dashboard-architecture-decisions.md` →
> **"Decision record — 2026-09-23"** (D-01 … D-23) and its **"Implementation prerequisites (Q15)"**.
> Evidence and line references: `docs/provider-dashboard-architecture-discovery.md` (§4, §8, §15, §17).
> This sprint **does not introduce or revisit any architectural decision.** Where this plan picks an
> *implementation* approach, it is marked as such and may be changed in code review without
> reopening the Decision Record.

## Sprint Goal

Make the state the controller stores **true**, before any provider dashboard reads it.

The provider dashboard's basic promise is: *what the console shows is the actual state of Zecurity.*
Today several backend paths store state that is wrong or will silently become wrong:

```text
DEFECT (today)                                         AFTER THIS SPRINT
--------------                                         -----------------
Healthy relays flap active↔inactive                    Relay liveness uses the real heartbeat
  (DB heartbeat written every 5 min, evicted after 90s)  (Valkey), DB write throttle kept

Revoked connector → stream closes → 'disconnected'     'revoked' / revoked_at is terminal; no status
  → reconnect → 'active' again; re-revoke is a no-op     write path can leave it

Revoked shield → next status batch → 'active'          Revoked shield stays revoked

Relay picks its own DNS/IP SANs (stored allowlist      CSR SANs ⊆ operator-registered allowlist;
  ignored)                                               mismatch rejected without burning the token

Disconnect watcher refreshes ACL plane only            Also refreshes the transport plane
  (clients keep dialing offline connectors)

Controller gRPC cert: 7 days, never reloaded           Rotated in-process before expiry

Relay cert: 30 days, no renewal                        In-band RelayService.RenewCert (D-19)

Connector cert: 7 days, controller never sends         Controller sends ReEnroll inside the renewal
  ReEnroll; renewed cert not used by all TLS paths       window; every connector TLS path uses the
                                                         renewed cert
```

## Constraints from the Decision Record (binding)

| Ref | Constraint for this sprint |
|-----|----------------------------|
| **D-02** | Single controller instance. Notifications, registry pushes and caches may stay **process-local**. Do not build cross-replica machinery. |
| **D-19** | Relay renewal is an **in-band `RelayService.RenewCert` RPC** over the existing mTLS channel, **same key**, CSR proof of possession, scheduled by the relay before expiry, recorded in `relay_certificates`. |
| **D-20** | A relay whose cert already expired or whose key is lost is **replaced**. No provisioning-token re-issue. |
| **D-21** | No relay drain. |
| **D-22** | Relays stay under the platform Intermediate. No new CA. |
| **D-23** | Telemetry = existing data only. Do not add heartbeat fields or persist `uptime_seconds` / `registered_connectors`. |
| **D-07/D-08** | Suspension is **not** in this sprint. Do not change the `workspaces.status='active'` predicates (SPIFFE trust-domain validator, disconnect watchers, enrollment). A later sprint owns them. |
| **D-03** | No pools / regions. The relay list stays global. |
| Existing ADRs | ADR-016 (controller labels, connector chooses), ADR-017 (relay/connectivity changes notify the **transport** plane, never the ACL), ADR-020 (single-use provisioning token), ADR-027 (serial CRLs) stay intact. |

## Implementation choices made in this plan (not architectural — reviewable)

| Area | Choice | Why |
|------|--------|-----|
| Relay liveness (A) | Expiry sweep consults the existing Valkey key `relay:heartbeat:last:<id>` before evicting; eviction clears the relay's Valkey throttle markers so the next heartbeat re-activates it. | Keeps the Sprint 10.3 DB-write throttle; Valkey already holds true liveness. |
| Controller cert (E) | Rotate at 2/3 of `CONNECTOR_CERT_TTL` via `tls.Config.GetCertificate`. | No new config; existing streams unaffected (cert only used at handshake). |
| Relay renewal (F) | Relay renews when ≤ 2/5 of cert lifetime remains (same ratio as the client's `RENEWAL_WINDOW_SECS`, `client/src/daemon.rs:1629`), with jitter; hot-swaps the QUIC server cert via a rustls cert resolver. | Mirrors client renewal scheduling (ADR-028); no listener restart. |
| Connector renewal (G) | ReEnroll sent from the health-report handler, throttled per stream; connector hot-swaps the renewed cert into every TLS consumer. | Health report already arrives every 15s; restart-based reload would drop live tunnels. |

## Team Assignments

| Member | Person | Role | Area |
|--------|--------|------|------|
| **M1** | Sathiya | Go (controller) | Connector/shield lifecycle guards, disconnect watcher → transport plane, controller gRPC cert rotation |
| **M2** | Barath | Go + Rust | Relay liveness, relay SAN allowlist, relay in-band renewal (proto + Go + Rust), connector renewal trigger + connector cert hot-swap (Go + Rust) |

## Critical Rule: Conflict Zones

| File | Who | Rule |
|------|-----|------|
| `controller/cmd/server/main.go` | M1 (D, E) + M2 (A) | Surgical edits only. **M2-A lands first** (RunExpiryLoop wiring), then M1-D (watcher wiring), then M1-E (gRPC TLS config). Rebase before each. |
| `controller/internal/connector/control_stream.go` | M1 (B) + M2 (G) | **M1-B lands first.** M2-G rebases onto it and adds only the ReEnroll send. |
| `controller/internal/connector/enrollment.go` (`RenewCert`) | M1 (B) | M1 owns the revocation guard. M2-G must not edit `RenewCert`. |
| `controller/internal/connector/goodbye.go`, `disconnect_watcher.go` | M1 | M1 owns. |
| `controller/internal/shield/heartbeat.go` | M1 (B) | M1 owns the revoked-shield guard. Leave `markDisconnected`'s active-workspace predicate unchanged. |
| `controller/internal/relay/{expiry,heartbeat,store,provision}.go` | M2 | M2 only. Order inside M2: A → C → F. |
| `proto/relay/v1/relay.proto` | M2 (C comments, F additive RPC) | **Never renumber or reuse fields.** Additive only. Run `buf generate` from repo root. |
| `controller/internal/pki/` | M1 (E) + M2 (F) | M1 adds controller-cert rotation helpers; M2 reuses `SignRelayCert` unchanged. No overlap expected — announce any pki edit. |
| `relay/src/**` | M2 | M2 only. |
| `connector/src/**` | M2 | M2 only. |
| `controller/migrations/` | nobody | **No migrations in this sprint.** No schema change is needed. |

## Dependency Graph

```text
M2-A  Relay liveness truth                    (Day 1, independent)
  ↓
M2-C  Relay SAN allowlist enforcement         (after A — same package, M2 sequential)
  ↓
M2-F  Relay in-band RenewCert (D-19)           (needs C: renewal binds SANs to the stored allowlist)

M1-B  Connector + shield revocation stickiness (Day 1, independent)
  ↓
M1-D  Disconnect watcher → transport plane     (after B — connector package, M1 sequential)
  ↓
M1-E  Controller gRPC cert rotation            (independent of B/D; sequenced last for M1 to keep main.go edits ordered)

M2-G  Connector renewal trigger + cert hot-swap (needs M1-B merged: control_stream.go + RenewCert guard)

All phases → Acceptance gate (Acceptance-Test-Plan.md)
```

> Day-1 parallelism: **M2-A** and **M1-B** have no dependencies and start immediately.
> **M2-G** is M2's last phase and starts only after M1-B is merged.

## Execution Path

### Phase A — M2: Relay Liveness Truth

> See [[Sprint20/Member2-Go-Rust/Phase1-Relay-Liveness]]. Depends on nothing — Day 1.

- [x] **M2-A1** `internal/relay/expiry.go` — liveness-aware eviction: DB-stale candidates are checked against Valkey `relay:heartbeat:last:<id>` before eviction.
- [x] **M2-A2** `internal/relay/store.go` — split eviction into select-candidates, refresh-from-liveness and guarded evict (`status='active' AND last_heartbeat_at < threshold`).
- [x] **M2-A3** `internal/relay/heartbeat.go` — expose a liveness reader and a "clear throttle markers" helper on `Service`.
- [x] **M2-A4** On eviction, delete `relay:heartbeat:db-write:<id>` and `relay:heartbeat:metadata:<id>` so the next heartbeat writes the DB and re-activates.
- [x] **M2-A5** `cmd/server/main.go` — pass the liveness source to `RunExpiryLoop`.
- [x] **M2-A6** Tests: `expiry_test.go`, `heartbeat_test.go`, `store_evict_integration_test.go` (race; ran against Postgres, not skipped).
- [x] **Build gate:** `cd controller && go build ./... && go test ./internal/relay/...`
- [ ] **Live acceptance — PENDING / NOT YET RUN:** relay stays `active` ≥ 10 min with unchanged metadata; stopped relay → `inactive` ≤ 150 s; restarted relay → `active` on first heartbeat (AT-CORE-2, AT-A.2, AT-A.3). Needs a running relay.

### Phase B — M1: Connector + Shield Revocation Stickiness

> See [[Sprint20/Member1-Go/Phase1-Connector-Shield-Revocation-Stickiness]]. Depends on nothing — Day 1.

- [x] **M1-B1** `control_stream.go` `Control` — reject when `status='revoked'` **or** `revoked_at IS NOT NULL`; guarded activation before registry add.
- [x] **M1-B2** `control_stream.go` stream-close defer — only `active → disconnected`.
- [x] **M1-B3** `goodbye.go` — only `active → disconnected`; notify both planes on a real transition.
- [x] **M1-B4** `enrollment.go` `RenewCert` — reject on `revoked_at`; guarded cert UPDATE; zero rows ⇒ deny, no cert returned.
- [x] **M1-B5** `control_stream.go` `handleConnectorHealth` — add `revoked_at IS NULL` to the guard.
- [x] **M1-B6** `shield/heartbeat.go` `UpdateShieldHealth` — never overwrite a `revoked` shield.
- [x] **M1-B7** Tests: `revocation_test.go` (Control/Goodbye/RenewCert/health, DB) + `shield/heartbeat_test.go` revoked-shield + regression.
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/connector/... ./internal/shield/...`

### Phase C — M2: Relay SAN Allowlist Enforcement

> See [[Sprint20/Member2-Go-Rust/Phase2-Relay-SAN-Allowlist]]. Depends on Phase A (M2 sequencing).

- [x] **M2-C1** `provision.go` — load the relay row **before** burning the token; require `pending`.
- [x] **M2-C2** `provision.go` — allowed SANs = stored `dns_allowlist` / `ip_allowlist`; request `dns_sans`/`ip_sans` must be ⊆ stored.
- [x] **M2-C3** Every SAN / status rejection happens **before** `BurnProvisioningJTI` (incl. a pre-check of every `SignRelayCert` CSR check: signature, SPIFFE URI, P-384, SANs).
- [x] **M2-C4** `proto/relay/v1/relay.proto` — fix stale comments only (no field changes; generated diff comment-only).
- [x] **M2-C5** `scripts/relay-local-install*.sh`, `scripts/run-relay-local.sh` — document that `RELAY_DNS_SANS`/`RELAY_IP_SANS` must match the allowlists registered at `POST /provider/relays`.
- [x] **M2-C6** Tests: `provision_test.go` (updated), `provision_allowlist_test.go` (allowlist + canonicalization).
- [x] **Build gate:** `buf generate && cd controller && go build ./... && go test ./internal/relay/... ./internal/pki/...` (0 skips with DB env; also `go test ./...` and relay `cargo build`, after rebase onto Phase B)
- [ ] **Live acceptance — PENDING / NOT YET RUN:** relay created with `ip_allowlist: []` + `RELAY_IP_SANS=127.0.0.1` is rejected, token still usable; retry without the IP SAN succeeds with the same token (AT-C.2, AT-C.5). Needs a running controller + provider token.

### Phase D — M1: Disconnect Watcher → Transport Plane

> See [[Sprint20/Member1-Go/Phase2-Disconnect-Watcher-Transport-Notify]]. Depends on Phase B.

- [x] **M1-D1** `disconnect_watcher.go` `markDisconnected` — return affected connector IDs grouped by workspace.
- [x] **M1-D2** `RunDisconnectWatcher` — accept a topology notifier; call `NotifyTopologyChange(ws, connectorIDs)` alongside `NotifyPolicyChange`.
- [x] **M1-D3** `cmd/server/main.go` — pass `transportNotifier`.
- [x] **M1-D4** Tests: disconnect watcher (DB).
- [x] **Build gate:** `cd controller && go build ./... && go test ./internal/connector/...`

### Phase E — M1: Controller gRPC Certificate Rotation

> See [[Sprint20/Member1-Go/Phase3-Controller-gRPC-Cert-Rotation]]. Independent; sequenced after D.

- [ ] **M1-E1** Rotating certificate holder with `GetCertificate`.
- [ ] **M1-E2** Background rotation at 2/3 TTL with retry; keep serving the current cert on failure.
- [ ] **M1-E3** `cmd/server/main.go` — `tls.Config{GetCertificate: …}`; start rotation loop under the existing `wg`/`ctx`.
- [ ] **M1-E4** Tests: holder unit tests (short TTL, failure path), TLS handshake test.
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/pki/... ./cmd/server/...`

### Phase F — M2: Relay In-Band Certificate Renewal (D-19)

> See [[Sprint20/Member2-Go-Rust/Phase3-Relay-Cert-Renewal]]. Depends on Phase C.

**F-1 (controller) — done. F-2 (relay Rust runtime) — next.**

- [x] **M2-F1** `proto/relay/v1/relay.proto` — additive `rpc RenewCert(RenewCertRequest) returns (RenewCertResponse)` + new messages; `buf generate`.
- [x] **M2-F2** `internal/relay/renew.go` (new) — identity checks, row status, presented-serial check, same-key CSR, stored-allowlist SANs, sign. Plus idempotent retries (Valkey result cache + per-attempt lock).
- [x] **M2-F3** `internal/relay/store.go` — transactional `RecordRenewedCert` (lock relay row `FOR UPDATE`, insert `relay_certificates`, update `relays.cert_*`, guarded on status; supersedes an earlier unused successor on retry).
- [ ] **M2-F4** `relay/src/renewal.rs` (new) — scheduler, CSR from existing key, atomic cert write. *(F-2)*
- [ ] **M2-F5** `relay/src/tls.rs` + `listener.rs` — swappable server cert resolver; `heartbeat.rs` — rebuild channel with renewed identity. *(F-2)*
- [x] **M2-F6 (Go)** Tests: handler (`renew_test.go`) + store integration incl. renew-vs-revoke race, 25 iterations × 3 runs, both lock orderings observed (`store_renew_integration_test.go`).
- [ ] **M2-F6 (Rust)** Tests: scheduler, atomic write, resolver swap. *(F-2)*
- [x] **F-1 build gate:** `buf generate && cd controller && go build ./... && go test ./internal/relay/... ./internal/pki/...` (0 skips with DB env); also `go test ./...` and relay `cargo build` with the additive proto.
- [ ] **Full build gate:** `… && cd ../relay && cargo build && cargo test` with the F-2 runtime. *(F-2)*
- [ ] **Live acceptance — NOT YET RUN** (`RELAY_CERT_TTL=15m` self-renewal without restart; revoke → both serials on `/relay.crl`). Needs F-2.

> **F-1 → F-2 contract:** the relay must not keep presenting its old certificate after successfully persisting the renewed one. The controller supersedes an unrevoked successor only while the relay still presents the certificate that successor replaced (such a successor was never put into use). See the phase file.

### Phase G — M2: Connector Renewal Trigger + Cert Hot-Swap

> See [[Sprint20/Member2-Go-Rust/Phase4-Connector-Cert-Renewal]]. Depends on M1 Phase B (merged).

- [ ] **M2-G1** `control_stream.go` `handleConnectorHealth` — send `ReEnroll` when `cert_not_after < now + Cfg.RenewalWindow`; throttle per stream.
- [ ] **M2-G2** `connector/src` — shared reloadable certificate holder; renewal publishes the new cert.
- [ ] **M2-G3** `connector/src` — every TLS consumer (device-tunnel listener, shield-proxy controller channel, relay client) uses the renewed cert.
- [ ] **M2-G4** Tests: Go ReEnroll trigger/throttle; Rust holder swap + consumers.
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/connector/... && cd ../connector && cargo build && cargo test`

## Final Build Gates

```bash
buf generate                                                       # from repo root (relay.proto changed)
cd controller && go build ./... && go vet ./... && go test ./...
cd connector && cargo build && cargo test
cd relay && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml                      # unchanged; must still build
cd client && cargo build                                           # unchanged; must still build
```

DB-backed Go tests need the CI env vars (`ENROLLMENT_TEST_DATABASE_URL`, `SHIELD_TEST_DATABASE_URL`,
`PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL`; see `.github/workflows/ci.yml`). A DB test that
**skips** does not count toward acceptance.

## Acceptance Criteria (sprint level)

- [ ] A relay heartbeating normally with unchanged metadata stays `active` across ≥ 2 DB-write intervals (≥ 10 min). *(Phase A code + tests done; live check PENDING / NOT YET RUN.)*
- [ ] A relay that stops heartbeating becomes `inactive` within expiry + sweep interval (≤ 150 s) and returns to `active` on its first heartbeat after recovery. *(Phase A code + tests done; live check PENDING / NOT YET RUN.)*
- [ ] A revoked connector remains `revoked` after its stream closes, after reconnect attempts, after `Goodbye`, and after `RenewCert` attempts.
- [ ] A revoked shield remains `revoked` while its connector keeps sending shield status batches.
- [ ] A relay cannot obtain a DNS/IP SAN outside its operator-registered allowlist; the rejection does not consume the provisioning token. *(Phase C code + tests done; live check PENDING / NOT YET RUN.)*
- [ ] A connector marked `disconnected` by the watcher disappears from the next `GetTransportSnapshot` without any other event.
- [ ] The controller keeps accepting new gRPC handshakes past the original cert's `NotAfter`.
- [ ] A relay renews its own cert in-band (same key); the new serial is in `relay_certificates`; revoking the relay revokes **every** serial, including one issued concurrently with the revoke. *(Controller side proved by F-1 tests; relay runtime (F-2) + live check pending.)*
- [ ] A connector renews automatically inside `CONNECTOR_RENEWAL_WINDOW` and keeps serving device tunnels, relay sessions and shield renewals after the **original** cert's `NotAfter`.
- [ ] All scenarios in [[Sprint20/Acceptance-Test-Plan]] pass.

## Out of Scope (tracked, not in the Decision Record prerequisites)

Found during discovery/planning; **do not fix in this sprint** without a separate decision or ticket:

- `ConnectorRegistry.remove` deletes by key, so a fast reconnect can drop the new stream and briefly mark a live connector `disconnected` (self-heals on next health report).
- Placement flap during relay migration (`DeletePlacement` on a late `disconnected` event after `switched`).
- In-memory policy/transport version counters reset on controller restart.
- Connector/client renewal overwrites `cert_serial`; no per-cert history, so earlier still-valid certs are not on the CRL.
- Shield cryptographic revocation (no CRL entry) and `RevokeShield` sending no notification.
- SPIFFE interceptor chain verification for roles other than connector/relay (ADR-015-SPIFFE).
- ADR-011 / ADR-012 token hardening (old-JTI invalidation, `aud`, leeway) for connector/shield tokens.
- Relays consuming `/relay.crl` for their own revocation; the relay learning of its own revocation.
- Relay capacity-label freshness and wiring `RELAY_LABEL_HOLDDOWN_SECS`.
- Provider read APIs (`GET /provider/relays`, `/provider/audit`, …) belong to **Dashboard Phase 2**.
- Suspension/deletion enforcement (D-07 … D-13) belongs to a later sprint.
- Migration runner / schema versioning.

## Notes for AI Agents Working on This Sprint

1. The team is **two people**: **M1 = Sathiya**, **M2 = Barath**. Ask which one you are working with; there is no M3/M4 in this sprint.
2. Read this `path.md`, then the first unchecked phase whose `depends_on` are satisfied, then that phase's **Post-Phase Fixes**.
3. Treat `docs/provider-dashboard-architecture-decisions.md` → *Decision record — 2026-09-23* as binding. If a phase seems to require a new architectural choice, **stop and ask**; don't decide it in code.
4. **Never renumber proto fields.** The only proto change is additive (`RelayService.RenewCert`) plus comment fixes.
5. No migrations. If a phase seems to need one, stop and ask.
6. On completion: tick the checkboxes here and in the phase file, record any bug fixes in the phase file's **Post-Phase Fixes** and in this file's **Post-Sprint Fixes**, and append a Session Log entry in `.zecurity-obs/Planning/Session Log.md`.

## Post-Sprint Fixes

- **Phase A — `ClearHeartbeatThrottle` multi-key DEL panic** (caught by `TestHeartbeat_FirstHeartbeatAfterEvictionPersists` before commit). valkey-go panics on a multi-key `DEL` whose keys hash to different slots, so the markers are now deleted one key at a time. Details: [[Sprint20/Member2-Go-Rust/Phase1-Relay-Liveness]] → Post-Phase Fixes.
