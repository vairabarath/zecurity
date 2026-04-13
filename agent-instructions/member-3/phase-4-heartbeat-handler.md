# Phase 4 — Heartbeat Handler

Implements the `Heartbeat` gRPC handler. The SPIFFE interceptor has already validated
the certificate before this code runs — identity is read from context, not parsed again.

---

## Dependencies

- Proto stubs (from Member 2's `connector.proto`)
- DB migration with connector table (Member 4)
- Phase 2 (spiffe.go context helpers)

---

## File to Create: `controller/internal/connector/heartbeat.go`

**Path:** `controller/internal/connector/heartbeat.go`

### Flow

```go
// Heartbeat implements the ConnectorService.Heartbeat gRPC handler.
// Called by: gRPC server (registered via proto-generated service definition)
//
// NOTE: The SPIFFE interceptor has ALREADY validated the mTLS certificate and
// injected identity into context before this code runs.
//
// Flow:
//   1. Read identity from context (injected by interceptor in spiffe.go):
//      - trustDomain = TrustDomainFromContext(ctx)
//      - role = SPIFFERoleFromContext(ctx)
//      - connectorID = SPIFFEEntityIDFromContext(ctx)
//   2. Verify role == appmeta.SPIFFERoleConnector
//      - Else → codes.PermissionDenied
//   3. Resolve tenant:
//      SELECT tenant_id FROM connectors WHERE id = $1 AND trust_domain = $2
//   4. Verify not revoked: check connector status != 'revoked'
//      - Else → codes.PermissionDenied
//   5. Update connector:
//      last_heartbeat_at=NOW(), version, hostname, public_ip, status='active'
//   6. Return HeartbeatResponse{Ok: true, LatestVersion: "...", ReEnroll: false}
//
// re_enroll is ALWAYS false this sprint. The field exists for next sprint's
// auto-renewal. Do NOT add renewal logic.
```

### Key implementation notes

- **Do NOT parse SPIFFE IDs here** — use `SPIFFERoleFromContext()`, `SPIFFEEntityIDFromContext()`, `TrustDomainFromContext()` from spiffe.go.
- **`re_enroll = false` always this sprint** — just return `false`. The proto field exists so next sprint doesn't need a proto change.
- **Config comes from Member 2** — never read environment variables directly.

---

## Phase 4 Checklist

```
✓ Identity read from context (not re-parsed from cert)
✓ Role verified as "connector"
✓ Non-connector role → codes.PermissionDenied
✓ Tenant resolved via DB query (connector_id + trust_domain)
✓ Revoked connector → codes.PermissionDenied
✓ Connector row updated: last_heartbeat_at, version, hostname, public_ip, status='active'
✓ HeartbeatResponse returned with Ok=true
✓ re_enroll = false (always, this sprint)
```
