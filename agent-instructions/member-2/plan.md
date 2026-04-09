# Member 2 — Go Auth + Enrollment Infrastructure

## Role

You own the proto definition (Day 1 blocker for Member 3 and 4), the connector config struct, enrollment token generation, the CA certificate HTTP endpoint, and the main.go wiring for the gRPC server. You bridge the auth/config layer with the gRPC handler layer that Member 3 implements.

---

## Your Files (CREATE or MODIFY only these)

### New files you create

```
controller/proto/connector/connector.proto
controller/internal/connector/config.go
controller/internal/connector/token.go
controller/internal/connector/ca_endpoint.go
```

### Files you modify

```
controller/cmd/server/main.go        ← add mustDuration, ConnectorConfig, gRPC server wiring
controller/.env.example              ← add new env vars
controller/.env                      ← add new env vars for local dev
```

---

## DO NOT TOUCH — Conflict Boundaries

- **`controller/internal/appmeta/identity.go`** (or any new `appmeta.go`) — Member 3 owns SPIFFE constants. You IMPORT from appmeta, never write to it.
- **`controller/internal/connector/spiffe.go`** — Member 3 writes this. You wire `UnarySPIFFEInterceptor` in main.go but do not implement it.
- **`controller/internal/connector/enrollment.go`** — Member 3 writes the Enroll gRPC handler.
- **`controller/internal/connector/heartbeat.go`** — Member 3 writes the Heartbeat handler + disconnect watcher.
- **`controller/internal/pki/*`** — Member 3 adds `SignConnectorCert`. Do not touch PKI code.
- **`controller/graph/schema.graphqls`** — Member 4 modifies the GraphQL schema.
- **`controller/graph/resolvers/connector.resolvers.go`** — Member 4 writes connector resolvers.
- **`controller/migrations/*`** — Member 4 owns all migration files.
- **`connector/`** — Member 4 owns the entire Rust codebase.
- **`admin/`** — Member 1 owns all frontend code.
- **`.github/`** — Member 4 owns CI.

---

## Phase-by-Phase Plan

### Phase 1 — connector.proto (DAY 1 — COMMIT FIRST)

This file unblocks Member 3 (Go stub generation) and Member 4 (Rust tonic-build). Commit it before doing anything else.

**File: `controller/proto/connector/connector.proto`**

```protobuf
syntax = "proto3";

package connector;
option go_package = "github.com/yourorg/zecurity/controller/proto/connector";

service ConnectorService {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}

message EnrollRequest {
  string enrollment_token = 1;
  bytes  csr_der          = 2;
  string version          = 3;
  string hostname         = 4;
}

message EnrollResponse {
  bytes  certificate_pem      = 1;
  bytes  workspace_ca_pem     = 2;
  bytes  intermediate_ca_pem  = 3;
  string connector_id         = 4;
}

message HeartbeatRequest {
  string connector_id = 1;
  string version      = 2;
  string hostname     = 3;
  string public_ip    = 4;
}

message HeartbeatResponse {
  bool   ok             = 1;
  string latest_version = 2;
  bool   re_enroll      = 3;
}
```

