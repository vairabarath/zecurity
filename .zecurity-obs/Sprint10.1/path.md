---
type: planning
status: planned
sprint: 10.1
tags:
  - sprint10_1
  - relay
  - pki
  - mtls
  - security
---

# Sprint 10.1 — Relay PKI & End-to-End Security

> Security prerequisite for Sprint 10 Relay integration. Complete this sprint
> before enabling Connector registration or Client fallback through the Relay.

## Sprint Goal

Define and implement the trust model that lets one Relay authenticate Connector
and Client certificates from every workspace without calling the Controller.
Preserve end-to-end confidentiality by adding an inner Client-to-Connector mTLS
session over each Relay-bridged stream.

## Security Model

### Certificate hierarchy

```text
Platform Root CA                 MaxPathLen=2
└── Platform Intermediate CA     MaxPathLen=1
    ├── Workspace A CA           MaxPathLen=0
    │   ├── Connector leaf
    │   └── Client-device leaf
    ├── Workspace B CA           MaxPathLen=0
    │   ├── Connector leaf
    │   └── Client-device leaf
    └── Relay server leaf        ServerAuth, platform relay SPIFFE ID
```

### Relay authentication

- Relay trusts the Platform Intermediate CA.
- Connectors and Clients present `leaf + Workspace CA`.
- Relay validates `leaf → Workspace CA → Platform Intermediate CA`.
- Relay then validates the exact SPIFFE URI and message-to-certificate binding.
- Relay does not call the Controller during connection handling.

### Peer authentication of Relay

- Relay presents a server certificate signed by the Platform Intermediate CA.
- Relay generates and retains its own private key. The private key never enters
  the Controller, database, Sprint repository, or certificate-signing response.
- Relay submits a CSR containing its requested SPIFFE URI and DNS/IP SANs to an
  authenticated Controller provisioning command. The Controller validates the
  request, signs it with the existing Platform Intermediate CA, and returns only
  the Relay leaf certificate and Intermediate CA certificate.
- Connector and Client trust the Platform Intermediate CA already included in
  their saved CA bundle.
- Relay server SPIFFE format:
  `spiffe://<global-trust-domain>/relay/<relay-id>`.
- Connector and Client require the configured exact Relay SPIFFE ID.

### Relay certificate provisioning

```text
Relay host                              Controller PKI
----------                              --------------
Generate relay.key (0600)
Generate relay.csr
                     relay.csr  ------>
                                      Validate requested identity and SANs
                                      Sign with existing Intermediate CA key
                    <------ relay.crt + intermediate-ca.crt
Store relay.key, relay.crt,
and intermediate-ca.crt
```

- Never generate a new Platform Intermediate CA for Relay deployment.
- Never copy or export the Platform Root or Intermediate private keys.
- Never commit `relay.key`, `relay.csr`, generated certificates, or CA private
  keys to the Zecurity or standalone Relay repositories.

### End-to-end confidentiality

Outer QUIC terminates at the Relay and protects each network hop. It does not
provide end-to-end confidentiality by itself.

For every successful lookup, Client and Connector establish an inner TLS 1.3
mTLS session over the Relay-bridged byte stream before sending `TunnelRequest`
or resource traffic:

```text
Client -- outer QUIC --> Relay -- outer QUIC --> Connector
       <--------- inner Client-to-Connector mTLS --------->
```

The Relay can inspect Register/Lookup metadata but only sees ciphertext for the
inner tunnel payload.

## Key Decisions

| Decision | Detail |
|----------|--------|
| Relay trust anchor | Platform Intermediate CA, provided as `RELAY_CLIENT_CA` |
| Relay identity | Intermediate-signed server leaf with exact relay SPIFFE URI |
| Relay private-key ownership | Generated and retained only on Relay host; Controller signs CSR only |
| Peer chains | Connector and Client send `leaf + Workspace CA`; never send private keys |
| Workspace isolation | Client and Connector leaf SPIFFE trust domains must match |
| Message binding | Register connector ID/SPIFFE must exactly match peer certificate |
| Inner encryption | TLS 1.3 mTLS between Client and Connector over bridged stream |
| Legacy PKI | Startup audit fails closed if stored CA path constraints cannot validate full chains |
| Rotation | Relay cert/key and CA bundle reload on restart; online hot reload deferred |
| Revocation | Existing leaf expiry and CRL behavior retained; Relay-specific CRL distribution deferred |

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| M2 | Go / PKI | Chain audit, tests, Relay certificate provisioning |
| M3 | Rust / Data Plane | Relay verifier, peer chain presentation, inner mTLS |

## Execution Path

### PHASE A — M2: PKI Chain Audit & Contract

