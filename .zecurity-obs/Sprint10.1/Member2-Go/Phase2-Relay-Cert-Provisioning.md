---
type: phase
sprint: 10.1
member: M2
phase: 2
status: in-progress
depends_on:
  - Sprint10.1-M2-Phase1
---

# M2 Phase 2 — Relay Certificate Provisioning

## What You're Building

Add an explicit provisioning path for a Relay server identity issued by the
Controller's existing internal PKI. The Relay host generates and retains the
private key; the Controller validates and signs only the Relay CSR.

## Certificate Contract

- Issuer: Platform Intermediate CA
- SPIFFE URI: `spiffe://<global-trust-domain>/relay/<relay-id>`
- Extended Key Usage: `ServerAuth`
- DNS/IP SANs: configured Relay public names/addresses
- Private key: generated on Relay host, stored with `0600`, never sent to Controller
- Relay client trust bundle: Platform Intermediate CA certificate

## Provisioning Flow

```text
Relay host:
  1. Generate relay.key locally.
  2. Generate relay.csr requesting:
     spiffe://<global-trust-domain>/relay/<relay-id>
     plus configured DNS/IP SANs.
  3. Submit relay.csr and relay-id to authenticated Controller provisioning tool.

Controller:
  4. Parse CSR and verify its self-signature.
  5. Reject unexpected SPIFFE identities, roles, SANs, algorithms, and key usages.
  6. Sign CSR public key with the existing Platform Intermediate CA.
  7. Return relay.crt and intermediate-ca.crt only.

Relay host:
  8. Store relay.key, relay.crt, and intermediate-ca.crt.
```

## Current Relay RPC Contract

Source of truth: `proto/relay/v1/relay.proto`

- `Provision(ProvisionRequest)`: server-authenticated TLS bootstrap request
  carrying a single-use provisioning token, Relay ID, CSR, version, and
  hostname.
- DNS/IP SAN fields are not yet represented in `ProvisionRequest`; add them
  before implementing SAN allowlist validation and certificate issuance.
- The heartbeat RPC is intentionally deferred. Define it before implementing
  periodic mTLS-authenticated Relay health reporting.
- The Relay private key is never represented in or sent through the protobuf
  contract.

## Required Relay Files

```text
relay.key                generated locally; mode 0600; never committed
relay.csr                generated locally; temporary; never committed
relay.crt                returned by Controller PKI
intermediate-ca.crt      returned by Controller PKI
```

## Requirements

1. Add an appmeta helper for exact Relay SPIFFE IDs.
2. Add a PKI service method that accepts a parsed, validated Relay CSR and
   returns only the signed certificate and Intermediate CA certificate.
3. Provide an authenticated operator-facing command/tool for CSR submission.
4. Provide a Relay-host script or documented OpenSSL command that generates
   only `relay.key` and `relay.csr`.
5. Never create a self-signed "Platform Intermediate CA" for Relay deployment.
6. Never expose or export the Root CA or Intermediate CA private keys.
7. Never generate, receive, store, or return the Relay private key from the
   Controller.
8. Add `.gitignore` coverage for generated Relay key, CSR, and certificates.
9. Test CSR self-signature, exact SPIFFE URI, SAN allowlist, EKU, validity,
   chain verification, and malformed/unauthorized CSR rejection.
10. Document deployment environment variables:

```text
RELAY_TLS_CERT
RELAY_TLS_KEY
RELAY_CLIENT_CA
RELAY_SPIFFE_ID
```

11. Implement `relay.v1.RelayService` Controller handlers:
    - `Provision` validates the token and CSR before issuing the Relay leaf.
    - Define and implement `Heartbeat`; require mTLS and record Relay
      health/status.

## Explicitly Forbidden

Do not add or run a script equivalent to:

```bash
openssl req -x509 -key platform-intermediate.key ...
```

That creates an unrelated self-signed CA and requires placing the Platform
Intermediate private key on the Relay host. Both violate this sprint's trust
model.

## Build Check

```bash
cd controller
go test ./internal/pki/...
go build ./...
cd ../relay
cargo test
cargo build
```

## Post-Phase Fixes

*(Empty)*
