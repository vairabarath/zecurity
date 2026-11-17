// Package totp is RFC 6238 TOTP with the parameters every authenticator app
// supports: HMAC-SHA1, 6 digits, a 30 s step, and one step of clock skew
// either side. It is a pure leaf package (no storage, no HTTP): the provider
// operators' TOTP (internal/provider, Sprint 22 Phase T) and the workspace
// users' TOTP (internal/mfa, PENDING-06) both build on it.
package totp

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

const (
	PeriodSeconds = 30
	Digits        = 6
	SkewSteps     = 1
	SecretBytes   = 20
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret returns a new random 160-bit secret.
func GenerateSecret() ([]byte, error) {
	b := make([]byte, SecretBytes)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}
	return b, nil
}

// EncodeSecret is the Base32 form shown to the user and used in the otpauth URI.
func EncodeSecret(secret []byte) string { return encoding.EncodeToString(secret) }

// DecodeSecret reverses EncodeSecret (case-insensitive, padding optional).
func DecodeSecret(s string) ([]byte, error) {
	return encoding.DecodeString(strings.TrimRight(strings.ToUpper(s), "="))
}

// Code is the code for the step that contains t.
func Code(secret []byte, t time.Time) string {
	return CodeAt(secret, t.Unix()/PeriodSeconds)
}

// CodeAt is the code for one step.
func CodeAt(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", Digits, bin%1_000_000)
}

// Match checks code against the steps around now and returns the matched
// step. A step at or before lastStep (the last one accepted) is a replay and
// never matches. Codes must be exactly six ASCII digits.
func Match(secret []byte, code string, now time.Time, lastStep *int64) (int64, bool) {
	if len(secret) == 0 || len(code) != Digits {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	current := now.Unix() / PeriodSeconds
	for d := int64(-SkewSteps); d <= SkewSteps; d++ {
		step := current + d
		if lastStep != nil && step <= *lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(CodeAt(secret, step)), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// ProvisioningURI is the otpauth:// URI authenticator apps scan.
func ProvisioningURI(issuer, account string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", EncodeSecret(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(PeriodSeconds))
	return (&url.URL{
		Scheme:   "otpauth",
		Host:     "totp",
		Path:     "/" + issuer + ":" + account,
		RawQuery: q.Encode(),
	}).String()
}
