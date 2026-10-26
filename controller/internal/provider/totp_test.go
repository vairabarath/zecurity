package provider

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 secret "12345678901234567890". The RFC lists
// 8-digit codes; a 6-digit code is the last six digits of the same value.
func TestTOTPCode_RFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, tc := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	} {
		if got := TOTPCode(secret, time.Unix(tc.unix, 0)); got != tc.want {
			t.Errorf("TOTPCode(t=%d) = %s, want %s", tc.unix, got, tc.want)
		}
	}
}

func TestMatchTOTP_AcceptsOneStepEitherSide(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / totpPeriodSeconds
	for _, d := range []int64{-1, 0, 1} {
		code := TOTPCode(secret, now.Add(time.Duration(d*totpPeriodSeconds)*time.Second))
		got, ok := matchTOTP(secret, code, now, nil)
		if !ok || got != step+d {
			t.Errorf("offset %d: ok=%v step=%d, want step %d", d, ok, got, step+d)
		}
	}
	for _, d := range []int64{-2, 2} {
		code := TOTPCode(secret, now.Add(time.Duration(d*totpPeriodSeconds)*time.Second))
		if _, ok := matchTOTP(secret, code, now, nil); ok {
			t.Errorf("offset %d accepted; only one step of skew is allowed", d)
		}
	}
}

func TestMatchTOTP_RejectsReplayAndOlderSteps(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / totpPeriodSeconds
	code := TOTPCode(secret, now)

	same := step
	if _, ok := matchTOTP(secret, code, now, &same); ok {
		t.Fatal("a code for the last accepted step was accepted again (replay)")
	}
	newer := step + 1
	if _, ok := matchTOTP(secret, code, now, &newer); ok {
		t.Fatal("a code older than the last accepted step was accepted")
	}
	older := step - 1
	if got, ok := matchTOTP(secret, code, now, &older); !ok || got != step {
		t.Fatalf("a fresh code after an older accepted step: ok=%v step=%d", ok, got)
	}
}

func TestMatchTOTP_RejectsMalformedCodes(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1_700_000_000, 0)
	good := TOTPCode(secret, now)
	for _, code := range []string{"", "12345", "1234567", "12a456", " " + good, good + " ", "+12345", "-12345"} {
		if _, ok := matchTOTP(secret, code, now, nil); ok {
			t.Errorf("malformed code %q accepted", code)
		}
	}
	if _, ok := matchTOTP(nil, good, now, nil); ok {
		t.Error("empty secret accepted a code")
	}
}

func TestGenerateTOTPSecret_LengthAndUniqueness(t *testing.T) {
	a, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != totpSecretBytes || len(b) != totpSecretBytes {
		t.Fatalf("secret lengths %d, %d; want %d", len(a), len(b), totpSecretBytes)
	}
	if string(a) == string(b) {
		t.Fatal("two generated secrets are identical")
	}
}

func TestTOTPProvisioningURI_RoundTrips(t *testing.T) {
	secret := []byte("12345678901234567890")
	raw := TOTPProvisioningURI("ops@inkyank.com", secret)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Fatalf("uri %q: want otpauth://totp/...", raw)
	}
	if !strings.Contains(u.Path, "ops@inkyank.com") {
		t.Fatalf("uri %q does not name the account", raw)
	}
	q := u.Query()
	if q.Get("issuer") == "" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("algorithm") != "SHA1" {
		t.Fatalf("uri %q: missing issuer/digits/period/algorithm", raw)
	}
	got, err := DecodeTOTPSecret(q.Get("secret"))
	if err != nil || string(got) != string(secret) {
		t.Fatalf("secret round trip: %q, %v", got, err)
	}
}

func TestRequiresTOTP(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		u    ProviderUser
		want bool
	}{
		{"super-admin", ProviderUser{Role: RoleSuperAdmin}, true},
		{"relay-ops", ProviderUser{Role: RoleRelayOps}, false},
		{"relay-ops with TOTP enabled", ProviderUser{Role: RoleRelayOps, TOTPEnabledAt: &now}, true},
		{"unknown role", ProviderUser{Role: "root"}, false},
	} {
		if got := RequiresTOTP(&tc.u); got != tc.want {
			t.Errorf("%s: RequiresTOTP = %v, want %v", tc.name, got, tc.want)
		}
	}
}
