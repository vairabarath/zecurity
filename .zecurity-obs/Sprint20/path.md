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
- [x] **Live acceptance — PASS (run sheet A1–A5):** relay stays `active` ≥ 10 min with unchanged metadata; stopped relay → `inactive` ≤ 150 s; restarted relay → `active` on first heartbeat (AT-CORE-2, AT-A.2, AT-A.3). A3–A5 were run on relay2 `c655ced6` (relay1 retired, Finding 17).

### Phase B — M1: Connector + Shield Revocation Stickiness

> See [[Sprint20/Member1-Go/Phase1-Connector-Shield-Revocation-Stickiness]]. Depends on nothing — Day 1.

- [x] **M1-B1** `control_stream.go` `Control` — reject when `status='revoked'` **or** `revoked_at IS NOT NULL`; guarded activation before registry add.
- [x] **M1-B2** `control_stream.go` stream-close defer — only `active → disconnected`.
- [x] **M1-B3** `goodbye.go` — only `active → disconnected`; notify both planes on a real transition.
- [x] **M1-B4** `enrollment.go` `RenewCert` — reject on `revoked_at`; guarded cert UPDATE; zero rows ⇒ deny, no cert returned.
- [x] **M1-B5** `control_stream.go` `handleConnectorHealth` — add `revoked_at IS NULL` to the guard.
- [x] **M1-B6** `shield/heartbeat.go` `UpdateShieldHealth` — never overwrite a `revoked` shield.
- [x] **M1-B7** Tests: `revocation_test.go` (Control/Goodbye/RenewCert/health, DB) + `shield/heartbeat_test.go` revoked-shield + regression.
- [x] **Build gate:** `cd controller && go build ./... && go test ./internal/connector/... ./internal/shield/...` (run sheet R2, 2026-09-28: build and vet OK; every package except `graph/resolvers` ok, so the connector and shield packages passed. R2 as a whole FAILED on `graph/resolvers`, Finding 35.)

### Phase C — M2: Relay SAN Allowlist Enforcement

> See [[Sprint20/Member2-Go-Rust/Phase2-Relay-SAN-Allowlist]]. Depends on Phase A (M2 sequencing).

- [x] **M2-C1** `provision.go` — load the relay row **before** burning the token; require `pending`.
- [x] **M2-C2** `provision.go` — allowed SANs = stored `dns_allowlist` / `ip_allowlist`; request `dns_sans`/`ip_sans` must be ⊆ stored.
- [x] **M2-C3** Every SAN / status rejection happens **before** `BurnProvisioningJTI` (incl. a pre-check of every `SignRelayCert` CSR check: signature, SPIFFE URI, P-384, SANs).
- [x] **M2-C4** `proto/relay/v1/relay.proto` — fix stale comments only (no field changes; generated diff comment-only).
- [x] **M2-C5** `scripts/relay-local-install*.sh`, `scripts/run-relay-local.sh` — document that `RELAY_DNS_SANS`/`RELAY_IP_SANS` must match the allowlists registered at `POST /provider/relays`.
- [x] **M2-C6** Tests: `provision_test.go` (updated), `provision_allowlist_test.go` (allowlist + canonicalization).
- [x] **Build gate:** `buf generate && cd controller && go build ./... && go test ./internal/relay/... ./internal/pki/...` (0 skips with DB env; also `go test ./...` and relay `cargo build`, after rebase onto Phase B)
- [x] **Live acceptance — PASS (run sheet C1–C3):** relay created with `ip_allowlist: []` + `RELAY_IP_SANS=127.0.0.1` is rejected, token still usable; retry without the IP SAN succeeds with the same token (AT-C.2, AT-C.5).

### Phase D — M1: Disconnect Watcher → Transport Plane

> See [[Sprint20/Member1-Go/Phase2-Disconnect-Watcher-Transport-Notify]]. Depends on Phase B.

- [x] **M1-D1** `disconnect_watcher.go` `markDisconnected` — return affected connector IDs grouped by workspace.
- [x] **M1-D2** `RunDisconnectWatcher` — accept a topology notifier; call `NotifyTopologyChange(ws, connectorIDs)` alongside `NotifyPolicyChange`.
- [x] **M1-D3** `cmd/server/main.go` — pass `transportNotifier`.
- [x] **M1-D4** Tests: disconnect watcher (DB).
- [x] **Build gate:** `cd controller && go build ./... && go test ./internal/connector/...`

### Phase E — M1: Controller gRPC Certificate Rotation

> See [[Sprint20/Member1-Go/Phase3-Controller-gRPC-Cert-Rotation]]. Independent; sequenced after D.

- [x] **M1-E1** Rotating certificate holder with `GetCertificate`.
- [x] **M1-E2** Background rotation at 2/3 TTL with retry; keep serving the current cert on failure.
- [x] **M1-E3** `cmd/server/main.go` — `tls.Config{GetCertificate: …}`; start rotation loop under the existing `wg`/`ctx`.
- [x] **M1-E4** Tests: holder unit tests (short TTL, failure path), TLS handshake test.
- [x] **Build gate:** `cd controller && go build ./... && go test ./internal/pki/... ./cmd/server/...` (+ `-race`)

### Phase F — M2: Relay In-Band Certificate Renewal (D-19)

