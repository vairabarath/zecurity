package provider

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const (
	testProviderKey = "provider-signing-key-0123456789abcdef"
	testTenantKey   = "tenant-jwt-secret-0123456789abcdefgh"
)

func testUser() *ProviderUser {
	return &ProviderUser{ID: "puid-1", Email: "ops@inkyank.com", Role: RoleSuperAdmin, SessionGeneration: 7}
}

func mustIdentity(t *testing.T) *IdentityService {
	t.Helper()
	s, err := NewIdentityService(testProviderKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProviderTokenRoundTrip(t *testing.T) {
	s := mustIdentity(t)
	tok, _, err := s.IssueSession(testUser(), []string{AMRPassword}, false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "puid-1" || c.Role != RoleSuperAdmin || c.Email != "ops@inkyank.com" || c.Gen != 7 || c.PWC {
		t.Fatalf("claims mismatch: %+v", c)
	}
	if c.Issuer != appmeta.ProviderIssuer || len(c.Audience) != 1 || c.Audience[0] != ProviderAudience {
		t.Fatalf("issuer/audience mismatch: iss=%q aud=%v", c.Issuer, c.Audience)
	}
	if len(c.AMR) != 1 || c.AMR[0] != AMRPassword {
		t.Fatalf("amr = %v, want [pwd]", c.AMR)
	}
}

func TestVerifyProviderToken_RejectsWrongKey(t *testing.T) {
	tok, _, _ := mustIdentity(t).IssueSession(testUser(), []string{AMRPassword}, false)
	if _, err := VerifyProviderToken([]byte("a-completely-different-key-0123456789"), tok); err == nil {
		t.Fatal("token verified with the wrong key")
	}
}

// signRaw builds an arbitrary HS256 token so each wall can be tested alone.
func signRaw(t *testing.T, key string, claims jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims() ProviderClaims {
	now := time.Now()
	return ProviderClaims{
		Role: RoleSuperAdmin, Email: "ops@inkyank.com", Gen: 1, AMR: []string{AMRPassword},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "puid-1",
			Issuer:    appmeta.ProviderIssuer,
			Audience:  jwt.ClaimStrings{ProviderAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}
}

// A token signed with the tenant JWT_SECRET is rejected even if every claim is
// otherwise provider-shaped (D-25: dedicated key).
func TestVerifyProviderToken_RejectsTenantSecret(t *testing.T) {
	tok := signRaw(t, testTenantKey, validClaims())
	if _, err := VerifyProviderToken([]byte(testProviderKey), tok); err == nil {
		t.Fatal("token signed with the tenant secret was accepted")
	}
}

// The tenant issuer is rejected even with the right key and audience (D-25:
// dedicated issuer).
func TestVerifyProviderToken_RejectsControllerIssuer(t *testing.T) {
	c := validClaims()
	c.Issuer = appmeta.ControllerIssuer
	if _, err := VerifyProviderToken([]byte(testProviderKey), signRaw(t, testProviderKey, c)); err == nil {
		t.Fatal("token with iss=zecurity-controller was accepted")
	}
}

func TestVerifyProviderToken_RequiresAudExpAndSubject(t *testing.T) {
	noAud := validClaims()
	noAud.Audience = nil
	noExp := validClaims()
	noExp.ExpiresAt = nil
	noSub := validClaims()
	noSub.Subject = ""
	expired := validClaims()
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))

	for name, c := range map[string]ProviderClaims{"no aud": noAud, "no exp": noExp, "no sub": noSub, "expired": expired} {
		if _, err := VerifyProviderToken([]byte(testProviderKey), signRaw(t, testProviderKey, c)); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

func TestVerifyProviderToken_RejectsAlgNoneAndOtherHMAC(t *testing.T) {
	none, err := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims()).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, validClaims()).SignedString([]byte(testProviderKey))
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"alg=none": none, "HS512": hs512} {
		if _, err := VerifyProviderToken([]byte(testProviderKey), tok); err == nil {
			t.Errorf("%s token accepted", name)
		}
	}
}

func TestValidateProviderSigningKey(t *testing.T) {
	if err := ValidateProviderSigningKey(testProviderKey, testTenantKey); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for name, c := range map[string][2]string{
		"missing":       {"", testTenantKey},
		"too short":     {"short-key", testTenantKey},
		"reused tenant": {testTenantKey, testTenantKey},
		"31 bytes":      {strings.Repeat("k", 31), testTenantKey},
	} {
		if err := ValidateProviderSigningKey(c[0], c[1]); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := NewIdentityService("short"); err == nil {
		t.Error("NewIdentityService accepted a short key")
	}
}
