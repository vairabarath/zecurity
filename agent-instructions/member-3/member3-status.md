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
- `controller/proto/connector/connector.proto`
- `controller/migrations/002_connector_schema.sql`
- `controller/internal/connector/config.go`
- `controller/internal/connector/token.go`

### Missing now

- `controller/internal/connector/spiffe.go`
- `controller/internal/connector/enrollment.go`
- `controller/internal/connector/heartbeat.go`

## Phase Status

- `[✓]` Phase 1: completed
  SPIFFE constants (`SPIFFEGlobalTrustDomain`, `SPIFFEControllerID`, `SPIFFETrustDomainPrefix/Suffix`, `SPIFFERoleConnector/Agent/Controller`, `PKIConnectorCNPrefix`, `PKIAgentCNPrefix`) and helper functions (`WorkspaceTrustDomain`, `ConnectorSPIFFEID`) added to `identity.go`. Tests written and all pass (5/5). Committed and compiles cleanly. Unblocks Member 2 + Member 4.

- `[-]` Phase 2: partially ready
  `spiffe.go` is still missing, but the connector package, proto definitions, and fallback gRPC wiring are now present for integration.

- `[x]` Phase 3: ready now
  Enrollment can now be implemented against the current shared contract: nested proto path, Member 2 token helpers in `controller/internal/connector/token.go`, and Member 4's connector schema migration.

- `[x]` Phase 4: ready now
  Heartbeat can now be implemented because the connector proto, package structure, and connector DB schema are all present in the repo.

- `[x]` Phase 5: ready now
  Disconnect watcher remains unimplemented, but it is no longer externally blocked; it should be built in `heartbeat.go` alongside the heartbeat handler.

- `[x]` Phase 6: ready now
  `workspace.go` and the existing PKI base are present, so `SignConnectorCert` can be added without waiting for proto or migrations.

## Dependency Breakdown

- Blocked by Member 2:
  nothing critical at the contract layer now; Member 2's proto/config/token/CA endpoint/main wiring contracts are present

- Blocked by Member 4:
  nothing critical at the schema layer now; `controller/migrations/002_connector_schema.sql` is present

## Next Actionable Order

1. ~~Start now: Phase 1~~ **completed**
2. Start now: Phase 2
3. Start now: Phase 3
4. Start now: Phase 4
5. Start now: Phase 5
6. Continue: Phase 6

## Bottom Line

- Completed: Phase 1
- Fully ready now: Phase 2, Phase 3, Phase 4, Phase 5, and Phase 6
- Remaining implementation gap: `spiffe.go`, `enrollment.go`, and `heartbeat.go`