> See [[Sprint20/Member2-Go-Rust/Phase3-Relay-Cert-Renewal]]. Depends on Phase C.

**F-1 (controller) — done. F-2 (relay Rust runtime) — done. Live acceptance pending.**

- [x] **M2-F1** `proto/relay/v1/relay.proto` — additive `rpc RenewCert(RenewCertRequest) returns (RenewCertResponse)` + new messages; `buf generate`.
- [x] **M2-F2** `internal/relay/renew.go` (new) — identity checks, row status, presented-serial check, same-key CSR, stored-allowlist SANs, sign. Plus idempotent retries (Valkey result cache + per-attempt lock).
- [x] **M2-F3** `internal/relay/store.go` — transactional `RecordRenewedCert` (lock relay row `FOR UPDATE`, insert `relay_certificates`, update `relays.cert_*`, guarded on status; supersedes an earlier unused successor on retry).
- [x] **M2-F4** `relay/src/renewal.rs` (new) — scheduler (2/5 remaining, jitter, backoff, `Aborted` retry, single instance), CSR from existing key, reply verification; `relay/src/cert_manager.rs` (new) — single cert owner, atomic persist before publish.
- [x] **M2-F5** `listener.rs` — live swap via `quinn::Endpoint::set_server_config()` on publish; `heartbeat.rs` — reconnects immediately with the renewed identity.
- [x] **M2-F6 (Go)** Tests: handler (`renew_test.go`) + store integration incl. renew-vs-revoke race, 25 iterations × 3 runs, both lock orderings observed (`store_renew_integration_test.go`).
- [x] **M2-F6 (Rust)** Tests: scheduler timing/jitter/backoff, existing-key CSR, expired never calls RenewCert, atomic write + crash-before-rename, bad-reply rejection, restart after crash between rename and swap, heartbeat reconnect with new cert, real-QUIC listener swap without dropping existing connections (65 relay tests pass).
- [x] **F-1 build gate:** `buf generate && cd controller && go build ./... && go test ./internal/relay/... ./internal/pki/...` (0 skips with DB env); also `go test ./...` and relay `cargo build` with the additive proto.
- [x] **Full build gate:** `cd relay && cargo build && cargo test` (65 passed); controller regression `go build ./... && go test ./...` pass; clippy clean for F-2 code.
- [x] **Live acceptance — PASS at `RELAY_CERT_TTL=1h` (run sheet F1–F4, AT-F.8):** relay renewed itself 16× (every 420–1060 s, same key) and kept serving 2 h 30 m past the first cert's `NotAfter` without restart. *(This box originally said `15m` / ~9 min. That target was invalid at this commit because of KI-1, so AT-F.8 and the runbook use 1h. The 15m case is Sprint 21 AT-K.1, not yet run live.)*
- [x] **Operational CRL verification — PASS (run sheet F5–F7):** revoke → 204, zero unrevoked `relay_certificates` rows; `/relay.crl` listed exactly the 4 unexpired serials; the connector dropped the relay in the same second. Run on relay2 `c655ced6`. Follow-up: the revoked relay keeps retrying every 5 s (Finding 32).

> **F-1 → F-2 contract:** save before publish; after saving, the relay switches every consumer and must not keep presenting its old certificate. The controller supersedes an unrevoked successor only while the relay still presents the certificate that successor replaced (such a successor was never put into use). See the phase file.

### Phase G — M2: Connector Renewal Trigger + Cert Hot-Swap

> See [[Sprint20/Member2-Go-Rust/Phase4-Connector-Cert-Renewal]]. Depends on M1 Phase B (merged).

**G-1 (controller trigger) — done. G-2a (cert holder + consumers) — done. G-2b (`:9091` shield server) — done. Remaining: live acceptance (needs Phase E).**