> See [[Sprint10.1/Member2-Go/Phase1-PKI-Chain-Audit]].

- [x] **M2-A1** Add full-chain tests for Connector and Client leaves across two workspaces.
- [x] **M2-A2** Correct stale Root/Intermediate path-length test assertions.
- [x] **M2-A3** Add startup audit for deployed CA path constraints.
- [x] **M2-A4** Document remediation for legacy incompatible CA hierarchies.

### PHASE B — M2: Relay Certificate Provisioning

> Depends on Phase A.
> See [[Sprint10.1/Member2-Go/Phase2-Relay-Cert-Provisioning]].

- [x] **M2-B0** Define the initial `relay.v1.RelayService.Provision` protobuf contract.
- [x] **M2-B1** Add Relay SPIFFE identity helper and exact format.
- [x] **M2-B2** Add authenticated PKI method/tool that validates and signs a Relay-generated CSR.
- [x] **M2-B3** Return only Relay cert and Intermediate CA; document Relay-host key/CSR generation.
- [x] **M2-B4** Add certificate property, CSR-validation, and chain-validation tests.
- [x] **M2-B5** Define and implement the mTLS Relay heartbeat RPC and Relay health persistence.

### PHASE C — M3: Relay Multi-Workspace mTLS

> Depends on Phase A and Phase B provisioning contract.
> See [[Sprint10.1/Member3-Rust/Phase1-Relay-Multi-Workspace-mTLS]].

- [x] **M3-C1** Require client certificates and trust only `RELAY_CLIENT_CA`.
- [x] **M3-C2** Validate complete peer chains and exact Connector/Client SPIFFE formats.
- [x] **M3-C3** Bind `RegisterMsg` identity to the verified Connector certificate.
- [x] **M3-C4** Enforce same-workspace trust domain during Lookup.
- [x] **M3-C5** Add positive and negative multi-workspace TLS tests.

### PHASE D — M3: Peer Chains & Inner mTLS

> Depends on Phase C.
> See [[Sprint10.1/Member3-Rust/Phase2-Peer-Chains-Inner-mTLS]].

- [x] **M3-D1** Connector presents `leaf + Workspace CA` to Relay and verifies exact Relay SPIFFE.
- [x] **M3-D2** Client presents `leaf + Workspace CA` to Relay and verifies exact Relay SPIFFE.
- [x] **M3-D3** Establish inner Client-to-Connector TLS 1.3 mTLS over bridged streams.
- [x] **M3-D4** Send TunnelRequest and resource bytes only after inner mTLS succeeds.
- [ ] **M3-D5** Add test proving Relay-observed bridged bytes do not contain plaintext payload.

## Final Build Gates

- [ ] `cd controller && go test ./internal/pki/... ./internal/connector/...`
- [x] `cd controller && go build ./...`
- [ ] `cd relay && cargo test && cargo build`
- [ ] `cd connector && cargo test && cargo build`
- [ ] `cd client && cargo test && cargo build`

## Acceptance Criteria

- [ ] One Relay accepts valid Connector and Client chains from two different workspaces.
- [ ] Unknown/self-signed Workspace CA is rejected.
- [ ] Leaf-only Connector or Client chain is rejected by Relay.
- [ ] Registration identity mismatch is rejected.
- [ ] Cross-workspace Lookup is rejected.
- [ ] Connector and Client reject a Relay certificate with the wrong SPIFFE ID.
- [ ] Controller provisioning never receives, generates, stores, or returns the Relay private key.
- [ ] Relay deployment contains no generated Platform Intermediate CA or CA private key.
- [ ] Relay cannot observe TunnelRequest or resource payload plaintext.
- [ ] Existing direct Client-to-Connector QUIC path still works.
- [ ] Sprint 10 Connector registration and Client fallback can proceed using this trust contract.

## Deferred

- Online Relay certificate hot reload.
- Relay-local CRL/OCSP distribution and refresh.
- Automated zero-downtime Platform Intermediate CA rotation.
- Hardware-backed Relay private keys.

## Post-Sprint Fixes

### Relay CSR Validation Hardening

- Replaced Relay UUID regex validation with canonical `google/uuid` parsing.
- Kept SAN allowlist enforcement in the PKI signer as defense-in-depth while
  leaving allowlist policy selection to the future `Provision` handler.
- Aligned Relay ECDSA leaf key usage with existing workspace-signed leaves and
  added focused CSR validation tests.
- Added the initial server-authenticated TLS `Provision` RPC; the reserved
  provisioning-token field remains ignored until authenticated provisioning is
  implemented.
- Added the Relay-side verified-CA TLS provisioning client. It generates the
  CSR in memory, validates the returned identity and CA, and persists the Relay
  key/certificate material before future listener and heartbeat startup.
