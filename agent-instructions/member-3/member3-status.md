# Member 3 Status

This file verifies the Member 3 plan against the current repository state.
It is a status overlay on top of the existing `plan.md` and `phase-*.md` files.

Status legend:
- `[x]` ready now
- `[-]` partially ready
- `[ ]` blocked
- `[✓]` completed

## Current Repo Verification

### Present now

- `controller/internal/appmeta/identity.go` — SPIFFE constants + helpers (Phase 1 done)
- `controller/internal/pki/workspace.go` — GenerateWorkspaceCA + **SignConnectorCert** (Phase 6 done)
- `controller/internal/pki/service.go` — Service interface extended with **SignConnectorCert**
- `controller/internal/pki/crypto.go` — helpers: newSerialNumber, encodeCertToPEM, encryptPrivateKey, decryptPrivateKey, parseCertFromPEM, certValidity, generateECKeyPair, zeroBytes
- `controller/cmd/server/main.go`
- `controller/proto/connector/connector.proto` — defines ConnectorService with Enroll + Heartbeat RPCs
- `controller/proto/connector/connector.pb.go` — generated message types (EnrollRequest, EnrollResponse, HeartbeatRequest, HeartbeatResponse)
- `controller/proto/connector/connector_grpc.pb.go` — generated gRPC server/client stubs
- `controller/migrations/002_connector_schema.sql` — connectors table, remote_networks table, workspace trust_domain column
- `controller/internal/connector/config.go` — Config struct (CertTTL, EnrollmentTokenTTL, HeartbeatInterval, DisconnectThreshold, GRPCPort, JWTSecret)
- `controller/internal/connector/token.go` — GenerateEnrollmentToken, VerifyEnrollmentToken, StoreEnrollmentJTI, BurnEnrollmentJTI (all implemented)
- `controller/internal/connector/spiffe.go` — **Phase 2 done**: parseSPIFFEID, UnarySPIFFEInterceptor, context helpers, TrustDomainValidator, WorkspaceStore interface
- `controller/internal/connector/spiffe_test.go` — SPIFFE unit tests
- `controller/internal/connector/enrollment.go` — **Phase 3 done**: EnrollmentHandler with Enroll gRPC handler (full 11-step flow)
- `controller/internal/connector/enrollment_test.go` — **Phase 3 tests**: 14 tests (8 unit + 6 integration) — all pass
- `controller/internal/connector/ca_endpoint.go` — HTTP handler serving intermediate CA cert at GET /ca.crt
- `controller/go.mod` — has google.golang.org/grpc v1.80.0 and google.golang.org/protobuf v1.36.11

### Still missing (Member 3 creates these)

- `controller/internal/connector/heartbeat.go` — Heartbeat gRPC handler + disconnect watcher (Phases 4 + 5)

## Phase Status

- `[✓]` Phase 1: completed
  SPIFFE constants (`SPIFFEGlobalTrustDomain`, `SPIFFEControllerID`, `SPIFFETrustDomainPrefix/Suffix`, `SPIFFERoleConnector/Agent/Controller`, `PKIConnectorCNPrefix`, `PKIAgentCNPrefix`) and helper functions (`WorkspaceTrustDomain`, `ConnectorSPIFFEID`) added to `identity.go`. Tests written and all pass (5/5). Committed and compiles cleanly. Unblocks Member 2 + Member 4.

- `[✓]` Phase 2: completed
  `spiffe.go` fully implemented with:
  - `parseSPIFFEID()` — extracts trustDomain, role, entityID from x509 cert URI SAN
  - Context helpers: `SPIFFEIDFromContext`, `SPIFFERoleFromContext`, `SPIFFEEntityIDFromContext`, `TrustDomainFromContext`
  - `WorkspaceStore` interface + `WorkspaceLookup` struct (decoupled from models)
  - `NewTrustDomainValidator()` — live DB lookup, no cache, accepts global domain + active workspaces
  - `UnarySPIFFEInterceptor()` — skips Enroll, validates mTLS cert, injects SPIFFE identity into context
  - `spiffe_test.go` — unit tests present

