# Phase 2 — ConnectorConfig Struct

## Objective

Create the `Config` struct that holds all tunable settings for the connector subsystem. This struct is the **contract between Member 2 and Member 3** — every handler and service in `internal/connector/` receives this struct instead of reading env vars directly.

---

## Prerequisites

- None. Can start Day 1 alongside Phase 1.

---

## File to Create

```
controller/internal/connector/config.go
```

---

## Implementation

**File: `controller/internal/connector/config.go`**

```go
package connector

import "time"

// Config holds all tunable settings for the connector subsystem.
// Populated in main.go from environment variables (Phase 5).
// Passed into every service and handler that needs these values.
//
// Rule: if a value might differ between dev, staging, and prod,
// it belongs here — not hardcoded in a handler.
type Config struct {
	// CertTTL is the validity window for connector leaf certificates.
	// Default: 168h (7 days) per GROK final instruction.
	// Env: CONNECTOR_CERT_TTL
	CertTTL time.Duration

	// EnrollmentTokenTTL is the Redis TTL for single-use enrollment JWTs.
	// Default: 24h
	// Env: CONNECTOR_ENROLLMENT_TOKEN_TTL
	EnrollmentTokenTTL time.Duration

	// HeartbeatInterval is how often connectors are expected to heartbeat.
	// The disconnect watcher uses this to derive the check cadence.
	// Default: 30s
	// Env: CONNECTOR_HEARTBEAT_INTERVAL
	HeartbeatInterval time.Duration

	// DisconnectThreshold is how long without a heartbeat before a connector
	// is marked DISCONNECTED. Must always be > 3x HeartbeatInterval.
	// Default: 90s
	// Env: CONNECTOR_DISCONNECT_THRESHOLD
	DisconnectThreshold time.Duration

	// GRPCPort is the port the gRPC server listens on.
	// Default: "9090"
	// Env: GRPC_PORT
	GRPCPort string

	// JWTSecret is reused from the existing auth config — same secret,
	// used to sign and verify enrollment tokens.
	// Env: JWT_SECRET (already required from sprint 1)
	JWTSecret string
}
```

---

## Verification

```bash
cd controller && go build ./internal/connector/...
```

Should compile with zero errors. The package has no external imports beyond `time`.

- [ ] File exists at `controller/internal/connector/config.go`
- [ ] Package declaration is `package connector`
- [ ] All 6 fields present: `CertTTL`, `EnrollmentTokenTTL`, `HeartbeatInterval`, `DisconnectThreshold`, `GRPCPort`, `JWTSecret`
- [ ] `go build ./internal/connector/...` passes

---

## DO NOT TOUCH

- Do not create `token.go`, `ca_endpoint.go`, or any other file in this phase — those are Phase 3 and 4
- Do not add methods to this struct — Member 3's handlers receive it as a value
- Do not read env vars here — main.go (Phase 5) populates this struct

---

## Notes for Member 3

This struct is your config contract. If you need a new field (e.g., a Redis URL, a DB pool), coordinate with Member 2 to add it here. Do not create a parallel config struct.

**Field names are final after this phase.** If Member 2 renames a field, Member 3 must be notified immediately to update handler code.

---

## After This Phase

Proceed to Phase 3 (token.go).