- [x] **M2-G1** `control_stream.go` `handleConnectorHealth` — send `ReEnroll` when `cert_not_after < now + Cfg.RenewalWindow`; throttle per stream (10 min, recorded only on a successful enqueue). Runs only after the revocation-guarded health UPDATE succeeds.
- [x] **M2-G2** `connector/src/tls/cert_holder.rs` (new) — single cert owner (`watch` channel); `install_renewed` = verify → atomic persist (temp → fsync → rename → fsync dir) → publish; key never rewritten. `renewal.rs` — single-flight + 60 s debounce, CSR from existing key.
- [x] **M2-G3 (G-2a)** Consumers switched: control stream (holder), device TLS `:9092` (holder resolver), device QUIC `:9092` (`set_server_config`), Relay inner TLS (holder resolver), Relay dials/probes (identity at dial time), Shield-proxy controller channel (rebuilt on publish).
- [x] **M2-G3 (G-2b)** Shield-facing server `:9091` — own TLS accept (`build_shield_server_tls`: holder resolver + workspace-CA client verifier, client cert required) + `serve_with_incoming` on the raw `tokio_rustls` stream (keeps `peer_certs()`); one handshake task per connection with a 10 s timeout; the accept loop never awaits a handshake or the hand-off queue. Live shield streams survive a renewal; the server is never restarted.
- [x] **M2-G4 (Go)** Tests: `reenroll_test.go`. Unit: `renewalDue`, exactly-one inside window, throttle, per-stream, outside window / NULL / disabled, full mailbox retries. Through `handleConnectorHealth` with a throwaway DB: inside window → one then throttled; outside / NULL → none; revoked (status or `revoked_at`) → none.
- [x] **M2-G4 (Rust, G-2a)** Tests: holder notify/verify/persist/single-flight, atomic write + crash-before-rename, device TLS new serial, real-QUIC swap keeps existing connections, Relay inner TLS new cert, Relay dials new identity, Shield-proxy rebuild, rapid ReEnroll → one renewal, all six consumers see the same serial (109 lib + 4 integration pass).
- [x] **M2-G4 (Rust, G-2b)** Tests (real TLS over localhost through `serve_on`): pre-renewal Control stream keeps round-tripping, new handshakes see the renewed serial (twice), `verify_shield_identity` accepts the matching shield / `PermissionDenied` on mismatch / no-cert and untrusted-CA refused, stalled clients do not block shields, stalled handshake closed after timeout, full hand-off queue does not stop accepts (115 lib + 4 integration pass).
- [x] **G-1 build gate:** `cd controller && go build ./... && go test ./internal/connector/... ./...` (connector DB tests run; the only skip is the pre-existing unconditional `TestEnroll_CSRSignatureInvalid`).
- [x] **G-2a build gate:** `cd connector && cargo build && cargo test` (109 + 4, stable over 5 runs); controller `go build ./... && go test ./...`; clippy clean for new G-2a code.
- [x] **Full build gate** with G-2b: `cd connector && cargo build && cargo test` (115 + 4, stable over 5 runs; 0 build warnings; clippy clean for touched files); `cargo build --manifest-path shield/Cargo.toml` (unchanged, builds); controller `go build ./... && go test ./...` (connector DB tests run on throwaway DBs; only skip is the pre-existing unconditional `TestEnroll_CSRSignatureInvalid`).
- [x] **Live acceptance — ACCEPTED 2026-10-20 (user decision) on the `a25bb33` re-test; the Sprint 20 run itself was NOT PASSED** (`CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`). Run sheet G1–G3 and G5–G8 PASS (auto-renewal every 5 min; direct, relayed and shield paths work after the original `NotAfter` with no restart). **G9 / AT-G.6 FAIL:** the client drops long-lived tunnels on each transport change (Finding 15). **G4 / AT-G.1 not exercised** live (unit-tested). Later evidence on newer code: the 2026-10-10 long hold PASS ([[Sprint20/Fixes/fix 1/U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]); that doesn't retro-pass this commit. **Re-test 2026-10-20 on `origin/fixed-pendings` `a25bb33`: PASS.** Connector `k2-conn` (inkyank 192.168.1.75) at `CONNECTOR_CERT_TTL=15m` / `CONNECTOR_RENEWAL_WINDOW=10m`; shield-protected resource `g9-nika-web` (nika 192.168.1.39 tcp/5173, HTTP/1.1 keep-alive); client `69eb3d83e8f3` on 192.168.1.164 with a fresh login. `fc_hold.py`: one TCP socket (local port 45552), GET every 2 s, 06:55:41–07:25:42 UTC: **895 requests, 0 failures, 0 socket changes**, through **6 connector renewals** (06:57:22, 07:02:20, 07:07:19, 07:12:22, 07:17:20, 07:22:18) and past the 07:07:18 `NotAfter` of the cert in use at the start. Controller: 6 `ReEnroll sent`, 6 `superseded stream closed`, **0 connector disconnects**, 0 ACL pushes. Connector: 6× `opening replacement Control stream` → 6× `switched to renewed Control stream`, 0 replacement failures, 6× device QUIC switched, **1 `tunnel_opened`** (the hold), 0 errors, 0 restarts. Client: **0 `restarting VPN`** (Finding 15's failure mode), 0 handshake timeouts, 0 WARN/ERROR. Setup note: a first hold attempt stopped at request 1 because the test server (Python `http.server`) defaulted to HTTP/1.0 and closed each connection; restarted with `--protocol HTTP/1.1`. That was a harness issue, not a product result. **Ticked 2026-10-20 at the user's request:** G9 / AT-G.6 accepted on the `fixed-pendings` `a25bb33` re-test. G4 / AT-G.1 is still not exercised live (unit-tested).

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

## Live Acceptance

> **Run and closed out.** The live run took place on 2026-09-26/28 (at `5a4f940`, code-identical to `46ab87d`) and was closed out on 2026-10-17: 40 PASS, 1 PASS only under a TEMP patch (U2), 3 FAIL (U5 hold, G9, R2), 1 not exercised (G4). The boxes above are ticked only where the run sheet shows PASS. See [[Sprint20/Live-Acceptance-Run-Sheet]] → "Close-out — 2026-10-17".

All code phases (A–G) are merged. The two boxes still unticked (Phase G live, "all scenarios pass") are **failures, not pending checks**. Passing them needs Fix02 (F35) plus a re-run of R2 with `RESOURCE_TEST_SHIELD_ID`, and a decision on G9 / AT-G.6. For reference, the original instructions were:
- **Runbook:** `docs/sprint20-live-acceptance-runbook.md`, one ~95 min sitting that covers A, B, C, D, E, F and G.
- **Run sheet** to fill in: [[Sprint20/Live-Acceptance-Run-Sheet]]. Each row names the box below that it closes.
- **Operator:** Sathiya (M1).

