---
type: phase
sprint: 10.1
member: M2
phase: 2
status: planned
depends_on:
  - Sprint10.1-M2-Phase1
---

# M2 Phase 2 — Relay Certificate Provisioning

## What You're Building

Add an explicit provisioning path for a Relay server identity issued by the
Controller's internal PKI.

## Certificate Contract

- Issuer: Platform Intermediate CA
- SPIFFE URI: `spiffe://<global-trust-domain>/relay/<relay-id>`
- Extended Key Usage: `ServerAuth`
- DNS/IP SANs: configured Relay public names/addresses
- Private key permissions: `0600`
- Relay client trust bundle: Platform Intermediate CA certificate

## Requirements

1. Add an appmeta helper for exact Relay SPIFFE IDs.
2. Add a PKI service method for issuing Relay server certificates.
3. Provide an operator-facing provisioning command or tool that writes:

```text
relay.crt
relay.key
intermediate-ca.crt
```

4. Never expose the Root CA private key or Intermediate CA private key.
5. Test SPIFFE URI, SANs, EKU, validity, and chain verification.
6. Document deployment environment variables:

```text
RELAY_TLS_CERT
RELAY_TLS_KEY
RELAY_CLIENT_CA
RELAY_SPIFFE_ID
```

## Build Check

```bash
cd controller
go test ./internal/pki/...
go build ./...
```

## Post-Phase Fixes

*(Empty)*
