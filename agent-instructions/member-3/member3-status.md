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

- `controller/internal/appmeta/identity.go`
- `controller/internal/pki/workspace.go`
- `controller/internal/pki/service.go`
- `controller/cmd/server/main.go`

### Missing now

- `controller/internal/connector/spiffe.go`
- `controller/internal/connector/enrollment.go`
- `controller/internal/connector/heartbeat.go`
- `controller/proto/connector.proto`
- `controller/migrations/002_connector_schema.sql`

## Phase Status

- `[✓]` Phase 1: completed
  SPIFFE constants (`SPIFFEGlobalTrustDomain`, `SPIFFEControllerID`, `SPIFFETrustDomainPrefix/Suffix`, `SPIFFERoleConnector/Agent/Controller`, `PKIConnectorCNPrefix`, `PKIAgentCNPrefix`) and helper functions (`WorkspaceTrustDomain`, `ConnectorSPIFFEID`) added to `identity.go`. Tests written and all pass (5/5). Committed and compiles cleanly. Unblocks Member 2 + Member 4.

- `[-]` Phase 2: partially ready
  `spiffe.go` can be drafted as a standalone file, but the connector package, proto definitions, and gRPC wiring are not present yet for integration.

- `[ ]` Phase 3: blocked
  Enrollment depends on connector proto stubs, token/JTI burn support, and connector DB schema that are not present in the repo now.

- `[ ]` Phase 4: blocked
  Heartbeat depends on connector proto stubs, connector table schema, and connector package structure that are not present now.

- `[ ]` Phase 5: blocked
  Disconnect watcher depends on the same connector runtime and DB pieces as Phase 4.

- `[x]` Phase 6: ready now
  `workspace.go` and the existing PKI base are present, so `SignConnectorCert` can be added without waiting for proto or migrations.

## Dependency Breakdown

- Blocked by Member 2:
  `controller/proto/connector.proto` and related token/config/enrollment infrastructure

- Blocked by Member 4:
  `controller/migrations/002_connector_schema.sql` and connector DB/schema support

## Next Actionable Order

1. ~~Start now: Phase 1~~ **completed**
2. Start now: Phase 6
3. Optional prep only: Phase 2
4. Wait for dependencies: Phase 3
5. Wait for dependencies: Phase 4
6. Wait for dependencies: Phase 5

## Bottom Line

- Completed: Phase 1
- Fully ready now: Phase 6
- Partially actionable: Phase 2
- Blocked right now: Phase 3, Phase 4, and Phase 5
