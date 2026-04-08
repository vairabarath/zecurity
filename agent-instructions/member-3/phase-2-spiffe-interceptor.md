# Phase 2 — spiffe.go (DAY 1 — COMMIT ALONGSIDE PHASE 1)

This file contains ALL SPIFFE parsing, validation, and gRPC interception logic.
No other file in the codebase should parse SPIFFE IDs. The interceptor parses and
injects into context; handlers read from context.

---

## File to Create: `controller/internal/connector/spiffe.go`

**Path:** `controller/internal/connector/spiffe.go`

### Contents

#### 1. `parseSPIFFEID` — SPIFFE URI parser

```go
// parseSPIFFEID extracts the trust domain, role, and entity ID from a certificate's
// SPIFFE URI SAN.
// Called by: UnarySPIFFEInterceptor() below (on every gRPC call except Enroll)
//
// Requires:
//   - Exactly 1 URI SAN on the certificate
//   - URI scheme must be "spiffe://"
//   - Path must have exactly 2 segments: /<role>/<id>
//
// Returns parsed components: trustDomain, role, id
func parseSPIFFEID(cert *x509.Certificate) (trustDomain, role, id string, err error)
```

#### 2. `UnarySPIFFEInterceptor` — gRPC unary interceptor

```go
// UnarySPIFFEInterceptor returns a gRPC unary server interceptor that:
//   - Skips the "/connector.ConnectorService/Enroll" method (connector has no cert during enrollment)
//   - For all other RPCs: extracts peer cert from mTLS, parses SPIFFE ID,
//     validates trust domain, injects identity into context
//
// Called by: main.go (Member 2 wires this into the gRPC server options)
//
// Context keys injected:
//   - spiffeIDKey{}       — full SPIFFE URI string
//   - spiffeRoleKey{}     — "connector", "agent", or "controller"
//   - spiffeEntityIDKey{} — the entity-specific ID (e.g. connector UUID)
//   - trustDomainKey{}    — the trust domain from the SPIFFE URI
func UnarySPIFFEInterceptor(validator TrustDomainValidator) grpc.UnaryServerInterceptor
```

**Important:** The interceptor's skip logic checks `info.FullMethod == "/connector.ConnectorService/Enroll"`. This exact string must match the proto package + service + method.

#### 3. `TrustDomainValidator` type

```go
// TrustDomainValidator checks if a trust domain is valid (accepted by the system).
// Called by: UnarySPIFFEInterceptor() above
type TrustDomainValidator func(domain string) bool
```

#### 4. `NewTrustDomainValidator` — factory function

```go
// NewTrustDomainValidator returns a validator that:
//   - Accepts appmeta.SPIFFEGlobalTrustDomain (the controller's own domain)
//   - Accepts any workspace trust domain found via store.GetByTrustDomain(domain)
//   - Rejects everything else
//
// Called by: main.go (Member 2 creates this and passes to UnarySPIFFEInterceptor)
//
// Trust domain validation is LIVE, not cached. If a workspace is suspended,
// its trust domain becomes invalid immediately. Do NOT add a cache.
func NewTrustDomainValidator(globalDomain string, store WorkspaceStore) TrustDomainValidator
```

#### 5. Context accessor helpers

```go
// SPIFFEIDFromContext returns the full SPIFFE URI from the context.
// Called by: enrollment.go, heartbeat.go
func SPIFFEIDFromContext(ctx context.Context) string

// SPIFFERoleFromContext returns the SPIFFE role ("connector", "agent", "controller").
// Called by: heartbeat.go (Phase 4 — verifies role == "connector")
func SPIFFERoleFromContext(ctx context.Context) string

// SPIFFEEntityIDFromContext returns the entity-specific ID (e.g. connector UUID).
// Called by: heartbeat.go (Phase 4 — used as connectorID)
func SPIFFEEntityIDFromContext(ctx context.Context) string

// TrustDomainFromContext returns the trust domain from the context.
// Called by: heartbeat.go (Phase 4 — used for tenant resolution)
func TrustDomainFromContext(ctx context.Context) string
```

#### 6. `WorkspaceStore` interface

```go
// WorkspaceStore defines the DB lookup needed by the trust domain validator.
// Decouples the validator from a concrete DB type.
// Called by: NewTrustDomainValidator() above
type WorkspaceStore interface {
    GetByTrustDomain(domain string) (*Workspace, error)
}
```

---

## Phase 2 Checklist

```
✓ parseSPIFFEID requires exactly 1 URI SAN
✓ parseSPIFFEID requires spiffe:// scheme
✓ parseSPIFFEID requires exactly 2 path segments
✓ UnarySPIFFEInterceptor skips Enroll method
✓ UnarySPIFFEInterceptor extracts peer cert from mTLS
✓ UnarySPIFFEInterceptor validates trust domain via validator
✓ UnarySPIFFEInterceptor injects 4 context keys
✓ NewTrustDomainValidator accepts global trust domain
✓ NewTrustDomainValidator accepts workspace trust domains (live DB lookup)
✓ NewTrustDomainValidator rejects unknown domains
✓ All 4 context accessor helpers implemented
✓ WorkspaceStore interface defined
✓ No SPIFFE parsing logic duplicated in any other file
```
