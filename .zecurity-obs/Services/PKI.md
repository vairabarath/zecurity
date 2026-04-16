---
type: service
status: active
language: Go
package: internal/pki
related:
  - "[[Services/Controller]]"
  - "[[Services/Connector]]"
tags:
  - pki
  - x509
  - spiffe
  - crypto
---

# PKI (Certificate Authority)

Internal controller service. Manages the 3-tier certificate hierarchy used for mTLS authentication and SPIFFE identity.

---

## CA Hierarchy

```
Root CA (self-signed)
  · 10-year validity · MaxPathLen=2
  · Stored in: ca_root table (AES-GCM encrypted key)

  └── Intermediate CA (signed by Root)
        · 5-year validity · MaxPathLen=1
        · Stored in: ca_intermediate table (AES-GCM encrypted key)
        · Loaded into memory at startup (signs workspace CAs)

        └── Workspace CA (per-tenant, signed by Intermediate)
              · 2-year validity · MaxPathLen=0
              · Stored in: workspace_ca_keys table (AES-GCM encrypted key)
              · Identified by: tenant SPIFFE URI (tenant:/<tenantID>)

              └── Connector Cert (signed by Workspace CA)
                    · 7-day validity · IsCA=false
                    · SPIFFE SAN: spiffe://<trust_domain>/connector/<id>
                    · CN: connector-<connectorID>
                    · KeyUsage: DigitalSignature · ExtKeyUsage: ClientAuth

              └── Controller TLS Cert (signed by Workspace CA)
                    · Ephemeral · SPIFFE SAN: spiffe://<trust_domain>/controller
                    · DNS SANs: localhost, CONTROLLER_ADDR host
```

---

## Key Encryption

All private keys are encrypted with **AES-256-GCM** before database storage:

```
masterSecret (env: PKI_MASTER_SECRET)
    + tenantID (context)
    ─── HKDF-SHA256 ───►  32-byte derived key
                              └── AES-256-GCM encrypt(privKeyDER)
                                    → base64(encrypted) + base64(nonce)
                                         stored in DB
```

This means: if `PKI_MASTER_SECRET` changes, all CA keys become unreadable.
The controller detects this and resets PKI only if no workspaces exist yet.

---

## Service Interface (`internal/pki/service.go`)

```go
type Service interface {
    GenerateWorkspaceCA(ctx, tenantID) (*WorkspaceCAResult, error)
    SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, certTTL) (*ConnectorCertResult, error)
    RenewConnectorCert(ctx, tenantID, connectorID, trustDomain, csrDER, certTTL) (*ConnectorCertResult, error)
    GenerateControllerServerTLS(ctx, hosts, certTTL) (*ControllerServerTLSResult, error)
}
```

---

## Files

| File | Purpose |
|------|---------|
| `internal/pki/root.go` | Root CA init/load |
| `internal/pki/intermediate.go` | Intermediate CA init/load |
| `internal/pki/workspace.go` | Workspace CA gen + `SignConnectorCert` + `RenewConnectorCert` |
| `internal/pki/controller.go` | Controller TLS cert generation |
| `internal/pki/crypto.go` | AES-GCM key encryption, PEM helpers, serial number gen |
| `internal/pki/service.go` | Service interface + Init() |

---

## Cert Renewal Flow

`RenewConnectorCert` is called when a connector's cert is within the renewal window.

1. Parse incoming CSR (`x509.ParseCertificateRequest`)
2. `csr.CheckSignature()` — verifies connector holds the private key
3. Extract `csr.PublicKey` — same key, fresh cert
4. Load + decrypt Workspace CA from DB
5. Issue new cert: same SPIFFE SAN + CN, fresh validity window
6. Load Intermediate CA PEM for response
7. Return `ConnectorCertResult` with full CA chain

The connector's keypair never changes. Only the cert validity window is extended.

---

## Clock Skew

All connector certs are backdated 1 hour (`notBefore = now - 1h`) to tolerate clock drift between controller and connector hosts.