- `[✓]` Phase 3: completed
  `enrollment.go` fully implemented with `EnrollmentHandler` struct and `Enroll()` method:
  - Step 1: Verify JWT via `VerifyEnrollmentToken()`
  - Step 2: Extract jti, connectorID, workspaceID, trustDomain from claims
  - Step 3: Burn JTI via `BurnEnrollmentJTI()` (atomic GET+DEL)
  - Step 4: Load connector row, verify status='pending', tenant match
  - Step 5: Verify workspace status='active'
  - Step 6: Parse CSR from DER bytes
  - Step 7: Verify CSR self-signature
  - Step 8: Verify CSR SPIFFE SAN matches expected identity
  - Step 9: Sign cert via `pki.SignConnectorCert()`
  - Step 10: UPDATE connector to 'active' with cert serial, expiry, hostname, version
  - Step 11: Load CA certs, return `EnrollResponse`
  - Helper: `loadCACerts()` — fetches workspace CA + intermediate CA from DB
  - Helper: `csrHasSPIFFEURI()` — checks CSR for expected SPIFFE URI SAN
  - Also: `ca_endpoint.go` — serves intermediate CA cert at GET /ca.crt

- `[✓]` Phase 6: completed
  `SignConnectorCert` implemented in `workspace.go`:
  - Loads workspace CA key from `workspace_ca_keys` table
  - Decrypts CA private key via `decryptPrivateKey()`
  - Parses CA cert via `parseCertFromPEM()`
  - Generates serial via `newSerialNumber()`
  - Builds SPIFFE URI SAN via `appmeta.ConnectorSPIFFEID()`
  - Signs connector CSR with workspace CA (client cert, ExtKeyUsageClientAuth)
  - Returns `ConnectorCertResult{CertificatePEM, Serial, NotBefore, NotAfter}`
  - `pki.Service` interface extended with `SignConnectorCert` method
  - `ConnectorCertResult` struct defined in `service.go`

- `[x]` Phase 4: READY NOW (was: blocked)
  ALL dependencies resolved:
  - Proto stubs: HeartbeatRequest/HeartbeatResponse generated
  - Connector table schema exists with status, last_heartbeat_at, version, hostname, public_ip
  - Phase 2 (spiffe.go context helpers) ✅ COMPLETED — can be used now
  Can now implement heartbeat.go.

- `[x]` Phase 5: READY NOW (was: blocked)
  Same deps as Phase 4 — all resolved.
  Config struct has HeartbeatInterval and DisconnectThreshold fields.
  Can now add disconnect watcher to heartbeat.go.

## Dependency Breakdown

- ~~Blocked by Member 2:~~ **ALL RESOLVED**
  - `controller/proto/connector/connector.proto` ✅ EXISTS + generated stubs
  - `controller/internal/connector/token.go` ✅ BurnEnrollmentJTI, VerifyEnrollmentToken implemented
  - `controller/internal/connector/config.go` ✅ Config struct with all fields

- ~~Blocked by Member 4:~~ **ALL RESOLVED**
  - `controller/migrations/002_connector_schema.sql` ✅ connectors + remote_networks tables
  - gRPC deps in go.mod ✅ grpc v1.80.0, protobuf v1.36.11

## Next Actionable Order

1. ~~Start now: Phase 1~~ **completed**
2. ~~Phase 6~~ **completed**
3. ~~Phase 2~~ **completed**
4. ~~Phase 3~~ **completed**
5. **Start now: Phase 4** (heartbeat.go — after Phase 2 ✅)
6. **Start now: Phase 5** (disconnect watcher — after Phase 4)

## Recommended implementation order

Phase 4 → Phase 5

Reason: Phase 2 and Phase 6 are done. Phase 3 is done. Only heartbeat.go remains.
Phase 4 (Heartbeat handler) and Phase 5 (disconnect watcher) can be implemented together in a single `heartbeat.go` file.

## Bottom Line

- Completed: Phase 1, Phase 2, Phase 3, Phase 6
- **2 remaining phases (4 + 5) are FULLY UNBLOCKED and ready to implement**
- Only `controller/internal/connector/heartbeat.go` needs to be created
- No external dependencies remain
