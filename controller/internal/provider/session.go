package provider

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// ProviderAudience is the JWT audience that isolates provider sessions from
// tenant sessions. RequireProvider enforces it; the tenant AuthMiddleware never
// sets or accepts it — this is the mechanical wall between the two identity tiers.
const ProviderAudience = "provider"

// ProviderClaims is the payload of a provider-scoped JWT. Deliberately has NO
// tenant_id — provider identity has no tenant. Signed HS256 with JWT_SECRET
// (reused), distinguished from tenant tokens purely by aud=provider.
type ProviderClaims struct {
	Role                 string `json:"role"` // "super-admin" | "relay-ops"
	Email                string `json:"email"`
	jwt.RegisteredClaims        // Subject=provider_user_id, Issuer=ControllerIssuer, Audience=[provider]
}

// IssueProviderToken signs a provider-scoped JWT for an allowlisted provider user.
// Called by the provider login callback after the Google email is verified.
func IssueProviderToken(jwtSecret, providerUserID, role, email string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := ProviderClaims{
		Role:  role,
		Email: email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   providerUserID,
			Issuer:    appmeta.ControllerIssuer,
			Audience:  jwt.ClaimStrings{ProviderAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		return "", fmt.Errorf("sign provider token: %w", err)
	}
	return signed, nil
}

// VerifyProviderToken parses and validates a provider JWT. Enforces the HS256
// method, the controller issuer, and — critically — aud=provider, so a tenant
// JWT is rejected here. Returns the claims on success.
func VerifyProviderToken(jwtSecret, tokenString string) (*ProviderClaims, error) {
	claims := &ProviderClaims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims,
		func(t *jwt.Token) (any, error) {
			// Enforce HS256 — blocks alg=none / alg confusion attacks.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return []byte(jwtSecret), nil
		},
		jwt.WithIssuer(appmeta.ControllerIssuer),
		jwt.WithAudience(ProviderAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("verify provider token: %w", err)
	}
	if !tok.Valid {
		return nil, fmt.Errorf("invalid provider token claims")
	}
	return claims, nil
}
