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
  2. Generate a DER-encoded relay CSR requesting:
     spiffe://<global-trust-domain>/relay/<relay-id>
     plus configured DNS/IP SANs.
  3. Submit the DER CSR and relay-id to the Controller Provision RPC.

Controller:
  4. Parse CSR and verify its self-signature.
  5. Reject unexpected SPIFFE identities, roles, SANs, algorithms, and key usages.
  6. Sign CSR public key with the existing Platform Intermediate CA.
  7. Return relay.crt and intermediate-ca.crt only.

Relay host:
  8. Store relay.key, relay.crt, and intermediate-ca.crt.
  9. On later starts, reuse the complete stored certificate material.
```

## Current Relay RPC Contract

Source of truth: `proto/relay/v1/relay.proto`

- `Provision(ProvisionRequest)`: server-authenticated TLS bootstrap request
  carrying Relay ID, DER CSR, version, hostname, and operator-confirmed DNS/IP
  SAN allowlists.
- `provisioning_token` is reserved in the proto but ignored by the current
  implementation. Authenticated/single-use provisioning is deferred.
- The current `Provision` handler validates the requested SAN allowlist.
  `SignRelayCert` independently enforces that the CSR contains no DNS/IP SAN
  outside that allowlist before signing.
- The heartbeat RPC is intentionally deferred. Define it before implementing
  periodic mTLS-authenticated Relay health reporting.
- The Relay private key is never represented in or sent through the protobuf
  contract.
- The Relay fetches `/ca.crt` for TLS bootstrap and requires
  `RELAY_CA_FINGERPRINT` to verify that certificate before connecting.
- The Relay validates the returned Relay ID, SPIFFE URI, leaf-certificate
  SPIFFE URI, and Intermediate CA fingerprint before storing any material.

## Required Relay Files

```text
relay.key                generated locally; mode 0600; never committed
relay.csr.der            generated in memory; sent to Controller; not persisted
relay.crt                returned by Controller PKI
intermediate-ca.crt      returned by Controller PKI
```

## Relay Startup Environment

```text
RELAY_ID                 canonical lowercase Relay UUID
CONTROLLER_ADDR          Controller gRPC host:port
CONTROLLER_HTTP_ADDR     optional Controller HTTP host:port; defaults to host:8080
RELAY_CA_FINGERPRINT     required SHA-256 hex fingerprint for fetched /ca.crt
RELAY_STATE_DIR          optional artifact directory; defaults to pki
RELAY_DNS_SANS           optional comma-separated DNS SAN allowlist and CSR SANs
RELAY_IP_SANS            optional comma-separated canonical IP SAN allowlist and CSR SANs
LOG_LEVEL                optional tracing filter; defaults to info
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
    - `Provision` validates the request and CSR before issuing the Relay leaf.
      Token validation is deferred until authenticated provisioning is enabled.
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

### Fix: Canonical Relay UUID and CSR Validation

**Issue:** Relay IDs were validated with a UUID-shaped regex and Relay server
certificates included ECDSA `KeyEncipherment`, unlike existing workspace-signed
ECDSA leaves.

**Fix Applied:**
- Parse Relay IDs with `google/uuid` and require the canonical lowercase,
  hyphenated representation.
- Keep SAN allowlist enforcement in `SignRelayCert` as defense-in-depth; the
  future `Provision` handler remains responsible for deciding the allowlist.
- Use `DigitalSignature` only for Relay ECDSA leaf certificates.
- Add focused tests for canonical UUIDs, exact Relay SPIFFE URI, P-384 keys,
  and DNS/IP SAN allowlist rejection.

### Current Provision RPC Scope

- Added and registered `controller/internal/relay.Service.Provision`.
- Provision uses server-authenticated TLS and currently ignores the reserved
  `provisioning_token`.
- Relay IDs, DER CSR, DNS SANs, and IP SANs are validated before PKI signing.
- Single-use authenticated provisioning remains a future implementation.
- Added Relay startup configuration and the TLS `Provision` client.
- Relay verifies the fetched CA fingerprint, sends its CSR, validates the
  returned identity/certificates, and stores `relay.key`, `relay.crt`, and
  `intermediate-ca.crt`.
- Building the mTLS QUIC listener and sending heartbeat messages remain pending
  until their runtime and protobuf contracts are implemented.