Read the runbook's §2 first. KI-1 and KI-2 (below) change the env values.

## Known Issues (found preparing live acceptance)

> **KI-1, KI-2 and KI-3 are fixed in Sprint 21 Phase K** ([[Sprint21/Member2-Go-Rust/Phase3-Sprint20-Cleanup]], branch `sprint21/k-sprint20-cleanup`). The Sprint 20 live run (Sprint 21 Phase V) is pinned to `46ab87d`, which still has them, so the runbook §2 workarounds still apply to that run.

- **KI-1 — relay short-TTL renewal loop (Phase F-2).** The controller issues relay certs with `NotBefore = now − 1h` (`controller/internal/pki/relay.go:61`). The relay scheduler takes the lifetime as `not_after − not_before` (`relay/src/renewal.rs` `plan`), so renewal lands at `0.6·TTL − 24 min` after issue.
  - For `RELAY_CERT_TTL` ≤ 40 min the relay renews immediately and back to back. The default 30 d is unaffected (renewal at about 18 d).
  - Same class of bug as PF-1 (fixed only for the controller rotator).
  - The AT-F.8 "~9 min at 15m" expectation is wrong; the live run uses `RELAY_CERT_TTL=1h`.
  - Candidate fix: measure the lifetime from the relay's install time, as PF-1 does with `issuedAt`, or stop backdating.
  - **FIXED (Sprint 21 K1, `7fb0fb7`, `fd8b6d7`):** the relay records its install time in `relay.crt.meta` and schedules from `max(NotBefore, installed_at)`. A 15m cert now renews ~9 min after install, with no loop.
- **KI-2 — shield ReEnroll every ~15 s when `SHIELD_CERT_TTL` ≤ 48h (pre-existing).**
  - The connector's shield renewal window is a hard-coded 48 h (`connector/src/agent_server.rs` `DEFAULT_RENEWAL_WINDOW_SECS`). The controller's `SHIELD_RENEWAL_WINDOW` is parsed but unused, and the shield has no debounce.
  - Harmless at the 7 d default. Needs the window plumbed through, plus a shield-side debounce.
  - **FIXED (Sprint 21 K2, `2c3dda0`, `657fdd0`):** the controller sends `min(SHIELD_RENEWAL_WINDOW, 2/5 × SHIELD_CERT_TTL)` in `AgentPolicy` (proto field 18); the connector throttles `ReEnroll` to one per shield per stream per 10 min; the shield ignores `ReEnroll` for 60 s after a successful renewal.
- **KI-3 — dev `PROVIDER_GOOGLE_REDIRECT_URI` points at the tenant callback** (`/auth/callback`, in `controller/.env` and `.env.example`). Provider login needs `/provider/auth/callback`.
  - **RESOLVED BY REMOVAL (Sprint 21 H, confirmed in K3):** provider Google login and `PROVIDER_GOOGLE_REDIRECT_URI` no longer exist (D-24).
- Smaller items (runbook §2):
  - No success log for controller cert rotation, or for the `:9091` / `:9092` TLS swaps.
  - The client can't force TLS or the relay path.
  - Stale "every 5 minutes" comment in `connector/src/crl.rs:30`. **Fixed in Sprint 21 K5 (`8e55a36`).**

## Acceptance Criteria (sprint level)

*Live results: [[Sprint20/Live-Acceptance-Run-Sheet]] (2026-09-26/28 at `5a4f940`, code-identical to `46ab87d`; closed out 2026-10-17).*

