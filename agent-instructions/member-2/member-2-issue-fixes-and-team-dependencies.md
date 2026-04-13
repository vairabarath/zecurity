# Member 2 Issue Fixes And Team Dependencies

## Summary

This document normalizes the Member 2 contract to the current shared branch state so the team can continue without churn.

The sprint architecture is unchanged. Only the low-level implementation contracts are being aligned to the code already shared across branches.

---

## Canonical Member 2 Contract

- Proto path:
  - `controller/proto/connector/connector.proto`
- Token API:
  - `GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, caFingerprint) (tokenString, jti, err)`
- Token verification helper:
  - `VerifyEnrollmentToken(cfg, tokenString)` lives in `controller/internal/connector/token.go`
- CA endpoint:
  - `CAEndpointHandler(pool *pgxpool.Pool) http.HandlerFunc`
- Main wiring:
  - fallback gRPC startup is acceptable until Member 3 runtime files land

---

## What Member 2 Has Implemented

- connector proto and generated Go stubs
- connector config contract
- enrollment token generation
- enrollment token verification helper
- Redis single-use JTI storage and burn flow
- public CA certificate endpoint
- fallback `main.go` connector wiring
- connector env configuration in `.env` and `.env.example`

---

## What Changed From The Older Written Plan

### Proto location

The canonical proto location is now:

```txt
controller/proto/connector/connector.proto
```

This is already the path used by generated stubs and downstream consumption.

### Token function contract

The canonical token contract is now:

```go
GenerateEnrollmentToken(...) (tokenString, jti, err)
```

The returned `jti` is required for:

- Redis storage via `StoreEnrollmentJTI(...)`
- persistence on the connector row as `enrollment_token_jti`

### Verification helper ownership

`VerifyEnrollmentToken(...)` is treated as a shared helper in `token.go`.

- Member 2 owns the helper implementation
- Member 3 consumes it in the enrollment handler

### CA endpoint interface

The current accepted interface is:

```go
CAEndpointHandler(pool *pgxpool.Pool) http.HandlerFunc
```

This keeps the endpoint as a simple DB-backed read and avoids widening the PKI service surface for a single certificate fetch.

---

## What Member 3 Must Still Do

Member 3 still owns the remaining runtime connector backend files:

- `controller/internal/connector/spiffe.go`
- `controller/internal/connector/enrollment.go`
- `controller/internal/connector/heartbeat.go`

Most importantly, `heartbeat.go` remains the key missing runtime file for full connector backend completion.

Once these land, Member 2 can replace the remaining `main.go` TODO placeholders with final interceptor and service wiring.

---

## What Member 4 Must Do

Member 4 should implement resolver logic against the normalized current contract.

For `generateConnectorToken`:

1. insert connector row
2. call `GenerateEnrollmentToken(...)` and receive `tokenString, jti, err`
3. call `StoreEnrollmentJTI(...)`
4. persist `enrollment_token_jti = jti` on the connector row
5. build the install command using `tokenString`
6. return the GraphQL response

Member 4 does not need to wait for `heartbeat.go` to implement this resolver path.

---

## What Member 1 Can Do

Member 1 can continue frontend work independently:

- GraphQL operation files
- route and sidebar wiring
- Remote Networks page
- Connectors page
- install command modal
- final codegen/wiring once backend schema and resolver output are ready

No SPIFFE or backend runtime details should leak into the frontend.

---

## What Should Not Change Now

- do not move the proto file back to `controller/proto/connector.proto`
- do not revert `GenerateEnrollmentToken` to `(string, error)`
- do not remove `VerifyEnrollmentToken`
- do not refactor the CA endpoint to `pki.Service` unless the team explicitly chooses extra churn

---

## Practical Next Step

- Member 2: keep docs aligned to the normalized contract
- Member 3: finish `heartbeat.go` and remaining runtime files
- Member 4: proceed with resolver implementation against the normalized token contract
- Member 1: continue frontend implementation against the current schema direction
