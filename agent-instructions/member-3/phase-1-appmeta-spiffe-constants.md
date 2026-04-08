# Phase 1 — appmeta SPIFFE Constants (DAY 1 — COMMIT FIRST)

This is the identity foundation for the entire system. Every SPIFFE string in Go and Rust
traces back to these constants. Get them right, commit them early.

---

## Role & Ownership

Member 3 owns the SPIFFE identity layer, gRPC handler implementations (Enroll + Heartbeat),
the disconnect watcher, and the PKI extension for connector certificate signing.

### Files Member 3 creates

```
controller/internal/connector/spiffe.go
controller/internal/connector/enrollment.go
controller/internal/connector/heartbeat.go
```

### Files Member 3 modifies

```
controller/internal/appmeta/identity.go   ← add SPIFFE constants + helper functions
controller/internal/pki/workspace.go      ← add SignConnectorCert method
```

### DO NOT TOUCH

- `controller/proto/connector.proto` — Member 2 writes this
- `controller/internal/connector/config.go` — Member 2 writes the Config struct
- `controller/internal/connector/token.go` — Member 2 writes token generation/burn
- `controller/internal/connector/ca_endpoint.go` — Member 2 writes HTTP CA endpoint
- `controller/cmd/server/main.go` — Member 2 wires everything
- `controller/graph/` — Member 4 owns
- `controller/migrations/` — Member 4 owns
- `controller/internal/pki/service.go`, `crypto.go`, `root.go`, `intermediate.go` — Sprint 1, do NOT modify

---

## File to Modify: `controller/internal/appmeta/identity.go`

Add new constants and functions BELOW the existing sprint 1 constants. Do NOT remove or modify any existing constants.

### New constants to add

```go
// SPIFFE identity constants — the single source of truth for all SPIFFE strings.
// No other file should contain "zecurity.in", "ws-", or "connector" as string literals.
// Called by: spiffe.go, enrollment.go, heartbeat.go, token.go (Member 2), appmeta.rs (Member 4)
const (
    SPIFFEGlobalTrustDomain = "zecurity.in"
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

### New functions to add

```go
// WorkspaceTrustDomain builds the trust domain string for a workspace slug.
// Format: "ws-<slug>.zecurity.in"
// Called by: token.go (Member 2), enrollment.go (Phase 3), appmeta.rs (Member 4 mirrors)
func WorkspaceTrustDomain(slug string) string {
    return SPIFFETrustDomainPrefix + slug + SPIFFETrustDomainSuffix
}

// ConnectorSPIFFEID builds the full SPIFFE ID URI for a connector.
// Format: "spiffe://<trustDomain>/connector/<connectorID>"
// Called by: enrollment.go (Phase 3 — CSR SPIFFE SAN verification),
//           pki/workspace.go (Phase 6 — SignConnectorCert)
func ConnectorSPIFFEID(trustDomain, connectorID string) string {
    return "spiffe://" + trustDomain + "/" + SPIFFERoleConnector + "/" + connectorID
}
```

### Why this is critical

- Member 2 imports `WorkspaceTrustDomain` in token.go
- Member 4 mirrors these constants into Rust `appmeta.rs`
- Every SPIFFE string in the entire system originates from this file
- If you change a constant name after Day 1, it cascades to Member 2, Member 4, and main.go

---

## Phase 1 Checklist

```
✓ Constants added BELOW existing sprint 1 constants
✓ No existing constants removed or modified
✓ WorkspaceTrustDomain() function added
✓ ConnectorSPIFFEID() function added
✓ No string literals "zecurity.in", "ws-", "connector" duplicated elsewhere
✓ Committed and pushed — unblocks Member 2 + Member 4
```
