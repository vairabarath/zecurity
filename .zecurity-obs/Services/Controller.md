---
type: service
status: active
language: Go
entry: cmd/server/main.go
ports:
  http: 8080
  grpc: 9090
related:
  - "[[PKI]]"
  - "[[Auth]]"
  - "[[Services/Connector]]"
tags:
  - go
  - grpc
  - graphql
  - pki
  - spiffe
---

# Controller (Go)

The central authority. Signs certificates, manages connector lifecycle, serves the admin GraphQL API.

---

## Entry Point

`cmd/server/main.go` — wires all services, starts HTTP + gRPC listeners.

```
main()
  ├── db.Init()                     pgx/v5 connection pool
  ├── pki.Init()                    load/generate Root + Intermediate CA
  ├── auth.NewService()             Google OAuth + JWT
  ├── connector.Config{}            env-driven config
  ├── HTTP :8080                    GraphQL + auth routes
  └── gRPC :9090                    connector RPCs (mTLS)
```

---

## HTTP API (:8080)

| Route | Handler | Auth |
|-------|---------|------|
| `POST /graphql` | GraphQL (gqlgen) | JWT required (except public ops) |
| `GET /auth/callback` | Google OAuth callback | Public |
| `POST /auth/refresh` | JWT refresh | Public |
| `GET /health` | Health check | Public |
| `GET /ca.crt` | Workspace CA cert download | Public |
| `GET /playground` | GraphQL playground | Dev only |

**Middleware stack:**
1. `AuthMiddleware` — validates JWT, injects workspace
2. `WorkspaceGuard` — ensures workspace is active

---

## gRPC API (:9090)

All gRPC uses mTLS. The `UnarySPIFFEInterceptor` runs before every handler and:
1. Extracts the client certificate from the TLS handshake
2. Validates SPIFFE URI format and trust domain
3. Injects `connectorID`, `trustDomain`, `role` into context

| RPC | Auth | Purpose |
|-----|------|---------|
| `Enroll` | Plain TLS + JWT | First-time enrollment, issues 7-day cert |
| `Heartbeat` | mTLS (SPIFFE) | Keepalive, sets `re_enroll` when cert expiring |
| `RenewCert` | mTLS (SPIFFE) | Cert renewal, CSR proof-of-possession |

**Handler struct:** `EnrollmentHandler` (all three RPCs on one struct, one gRPC registration).

---

## Internal Services

### [[PKI]]
- `internal/pki/` — 3-tier CA hierarchy
- `SignConnectorCert()` — enrollment: CSR → 7-day SPIFFE cert
- `RenewConnectorCert()` — renewal: CSR (proof-of-possession) → fresh cert

### [[Auth]]
- `internal/auth/` — Google OAuth flow + JWT issuance
- `internal/connector/token.go` — enrollment JWT generation + Redis JTI burn

### Bootstrap
- `internal/bootstrap/` — first-user signup, workspace creation, CA provisioning

### Connector Subsystem
- `internal/connector/config.go` — `ConnectorConfig` (CertTTL, RenewalWindow, etc.)
- `internal/connector/spiffe.go` — SPIFFE interceptor + cert verification
- `internal/connector/enrollment.go` — Enroll + Heartbeat + RenewCert handlers
- `internal/connector/heartbeat.go` — disconnect watcher goroutine

---

## Key Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `CONNECTOR_CERT_TTL` | `168h` | Connector cert lifetime |
| `CONNECTOR_RENEWAL_WINDOW` | `48h` | Trigger re_enroll when cert < this |
| `CONNECTOR_HEARTBEAT_INTERVAL` | `30s` | Heartbeat tick interval |
| `CONNECTOR_DISCONNECT_THRESHOLD` | `90s` | Mark disconnected after silence |
| `GRPC_PORT` | `9090` | gRPC listener port |
| `PKI_MASTER_SECRET` | required | AES-GCM key for CA encryption |

---

## Dependencies

- `pgx/v5` — PostgreSQL (connector + workspace state)
- `go-redis/v9` — Redis (JTI burn, sessions)
- `gqlgen` — GraphQL server
- `google.golang.org/grpc` — gRPC server
- `golang-jwt/jwt` — enrollment token verification
