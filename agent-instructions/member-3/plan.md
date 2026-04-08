# Member 3 — Go PKI + gRPC Handlers + SPIFFE Core

## Role

You own the SPIFFE identity layer, the gRPC handler implementations (Enroll + Heartbeat), the disconnect watcher, and the PKI extension for connector certificate signing. You define the security-critical identity constants that every other member imports. Your Day 1 deliverables (appmeta additions + spiffe.go) unblock Member 2's main.go wiring and Member 4's Rust appmeta constants.

---

## Your Files (CREATE or MODIFY only these)

### New files you create

```
controller/internal/connector/spiffe.go
controller/internal/connector/enrollment.go
controller/internal/connector/heartbeat.go
```

### Files you modify

```
controller/internal/appmeta/identity.go   ← add SPIFFE constants + helper functions
controller/internal/pki/workspace.go      ← add SignConnectorCert method
```

---

## DO NOT TOUCH — Conflict Boundaries

- **`controller/proto/connector.proto`** — Member 2 writes this. You consume the generated stubs.
- **`controller/internal/connector/config.go`** — Member 2 writes the Config struct. You receive it as a parameter; never create your own config.
- **`controller/internal/connector/token.go`** — Member 2 writes token generation/burn. You call `BurnEnrollmentJTI` in your enrollment handler; never reimplement it.
- **`controller/internal/connector/ca_endpoint.go`** — Member 2 writes the HTTP CA endpoint.
- **`controller/cmd/server/main.go`** — Member 2 wires everything in main.go. Do not edit main.go.
- **`controller/graph/schema.graphqls`** — Member 4 modifies the schema.
- **`controller/graph/resolvers/connector.resolvers.go`** — Member 4 writes resolvers.
- **`controller/migrations/*`** — Member 4 owns all migration files.
- **`controller/internal/pki/service.go`** — Do not modify the service interface file. Add your new method to `workspace.go` and update the interface if needed via coordination with the team.
- **`controller/internal/pki/crypto.go`** — Sprint 1 crypto helpers. Do not modify. Reuse existing functions (`newSerialNumber`, `encryptPrivateKey`, `decryptPrivateKey`, `encodeCertToPEM`, `parseCertFromPEM`).
- **`controller/internal/pki/root.go`** — Sprint 1 root CA. Do not modify.
- **`controller/internal/pki/intermediate.go`** — Sprint 1 intermediate CA. Do not modify.
- **`connector/`** — Member 4 owns the Rust binary.
- **`admin/`** — Member 1 owns all frontend code.
- **`.github/`** — Member 4 owns CI.

---

## Phase-by-Phase Plan

### Phase 1 — appmeta.go SPIFFE Constants (DAY 1 — COMMIT FIRST)

**Modify: `controller/internal/appmeta/identity.go`**

Add new constants and functions BELOW the existing sprint 1 constants. Do NOT remove or modify any existing constants.

**New constants to add:**

```go
// SPIFFE identity constants
const (
    SPIFFEGlobalTrustDomain = "zecurity.io"
    SPIFFEControllerID      = "spiffe://" + SPIFFEGlobalTrustDomain + "/controller/global"
    SPIFFETrustDomainPrefix = "ws-"
    SPIFFETrustDomainSuffix = "." + SPIFFEGlobalTrustDomain
    SPIFFERoleConnector     = "connector"
    SPIFFERoleAgent         = "agent"
    SPIFFERoleController    = "controller"
    PKIConnectorCNPrefix    = "connector-"
    PKIAgentCNPrefix        = "agent-"
)
```

**New functions to add:**

```go
func WorkspaceTrustDomain(slug string) string {
    return SPIFFETrustDomainPrefix + slug + SPIFFETrustDomainSuffix
}

func ConnectorSPIFFEID(trustDomain, connectorID string) string {
    return "spiffe://" + trustDomain + "/" + SPIFFERoleConnector + "/" + connectorID
}
```

**Why this is critical:** Member 2 imports `WorkspaceTrustDomain` in token.go. Member 4 mirrors these constants into Rust `appmeta.rs`. Every SPIFFE string in the entire system originates from this file. No other file should contain `"zecurity.io"`, `"ws-"`, or `"connector"` as string literals.

