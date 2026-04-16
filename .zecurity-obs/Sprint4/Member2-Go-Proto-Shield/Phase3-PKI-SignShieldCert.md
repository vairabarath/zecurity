---
type: task
status: pending
sprint: 4
member: M2
phase: 3
depends_on:
  - Phase1-Proto-appmeta (appmeta.ShieldSPIFFEID + PKIShieldCNPrefix constants)
unlocks:
  - Phase2-Shield-Package (enrollment.go calls pki.SignShieldCert)
  - Phase4-Main-Wiring
tags:
  - go
  - pki
  - x509
  - shield
---

# M2 · Phase 3 — PKI: SignShieldCert + RenewShieldCert

**Depends on: appmeta constants (Phase 1). Can run in parallel with Phase 2.**

---

## Goal

Add `SignShieldCert` and `RenewShieldCert` to `controller/internal/pki/workspace.go` alongside the existing connector cert functions. The Shield cert is identical in structure to the Connector cert — only the SPIFFE role path and CN prefix differ.

---

## File to Modify

`controller/internal/pki/workspace.go`

---

## Checklist

### Add to PKI Service interface (`internal/pki/service.go`)

- [ ] Add to `Service` interface:
  ```go
  SignShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*ShieldCertResult, error)
  RenewShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*ShieldCertResult, error)
  ```

### Add `ShieldCertResult` type

- [ ] In `workspace.go` (or `service.go`), add:
  ```go
  type ShieldCertResult struct {
      CertificatePEM    []byte
      WorkspaceCAPEM    []byte
      IntermediateCAPEM []byte
      Serial            string
      NotBefore         time.Time
      NotAfter          time.Time
  }
  ```

### Implement `SignShieldCert` in `workspace.go`

- [ ] Identical to `SignConnectorCert` except:
  - SPIFFE SAN uses `appmeta.ShieldSPIFFEID(trustDomain, shieldID)`
  - CN uses `appmeta.PKIShieldCNPrefix + shieldID` (i.e. `"shield-<id>"`)
  - `certTTL` comes from `ShieldConfig.CertTTL` (passed as parameter)
- [ ] Fields: `KeyUsage: DigitalSignature`, `ExtKeyUsage: ClientAuth`, `IsCA: false`
- [ ] Use existing `loadWorkspaceCA()`, `newSerial()`, `zeroKey()` helpers
- [ ] Return full cert + CA chain

### Implement `RenewShieldCert` in `workspace.go`

- [ ] Delegates directly to `SignShieldCert` — CSR already has correct public key
  ```go
  func (s *serviceImpl) RenewShieldCert(...) (*ShieldCertResult, error) {
      return s.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, certTTL)
  }
  ```

---

## Build Check

```bash
cd controller && go build ./...
# pki.Service interface must be fully implemented
# No interface compliance errors
```

---

## Notes

- Do not modify existing `SignConnectorCert` or `RenewConnectorCert`. Shield cert is a separate function.
- `ShieldCertResult` can be defined in the same file or in a separate `types.go` — whichever the codebase pattern dictates.
- Check if `ConnectorCertResult` already has a reusable structure to mirror.

---

## Related

- [[Services/PKI]] — existing PKI service documentation
- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — calls this function