- [x] A relay heartbeating normally with unchanged metadata stays `active` across ≥ 2 DB-write intervals (≥ 10 min). *(Live PASS: A1–A2.)*
- [x] A relay that stops heartbeating becomes `inactive` within expiry + sweep interval (≤ 150 s) and returns to `active` on its first heartbeat after recovery. *(Live PASS: A3–A5.)*
- [x] A revoked connector remains `revoked` after its stream closes, after reconnect attempts, after `Goodbye`, and after `RenewCert` attempts. *(Live PASS: B1–B2 for the DB state and refused reconnects; `Goodbye` and `RenewCert` covered by `revocation_test.go`, since the connector never sends `Goodbye`.)* ~~Open security gap (Finding 34)~~ **Closed 2026-10-23:** the connector is fail-closed since B2, and phases C–F make the client, relay and shield refuse a revoked connector even if it ignores its revocation. End-to-end live run PASS ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-E2E-Live-2026-10-23]]). Originally: a revoked connector kept serving existing tunnels until its process restarted ([[Sprint20/Handoff-Connector-Revocation-PhaseA-2026-10-15]]).
- [x] A revoked shield remains `revoked` while its connector keeps sending shield status batches. *(Live PASS: B3.)* **At this commit a revoked shield still carried traffic (Finding 33).** Fixed later in code (bug 3, PR #123); live end-to-end check still pending (`docs/certificate-audit-2026-10-15.md`).
- [x] A relay cannot obtain a DNS/IP SAN outside its operator-registered allowlist; the rejection does not consume the provisioning token. *(Live PASS: C1–C3.)*
- [x] A connector marked `disconnected` by the watcher disappears from the next `GetTransportSnapshot` without any other event. *(Live PASS: D1–D2, on try 3 with `kill -STOP`. The runbook's `kill -9` goes through the stream-close path instead of the watcher (Finding 30).)*
- [x] The controller keeps accepting new gRPC handshakes past the original cert's `NotAfter`. *(Live PASS: E1–E4; 6 rotations, 10-min cadence, 0 failures.)*
- [x] A relay renews its own cert in-band (same key); the new serial is in `relay_certificates`; revoking the relay revokes **every** serial, including one issued concurrently with the revoke. *(Live PASS: F1–F7. The concurrent-revoke race is covered by `store_renew_integration_test.go`.)*
- [x] A connector renews automatically inside `CONNECTOR_RENEWAL_WINDOW` and keeps serving device tunnels, relay sessions and shield renewals after the **original** cert's `NotAfter`. *(Live PASS: G1–G3, G5–G8. The shield-renewal part rests on evidence from before 17:16, while the shield TTL was 24h. Established tunnels during a swap are AT-G.6, which FAILED; see Phase G.)*
- [ ] All scenarios in [[Sprint20/Acceptance-Test-Plan]] pass. **Not met (2026-10-20):** R2 regression gate FAIL (Finding 35: 4 `graph/resolvers` tests, still failing on `fixed-pendings`, waiting on Fix02); G4 / AT-G.1 not exercised live (unit-tested). G9 / AT-G.6 was accepted on 2026-10-20 on the `a25bb33` re-test (see Phase G).

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
- **Established connection recovery / connector failover (deferred from Q3, 2026-10-10).** An established
  flow is pinned to the connector it opened on and is reset or times out when that connector fails.
  New-connection fallback within the same remote network does exist. **DEFERRED / NOT IMPLEMENTED**;
  tracked as [[PENDING-18-Established-Connection-Recovery-Connector-Failover]] (`.zecurity-obs/pending/`).

## Notes for AI Agents Working on This Sprint

1. The team is **two people**: **M1 = Sathiya**, **M2 = Barath**. Ask which one you are working with; there is no M3/M4 in this sprint.
2. Read this `path.md`, then the first unchecked phase whose `depends_on` are satisfied, then that phase's **Post-Phase Fixes**.
3. Treat `docs/provider-dashboard-architecture-decisions.md` → *Decision record — 2026-09-23* as binding. If a phase seems to require a new architectural choice, **stop and ask**; don't decide it in code.
4. **Never renumber proto fields.** The only proto change is additive (`RelayService.RenewCert`) plus comment fixes.
5. No migrations. If a phase seems to need one, stop and ask.
6. On completion: tick the checkboxes here and in the phase file, record any bug fixes in the phase file's **Post-Phase Fixes** and in this file's **Post-Sprint Fixes**, and append a Session Log entry in `.zecurity-obs/Planning/Session Log.md`.

## Post-Sprint Fixes

- **Phase G-2b — `:9091` TLS approach recorded** (the phase spec left the choice open). Own rustls acceptor with the CertHolder resolver + `serve_with_incoming`, instead of rebuilding tonic's `ServerTlsConfig` (which would restart the server and drop shield streams). The raw `tokio_rustls` stream must reach tonic unwrapped, or `peer_certs()` returns nothing and every shield is denied. Details: [[Sprint20/Member2-Go-Rust/Phase4-Connector-Cert-Renewal]] → Post-Phase Fixes.
- **Phase G-2a — renewal wrote `connector.crt` as leaf only** (pre-existing in `connector/src/renewal.rs`). Enrollment stores leaf + Workspace CA; renewal overwrote it with the bare leaf. `CertHolder::install_renewed` now writes the enrollment shape. Details: [[Sprint20/Member2-Go-Rust/Phase4-Connector-Cert-Renewal]] → Post-Phase Fixes.
- **Phase A — `ClearHeartbeatThrottle` multi-key DEL panic** (caught by `TestHeartbeat_FirstHeartbeatAfterEvictionPersists` before commit). valkey-go panics on a multi-key `DEL` whose keys hash to different slots, so the markers are now deleted one key at a time. Details: [[Sprint20/Member2-Go-Rust/Phase1-Relay-Liveness]] → Post-Phase Fixes.
- **Phase E — rotation threshold measured from the backdated `NotBefore` (PF-1).** `GenerateControllerServerTLS` backdates `NotBefore` by 1 h (`internal/pki/controller.go:39-41`), so a threshold computed from `NotBefore` made every cert due at issuance for any `CONNECTOR_CERT_TTL` ≤ 30m — the rotator reissued once per minute instead of once per 2/3 lifetime. Fix: `rotatedCert.issuedAt` records the rotator clock at store time; `rotationDue()` = `start + 2/3·(notAfter − start)` with `start = max(notBefore, issuedAt)`; `needsRotation` and `nextDue` both use it. Regression test `TestNeedsRotationBackdatedNotBefore` (10m/30m/2h/7d) failed on the old code and passes now. Details: [[Sprint20/Member1-Go/Phase3-Controller-gRPC-Cert-Rotation]] → Post-Phase Fixes.
- **Connector revocation — Phase A (controller–connector authorization protocol), `4cb5a6a`.** heartbeat_seq / AuthorizationAck / ConnectorRevoked / AgentPolicy.authorization_acks; structured `zecurity-connector-state` trailer marker (revoked/unknown); DB failure → Unavailable unmarked. Development intermediate: the connector is **not fail-closed** until Phase B1. **Continue from:** [[Sprint20/Handoff-Connector-Revocation-PhaseA-2026-10-15]] (next = Phase B1; O9 ACCEPTED 2026-10-20 as D12; B1 acceptance list [[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseB1-Acceptance-2026-10-20]] signed off 2026-10-20, B1-Q1 = (a); **B1 IMPLEMENTED 2026-10-21** `da5b12f`, unit-tested intermediate, not for the lab before B2: [[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseB1-Implementation-2026-10-21]]; Phase M done 2026-10-21 ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseM-Results-2026-10-21]]); B2 decisions D13–D16 accepted (P 4.5 s, backoff reset + 5 s cap, Disabled keeps connections / shield close 30 s, full report every P; build-time values in connector/.env); **B2 unit level IMPLEMENTED 2026-10-21** `63e9e90`/`4f00448` (233 lib tests, 15/15 mutants killed): [[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseB2-Implementation-2026-10-21]]; **B2 live acceptance L-1…L-8 PASS 2026-10-21**; O11/O12/O13 accepted as D19–D21; **Phase C (client) DONE 2026-10-22** `af30176`/`5c55bc9` ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseC-Implementation-2026-10-22]]); **Phase D (relay) DONE 2026-10-23** `c27d733`/`1005c12`/`8e09c18` ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseD-Implementation-2026-10-23]]); **Phase E (shield) DONE 2026-10-23** `c9d9cda` ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseE-Implementation-2026-10-23]]); each unit + mutation + live with a revocation-ignoring connector; Phase F: history/CRL/serials-after-delete already done by `52c16fa`; **Phase F fixes DONE 2026-10-23** `faa957f`/`4994638`: delete refused while shields are attached, and shields fail over from a revoked connector ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseF-Fixes-Implementation-2026-10-23]]); tombstone + identity-reuse prevention **DONE 2026-10-23** migration `041_connector_revocations.sql` `3995af6`/`b2ad199` **[schema: reset DB]** ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseF-Schema-041-2026-10-23]]); connector revocation phases A–F complete; nothing pushed). Record: [[Sprint20/Fixes/connector-revocation/Connector-Revocation-PhaseA-Implementation-2026-10-15]].
- **Fix 06 — admin and client sessions killed each other (B2 live finding 4), 2026-10-22.**
  - Cause: refresh sessions were one Valkey key per user (`refresh:<userID>`), shared by the admin UI and the client daemon, so every login or rotation of one invalidated the other (`refresh token mismatch`). Reproduced live in both directions.
  - Fix: one session per login. Token `<sid>.<secret>` at `refresh:{<userID>}:<sid>`, index `refresh_sids:{<userID>}`, access-JWT `sid` claim.
  - Behaviour: logout ends only its own session; user revocation ends all. Legacy tokens migrate on their next refresh.
  - ADR-006 amended. Details: [[Sprint20/Fixes/Fix06-Session-Refresh-Per-Login]].
