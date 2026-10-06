package provider

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// ProviderAudience is the JWT audience that isolates provider sessions from
// tenant sessions. RequireProvider enforces it; the tenant AuthMiddleware never
// sets or accepts it. Together with the separate issuer (appmeta.ProviderIssuer)
// and signing key (PROVIDER_JWT_SECRET) it forms the wall between the two
// identity tiers (D-25).
const ProviderAudience = "provider"

// ProviderClaims is the payload of a provider-scoped JWT. Deliberately has NO
// tenant_id — provider identity has no tenant (ADR-021). Signed HS256 with the
// dedicated provider key; minted only by IdentityService.IssueSession.
type ProviderClaims struct {
	Role  string `json:"role"` // "super-admin" | "relay-ops" (informational; RequireProvider reads the role from the DB)
	Email string `json:"email"`
	// Gen must equal provider_users.session_generation at verification time;
	// bumping the column revokes every outstanding token for the user.
	Gen int64 `json:"gen"`
	// PWC marks a password-change-only token (forced first/temporary password
	// change). RequireProvider rejects it everywhere except the password route.
	PWC bool `json:"pwc,omitempty"`
	// AMR records how the operator authenticated (RFC 8176 values): ["pwd"] for
	// local password login. Mandatory super-admin TOTP (D-28) adds "otp".
	AMR                  []string `json:"amr"`
	jwt.RegisteredClaims          // Subject=provider_user_id, Issuer=ProviderIssuer, Audience=[provider]
}

// issueProviderToken signs a provider JWT for u. Unexported on purpose: the ONLY
// caller is IdentityService.IssueSession, the single minting path (D-29).
func issueProviderToken(key []byte, u *ProviderUser, amr []string, pwc bool, ttl time.Duration, now time.Time) (string, error) {
	claims := ProviderClaims{
		Role:  u.Role,
		Email: u.Email,
		Gen:   u.SessionGeneration,
		PWC:   pwc,
		AMR:   amr,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			Issuer:    appmeta.ProviderIssuer,
			Audience:  jwt.ClaimStrings{ProviderAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign provider token: %w", err)
	}
	return signed, nil
}

// VerifyProviderToken parses and validates a provider JWT: HS256 only (blocks
// alg=none and algorithm confusion), iss=zecurity-provider, aud=provider, exp
// required, subject present. A tenant JWT fails on the key, the issuer and the
// audience independently.
func VerifyProviderToken(key []byte, tokenString string) (*ProviderClaims, error) {
	claims := &ProviderClaims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims,
		func(*jwt.Token) (any, error) { return key, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(appmeta.ProviderIssuer),
		jwt.WithAudience(ProviderAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("verify provider token: %w", err)
	}
	if !tok.Valid || claims.Subject == "" {
		return nil, errors.New("verify provider token: invalid claims")
	}
	return claims, nil
}
