// Package mfa is first-party MFA for workspace users (PENDING-06 M-1): one
// TOTP authenticator per user, enrolled on first use and verified whenever a
// policy asks for MFA.
//
// Rules (they mirror the provider operators' TOTP, Sprint 22 Phase T):
//   - The secret is sealed with pki.EncryptSecret and is shown exactly once,
//     at enrolment. It is never logged, audited or returned again.
//   - A code is accepted once: last_step only moves forward, in one UPDATE,
//     so two concurrent requests with the same code cannot both pass.
//   - Verification is rate-limited per user and fails CLOSED: a limiter
//     error refuses the code.
//
// The package knows nothing about sessions or devices; callers record a
// successful verification where it belongs (the console session, or the
// client device's session).
package mfa

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yourorg/ztna/controller/internal/totp"
)

// Issuer is the account label authenticator apps show.
const Issuer = "Zecurity"

var (
	// ErrInvalidCode: the code is wrong, malformed, or already used.
	ErrInvalidCode = errors.New("mfa: invalid code")
	// ErrRateLimited: too many wrong codes; try again later.
	ErrRateLimited = errors.New("mfa: too many failed codes")
	// ErrNotEnrolled: the user has no authenticator (or an unconfirmed one
	// that has to be started again).
	ErrNotEnrolled = errors.New("mfa: not enrolled")
	// ErrAlreadyEnrolled: the user already has a confirmed authenticator.
	ErrAlreadyEnrolled = errors.New("mfa: already enrolled")
	// ErrUnavailable: the limiter or the store failed; nothing was accepted.
	ErrUnavailable = errors.New("mfa: temporarily unavailable")
)

// Sealer seals and opens the TOTP secret (pki.EncryptSecret).
type Sealer interface {
	EncryptSecret(plaintext []byte, context string) (ciphertextB64, nonceB64 string, err error)
	DecryptSecret(ciphertextB64, nonceB64, context string) ([]byte, error)
}

func sealContext(userID string) string { return "workspace-user-totp:" + userID }

// Record is a user's stored authenticator.
type Record struct {
	UserID      string
	WorkspaceID string
	Ciphertext  string
	Nonce       string
	EnabledAt   *time.Time
	LastStep    *int64
}

// Repo is the storage the service needs (Store implements it).
type Repo interface {
	Get(ctx context.Context, userID string) (*Record, error)
	// BeginEnrollment stores a new, unconfirmed secret. It replaces an
	// earlier unconfirmed one and refuses (ErrAlreadyEnrolled) a confirmed one.
	BeginEnrollment(ctx context.Context, userID, workspaceID, ciphertext, nonce string) error
	// AcceptStep marks the authenticator enabled (if it was not) and moves
	// last_step to step. It reports false when step is not newer than the
	// stored one (a replay, or a lost race with the same code).
	AcceptStep(ctx context.Context, userID string, step int64) (bool, error)
	Delete(ctx context.Context, workspaceID, userID string) (bool, error)
}

// Limiter counts wrong codes per user.
type Limiter interface {
	Blocked(ctx context.Context, userID string) (blocked bool, retryAfter time.Duration, err error)
	// RecordFailure counts one wrong code; tripped is true when this failure
	// crossed the threshold (the start of a lockout).
	RecordFailure(ctx context.Context, userID string) (tripped bool, err error)
	Reset(ctx context.Context, userID string) error
}

// Service enrols and verifies workspace users' TOTP.
type Service struct {
	repo    Repo
	sealer  Sealer
	limiter Limiter
	now     func() time.Time
}

func NewService(repo Repo, sealer Sealer, limiter Limiter) *Service {
	return &Service{repo: repo, sealer: sealer, limiter: limiter, now: time.Now}
}

// Enrolled reports whether the user has a confirmed authenticator.
func (s *Service) Enrolled(ctx context.Context, userID string) (bool, error) {
	rec, err := s.repo.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	return rec != nil && rec.EnabledAt != nil, nil
}

// Enrollment is what the user needs to add the account to an authenticator
// app. It exists only in the response to StartEnrollment.
type Enrollment struct {
	Secret string // Base32
	URI    string // otpauth://
}

// StartEnrollment creates a new secret for a user with no confirmed
// authenticator. The authenticator only counts once Verify accepts its
// first code.
func (s *Service) StartEnrollment(ctx context.Context, userID, workspaceID, email string) (*Enrollment, error) {
	secret, err := totp.GenerateSecret()
	if err != nil {
		return nil, err
	}
	ct, nonce, err := s.sealer.EncryptSecret(secret, sealContext(userID))
	if err != nil {
		return nil, fmt.Errorf("mfa: seal secret: %w", err)
	}
	if err := s.repo.BeginEnrollment(ctx, userID, workspaceID, ct, nonce); err != nil {
		return nil, err
	}
	return &Enrollment{
		Secret: totp.EncodeSecret(secret),
		URI:    totp.ProvisioningURI(Issuer, email, secret),
	}, nil
}

// Result describes a successful Verify.
type Result struct {
	// Enrolled is true when this code confirmed a new authenticator.
	Enrolled bool
}

// Failure describes a refused Verify alongside its error.
type Failure struct {
	// Tripped is true when this wrong code started a lockout (the risk
	// signal of M-9).
	Tripped    bool
	RetryAfter time.Duration
}

// Verify checks one code. A first correct code confirms a pending
// enrolment. On refusal the error is ErrInvalidCode, ErrRateLimited,
// ErrNotEnrolled or ErrUnavailable.
func (s *Service) Verify(ctx context.Context, userID, code string) (Result, Failure, error) {
	blocked, retry, err := s.limiter.Blocked(ctx, userID)
	if err != nil {
		return Result{}, Failure{}, ErrUnavailable
	}
	if blocked {
		return Result{}, Failure{RetryAfter: retry}, ErrRateLimited
	}
	rec, err := s.repo.Get(ctx, userID)
	if err != nil {
		return Result{}, Failure{}, ErrUnavailable
	}
	if rec == nil {
		return Result{}, Failure{}, ErrNotEnrolled
	}
	secret, err := s.sealer.DecryptSecret(rec.Ciphertext, rec.Nonce, sealContext(userID))
	if err != nil {
		return Result{}, Failure{}, ErrUnavailable
	}
	step, ok := totp.Match(secret, code, s.now(), rec.LastStep)
	if ok {
		// The UPDATE is the authority: it only succeeds for a newer step.
		ok, err = s.repo.AcceptStep(ctx, userID, step)
		if err != nil {
			return Result{}, Failure{}, ErrUnavailable
		}
	}
	if !ok {
		tripped, lerr := s.limiter.RecordFailure(ctx, userID)
		if lerr != nil {
			return Result{}, Failure{}, ErrUnavailable
		}
		return Result{}, Failure{Tripped: tripped}, ErrInvalidCode
	}
	// Best effort: a stale failure counter only shortens the next window.
	_ = s.limiter.Reset(ctx, userID)
	return Result{Enrolled: rec.EnabledAt == nil}, Failure{}, nil
}

// Reset deletes the user's authenticator (M-8). It reports whether one existed.
func (s *Service) Reset(ctx context.Context, workspaceID, userID string) (bool, error) {
	existed, err := s.repo.Delete(ctx, workspaceID, userID)
	if err != nil {
		return false, err
	}
	_ = s.limiter.Reset(ctx, userID)
	return existed, nil
}