- **Fix 07 — a disconnected connector never re-read the relay list (B2 live finding 3; second half of F4/F5/F22), 2026-10-22.**
  - Cause: `relay_selector::run` refreshed `current_list` only before the first list and inside `connected_step`, so a list pushed during `Disconnected`/`Backoff` was ignored until restart.
  - Fix: `wait_backoff` selects on `sleep` vs `relay_list_rx.changed()`; a pushed list ends the backoff and is dialed next. The backoff keeps growing (no tight loop).
  - Tests: 4 new paused-clock tests (2 fail on the old code). Live: empty list → relay pushed → dial at the same millisecond.
  - Details: [[Sprint20/Fixes/Fix07-Connector-Relay-List-While-Disconnected]].
- **Fix 08 — the shield re-attached up to ~40 s after the connector served again (B2 live finding 1, mis-attributed to client KI-5), 2026-10-22.**
  - Cause: the shield's `reconnect_loop` backed off 5 → 60 s on the `Disabled` connector's `Unavailable("connector is not authorized to serve")` refusals.
  - Fix: `connector_not_serving()` recognises those refusals (at open and mid-stream; `run_once` now keeps the `Status`). The shield retries every 2 s and resets the backoff; other errors keep the exponential backoff.
  - Tests: 3 new (all fail on the old code). Live: 37.3 s → 0.24 s.
  - Follow-up: the shield doesn't fail over away from a TLS-reachable but `Disabled` connector.
  - Details: [[Sprint20/Fixes/Fix08-Shield-Reattach-After-Connector-Disabled]].
- **Fix 09 — the client served only one hot-added resource IP; shared-port resources were misrouted (B2 live finding 5), 2026-10-22.**
  - Cause: smoltcp `IFACE_MAX_ADDR_COUNT=2` with one address per resource IP. Pending IPs went **direct over the LAN**, and startup silently dropped IPs beyond 2. Port-only listeners sent a connection to `a:80` to another resource on `:80`.
  - Fix: `configure_capture_iface` (only `100.64.0.1`, AnyIP, default route via it) plus listeners bound to `(ip, port)`. The planner capacity and address commands are removed; **H1 is superseded**.
  - Tests: 5 new (all fail on the old code); 252 client tests PASS.
  - Live (connector `s3-luffy` on .40, 4 SSH resources): hot-add of 3 IPs is now 1 pass, all tunnelled (baseline: all 3 pending, direct over the LAN); startup all tunnelled (baseline: 2 silently dropped).
  - Details: [[Sprint20/Fixes/Fix09-Client-AnyIP-Resource-Capacity]].