Key decisions:
- `re_enroll` field is plumbed now. Member 3's handler returns `false` this sprint. No proto change needed next sprint.
- `connector_id` in `HeartbeatRequest` is for logging only — authoritative identity comes from the mTLS SPIFFE cert (Member 3's interceptor).

### Phase 2 — ConnectorConfig struct

**File: `controller/internal/connector/config.go`**

Create the `Config` struct with fields:
- `CertTTL time.Duration` (default 168h)
- `EnrollmentTokenTTL time.Duration` (default 24h)
- `HeartbeatInterval time.Duration` (default 30s)
- `DisconnectThreshold time.Duration` (default 90s)
- `GRPCPort string` (default "9090")
- `JWTSecret string` (reuses existing JWT_SECRET)

This struct is the single source of tunable values. Member 3's handlers receive this struct — they never read env vars directly.

### Phase 3 — Enrollment Token Generation

**File: `controller/internal/connector/token.go`**

Implements the JWT enrollment token used by `generateConnectorToken` GraphQL mutation (resolver is Member 4's, but it calls your function).

**Function: `GenerateEnrollmentToken(cfg Config, connectorID, workspaceID, workspaceSlug, caFingerprint string) (tokenString string, jti string, err error)`**

JWT payload:
```json
{
  "jti": "<uuid-v4>",
  "connector_id": "<uuid>",
  "workspace_id": "<uuid>",
  "trust_domain": "<derived via appmeta.WorkspaceTrustDomain(slug)>",
  "ca_fingerprint": "<sha256-hex-of-intermediate-ca-cert-DER>",
  "iss": "<appmeta.ControllerIssuer>",
  "exp": "<now + cfg.EnrollmentTokenTTL>"
}
```

Critical rules:
- `trust_domain` MUST be derived via `appmeta.WorkspaceTrustDomain(slug)` — import from appmeta, never concatenate manually.
- `iss` MUST equal `appmeta.ControllerIssuer` — import it.
- Sign with HMAC using `cfg.JWTSecret`.

**Single-use burn via Redis:**
- On token generation: `SET enrollment:jti:<jti> <connector_id>` with TTL = `cfg.EnrollmentTokenTTL`
- On enrollment (called by Member 3's handler): `GET+DEL enrollment:jti:<jti>` atomically. Not found = token expired or already used.

Expose all shared token helpers:
- `VerifyEnrollmentToken(cfg Config, tokenString string) (*EnrollmentClaims, error)`
- `StoreEnrollmentJTI(ctx, redis, jti, connectorID, ttl)`
- `BurnEnrollmentJTI(ctx, redis, jti) (connectorID string, found bool, err error)`

Member 4's resolver uses the returned `jti` to:
- store `enrollment:jti:<jti>` in Redis
- persist `enrollment_token_jti = jti` on the connector row

`VerifyEnrollmentToken` is treated as a shared helper in `token.go` that Member 3's enrollment handler consumes.

### Phase 4 — CA Certificate HTTP Endpoint

**File: `controller/internal/connector/ca_endpoint.go`**

**Handler: `CAEndpointHandler(pool *pgxpool.Pool) http.HandlerFunc`**

- `GET /ca.crt`
- Returns the Intermediate CA certificate as PEM
- Content-Type: `application/x-pem-file`
- This is a public, unauthenticated endpoint
- The Rust connector fetches this during enrollment to establish initial trust
- The connector then verifies the SHA-256 fingerprint against the JWT claim

### Phase 5 — main.go Wiring

**Modify: `controller/cmd/server/main.go`**

Add alongside existing sprint 1 wiring:

1. **`mustDuration` helper** — same pattern as existing `mustEnv`/`envOr`:
   ```go
   func mustDuration(key string, fallback time.Duration) time.Duration
   ```

2. **ConnectorConfig population** — read from env vars using `mustDuration`/`envOr`:
   ```go
   connectorCfg := connector.Config{
       CertTTL:             mustDuration("CONNECTOR_CERT_TTL", 7*24*time.Hour),
       EnrollmentTokenTTL:  mustDuration("CONNECTOR_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
       HeartbeatInterval:   mustDuration("CONNECTOR_HEARTBEAT_INTERVAL", 30*time.Second),
       DisconnectThreshold: mustDuration("CONNECTOR_DISCONNECT_THRESHOLD", 90*time.Second),
       GRPCPort:            envOr("GRPC_PORT", "9090"),
       JWTSecret:           mustEnv("JWT_SECRET"),
   }
   ```

3. **HTTP route for `/ca.crt`** — register on the existing HTTP mux:
   ```go
   mux.HandleFunc("/ca.crt", connector.CAEndpointHandler(db.Pool))
   ```

4. **gRPC server startup** — wire the SPIFFE interceptor (from Member 3) and register the service:
   ```go
   grpcServer := grpc.NewServer(
       grpc.Creds(tlsCreds),
       grpc.UnaryInterceptor(
           connector.UnarySPIFFEInterceptor(
               connector.NewTrustDomainValidator(
                   appmeta.SPIFFEGlobalTrustDomain,
                   workspaceStore,
               ),
           ),
       ),
   )
   pb.RegisterConnectorServiceServer(grpcServer, connector.NewService(...))
   go grpcServer.Serve(grpcListener)
   ```

**Important:** The `UnarySPIFFEInterceptor` and `NewTrustDomainValidator` are implemented by Member 3 in `spiffe.go`. You call them; you do not implement them. If Member 3's code isn't merged yet, leave a `// TODO: wire SPIFFE interceptor when spiffe.go lands` comment and use a placeholder.

### Phase 6 — .env Updates

Add to both `.env.example` and `.env`:

```env
# ── Connector sprint additions ───────────────────────────────────────────────
GRPC_PORT=9090
CONNECTOR_CERT_TTL=168h
CONNECTOR_ENROLLMENT_TOKEN_TTL=24h
CONNECTOR_HEARTBEAT_INTERVAL=30s
CONNECTOR_DISCONNECT_THRESHOLD=90s
```

---

## Dependency Timeline

```
Day 1:  Phase 1 (connector.proto) — COMMIT AND PUSH IMMEDIATELY
        Phase 2 (config.go) — can start immediately, no dependencies
        Phase 3 (token.go) — needs appmeta.go from Member 3 for WorkspaceTrustDomain
                             Start the file, leave trust_domain derivation as TODO if needed

Day 2:  Phase 4 (ca_endpoint.go) — needs PKI service interface stable
        Phase 5 (main.go wiring) — needs Member 3's spiffe.go for interceptor
        Phase 6 (.env) — do anytime
```

---

## Special Instructions

1. **You import from `appmeta`, never write to it.** Member 3 owns `appmeta.go`. Use `appmeta.WorkspaceTrustDomain(slug)`, `appmeta.ControllerIssuer`, `appmeta.SPIFFEGlobalTrustDomain` — never hardcode these strings.

2. **Your proto commit unblocks two people.** Commit `connector.proto` as your very first action. Member 3 needs it for Go stub generation, Member 4 needs it for Rust tonic-build. A delay here delays everyone.

3. **Config is the contract.** Your `Config` struct is what Member 3's handlers consume. Agree on the field names and types before either of you writes handler code. If you rename a field, tell Member 3 immediately.

4. **Token generation vs. token verification.** You generate and store the enrollment JWT + Redis jti. Member 3's enrollment handler verifies the JWT and calls your `BurnEnrollmentJTI` to consume the jti. Keep these responsibilities clean — don't verify in token.go, don't generate in enrollment.go.

5. **The gRPC server in main.go depends on Member 3's interceptor and runtime files.** If `spiffe.go`, `enrollment.go`, or `heartbeat.go` aren't merged yet, keep fallback wiring with clear TODO comments. Don't block your own progress, but don't implement Member 3's runtime files yourself.

6. **Do not modify existing sprint 1 auth code.** `auth/service.go`, `auth/callback.go`, `auth/refresh.go`, etc. are unchanged. The `JWT_SECRET` is reused but the auth package itself is not modified.
