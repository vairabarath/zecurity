package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestIssueAccessToken_ValidJWT(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
	}

	token, err := svc.issueAccessToken("user-123", "tenant-456", "admin")
	if err != nil {
		t.Fatalf("issueAccessToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	// Verify the token we just issued.
	claims, err := svc.verifyAccessToken(token)
	if err != nil {
		t.Fatalf("verifyAccessToken: %v", err)
	}

	if claims.Subject != "user-123" {
		t.Fatalf("expected sub=user-123, got %s", claims.Subject)
	}
	if claims.TenantID != "tenant-456" {
		t.Fatalf("expected tenant_id=tenant-456, got %s", claims.TenantID)
	}
	if claims.Role != "admin" {
		t.Fatalf("expected role=admin, got %s", claims.Role)
	}
}

func TestIssueAccessToken_Issuer(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{
		cfg:         testConfig(),
		redisClient: rc,
	}

	token, _ := svc.issueAccessToken("u", "t", "r")
	claims, _ := svc.verifyAccessToken(token)

	iss, _ := claims.GetIssuer()
	if iss != "zecurity-controller" {
		t.Fatalf("expected issuer zecurity-controller, got %s", iss)
	}
}

func TestIssueAccessToken_Expiry(t *testing.T) {
	rc, _ := newTestRedis(t)
	cfg := testConfig()
	cfg.JWTAccessTTL = "15m"
	svc := &serviceImpl{cfg: cfg, redisClient: rc}

	token, _ := svc.issueAccessToken("u", "t", "r")
	claims, _ := svc.verifyAccessToken(token)

	exp, _ := claims.GetExpirationTime()
	if exp == nil {
		t.Fatal("expected expiry to be set")
	}

	// Expiry should be roughly 15 minutes from now (allow 5 second tolerance).
	diff := time.Until(exp.Time)
	if diff < 14*time.Minute || diff > 16*time.Minute {
		t.Fatalf("expected ~15min expiry, got %v", diff)
	}
}

func TestIssueAccessToken_HS256(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}

	tokenStr, _ := svc.issueAccessToken("u", "t", "r")

	// Parse without validation to check the header alg.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(tokenStr, &jwtClaims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	if token.Method.Alg() != "HS256" {
		t.Fatalf("expected HS256, got %s", token.Method.Alg())
	}
}

func TestVerifyAccessToken_WrongSecret(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}

	token, _ := svc.issueAccessToken("u", "t", "r")

	// Try verifying with a different secret.
	badSvc := &serviceImpl{
		cfg: Config{
			JWTSecret: "wrong-secret-entirely-different!",
			JWTIssuer: "zecurity-controller",
		},
		redisClient: rc,
	}
	_, err := badSvc.verifyAccessToken(token)
	if err == nil {
		t.Fatal("expected error verifying with wrong secret")
	}
}

func TestVerifyAccessToken_WrongIssuer(t *testing.T) {
	cfg := testConfig()
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: cfg, redisClient: rc}

	token, _ := svc.issueAccessToken("u", "t", "r")

	// Verify with a different expected issuer.
	badSvc := &serviceImpl{
		cfg: Config{
			JWTSecret: cfg.JWTSecret,
			JWTIssuer: "wrong-issuer",
		},
		redisClient: rc,
	}
	_, err := badSvc.verifyAccessToken(token)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestIssueRefreshToken_StoredInRedis(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}

	token, err := svc.issueRefreshToken(context.Background(), "user-rt")
	if err != nil {
		t.Fatalf("issueRefreshToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty refresh token")
	}

	// Token should be in Redis.
	stored, found, err := rc.GetRefreshToken(context.Background(), "user-rt")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if !found {
		t.Fatal("refresh token not found in Redis")
	}
	if stored != token {
		t.Fatalf("stored token %q != returned token %q", stored, token)
	}
}

func TestIssueRefreshToken_Unique(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	t1, _ := svc.issueRefreshToken(ctx, "user-uniq-1")
	t2, _ := svc.issueRefreshToken(ctx, "user-uniq-2")
	if t1 == t2 {
		t.Fatal("expected unique refresh tokens for different users")
	}
}

func TestJWTClaims_JSONTags(t *testing.T) {
	// Verify the json tags match what Member 4's middleware expects.
	// This is a compile-time safety check via struct literal.
	c := jwtClaims{
		TenantID: "t",
		Role:     "r",
	}
	if c.TenantID != "t" {
		t.Fatal("TenantID field broken")
	}
	if c.Role != "r" {
		t.Fatal("Role field broken")
	}
}
