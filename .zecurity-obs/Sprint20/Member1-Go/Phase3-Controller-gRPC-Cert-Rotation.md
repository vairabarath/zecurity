---
type: phase
member: M1
person: Sathiya
sprint: 20
phase: 3
execution: E
title: Controller gRPC Certificate Rotation
status: done
depends_on: [2]
tags:
  - go
  - pki
  - tls
  - controller
  - provider-dashboard
---

# Phase 3 (E) — Controller gRPC Certificate Rotation

> Technically independent. `depends_on: [2]` only orders M1's `main.go` edits (after Phase D).
> Decision Record prerequisite #4 (Q15; discovery §8: "likely" expired cert after ~7 days uptime).

## Problem (verified)

- At startup `main.go:~457` calls `pkiService.GenerateControllerServerTLS(ctx, controllerCertHosts(...), connectorCfg.CertTTL)`. `CertTTL` is `CONNECTOR_CERT_TTL`, default 7 d.
- The key pair is loaded into a static `tls.Config{Certificates: []tls.Certificate{controllerCert}}` (`main.go:~468-473`).
- Nothing regenerates it. A controller process running longer than the TTL presents an **expired** certificate on every new gRPC handshake: connector control streams, relay heartbeats, enrollments, `ClientService`.
- The generator (`internal/pki/controller.go:19`) is cheap and in-memory, signed by the Intermediate, with SPIFFE `spiffe://zecurity.in/controller/global`.

## Goal

The gRPC listener always presents a valid certificate with the same identity (SPIFFE ID + SANs),
for any process uptime. The key stays in memory only (existing design).

## Files

| File | Change |
|------|--------|
| `controller/internal/pki/controller_rotator.go` (new; or `cmd/server` if preferred) | `ControllerCertRotator`: holds current `*tls.Certificate` + `NotAfter` behind a lock / `atomic.Pointer`; `GetCertificate`; `Run(ctx)` loop |
| `controller/cmd/server/main.go` (~455-475) | Build the rotator from the first generated cert; `tls.Config{GetCertificate: rotator.GetCertificate, ClientAuth: tls.RequestClientCert, MinVersion: tls.VersionTLS13}`; start `rotator.Run` under the existing `wg`/`ctx` |
| `controller/internal/pki/controller_rotator_test.go` (new) | Unit + handshake tests |

## Design

```go
type ControllerCertRotator struct {
    gen   func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) // cert, notBefore, notAfter
    cur   atomic.Pointer[rotatedCert]
    clock func() time.Time
}

func (r *ControllerCertRotator) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error)

// Run regenerates when now >= notBefore + 2/3*(notAfter-notBefore).
// (Superseded by PF-1: measured from issuance, since notBefore is backdated 1h.)
// On generation failure: log loudly, retry with capped backoff (e.g. 30s → 10m),
// keep serving the current cert. Never swap in a nil/invalid cert.
func (r *ControllerCertRotator) Run(ctx context.Context)
```

- `gen` wraps `GenerateControllerServerTLS(ctx, hosts, ttl)` + `tls.X509KeyPair`, using the **same** `hosts` computed at startup (`controllerCertHosts`) and the same TTL. No new env var.
- The first certificate is generated synchronously at startup, as today. A failure there is still `log.Fatalf`.
- Existing TLS sessions and streams are unaffected, since the certificate is only used at handshake. New handshakes get the new cert.

## Invariants

- **I1** The certificate identity is unchanged: `spiffe://zecurity.in/controller/global`, same SANs, signed by the platform Intermediate. Connector (`connector/src/tls/mod.rs` `verify_controller_spiffe`), relay (trusts Intermediate) and client verifiers need no change.
- **I2** `GetCertificate` never returns an expired certificate while a newer valid one exists, and never returns nil after startup.
- **I3** The controller private key is never persisted (existing design).
- **I4** No change to `ClientAuth: tls.RequestClientCert`, `MinVersion`, or the SPIFFE interceptors.
- **I5** Rotation uses the existing `CONNECTOR_CERT_TTL`. Don't add configuration. Don't introduce a separate controller CA (D-22 keeps the PKI hierarchy as is).

## Tests

Unit (fake `gen`, fake clock):
- Initial cert served; clock advanced past 2/3 lifetime → `Run` swaps; `GetCertificate` returns the new cert (different serial).
- Generation error at rotation time → the old cert keeps being served; retry happens; a later success swaps.
- Concurrent `GetCertificate` calls during a swap: no data race (`go test -race`).

Handshake:
- `tls.Server` with `GetCertificate` from a rotator with a very short TTL (a few seconds, real `GenerateControllerServerTLS` if a PKI test DB is available (`PKI_TEST_DATABASE_URL`), otherwise a local test CA). A client verifying against the issuing CA succeeds before and **after** the original `NotAfter`, and sees the new serial after rotation.

## Acceptance Criteria

- [x] Built and unit-tested; handshake test proves the controller accepts new gRPC handshakes past the original cert's `NotAfter` with a rotated cert (TestRotatorHandshakeRotation, 4s TTL). Rotation timing correct under the real 1 h `NotBefore` backdate (PF-1, `TestNeedsRotationBackdatedNotBefore`).
- [ ] Live `CONNECTOR_CERT_TTL=10m` dev stack: controller up > 15 min, connectors/relays reconnect after the first cert's `NotAfter`, logs show ~1 rotation per ~6m40s. **PENDING / NOT YET RUN.**
- [x] `go test -race ./internal/pki/...` passes.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test -race ./internal/pki/... ./cmd/server/...
```

## Implementation Checklist

- [x] **M1-E1** `ControllerCertRotator` with `GetCertificate`
- [x] **M1-E2** `Run`: rotate at 2/3 lifetime, retry with backoff, keep current on failure
- [x] **M1-E3** `main.go`: `GetCertificate` in `tls.Config`; start `Run` under `wg`/`ctx`
- [x] **M1-E4** Tests (unit, race, handshake)
- [x] **Build gate:** `cd controller && go build ./...` (+ `go test -race ./internal/pki/... ./cmd/server/...`)

## Post-Phase Fixes

### PF-1 (2026-09-25) — rotation threshold measured from the backdated `NotBefore`

- **Bug:** `GenerateControllerServerTLS` backdates `NotBefore` by 1 h (clock skew, `internal/pki/controller.go:39-41`). The rotator applied the Design formula literally, `notBefore + 2/3·(notAfter − notBefore)`, so the hour of backdating counted toward the threshold. Result: for any `CONNECTOR_CERT_TTL` ≤ 30m, every freshly issued cert was already due, so the rotator reissued once per `checkInterval` (1 min) for as long as the process ran. With 10m the threshold fell 13m20s *before* issuance, not 6m40s after. The default 7 d TTL was only 20 min early. Unit tests missed it because every fake/test-CA cert used a non-backdated `NotBefore`.
- **Fix:** `rotatedCert.issuedAt` stores the rotator clock at store time (constructor + `rotate`). New `rotationDue()` = `start + 2/3·(notAfter − start)` with `start = max(notBefore, issuedAt)`; `needsRotation` and `nextDue` both use it. No config, API or `main.go` change.
- **Regression test:** `TestNeedsRotationBackdatedNotBefore` (10m / 30m / 2h / 7d, 1 h backdate, constructor and `rotate` paths). It failed on the old code for all four TTLs and passes now.
- **Gate:** `go build ./...`, `go vet ./cmd/server/... ./internal/pki/...`, `go test -race ./internal/pki/... ./cmd/server/...`: green.
- Live expectation with `CONNECTOR_CERT_TTL=10m` is now correct: one rotation about every 6m40s.
