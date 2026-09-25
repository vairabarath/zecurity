---
type: test-plan
sprint: 20
owner: M1 (Sathiya) + M2 (Barath)
status: planned
tags: [sprint20, tests, acceptance, provider-dashboard, backend-truth, relay, connector, pki]
---

# Sprint 20 — Acceptance Test Plan

> **Purpose.** Prove that the controller's stored state is true before any provider dashboard reads
> it. Each case is concrete and checkable after implementation. The sprint is **not done** until
> §1 passes and every phase table below is green.
>
> Go tests are table-driven `*_test.go`. DB-backed tests use the CI env vars
> (`ENROLLMENT_TEST_DATABASE_URL`, `SHIELD_TEST_DATABASE_URL`, `PKI_TEST_DATABASE_URL`,
> `AUTH_TEST_VALKEY_URL`). **A skipped DB test is a failed acceptance case.**

---

## 1. The core invariant — terminal and live states are never misreported

### AT-CORE-1 — Revocation is terminal

**Given** an active connector `C` with an open control stream, and an active shield `S` behind it.
**When** an admin revokes `C` and `S`, and `C` then reconnects repeatedly and keeps reporting `S`
as active.
**Then** after 5 minutes: `connectors.status='revoked'` (and `revoked_at` unchanged),
`shields.status='revoked'`, `C`'s `Control` and `RenewCert` calls are denied, and no ACL snapshot
or relay list is pushed to `C`.

### AT-CORE-2 — A live relay is never reported dead

**Given** a relay `R` heartbeating every 30 s with unchanged metadata.
**When** 12 minutes pass (≥ 2 × `RELAY_HEARTBEAT_DB_WRITE_INTERVAL`).
**Then** `relays.status` is `active` at every 30 s sample, and connectors never receive a
`LabelledRelayList` without `R`.

If either core case fails, the sprint has not achieved its goal, whatever else passes.

---

## 2. Phase A — Relay liveness (M2)

| ID | Case | Expected |
|----|------|----------|
| AT-A.1 | DB `last_heartbeat_at` stale, Valkey liveness fresh | not evicted; DB timestamp refreshed from Valkey; no topology notify; no relay-list broadcast |
| AT-A.2 | Relay process stopped | `inactive` within ≤ 150 s; topology notified for its connectors; one relay-list broadcast |
| AT-A.3 | Stopped relay restarted within 5 min | `active` on its first heartbeat (throttle markers were cleared at eviction) |
| AT-A.4 | Valkey unavailable during sweep | eviction falls back to DB; live relays still `active` (heartbeats force DB writes on cache error) |
| AT-A.5 | Heartbeat DB write lands between candidate select and evict | relay not evicted (guarded UPDATE) |
| AT-A.6 | `revoked` / `deleted` relay with stale heartbeat | untouched by sweep and by refresh |

## 3. Phase B — Connector + shield revocation stickiness (M1)

| ID | Case | Expected |
|----|------|----------|
| AT-B.1 | Revoke connector with open stream | stream closed on next health report; row stays `revoked` after the close defer |
| AT-B.2 | `Control` from `status='revoked'` | `PermissionDenied`; client not registered |
| AT-B.3 | `Control` from legacy row (`disconnected`, `revoked_at` set) | `PermissionDenied` |
| AT-B.4 | Revoke between `Control`'s read and activation | `PermissionDenied`; row `revoked`; no disconnect defer armed |
| AT-B.5 | `Goodbye` on `revoked` / on `active` | unchanged, no notify / `disconnected` + both planes notified |
| AT-B.6 | `RenewCert` on revoked (status or `revoked_at`) | `PermissionDenied` |
| AT-B.7 | `RenewCert` loses race with revoke | `PermissionDenied`; `cert_serial` unchanged; no cert returned |
| AT-B.8 | Revoked shield reported `active` with new `lan_ip` | shield stays `revoked`; `lan_ip` and resource hosts unchanged; no error |
| AT-B.9 | Non-revoked shield status batch | behaviour unchanged (regression) |

## 4. Phase C — Relay SAN allowlist (M2)

