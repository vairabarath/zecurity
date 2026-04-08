# Phase 6 — .env Updates

## Objective

Add the connector-specific environment variables to both `.env` (local dev) and `.env.example` (template for new developers). These variables are read by `mustDuration`/`envOr` in main.go (Phase 5) to populate `connector.Config`.

---

## Prerequisites

- None. Can be done anytime — even Day 1.

---

## Files to Modify

```
controller/.env
controller/.env.example
```

---

## Changes

### Append to `controller/.env`

Add at the bottom of the file, after the existing `ALLOWED_ORIGIN` line:

```env

# ── Connector sprint additions ───────────────────────────────────────────────

# gRPC server port for connector Enroll + Heartbeat RPCs (9090 dev / 8443 prod)
GRPC_PORT=9090

# Connector certificate validity — 7 days per GROK final instruction.
# Set to 1h or 24h for faster expiry testing during development.
CONNECTOR_CERT_TTL=168h

# Enrollment JWT Redis TTL — single-use token lifetime.
CONNECTOR_ENROLLMENT_TOKEN_TTL=24h

# Heartbeat and disconnect — must satisfy: DISCONNECT_THRESHOLD > 3 x HEARTBEAT_INTERVAL
CONNECTOR_HEARTBEAT_INTERVAL=30s
CONNECTOR_DISCONNECT_THRESHOLD=90s
```

### Append to `controller/.env.example`

Add the same block at the bottom:

```env

# ── Connector sprint additions ───────────────────────────────────────────────

# gRPC server port for connector Enroll + Heartbeat RPCs (9090 dev / 8443 prod)
GRPC_PORT=9090

# Connector certificate validity — 7 days per GROK final instruction.
# Set to 1h or 24h for faster expiry testing during development.
CONNECTOR_CERT_TTL=168h

# Enrollment JWT Redis TTL — single-use token lifetime.
CONNECTOR_ENROLLMENT_TOKEN_TTL=24h

# Heartbeat and disconnect — must satisfy: DISCONNECT_THRESHOLD > 3 x HEARTBEAT_INTERVAL
CONNECTOR_HEARTBEAT_INTERVAL=30s
CONNECTOR_DISCONNECT_THRESHOLD=90s
```

---

## Variable Reference

| Variable | Default | Type | Used By |
|---|---|---|---|
| `GRPC_PORT` | `9090` | string | main.go → `connector.Config.GRPCPort` |
| `CONNECTOR_CERT_TTL` | `168h` | duration | main.go → `connector.Config.CertTTL` |
| `CONNECTOR_ENROLLMENT_TOKEN_TTL` | `24h` | duration | main.go → `connector.Config.EnrollmentTokenTTL` |
| `CONNECTOR_HEARTBEAT_INTERVAL` | `30s` | duration | main.go → `connector.Config.HeartbeatInterval` |
| `CONNECTOR_DISCONNECT_THRESHOLD` | `90s` | duration | main.go → `connector.Config.DisconnectThreshold` |

All have defaults in main.go via `mustDuration`/`envOr`, so the server starts even without these set. The `.env` file provides explicit values for local development.

---

## Verification

- [ ] `controller/.env` has all 5 new variables at the bottom
- [ ] `controller/.env.example` has all 5 new variables at the bottom
- [ ] Existing sprint 1 variables are NOT modified
- [ ] Comments explain each variable's purpose and constraints
- [ ] Server starts correctly after adding these vars:
  ```bash
  cd controller && go run ./cmd/server/
  ```

---

## DO NOT TOUCH

- Existing env vars (`DATABASE_URL`, `REDIS_URL`, `PORT`, `ENV`, `JWT_SECRET`, `GOOGLE_*`, `PKI_MASTER_SECRET`, `ALLOWED_ORIGIN`) — do not modify values or comments
- Do not add secrets or credentials to `.env.example` — keep placeholder text

---

## After This Phase

Member 2's individual work is complete. Remaining integration work:

1. **When Member 3 merges `spiffe.go`**: update main.go TODOs to wire `UnarySPIFFEInterceptor`
2. **When Member 3 merges `enrollment.go` + `heartbeat.go`**: register `ConnectorServiceServer` in main.go
3. **When Member 3 merges `appmeta` additions**: verify `token.go` compiles with real `appmeta.WorkspaceTrustDomain()`
