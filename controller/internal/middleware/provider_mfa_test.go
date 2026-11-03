package middleware

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 22 Phase T (D-28, OQ-2, OQ-3): RequireProvider rejects a full token
// without "otp" for any operator who requires TOTP — decided from the DB row on
// every request, so a token minted before the deploy is refused at once.

// legacyToken signs a full provider token the way the pre-Phase-T controller
// did (amr ["pwd"] only). IssueSession can no longer mint one for a super-admin.
func legacyToken(t *testing.T, u provider.ProviderUser) string {
	t.Helper()
	now := time.Now()
	claims := provider.ProviderClaims{
		Role:  u.Role,
		Email: u.Email,
		Gen:   u.SessionGeneration,
		AMR:   []string{provider.AMRPassword},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			Issuer:    appmeta.ProviderIssuer,
			Audience:  jwt.ClaimStrings{provider.ProviderAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *requireProviderFixture) fullToken(t *testing.T, u provider.ProviderUser) string {
	t.Helper()
	tok, _, err := f.ids.IssueSession(&u, []string{provider.AMRPassword, provider.AMROTP}, false)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *requireProviderFixture) mfaToken(t *testing.T, u provider.ProviderUser, stage string) string {
	t.Helper()
	tok, _, err := f.ids.IssueMFAToken(&u, stage)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRequireProvider_SuperAdminLegacyTokenRejected(t *testing.T) {
	f := newRequireProviderFixture(t)
	u := *f.users.users["u1"] // super-admin
	code, body, actor := f.serve(t, "Bearer "+legacyToken(t, u))
	if code != http.StatusForbidden || !strings.Contains(body, "mfa_required") || actor != nil {
		t.Fatalf("pre-deploy super-admin token: %d %s actor=%v", code, body, actor)
	}
	if code, _, _ := f.serve(t, "Bearer "+f.fullToken(t, u)); code != http.StatusOK {
		t.Fatalf("super-admin token with otp: %d", code)
	}
}

func TestRequireProvider_RelayOpsWithoutTOTPAllowed(t *testing.T) {
	f := newRequireProviderFixture(t)
	f.users.users["u2"] = &provider.ProviderUser{ID: "u2", Email: "relay@inkyank.com", Role: provider.RoleRelayOps, SessionGeneration: 1}
	tok, _, err := f.ids.IssueSession(f.users.users["u2"], []string{provider.AMRPassword}, false)
	if err != nil {
		t.Fatal(err)
	}
	if code, body, actor := f.serve(t, "Bearer "+tok); code != http.StatusOK || actor == nil || actor.Role != provider.RoleRelayOps {
		t.Fatalf("relay-ops password-only token: %d %s %v", code, body, actor)
	}
}

func TestRequireProvider_EnrolledRelayOpsNeedsOTP(t *testing.T) {
	f := newRequireProviderFixture(t)
	now := time.Now()
	f.users.users["u2"] = &provider.ProviderUser{ID: "u2", Email: "relay@inkyank.com", Role: provider.RoleRelayOps, SessionGeneration: 1, TOTPEnabledAt: &now}
	u := *f.users.users["u2"]
	if code, body, _ := f.serve(t, "Bearer "+legacyToken(t, u)); code != http.StatusForbidden || !strings.Contains(body, "mfa_required") {
		t.Fatalf("enrolled relay-ops without otp: %d %s", code, body)
	}
	if code, _, _ := f.serve(t, "Bearer "+f.fullToken(t, u)); code != http.StatusOK {
		t.Fatalf("enrolled relay-ops with otp: %d", code)
	}
}

func TestRequireProvider_MFATokenConfinedToItsStage(t *testing.T) {
	f := newRequireProviderFixture(t)
	u := *f.users.users["u1"]
	verify := f.mfaToken(t, u, provider.MFAStageVerify)
	enroll := f.mfaToken(t, u, provider.MFAStageEnroll)

	// Ordinary routes refuse both.
	for name, tok := range map[string]string{"verify": verify, "enroll": enroll} {
		if code, body, _ := f.serve(t, "Bearer "+tok); code != http.StatusForbidden || !strings.Contains(body, "mfa_required") {
			t.Errorf("%s token on an ordinary route: %d %s", name, code, body)
		}
	}
	// The password route doesn't accept them either.
	if code, _, _ := f.serve(t, "Bearer "+verify, AllowPasswordChangeToken()); code != http.StatusForbidden {
		t.Errorf("verify token on the password route: %d", code)
	}
	// Each is accepted only on its own stage's routes.
	if code, _, actor := f.serve(t, "Bearer "+verify, AllowMFAToken(provider.MFAStageVerify)); code != http.StatusOK || actor == nil {
		t.Errorf("verify token on the verify route: %d", code)
	}
	if code, _, _ := f.serve(t, "Bearer "+enroll, AllowMFAToken(provider.MFAStageVerify)); code != http.StatusForbidden {
		t.Errorf("enroll token on the verify route: %d", code)
	}
	if code, _, _ := f.serve(t, "Bearer "+enroll, AllowMFAToken(provider.MFAStageEnroll)); code != http.StatusOK {
		t.Errorf("enroll token on the enroll route: %d", code)
	}
	// An MFA route doesn't take a full token (it would let a session re-verify forever).
	if code, _, _ := f.serve(t, "Bearer "+f.fullToken(t, u), AllowMFAToken(provider.MFAStageVerify)); code != http.StatusForbidden {
		t.Errorf("full token on the verify route: %d", code)
	}
}
