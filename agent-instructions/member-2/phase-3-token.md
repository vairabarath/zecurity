# Phase 3 — Enrollment Token Generation + Redis JTI

## Objective

Create the enrollment token system: JWT generation for connector enrollment and single-use burn via Redis. This file provides two interfaces:

- **For Member 4's resolver** (`generateConnectorToken` mutation): calls `GenerateEnrollmentToken()` to create the JWT + stores the JTI in Redis
- **For Member 3's enrollment handler**: calls `BurnEnrollmentJTI()` to atomically consume the JTI during enrollment

---

## Prerequisites

- **Phase 2** completed (Config struct exists)
- **Member 3's appmeta additions** committed (needs `appmeta.WorkspaceTrustDomain()` and `appmeta.ControllerIssuer`)
  - If not yet merged: use `// TODO: replace with appmeta.WorkspaceTrustDomain(slug)` placeholder and hardcode temporarily for compilation

---

## File to Create

```
controller/internal/connector/token.go
```

---

## Implementation

**File: `controller/internal/connector/token.go`**

```go
package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// enrollmentJTIPrefix is the Redis key prefix for enrollment token JTIs.
// Format: "enrollment:jti:<jti>" → value is the connector_id.
const enrollmentJTIPrefix = "enrollment:jti:"

// EnrollmentClaims are the JWT claims embedded in an enrollment token.
// The Rust connector base64-decodes the payload (no signature verification —
// it has no JWT_SECRET). Trust is established via CA fingerprint verification.
type EnrollmentClaims struct {
	jwt.RegisteredClaims
	ConnectorID   string `json:"connector_id"`
	WorkspaceID   string `json:"workspace_id"`
	TrustDomain   string `json:"trust_domain"`
	CAFingerprint string `json:"ca_fingerprint"`
}

// GenerateEnrollmentToken creates a signed JWT for connector enrollment.
//
// Called by: Member 4's generateConnectorToken GraphQL resolver.
//
// The returned token is shown to the admin once (in the install command)
// and is never stored server-side — only the JTI is stored in Redis.
//
// Parameters:
//   - cfg:            connector config (provides JWTSecret and EnrollmentTokenTTL)
//   - connectorID:    UUID of the newly created connector row
//   - workspaceID:    UUID of the workspace
//   - workspaceSlug:  workspace slug (used to derive trust_domain via appmeta)
//   - caFingerprint:  SHA-256 hex of the Intermediate CA cert DER bytes
//
// Returns the signed JWT string and the generated JTI (for Redis storage).
func GenerateEnrollmentToken(
	cfg Config,
	connectorID, workspaceID, workspaceSlug, caFingerprint string,
) (tokenString string, jti string, err error) {
	jti = uuid.New().String()
	now := time.Now()

	claims := EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(now.Add(cfg.EnrollmentTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		ConnectorID:   connectorID,
		WorkspaceID:   workspaceID,
		TrustDomain:   appmeta.WorkspaceTrustDomain(workspaceSlug),
		CAFingerprint: caFingerprint,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err = token.SignedString([]byte(cfg.JWTSecret))
	if err != nil {
		return "", "", fmt.Errorf("sign enrollment token: %w", err)
	}

	return tokenString, jti, nil
}

// VerifyEnrollmentToken parses and validates an enrollment JWT.
//
// Called by: Member 3's Enroll gRPC handler.
//
// Verifies:
//   - HMAC signature using cfg.JWTSecret
//   - Token is not expired (exp > now)
//   - Issuer matches appmeta.ControllerIssuer
//
// Returns the parsed claims on success.
func VerifyEnrollmentToken(cfg Config, tokenString string) (*EnrollmentClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &EnrollmentClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(cfg.JWTSecret), nil
	}, jwt.WithIssuer(appmeta.ControllerIssuer), jwt.WithExpirationRequired())

	if err != nil {
		return nil, fmt.Errorf("verify enrollment token: %w", err)
	}

	claims, ok := token.Claims.(*EnrollmentClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid enrollment token claims")
	}

	return claims, nil
}

// StoreEnrollmentJTI stores the JTI in Redis with a TTL.
// The value is the connector_id — used to look up which connector
// this token belongs to during enrollment.
//
// Called by: Member 4's generateConnectorToken resolver (after GenerateEnrollmentToken).
//
// Key format: "enrollment:jti:<jti>" → "<connector_id>"
// TTL: cfg.EnrollmentTokenTTL (default 24h)
func StoreEnrollmentJTI(ctx context.Context, rdb *redis.Client, jti, connectorID string, ttl time.Duration) error {
	key := enrollmentJTIPrefix + jti
	return rdb.Set(ctx, key, connectorID, ttl).Err()
}

// BurnEnrollmentJTI atomically retrieves and deletes the JTI from Redis.
// Single-use — cannot be replayed.
//
// Called by: Member 3's Enroll gRPC handler (step 3).
//
// Uses a Redis pipeline for atomic GET+DEL (same pattern as auth/redis.go
// GetAndDeletePKCEState). If we GET then DEL separately, a crash between
// the two could leave a used JTI in Redis (replay risk).
//
// Returns:
//   - connectorID: the connector UUID associated with this JTI
//   - found: true if the JTI existed (token is valid and unused)
//   - err: Redis errors only
func BurnEnrollmentJTI(ctx context.Context, rdb *redis.Client, jti string) (connectorID string, found bool, err error) {
	key := enrollmentJTIPrefix + jti

	// Pipeline ensures GET+DEL happen atomically.
	pipe := rdb.Pipeline()
	getCmd := pipe.Get(ctx, key)
	pipe.Del(ctx, key)
	_, err = pipe.Exec(ctx)

	if err != nil && err != redis.Nil {
		return "", false, fmt.Errorf("redis pipeline burn jti: %w", err)
	}

	val, err := getCmd.Result()
	if err == redis.Nil {
		return "", false, nil // expired or already used
	}
	if err != nil {
		return "", false, fmt.Errorf("get enrollment jti: %w", err)
	}

	return val, true, nil
}
```

