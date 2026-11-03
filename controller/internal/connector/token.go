package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const enrollmentJTIPrefix = "enrollment:jti:"

// EnrollmentClaims are the JWT claims embedded in a connector enrollment token.
type EnrollmentClaims struct {
	jwt.RegisteredClaims
	ConnectorID   string `json:"connector_id"`
	WorkspaceID   string `json:"workspace_id"`
	TrustDomain   string `json:"trust_domain"`
	CAFingerprint string `json:"ca_fingerprint"`
}

// GenerateEnrollmentToken creates a signed JWT and returns it with its JTI.
func GenerateEnrollmentToken(
	cfg Config,
	connectorID, workspaceID, workspaceSlug, caFingerprint string,
) (tokenString string, jti string, err error) {
	jti = uuid.NewString()
	now := time.Now()

	trustDomain := appmeta.WorkspaceTrustDomain(workspaceSlug)

	claims := EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(now.Add(cfg.EnrollmentTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
		},
		ConnectorID:   connectorID,
		WorkspaceID:   workspaceID,
		TrustDomain:   trustDomain,
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
func VerifyEnrollmentToken(cfg Config, tokenString string) (*EnrollmentClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &EnrollmentClaims{}, func(t *jwt.Token) (any, error) {
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

// StoreEnrollmentJTI stores an enrollment token JTI with a TTL in Valkey.
func StoreEnrollmentJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti, connectorID string, ttl time.Duration) error {
	return rdb.Set(ctx, enrollmentJTIPrefix+jti, connectorID, ttl).Err()
}

// PeekEnrollmentJTI reports whether an enrollment token JTI is still stored
// (not expired, not consumed) and for which connector, without consuming it.
// Enroll checks it before claiming the connector row and deletes the entry
// only after the enrollment commits (certificate audit bug 8), so a failed
// enrollment leaves the token usable for a retry.
func PeekEnrollmentJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti string) (connectorID string, found bool, err error) {
	val, err := rdb.Get(ctx, enrollmentJTIPrefix+jti).Result()
	if err == valkeycompat.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read enrollment jti: %w", err)
	}
	return val, true, nil
}

// DeleteEnrollmentJTI removes an enrollment token JTI. It is cleanup, not the
// authority: the connector row's enrollment_token_jti decides which token is
// current, so an entry left behind by a failed delete cannot enroll.
func DeleteEnrollmentJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti string) error {
	if err := rdb.Del(ctx, enrollmentJTIPrefix+jti).Err(); err != nil {
		return fmt.Errorf("delete enrollment jti: %w", err)
	}
	return nil
}

// BurnEnrollmentJTI atomically fetches and deletes an enrollment token JTI.
func BurnEnrollmentJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti string) (connectorID string, found bool, err error) {
	val, err := rdb.GetDel(ctx, enrollmentJTIPrefix+jti).Result()
	if err == valkeycompat.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("burn enrollment jti: %w", err)
	}
	return val, true, nil
}
