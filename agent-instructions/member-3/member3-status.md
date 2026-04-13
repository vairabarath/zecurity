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

- `controller/internal/appmeta/identity.go` — SPIFFE constants + helpers
- `controller/proto/connector/connector.proto` — canonical connector proto path
- `controller/proto/connector/connector.pb.go` — generated connector message types
- `controller/proto/connector/connector_grpc.pb.go` — generated connector gRPC stubs
- `controller/migrations/002_connector_schema.sql` — connector and remote network schema
- `controller/internal/connector/config.go` — Member 2 connector config contract
- `controller/internal/connector/token.go` — Member 2 token/JTI helpers, including `VerifyEnrollmentToken`
- `controller/internal/connector/ca_endpoint.go` — Member 2 DB-backed CA endpoint
- `controller/internal/connector/spiffe.go` — SPIFFE parsing, validation, interceptor, and context helpers
- `controller/internal/connector/spiffe_test.go` — SPIFFE unit tests
- `controller/internal/connector/enrollment.go` — enrollment gRPC handler
- `controller/internal/connector/heartbeat.go` — heartbeat handler and disconnect watcher
- `controller/internal/pki/workspace.go` — workspace CA plus `SignConnectorCert`
- `controller/internal/pki/service.go` — PKI service interface including `SignConnectorCert`
- `controller/cmd/server/main.go` — Member 2 fallback wiring; final service/interceptor wiring still needs follow-up

### Missing now

- No Member 3 runtime source files are missing.
- Final `main.go` integration remains a Member 2 follow-up after reviewing the landed Member 3 service/interceptor API.

## Phase Status

- `[✓]` Phase 1: completed
  SPIFFE constants and helper functions (`WorkspaceTrustDomain`, `ConnectorSPIFFEID`) are present in `identity.go`.

- `[✓]` Phase 2: completed
  `spiffe.go` contains SPIFFE ID parsing, context accessors, trust-domain validation, and the unary SPIFFE interceptor.

- `[✓]` Phase 3: completed
  `enrollment.go` implements the enrollment handler and consumes Member 2's shared `VerifyEnrollmentToken` and `BurnEnrollmentJTI` helpers.

- `[✓]` Phase 4: completed
  `heartbeat.go` implements the heartbeat handler using SPIFFE identity from context.

- `[✓]` Phase 5: completed
  `heartbeat.go` includes the disconnect watcher behavior using `HeartbeatInterval` and `DisconnectThreshold`.

- `[✓]` Phase 6: completed
  `workspace.go` implements `SignConnectorCert`, and `service.go` exposes `ConnectorCertResult` plus the service method.

## Dependency Breakdown

- Blocked by Member 2:
  no current Member 3 runtime blocker. Member 2 still needs to replace fallback TODOs in `main.go` with the final landed interceptor and service registration.

- Blocked by Member 4:
  no current Member 3 runtime blocker at the schema layer; `controller/migrations/002_connector_schema.sql` is present.

## Next Actionable Order

1. ~~Phase 1~~ completed
2. ~~Phase 2~~ completed
3. ~~Phase 3~~ completed
4. ~~Phase 4~~ completed
5. ~~Phase 5~~ completed
6. ~~Phase 6~~ completed
7. Next integration step: Member 2 should update `main.go` from fallback gRPC wiring to real service/interceptor registration.

## Bottom Line

- Member 3 runtime source work is now present.
- Canonical contract remains `controller/proto/connector/connector.proto`.
- Token verification remains shared through `VerifyEnrollmentToken` in `controller/internal/connector/token.go`.
- The CA endpoint remains `CAEndpointHandler(pool *pgxpool.Pool)`.
- Remaining backend integration work is final `main.go` wiring and verification.
