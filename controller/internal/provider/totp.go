package provider

import (
	"time"

	"github.com/yourorg/ztna/controller/internal/totp"
)

// TOTP for provider operators (Sprint 22 Phase T; D-28). The RFC 6238
// primitives live in internal/totp (shared with the workspace users' TOTP,
// PENDING-06); these wrappers keep the provider's names and issuer.

const (
	totpPeriodSeconds = totp.PeriodSeconds
	totpDigits        = totp.Digits
	totpSkewSteps     = totp.SkewSteps
	totpSecretBytes   = totp.SecretBytes
	totpIssuer        = "Zecurity Provider"
)

// GenerateTOTPSecret returns a new random 160-bit secret.
func GenerateTOTPSecret() ([]byte, error) { return totp.GenerateSecret() }

// EncodeTOTPSecret is the Base32 form shown to the operator and used in the
// otpauth URI.
func EncodeTOTPSecret(secret []byte) string { return totp.EncodeSecret(secret) }

// DecodeTOTPSecret reverses EncodeTOTPSecret (case-insensitive, padding optional).
func DecodeTOTPSecret(s string) ([]byte, error) { return totp.DecodeSecret(s) }

// TOTPCode is the code for the step that contains t.
func TOTPCode(secret []byte, t time.Time) string { return totp.Code(secret, t) }

func totpCodeAt(secret []byte, step int64) string { return totp.CodeAt(secret, step) }

// matchTOTP checks code against the steps around now and returns the matched
// step. A step at or before lastStep (the last one accepted) is a replay and
// never matches. Codes must be exactly six ASCII digits.
func matchTOTP(secret []byte, code string, now time.Time, lastStep *int64) (int64, bool) {
	return totp.Match(secret, code, now, lastStep)
}

// TOTPProvisioningURI is the otpauth:// URI authenticator apps scan.
func TOTPProvisioningURI(email string, secret []byte) string {
	return totp.ProvisioningURI(totpIssuer, email, secret)
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
