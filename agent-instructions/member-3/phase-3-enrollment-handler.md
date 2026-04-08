# Phase 3 — Enrollment Handler

Implements the `Enroll` gRPC handler. This is the most complex handler — every step
is sequential and security-critical.

---

## Dependencies (must be available before starting)

- Member 2's proto stubs generated (from `connector.proto`)
- Member 2's `BurnEnrollmentJTI` function (from `token.go`)
- Member 4's DB migration (connector table schema)

---

## File to Create: `controller/internal/connector/enrollment.go`

**Path:** `controller/internal/connector/enrollment.go`

### Flow

```go
// Enroll implements the ConnectorService.Enroll gRPC handler.
// Called by: gRPC server (registered via proto-generated service definition)
//
// NOTE: The SPIFFE interceptor SKIPS this method — the connector has no certificate
// during enrollment. Authentication is via the enrollment JWT.
//
// Full sequence:
//   1. Verify JWT signature using cfg.JWTSecret, check exp, verify iss == appmeta.ControllerIssuer
//   2. Extract jti, connector_id, workspace_id, trust_domain from JWT claims
//   3. Call Member 2's BurnEnrollmentJTI(ctx, redis, jti) — atomic GET+DEL
//      - Not found → codes.PermissionDenied ("token expired or already used")
//   4. Load connector row: verify status='pending', verify tenant_id == workspace_id
//      - Fail → codes.PermissionDenied
//   5. Verify workspace status='active'
//      - Fail → codes.FailedPrecondition
//   6. Parse CSR from request.csr_der
//   7. Verify CSR self-signature (proves connector holds the private key)
//   8. Verify CSR SPIFFE SAN matches expected:
//        expected := appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
//      - Mismatch → codes.PermissionDenied ("SPIFFE ID in CSR does not match token")
//   9. Call pki.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, cfg.CertTTL)
//  10. UPDATE connector row: status='active', trust_domain, cert_serial, cert_not_after,
//      hostname, version, last_heartbeat_at=NOW(), enrollment_token_jti=NULL
//  11. Return EnrollResponse with signed cert PEM, workspace CA PEM, intermediate CA PEM,
//      and connector ID
```

### Key implementation notes

- **JWT verification is done locally** — it's just HMAC verification using `cfg.JWTSecret`. Coordinate the signing/verification key handling with Member 2.
- **BurnEnrollmentJTI is from Member 2's token.go** — you call it, you don't implement token storage.
- **Config comes from Member 2** — your handler receives `connector.Config` as a struct parameter. Never read environment variables directly.
- **CSR SPIFFE SAN verification** uses `appmeta.ConnectorSPIFFEID()` from Phase 1.
- **SignConnectorCert** is from Phase 6 (PKI extension).

---

## Phase 3 Checklist

```
✓ JWT signature verified with cfg.JWTSecret
✓ JWT exp checked
✓ JWT iss checked against appmeta.ControllerIssuer
✓ jti, connector_id, workspace_id, trust_domain extracted from claims
✓ BurnEnrollmentJTI called (atomic single-use)
✓ Not found → codes.PermissionDenied
✓ Connector row loaded and status='pending' verified
✓ tenant_id == workspace_id verified
✓ Workspace status='active' verified
✓ CSR parsed from request.csr_der
✓ CSR self-signature verified
✓ CSR SPIFFE SAN matches appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
✓ Mismatch → codes.PermissionDenied
✓ pki.SignConnectorCert called
✓ Connector row updated: status='active', cert fields, enrollment_token_jti=NULL
✓ EnrollResponse returned with cert PEM, workspace CA PEM, intermediate CA PEM, connector ID
```
