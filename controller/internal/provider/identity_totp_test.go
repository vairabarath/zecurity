package provider

import (
	"errors"
	"testing"
	"time"
)

// OQ-3: no token issuer can mint a full session that skips TOTP for an
// operator who requires it. IssueSession is the only full-session path.
func TestIssueSession_RefusesFullSessionWithoutOTPWhenRequired(t *testing.T) {
	s := mustIdentity(t)
	now := time.Now()
	super := &ProviderUser{ID: "u1", Email: "admin@inkyank.com", Role: RoleSuperAdmin, SessionGeneration: 1}
	enrolledOps := &ProviderUser{ID: "u2", Email: "ops@inkyank.com", Role: RoleRelayOps, SessionGeneration: 1, TOTPEnabledAt: &now}
	plainOps := &ProviderUser{ID: "u3", Email: "ops2@inkyank.com", Role: RoleRelayOps, SessionGeneration: 1}

	for _, u := range []*ProviderUser{super, enrolledOps} {
		if _, _, err := s.IssueSession(u, []string{AMRPassword}, false); !errors.Is(err, ErrTOTPRequired) {
			t.Errorf("%s: full session without otp: err=%v, want ErrTOTPRequired", u.Email, err)
		}
		if _, _, err := s.IssueSession(u, []string{AMRPassword, AMROTP}, false); err != nil {
			t.Errorf("%s: full session with otp: %v", u.Email, err)
		}
		// A password-change-only token is confined to one route; it stays allowed.
		if _, _, err := s.IssueSession(u, []string{AMRPassword}, true); err != nil {
			t.Errorf("%s: pwc token: %v", u.Email, err)
		}
	}
	if _, _, err := s.IssueSession(plainOps, []string{AMRPassword}, false); err != nil {
		t.Errorf("relay-ops without TOTP: %v", err)
	}
}

func TestIssueMFAToken_ConfinedAndShortLived(t *testing.T) {
	s := mustIdentity(t)
	u := &ProviderUser{ID: "u1", Email: "admin@inkyank.com", Role: RoleSuperAdmin, SessionGeneration: 4}
	for _, stage := range []string{MFAStageVerify, MFAStageEnroll} {
		tok, exp, err := s.IssueMFAToken(u, stage)
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if exp != int64(MFATokenTTL/time.Second) {
			t.Errorf("%s: expires_in %d, want %d", stage, exp, int64(MFATokenTTL/time.Second))
		}
		c, err := s.Verify(tok)
		if err != nil {
			t.Fatal(err)
		}
		if c.MFA != stage || c.PWC || c.Gen != 4 || len(c.AMR) != 1 || c.AMR[0] != AMRPassword {
			t.Errorf("%s: claims %+v", stage, c)
		}
	}
	if _, _, err := s.IssueMFAToken(u, "bogus"); err == nil {
		t.Error("unknown MFA stage accepted")
	}
}
