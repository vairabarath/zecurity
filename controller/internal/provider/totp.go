package provider

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP for provider operators (Sprint 22 Phase T; D-28). RFC 6238 with the
// parameters every authenticator app supports: HMAC-SHA1, 6 digits, a 30 s
// step, and one step of clock skew either side.

const (
	totpPeriodSeconds = 30
	totpDigits        = 6
	totpSkewSteps     = 1
	totpSecretBytes   = 20
	totpIssuer        = "Zecurity Provider"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new random 160-bit secret.
func GenerateTOTPSecret() ([]byte, error) {
	b := make([]byte, totpSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}
	return b, nil
}

// EncodeTOTPSecret is the Base32 form shown to the operator and used in the
// otpauth URI.
func EncodeTOTPSecret(secret []byte) string { return totpEncoding.EncodeToString(secret) }

// DecodeTOTPSecret reverses EncodeTOTPSecret (case-insensitive, padding optional).
func DecodeTOTPSecret(s string) ([]byte, error) {
	return totpEncoding.DecodeString(strings.TrimRight(strings.ToUpper(s), "="))
}

// TOTPCode is the code for the step that contains t.
func TOTPCode(secret []byte, t time.Time) string {
	return totpCodeAt(secret, t.Unix()/totpPeriodSeconds)
}

func totpCodeAt(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, bin%1_000_000)
}

// matchTOTP checks code against the steps around now and returns the matched
// step. A step at or before lastStep (the last one accepted) is a replay and
// never matches. Codes must be exactly six ASCII digits.
func matchTOTP(secret []byte, code string, now time.Time, lastStep *int64) (int64, bool) {
	if len(secret) == 0 || len(code) != totpDigits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	current := now.Unix() / totpPeriodSeconds
	for d := int64(-totpSkewSteps); d <= totpSkewSteps; d++ {
		step := current + d
		if lastStep != nil && step <= *lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCodeAt(secret, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// TOTPProvisioningURI is the otpauth:// URI authenticator apps scan.
func TOTPProvisioningURI(email string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", EncodeTOTPSecret(secret))
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(totpDigits))
	q.Set("period", fmt.Sprint(totpPeriodSeconds))
	return (&url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + totpIssuer + ":" + email,
		RawQuery: q.Encode(),
	}).String()
}

// RequiresTOTP: a super-admin always (D-28), and anyone who has TOTP enabled
// (e.g. a demoted super-admin), so an enrolled factor can't be skipped. Both
// IssueSession and RequireProvider apply this rule to the DB row.
func RequiresTOTP(u *ProviderUser) bool {
	return u.Role == RoleSuperAdmin || u.TOTPEnabledAt != nil
}

func hasAMR(amr []string, method string) bool {
	for _, m := range amr {
		if m == method {
			return true
		}
	}
	return false
}