| ID | Case | Expected |
|----|------|----------|
| AT-C.1 | Request + CSR SANs ⊆ stored allowlist | provisioned; JTI burned |
| AT-C.2 | Request SAN not in allowlist | `PermissionDenied`; **JTI still in Valkey**; row `pending` |
| AT-C.3 | CSR SAN not in allowlist (request lists empty) | `PermissionDenied` before burn; JTI still in Valkey |
| AT-C.4 | Empty stored lists, SPIFFE-URI-only CSR | provisioned |
| AT-C.5 | Retry AT-C.2 with the SAN removed, same token | provisioned |
| AT-C.6 | Relay row not `pending` / unregistered | `FailedPrecondition`; JTI still in Valkey |
| AT-C.7 | `buf generate` diff | comment-only for `relay.proto` |

## 5. Phase D — Disconnect watcher → transport (M1)

| ID | Case | Expected |
|----|------|----------|
| AT-D.1 | Connector killed (no clean shutdown) | `disconnected` within ~90–120 s; next `GetTransportSnapshot` has a new version without it |
| AT-D.2 | Stale connectors across two workspaces | topology notified with exact connector IDs per workspace; policy notified once per workspace |
| AT-D.3 | Revoked connector / non-active workspace | untouched; no notification |
| AT-D.4 | Nil or failing topology notifier | policy notification still happens; no panic |

## 6. Phase E — Controller gRPC cert rotation (M1)

| ID | Case | Expected |
|----|------|----------|
| AT-E.1 | Rotation time reached | new cert (new serial, same SPIFFE ID + SANs) served on new handshakes |
| AT-E.2 | Generation fails at rotation | old cert still served; retried; later success swaps |
| AT-E.3 | Dev stack `CONNECTOR_CERT_TTL=10m`, run 15 min | connectors, relays and clients reconnect successfully past the first cert's `NotAfter` |
| AT-E.4 | `go test -race` on rotator | no races |

## 7. Phase F — Relay in-band renewal (M2)

| ID | Case | Expected |
|----|------|----------|
| AT-F.1 | Same-key CSR from active relay | renewed; new row in `relay_certificates`; `relays.cert_serial` updated; old serial not revoked |
| AT-F.2 | Different-key CSR | `PermissionDenied` |
| AT-F.3 | Revoked / deleted / pending relay | refused |
| AT-F.4 | Presented serial unknown or revoked | `PermissionDenied` |
| AT-F.5 | CSR SAN outside allowlist | refused |
| AT-F.6 | Concurrent `RevokeRelay` + `RecordRenewedCert` (many iterations) | no unrevoked `relay_certificates` row remains for the relay |
| AT-F.7 | Revoke after renewal | both serials on `/relay.crl`; connectors drop the relay |
| AT-F.8 | Dev stack `RELAY_CERT_TTL=1h` *(not 15m, because of KI-1 in `path.md`: at ≤ 40 min the backdated `NotBefore` causes a renewal loop)* | relay renews by itself (6–18 min after each issue) and serves past the first cert's `NotAfter` (+60 min) without restart |
| AT-F.9 | Relay with already-expired cert | no `RenewCert` call; error log says replace the relay (D-20) |

## 8. Phase G — Connector renewal (M2)

| ID | Case | Expected |
|----|------|----------|
| AT-G.1 | Health report inside `CONNECTOR_RENEWAL_WINDOW` | exactly one `ReEnroll`; repeated only after the resend interval |
| AT-G.2 | Outside window / NULL `cert_not_after` | no `ReEnroll` |
| AT-G.3 | Revoked connector | no `ReEnroll`; `RenewCert` denied |
| AT-G.4 | Dev stack `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m` | auto-renewal; `connectors.cert_serial` changes |
| AT-G.5 | After the original cert's `NotAfter`, no restart | direct tunnel `:9092` (TLS + QUIC) works; relayed tunnel works; shield renews via connector |
| AT-G.6 | Established tunnels during swap | not dropped |
| AT-G.7 | Crash during renewal write | cert files are either old or new, never partial |

---

## 9. Regression gate

```bash
buf generate
cd controller && go build ./... && go vet ./... && go test ./...
cd connector && cargo build && cargo test
cd relay && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml
cd client && cargo build
```

Existing suites that must stay green without modification of their assertions:
`internal/relay/*_test.go`, `internal/connector/*_test.go`, `internal/shield/heartbeat_test.go`,
`internal/transport/*_test.go`, `internal/policy/*_test.go`, `internal/pki/*_test.go`,
`connector` / `relay` / `shield` / `client` Rust suites.
