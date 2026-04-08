# Phase 6 — SignConnectorCert (PKI Extension)

Adds a new method to the existing PKI service for signing connector certificates.
Do NOT modify existing methods — only add the new one to `workspace.go`.

---

## Dependencies

- Phase 1 (appmeta constants — `ConnectorSPIFFEID`, `PKIConnectorCNPrefix`, `PKIWorkspaceOrganization`)
- Existing PKI code: `crypto.go` helpers (`newSerialNumber`, `encryptPrivateKey`, `decryptPrivateKey`, `encodeCertToPEM`, `parseCertFromPEM`)

---

## File to Modify: `controller/internal/pki/workspace.go`

Add alongside the existing `GenerateWorkspaceCA` method.

### Method: `SignConnectorCert`

```go
// SignConnectorCert signs a connector's CSR with the workspace CA, producing
// a short-lived client certificate with the connector's SPIFFE ID as URI SAN.
// Called by: enrollment.go (Phase 3, Step 9)
//
// Parameters:
//   - tenantID: workspace ID (for loading the workspace CA key)
//   - connectorID: connector UUID (for CN and SPIFFE ID)
//   - trustDomain: workspace trust domain (for SPIFFE ID)
//   - csr: parsed x509.CertificateRequest from the connector
//   - certTTL: certificate lifetime (typically 7 days from cfg.CertTTL)
//
// Certificate properties:
//   - Subject.CommonName = appmeta.PKIConnectorCNPrefix + connectorID
//   - Subject.Organization = appmeta.PKIWorkspaceOrganization
//   - Single URI SAN = appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
//   - NotAfter = now + certTTL
//   - KeyUsage = DigitalSignature
//   - ExtKeyUsage = ClientAuth
//   - IsCA = false
//   - Signed by workspace CA (loaded + decrypted from workspace_ca_keys table)
//
// Returns ConnectorCertResult{CertificatePEM, Serial, NotBefore, NotAfter}
//
// Reuses existing helpers: newSerialNumber(), encodeCertToPEM(), decryptPrivateKey()
func (s *service) SignConnectorCert(
    ctx context.Context,
    tenantID, connectorID, trustDomain string,
    csr *x509.CertificateRequest,
    certTTL time.Duration,
) (*ConnectorCertResult, error)
```

### ConnectorCertResult struct

```go
// ConnectorCertResult holds the output of SignConnectorCert.
// Called by: enrollment.go (Phase 3) to build the EnrollResponse.
type ConnectorCertResult struct {
    CertificatePEM string
    Serial         string
    NotBefore      time.Time
    NotAfter       time.Time
}
```

### Key implementation notes

- **Do NOT modify existing PKI files** beyond `workspace.go` — `service.go`, `crypto.go`, `root.go`, `intermediate.go` are sprint 1 code.
- **Reuse existing helpers** — `newSerialNumber`, `encodeCertToPEM`, `decryptPrivateKey`, etc.
- **Load workspace CA key** from `workspace_ca_keys` table (same pattern as `GenerateWorkspaceCA`).
- **SPIFFE URI** built via `appmeta.ConnectorSPIFFEID(trustDomain, connectorID)`.
- **KeyUsage = DigitalSignature** only (no KeyEncipherment — this is a client cert).
- **ExtKeyUsage = ClientAuth** only (connector authenticates to the controller via mTLS).
- **IsCA = false** — leaf certificate, cannot sign other certificates.

---

## Phase 6 Checklist

```
✓ ConnectorCertResult struct defined
✓ SignConnectorCert method added to workspace.go
✓ CommonName = PKIConnectorCNPrefix + connectorID
✓ Organization = PKIWorkspaceOrganization
✓ URI SAN = ConnectorSPIFFEID(trustDomain, connectorID)
✓ NotAfter = now + certTTL
✓ KeyUsage = DigitalSignature
✓ ExtKeyUsage = ClientAuth
✓ IsCA = false
✓ Workspace CA loaded and decrypted
✓ Certificate signed by workspace CA
✓ Existing helpers reused (newSerialNumber, encodeCertToPEM, etc.)
✓ No existing PKI methods modified
```

---

## Dependency Timeline

```
Day 1:  Phase 1 (appmeta constants) — COMMIT FIRST, unblocks Member 2 + 4
        Phase 2 (spiffe.go) — COMMIT ALONGSIDE, unblocks Member 2's main.go wiring
        Phase 6 (SignConnectorCert) — can start, depends only on existing PKI code

Day 2:  Phase 3 (enrollment.go) — needs:
          - Member 2's proto stubs generated (from connector.proto)
          - Member 2's BurnEnrollmentJTI function
          - Member 4's DB migration (connector table schema)
        Phase 4 + 5 (heartbeat + watcher) — needs:
          - Proto stubs
          - DB migration
```

---

## Special Instructions Summary

1. Your appmeta commit is the identity foundation — commit early, don't change names after Day 1
2. spiffe.go is the ONLY file that parses SPIFFE IDs — no duplication
3. You call Member 2's token functions, not the reverse
4. Config comes from Member 2 — never read env vars directly in handlers
5. The interceptor skips Enroll — exact method string must match proto
6. Do not modify existing PKI files beyond workspace.go
7. Trust domain validation is live, not cached — no cache
8. `re_enroll = false` always this sprint — no renewal logic