---

## Design Decisions

1. **`GenerateEnrollmentToken` returns the JTI separately** so the caller (Member 4's resolver) can pass it to `StoreEnrollmentJTI` and also store it on the connector DB row.

2. **`VerifyEnrollmentToken` is provided here** (not in Member 3's code) because token generation and verification are paired — same package, same claims struct. Member 3's enrollment handler calls this function.

3. **Redis `GET+DEL` pipeline** follows the exact pattern from `auth/redis.go:GetAndDeletePKCEState` (lines 71-110). Atomic to prevent replay on crash.

4. **`*redis.Client` passed directly** — not wrapped. The connector package doesn't own the Redis connection; it receives it from main.go. Member 2 passes the raw client; Member 3 and 4 call these functions with it.

---

## Verification

```bash
cd controller && go build ./internal/connector/...
```

- [ ] File exists at `controller/internal/connector/token.go`
- [ ] Package is `package connector`
- [ ] Imports `appmeta.WorkspaceTrustDomain` and `appmeta.ControllerIssuer` — NOT hardcoded strings
- [ ] `GenerateEnrollmentToken` returns `(tokenString, jti, err)`
- [ ] `VerifyEnrollmentToken` validates signature, expiry, and issuer
- [ ] `StoreEnrollmentJTI` uses `enrollmentJTIPrefix` + TTL
- [ ] `BurnEnrollmentJTI` uses pipeline for atomic GET+DEL
- [ ] `go build ./internal/connector/...` passes (requires appmeta additions from Member 3)

---

## DO NOT TOUCH

- `controller/internal/auth/redis.go` — Sprint 1 Redis client. Reuse the pattern but don't modify it.
- `controller/internal/connector/enrollment.go` — Member 3 writes the handler that calls `VerifyEnrollmentToken` + `BurnEnrollmentJTI`.
- Do not add Redis connection setup here — main.go (Phase 5) creates the Redis client and passes it down.

---

## Dependency Note

If Member 3's appmeta additions (`WorkspaceTrustDomain`, `ControllerIssuer`) are not yet merged, this file won't compile. Options:

1. **Preferred:** Wait for Member 3's Day 1 commit of `identity.go` additions.
2. **Fallback:** Temporarily import existing `appmeta.ControllerIssuer` (already exists) and hardcode `WorkspaceTrustDomain` with a `// TODO` comment. Replace once Member 3 merges.

---

## After This Phase

Proceed to Phase 4 (ca_endpoint.go).
