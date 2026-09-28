package provider

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for provider operator passwords (Sprint 21 Phase H,
// D-24). Stored in the PHC string, so VerifyPassword always uses the
// parameters a hash was created with and these can be raised later without
// invalidating existing hashes.
const (
	argonMemoryKiB = 64 * 1024 // 64 MiB
	argonTime      = 3
	argonThreads   = 2
	argonSaltLen   = 16
	argonKeyLen    = 32

	// providerMaxConcurrentHashes caps simultaneous Argon2id computations (hash,
	// verify and dummy verify alike). Each one allocates argonMemoryKiB, so an
	// unauthenticated login flood from many IPs could otherwise exhaust memory.
	providerMaxConcurrentHashes = 4

	minPasswordRunes = 12
	maxPasswordRunes = 128

	temporaryPasswordLen = 20
)

var (
	// ErrHashBusy means no hashing slot freed up within hashSlotWait. Callers
	// map it to 503 login_unavailable.
	ErrHashBusy = errors.New("password hashing capacity exhausted")
	// ErrPasswordPolicy wraps every password-policy violation.
	ErrPasswordPolicy = errors.New("password does not meet policy")
	// errMalformedHash means a stored password_hash is not a PHC string this
	// package can verify.
	errMalformedHash = errors.New("malformed password hash")
)

// hashSlots is the semaphore behind providerMaxConcurrentHashes; hashSlotWait is
// how long a caller waits for a slot. Package variables so tests can observe
// and shorten them.
var (
	hashSlots    = make(chan struct{}, providerMaxConcurrentHashes)
	hashSlotWait = 2 * time.Second
)

func acquireHashSlot(ctx context.Context) (release func(), err error) {
	timer := time.NewTimer(hashSlotWait)
	defer timer.Stop()
	select {
	case hashSlots <- struct{}{}:
		return func() { <-hashSlots }, nil
	case <-timer.C:
		return nil, ErrHashBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HashPassword returns an Argon2id PHC string:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
func HashPassword(ctx context.Context, password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return encodePHC(argonMemoryKiB, argonTime, argonThreads, salt, key), nil
}

// VerifyPassword reports whether password matches the stored PHC string, using
// the parameters recorded in that string. The comparison is constant-time.
func VerifyPassword(ctx context.Context, password, phc string) (bool, error) {
	p, err := decodePHC(phc)
	if err != nil {
		return false, err
	}
	release, err := acquireHashSlot(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	key := argon2.IDKey([]byte(password), p.salt, p.time, p.memory, p.threads, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

// dummyHash is a fixed Argon2id hash with the production parameters (of an
// irrelevant password). It is a constant rather than computed at runtime so
// there is nothing to build or cache: a lazily computed hash would fail under
// the very login flood it defends against (all hashing slots busy) and a
// cached failure would break every later unknown-email login.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$+G+pcGdA+jcHQWp+0FGfIQ$BRx1BFBjmjbNJXMW7rJHuEiBSO95yxl+ZidXfxhQr8I"

// VerifyDummy spends the same work as a real verification against dummyHash.
// Login calls it when there is no real hash to check (unknown email, disabled
// account, NULL password_hash) so response time does not reveal which accounts
// exist. It returns only capacity/context errors.
func VerifyDummy(ctx context.Context, password string) error {
	_, err := VerifyPassword(ctx, password, dummyHash)
	return err
}

// ValidateNewPassword enforces the minimal Sprint 21 policy (rotation and
// complexity policies are deferred, D-28): 12–128 characters, not the email,
// and different from the current password.
func ValidateNewPassword(password, email, current string) error {
	n := utf8.RuneCountInString(password)
	if n < minPasswordRunes || n > maxPasswordRunes {
		return fmt.Errorf("%w: must be %d-%d characters", ErrPasswordPolicy, minPasswordRunes, maxPasswordRunes)
	}
	if strings.EqualFold(strings.TrimSpace(password), strings.TrimSpace(email)) {
		return fmt.Errorf("%w: must not be the email address", ErrPasswordPolicy)
	}
	if current != "" && password == current {
		return fmt.Errorf("%w: must differ from the current password", ErrPasswordPolicy)
	}
	return nil
}

// temporaryPasswordAlphabet omits look-alike characters (0/O, 1/l/I) because
// temporary passwords are read off a screen and retyped once.
const temporaryPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// GenerateTemporaryPassword returns a random temporary password for bootstrap
// resets and operator creation/reset (Phase U). It is shown once and must be
// changed at first login.
func GenerateTemporaryPassword() (string, error) {
	max := big.NewInt(int64(len(temporaryPasswordAlphabet)))
	var b strings.Builder
	b.Grow(temporaryPasswordLen)
	for i := 0; i < temporaryPasswordLen; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate temporary password: %w", err)
		}
		b.WriteByte(temporaryPasswordAlphabet[n.Int64()])
	}
	return b.String(), nil
}

type phcParams struct {
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

func encodePHC(memory, time uint32, threads uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// Upper bounds applied when decoding a stored hash, so a corrupted or tampered
// row cannot make a verification allocate unbounded memory or CPU.
const (
	maxArgonMemoryKiB = 256 * 1024
	maxArgonTime      = 10
	maxArgonThreads   = 16
)

func decodePHC(phc string) (*phcParams, error) {
	parts := strings.Split(phc, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, errMalformedHash
	}
	var p phcParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return nil, errMalformedHash
	}
	if p.memory == 0 || p.memory > maxArgonMemoryKiB || p.time == 0 || p.time > maxArgonTime ||
		p.threads == 0 || p.threads > maxArgonThreads {
		return nil, errMalformedHash
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) < 8 {
		return nil, errMalformedHash
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) < 16 || len(p.key) > 64 {
		return nil, errMalformedHash
	}
	return &p, nil
}