- **Fix 10 — the admin UI couldn't protect a resource created before its shield (B2 live finding 2), 2026-10-22; `admin/` change approved by the user.**
  - Cause: `ResourceDetail.tsx` gated Protect on the *bound* shield, while `CreateResource` binds only a shield already active and `MarkProtecting` picks any active shield on the host.
  - Fix: `lib/protectShield.ts` mirrors `MarkProtecting` over `GetShields` (+`lanIp`).
  - Tests: 8 new (the button tests fail on the old page); admin 114 PASS. Live: the old UI was disabled; the fixed UI bound the shield.
  - Details: [[Sprint20/Fixes/Fix10-Admin-Protect-Resource-Created-Before-Shield]].
- **Fix 07 addendum (2026-10-22):** a real relay attach was verified. A new relay `s3-relay-manoj` replaced `pm-relay-manoj`, whose cert expired (`RELAY_CERT_TTL=1h` in B2). Connector `s3-luffy` registered with it 40 ms after the list push, with no restart.
- **B2 findings full live retest on the final build `2f5f411` (2026-10-22): 6/6 PASS.**
  - Every component was rebuilt from HEAD and hash-compared; the connector was redeployed because its earlier binary wasn't provably HEAD.
  - Results: F4 sessions incl. client gRPC refresh; F1 shield re-attach 1.67 s; F6 dev flag off/on; F5 3-step hot-add with shared-port attribution; F2 admin UI binds the shield; F3 real relay stop/start, attach 36 ms after the push, no restart.
  - Details: [[Sprint20/Fixes/B2-Findings-Retest-2026-10-22]].
