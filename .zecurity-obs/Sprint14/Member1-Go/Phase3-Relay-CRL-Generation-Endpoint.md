---
type: phase
member: M1
sprint: 14
phase: 3
title: Relay CRL Generation + Endpoint
status: done
depends_on:
  - Sprint14/Member1-Go/Phase1-Relay-Cert-History-Data-Model
tags: [go, pki, crl, relay, revocation, pending-02]
---

# Phase 3 — Relay CRL Generation + Endpoint

> Depends on Phase 1 (`ListRevokedRelaySerials`). Additive — no verifier depends on it until M2-F.

## Goal

Publish revoked relay serials as an **Intermediate-CA-signed** DER CRL for off-controller verifiers
(connector, client). Mirror the existing `GenerateClientCRL` (`pki/workspace.go:530`) structure; only
the signing CA, source table, and scope differ.

## Files

| File | Change |
|------|--------|
| `controller/internal/pki/service.go` | add `GenerateRelayCRL(ctx) ([]byte, error)` to the `Service` interface |
| `controller/internal/pki/relay_crl.go` | **new** — Intermediate-CA-signed CRL impl |
| `controller/internal/connector/ca_endpoint.go` | add `RelayCRLEndpointHandler` |
| `controller/cmd/server/main.go` | route `GET /relay.crl` |

## relay_crl.go
`(*serviceImpl) GenerateRelayCRL(ctx)`:
- Sign with the **Intermediate CA** key already used by `SignRelayCert` (`pki/relay.go:9,54-56`,
  `s.intermediateKey.cert` / `.privKey`) — **not** a workspace CA.
- Source = `relayStore.ListRevokedRelaySerials(ctx)` (revoked + unexpired). Each entry →
  `x509.RevocationListEntry{SerialNumber: hex→big.Int, RevocationTime}`.
- Template `x509.RevocationList{Number, ThisUpdate: now, NextUpdate: now + N}` where **`NextUpdate`
  MUST exceed the client/connector refresh interval** (60s) with margin — e.g. **`now + 10m`** — so a
  fresh puller never sees an already-expired CRL, but stale caches still expire (deny-past-`nextUpdate`
  is enforced by the M2 manager).
- `x509.CreateRevocationList(rand, template, s.intermediateKey.cert, s.intermediateKey.privKey)` → DER.
- On-demand per request (like `GenerateClientCRL`) is fine for Track 1.

> **Note:** the relay store lives in `internal/relay`; `pki` must not import it circularly. Pass the
> revoked-serial list in (e.g. a small `RelayRevocationSource` interface the PKI service is given at
> construction), rather than importing `relay` into `pki`. Confirm the wiring during implementation.

## ca_endpoint.go + main.go
- `RelayCRLEndpointHandler(pkiSvc)` → `application/pkix-crl` DER, platform-wide (**no** `workspace_id`).
- `mux.HandleFunc("/relay.crl", connector.RelayCRLEndpointHandler(pkiService))` near `main.go:224-225`.
- Unauthenticated like `/ca.crl` (signed data) — note for the security review.

## Tests
- `GenerateRelayCRL` on 0 / 1 / N revoked relays → parseable CRL whose entries exactly match; verifies against the Intermediate CA public key; `NextUpdate > now + refresh`.
- `GET /relay.crl` → 200, `application/pkix-crl`, `x509.ParseRevocationList` succeeds.

## Build Check
```bash
cd controller && go build ./...
```

## Implementation Checklist
- [x] **M1-C1** `GenerateRelayCRL` on the `Service` interface.
- [x] **M1-C2** `relay_crl.go` — Intermediate-CA-signed; `NextUpdate` margin > refresh interval.
- [x] **M1-C3** `internal/relay.RelayCRLHandler` + `GET /relay.crl`.
- [x] **Build gate:** `cd controller && go build ./...`

## Pre-Implementation Corrections (validated review — codex)
- **Stale line references.** Re-locate at implementation time: in `pki/relay.go` the method begins at
  `:34` (key availability ~`:42`, signing later) — not `:9,54-56`; the CA-route area in `main.go` is
  around the other `/ca.*` handlers (~`:232-233` on this branch), not `:224-225`.
- **`pki.Init` ordering (must-fix wiring).** `pki.Init(...)` runs at `cmd/server/main.go:68`, **before**
  `relayStore` is created (~`:119`), and returns only `pki.Service` — so the revoked-serial source
  can't be injected at Init. Fix by **either** creating `relayStore` first and passing a narrow
  `RelayRevocationSource` interface into `pki.Init`, **or** adding an explicit post-init wiring call
  (e.g. `pkiService.SetRelayRevocationSource(relayStore)`). **Do not import `relay` from `pki`**
  (cycle).

## Post-Phase Fixes
_None yet._
