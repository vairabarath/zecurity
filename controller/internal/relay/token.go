package relay

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const (
	provisioningJTIPrefix = "relay:provisioning:jti:"
	provisioningAudience  = "relay-provisioning"
)

// ProvisioningClaims are the JWT claims embedded in a relay provisioning token.
type ProvisioningClaims struct {
	jwt.RegisteredClaims
	RelayID string `json:"relay_id"`
}

// IssueProvisioningToken signs a single-use provisioning JWT and returns the
// token + its jti. Caller persists the jti to the relays row and to Valkey
// (the latter is the atomic burn store).
func IssueProvisioningToken(jwtSecret, relayID string, ttl time.Duration) (tokenString, jti string, err error) {
	jti = uuid.NewString()
	now := time.Now()
	claims := ProvisioningClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   relayID,
			Issuer:    appmeta.ControllerIssuer,
			Audience:  jwt.ClaimStrings{provisioningAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		RelayID: relayID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err = token.SignedString([]byte(jwtSecret))
	if err != nil {
		return "", "", fmt.Errorf("sign provisioning token: %w", err)
	}
	return tokenString, jti, nil
}

// VerifyProvisioningToken parses and validates a provisioning JWT. It does NOT
// burn the jti — burn is the caller's atomic Valkey step.
func VerifyProvisioningToken(jwtSecret, tokenString string) (*ProvisioningClaims, error) {
	tok, err := jwt.ParseWithClaims(tokenString, &ProvisioningClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(jwtSecret), nil
	},
		jwt.WithIssuer(appmeta.ControllerIssuer),
		jwt.WithAudience(provisioningAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("verify provisioning token: %w", err)
	}
	c, ok := tok.Claims.(*ProvisioningClaims)
	if !ok || !tok.Valid {
		return nil, fmt.Errorf("invalid provisioning token claims")
	}
	return c, nil
}

// StoreProvisioningJTI puts the jti in Valkey with TTL for atomic burn.
func StoreProvisioningJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti, relayID string, ttl time.Duration) error {
	return rdb.Set(ctx, provisioningJTIPrefix+jti, relayID, ttl).Err()
}

// BurnProvisioningJTI atomically fetches and deletes the jti. Returns
// (relayID, true) if the jti existed and was burned, (_, false) if not found.
func BurnProvisioningJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti string) (string, bool, error) {
	v, err := rdb.GetDel(ctx, provisioningJTIPrefix+jti).Result()
	if err == valkeycompat.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("burn provisioning jti: %w", err)
	}
	return v, true, nil
}