### Phase 2 — spiffe.go (DAY 1 — COMMIT ALONGSIDE PHASE 1)

**Create: `controller/internal/connector/spiffe.go`**

This file contains ALL SPIFFE parsing, validation, and gRPC interception logic. No other file in the codebase should parse SPIFFE IDs.

**Contents:**

1. **`parseSPIFFEID(cert *x509.Certificate) (trustDomain, role, id string, err error)`**
   - Requires exactly 1 URI SAN
   - Requires `spiffe://` scheme
   - Requires exactly 2 path segments: `/<role>/<id>`
   - Returns parsed components

2. **`UnarySPIFFEInterceptor(validator TrustDomainValidator) grpc.UnaryServerInterceptor`**
   - Skips the `/connector.ConnectorService/Enroll` method (connector has no cert during enrollment)
   - For all other RPCs: extracts peer cert from mTLS, parses SPIFFE ID, validates trust domain, injects identity into context
   - Context keys: `spiffeIDKey{}`, `spiffeRoleKey{}`, `spiffeEntityIDKey{}`, `trustDomainKey{}`

3. **`TrustDomainValidator` type** — `func(domain string) bool`

4. **`NewTrustDomainValidator(globalDomain string, store WorkspaceStore) TrustDomainValidator`**
   - Accepts `appmeta.SPIFFEGlobalTrustDomain` (the controller's own domain)
   - Accepts any workspace trust domain found via `store.GetByTrustDomain(domain)`
   - Rejects everything else

5. **Context accessor helpers:**
   - `SPIFFEIDFromContext(ctx) string`
   - `SPIFFERoleFromContext(ctx) string`
   - `SPIFFEEntityIDFromContext(ctx) string`
   - `TrustDomainFromContext(ctx) string`

6. **`WorkspaceStore` interface** — defines `GetByTrustDomain(domain string) (*Workspace, error)` so the validator doesn't depend on a concrete DB type.

### Phase 3 — Enrollment Handler

**Create: `controller/internal/connector/enrollment.go`**

Implements the `Enroll` gRPC handler.

**Flow:**

1. Verify JWT signature using `cfg.JWTSecret`, check `exp`, verify `iss == appmeta.ControllerIssuer`
2. Extract `jti`, `connector_id`, `workspace_id`, `trust_domain` from JWT claims
3. Call Member 2's `BurnEnrollmentJTI(ctx, redis, jti)` — atomic GET+DEL
   - Not found → `codes.PermissionDenied` ("token expired or already used")
4. Load connector row: verify `status='pending'`, verify `tenant_id == workspace_id`
   - Fail → `codes.PermissionDenied`
5. Verify workspace `status='active'`
   - Fail → `codes.FailedPrecondition`
6. Parse CSR from `request.csr_der`
7. Verify CSR self-signature (proves connector holds the private key)
8. Verify CSR SPIFFE SAN matches expected:
   ```go
   expected := appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
   ```
   - Mismatch → `codes.PermissionDenied` ("SPIFFE ID in CSR does not match token")
9. Call `pki.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, cfg.CertTTL)`
10. UPDATE connector row: `status='active'`, `trust_domain`, `cert_serial`, `cert_not_after`, `hostname`, `version`, `last_heartbeat_at=NOW()`, `enrollment_token_jti=NULL`
11. Return `EnrollResponse` with signed cert PEM, workspace CA PEM, intermediate CA PEM, and connector ID

### Phase 4 — Heartbeat Handler

**Create: `controller/internal/connector/heartbeat.go`**

Implements the `Heartbeat` gRPC handler. The SPIFFE interceptor has already validated the certificate before this code runs.

**Flow:**

1. Read identity from context (injected by interceptor):
   - `trustDomain = TrustDomainFromContext(ctx)`
   - `role = SPIFFERoleFromContext(ctx)`
   - `connectorID = SPIFFEEntityIDFromContext(ctx)`
2. Verify `role == appmeta.SPIFFERoleConnector` → else `codes.PermissionDenied`
3. Resolve tenant: `SELECT tenant_id FROM connectors WHERE id = $1 AND trust_domain = $2`
4. Verify not revoked: check connector `status != 'revoked'` → else `codes.PermissionDenied`
5. Update connector: `last_heartbeat_at=NOW()`, `version`, `hostname`, `public_ip`, `status='active'`
6. Return `HeartbeatResponse{Ok: true, LatestVersion: "...", ReEnroll: false}`

**`re_enroll` is always `false` this sprint.** The field exists for next sprint's auto-renewal. Do not add renewal logic.

### Phase 5 — Disconnect Watcher

Add to `heartbeat.go` (same file as the handler).

**Function: `runDisconnectWatcher(ctx context.Context)`**

- Runs as a background goroutine, started alongside the gRPC server
- Ticks every `cfg.HeartbeatInterval`
- Marks connectors `DISCONNECTED` where `status='active'` and `last_heartbeat_at < NOW() - cfg.DisconnectThreshold`
- Only affects connectors in active workspaces

```sql
UPDATE connectors
   SET status = 'disconnected', updated_at = NOW()
 WHERE status = 'active'
   AND last_heartbeat_at < NOW() - $1
   AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')
```

Uses `cfg.HeartbeatInterval` and `cfg.DisconnectThreshold` — no hardcoded durations.

### Phase 6 — SignConnectorCert (PKI Extension)

**Modify: `controller/internal/pki/workspace.go`**

Add a new method to the existing PKI service. Do NOT modify existing methods.

**Method: `SignConnectorCert(ctx, tenantID, connectorID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*ConnectorCertResult, error)`**

- Build SPIFFE URI via `appmeta.ConnectorSPIFFEID(trustDomain, connectorID)`
- Set `Subject.CommonName` to `appmeta.PKIConnectorCNPrefix + connectorID`
- Set `Subject.Organization` to `appmeta.PKIWorkspaceOrganization`
- Single URI SAN = the SPIFFE URI
- `NotAfter = now + certTTL` (7 days from `cfg.CertTTL`)
- `KeyUsage = DigitalSignature`
- `ExtKeyUsage = ClientAuth`
- `IsCA = false`
- Load and decrypt workspace CA key (same pattern as `GenerateWorkspaceCA`)
- Sign with workspace CA
- Return `ConnectorCertResult{CertificatePEM, Serial, NotBefore, NotAfter}`

Reuse existing helpers: `newSerialNumber`, `encodeCertToPEM`, etc.

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

## Special Instructions

1. **Your appmeta commit is the identity foundation.** Every SPIFFE string in Go and Rust traces back to your constants. Get them right, commit them early. If you change a constant name after Day 1, it cascades to Member 2 (token.go), Member 4 (appmeta.rs), and potentially main.go.

2. **spiffe.go is the ONLY file that parses SPIFFE IDs.** Do not duplicate `parseSPIFFEID` logic in enrollment.go or heartbeat.go. The interceptor parses and injects into context; handlers read from context.

3. **You call Member 2's token functions, not the reverse.** Your enrollment handler calls `BurnEnrollmentJTI` from Member 2's `token.go`. You do not implement token storage. If you need to verify the JWT signature in enrollment.go, do it locally (it's just HMAC verification) but coordinate the signing/verification key handling with Member 2.

4. **Config comes from Member 2.** Your handlers receive `connector.Config` as a struct parameter. Never read environment variables directly in handler code. If you need a new config field, ask Member 2 to add it to the struct.

5. **The interceptor skips Enroll.** During enrollment the connector has no certificate yet — it uses plain TLS with the enrollment JWT. The interceptor's skip logic checks `info.FullMethod == "/connector.ConnectorService/Enroll"`. Make sure this exact string matches the proto package + service + method.

6. **Do not modify existing PKI files beyond workspace.go.** `service.go`, `crypto.go`, `root.go`, `intermediate.go` are sprint 1 code. Reuse their helpers but don't modify them. Your new `SignConnectorCert` goes in `workspace.go` alongside the existing `GenerateWorkspaceCA`.

7. **Trust domain validation is live, not cached.** `NewTrustDomainValidator` does a DB lookup per call. This is intentional — if a workspace is suspended, its trust domain becomes invalid immediately. Do not add a cache.

8. **`re_enroll = false` always this sprint.** Do not add cert expiry checking or renewal logic. Just return `false`. The proto field exists so next sprint doesn't need a proto change.