- **Live acceptance findings (2026-09-26/28), filed 2026-10-17.** Full text: [[Sprint20/Live-Acceptance-Run-Sheet]] → Findings 1–36. Medium and higher, with tracking doc and current status:
  - **F33 — High (security): a revoked shield still carried traffic.** **Fixed in code:** bug 3, PR #123 (`86a436d`, `b2b00b5`). Live end-to-end acceptance still pending. `docs/certificate-audit-2026-10-15.md`. Phase B → Post-Phase Fixes.
  - **F34 — High (security): a revoked connector keeps serving tunnels until its process restarts.** **CLOSED 2026-10-23** (was open on 2026-10-17). Connector revocation is at Phase A only (`4cb5a6a`), not fail-closed; Phase B1 not started. [[Sprint20/Handoff-Connector-Revocation-PhaseA-2026-10-15]]. Phase B → Post-Phase Fixes. *Update 2026-10-23:* the connector is fail-closed since B2 (live L-1…L-8 PASS 2026-10-21), and phases C/D/E make the client, relay and shield refuse a revoked connector that ignores its revocation (see the connector-revocation entry above). Phase F (fixes + migration 041) done 2026-10-23; end-to-end live run PASS. **Closed.**
  - **F28 — High: the client never passes the server's TCP close (FIN) to the app**, so browsers reuse dead connections. Tracked as Fix 05 Phase 5-A (`Fixes/fix 5/`, [[Sprint20/Handoff-Fix05-5A-2026-10-06]]).
  - **F15 — High (AT-G.6 FAIL): the client restarts the tunnel on every transport snapshot change**, dropping long-lived connections. Fix01 (`status: partial`); live long hold PASS on later code 2026-10-10 ([[Sprint20/Fixes/fix 1/U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]).
  - **F35 — Medium (R2 FAIL): 4 `graph/resolvers` ACL-coherence tests fail, hidden from CI** (they skip without `RESOURCE_TEST_SHIELD_ID`). **Still failing on `fixed-pendings` 2026-10-17** (run with a seeded shield). 3 are real double or extra `NotifyPolicyChange` calls (over-invalidation only); `TestProtectResource_DoesNotInvalidate` is outdated since migration 021. [[Sprint20/Fixes/Fix02-Resource-ACL-Double-Notify]] (`status: planned`).
  - **F36 — Medium (R2): the auth integration test always skipped** (`/15` in the Valkey URL). **Fixed** in `411e216` (Sprint 21 K6); passes on `fixed-pendings` 2026-10-17. [[Sprint20/Fixes/Fix03-Auth-Integration-Test-Skip-Valkey-URL]] (doc still says `planned`).
  - **F4 / F5 / F22 — Medium: relays on private IPs are never pushed to connectors**, and a disconnected connector's relay selector never re-reads a new relay list. (Selector half **fixed**: Fix 07, 2026-10-22.) U2 passed only under a TEMP patch. No fix recorded. Phase A → Post-Phase Fixes.
  - **F7 / F18 / F23 / F31 — High (test validity): install scripts enable update timers that replace branch binaries**, ignoring `AUTO_UPDATE_ENABLED=false`; the units can't be masked. No fix recorded. **Reproduced again 2026-10-23 on current code** with the product installers: sd-a replaced by release `983a4ac2` 5 s after install, sd-shield by release v1.0.11 (`0131b3a8`) 4 s after start, so a revocation-capable build was silently replaced by a release without it. Lab workaround `sd-pin.sh`; see [[Sprint20/Fixes/connector-revocation/Connector-Revocation-Systemd-Live-2026-10-23]] Finding A. **FIXED 2026-10-24 (Sprint 20 Fix 11, `905989a2`):** `--check-update` honours `AUTO_UPDATE_ENABLED`, timers have no `Requires=`, services no hard-coded env, installers enable the timer only when auto-update is true; 10/10 mutants; live systemd check PASS (no update runs, binaries unchanged, manual update run skipped). [[Sprint20/Fixes/Fix11-Update-Timer-Honours-Auto-Update]].
  - **F19 / F20 / F24 / F25 — Medium (admin):**
    - deleting a connector silently deletes its shield; **fixed 2026-10-23** (`faa957f`): `deleteConnector` is refused while an enrolled, non-revoked shield is attached (live AF-L and systemd run);
    - one orphaned protected resource blocks the whole ACL push;
    - a resource's host can't be edited;
    - deleting a resource silently deletes its access rules.

    No fix recorded.
  - **F30 — Medium (runbook): `kill -9` doesn't exercise the disconnect watcher**; use `kill -STOP`. Runbook not yet updated. Phase D → Post-Phase Fixes.
  - **F32 — Low: a revoked relay retries its heartbeat every 5 s forever.** No fix recorded. Phase F → Post-Phase Fixes.
- **Connector-revocation live findings (2026-10-23)**, from the end-to-end run ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-E2E-Live-2026-10-23]]) and the systemd / root-shield run ([[Sprint20/Fixes/connector-revocation/Connector-Revocation-Systemd-Live-2026-10-23]]). Revocation itself PASSED on every layer; these are separate issues found along the way. All **open** unless noted.
  - **F37 — High (client data plane): the client resets concurrent new connections to the same resource.**
    - Cause: one smoltcp listening socket per `(ip, port)` (`client/src/net_stack.rs` `new_listen_socket`), re-armed only on a later tick. SYNs arriving before that get RST and never reach the flow table.
    - Evidence: same resource, same minute, **24/24 sequential OK vs 5/24 OK in parallel bursts of 8**; the refused attempts have no `new TCP connection` line.
    - Impact: browsers open ~6 parallel connections per host. It also explains the "isolated single refusals" seen at probe start and around ACL hot-apply.
    - Suggested fix: keep a small pool (backlog) of listening sockets per key, topped up every poll.
  - **F38 — Low: a revoked (terminal) connector keeps retrying relay registration** with backoff (10 s, 20 s, …); the relay refuses each attempt. The control loop stops as designed, but the relay selector doesn't. Fix: stop the relay selector on entering a terminal state. (Similar to F32 for relays.)
  - **F39 — Low: the shield's shutdown goodbye uses the connector list loaded at startup** (`shield/src/main.rs:211`), so after a revocation/failover it contacts the pruned or stale head first. Non-fatal. Fix: give the shutdown path the live state.
  - **F40 — Low: a deleted revoked connector is told `UNKNOWN`, not `revoked`.** The controller doesn't consult the 041 tombstone. Both are terminal, and the client/relay/CRL still refuse it. Fix: classify a tombstoned id as revoked.
  - **F41 — Low (noise): shields log `ignored empty PeerConnectorList` every 15 s** while any revoked connector's certificate is valid, if the shield's network has no resources (connectors now send lists holding only revoked ids, by design). Fix: log at debug when the list carried revoked ids.
  - **F42 — Observation (unexplained): one shield failover took 12.9 s** (the connect to manojinkyank .49:9091 stalled ~11 s, nothing logged on either side). Not reproduced: the same failover to .49 took 2.35 s in the systemd run, and the restart re-attach 0.34 s. Watch for it.
  - **F43 — Low (client, from Fix 05-B's record, never tracked here):**
    - cooldown expiry re-dials a dead or frozen connector (2 × 2 s), and the backoff then delays reuse of a recovered one (5-B "Finding 3");
    - the `relay_pool.rs` close-on-age finding is unfixed.

    See [[Sprint20/Fixes/fix 5/Fix05-Stale-QUIC-Pool-Live-Acceptance-2026-10-06]] §15.
  - **Deployment notes (not defects):**
    - the Phase F shield-failover fix needs both the new connector and the new shield;
    - a connector must not run on a host whose client captures that connector's resources (the capture loops its own traffic);
    - test web servers for held-flow checks must use `--protocol HTTP/1.1`.
  - **Open design decisions (connector revocation):** O10 (recovery after revocation; today only a new identity), PENDING-18 (sessions are cut, not migrated), same-key connector renewal.
- **Regression gate on current code (2026-10-17, `1c164c7`):** `go build`, `go vet` OK; `go test ./...` 1161 PASS, 0 FAIL, **32 SKIP**. 30 of the skips need `RESOURCE_TEST_SHIELD_ID`, and with it set, 4 tests fail (F35). CI looks green only because they skip.
