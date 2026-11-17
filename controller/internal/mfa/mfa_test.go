package mfa

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/totp"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type fakeRepo struct {
	rec *Record
	err error
}

func (f *fakeRepo) Get(context.Context, string) (*Record, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.rec == nil {
		return nil, nil
	}
	c := *f.rec
	return &c, nil
}

func (f *fakeRepo) BeginEnrollment(_ context.Context, userID, workspaceID, ct, nonce string) error {
	if f.rec != nil && f.rec.EnabledAt != nil {
		return ErrAlreadyEnrolled
	}
	f.rec = &Record{UserID: userID, WorkspaceID: workspaceID, Ciphertext: ct, Nonce: nonce}
	return nil
}

func (f *fakeRepo) AcceptStep(_ context.Context, _ string, step int64) (bool, error) {
	if f.rec == nil || (f.rec.LastStep != nil && *f.rec.LastStep >= step) {
		return false, nil
	}
	f.rec.LastStep = &step
	if f.rec.EnabledAt == nil {
		now := time.Now()
		f.rec.EnabledAt = &now
	}
	return true, nil
}

func (f *fakeRepo) Delete(context.Context, string, string) (bool, error) {
	had := f.rec != nil
	f.rec = nil
	return had, nil
}

// fakeSealer "seals" by prefixing; enough to prove the stored value is not
// the Base32 secret and that the context is bound to the user.
type fakeSealer struct{}

func (fakeSealer) EncryptSecret(p []byte, ctx string) (string, string, error) {
	return "sealed:" + ctx + ":" + string(p), "nonce", nil
}

func (fakeSealer) DecryptSecret(ct, _ string, ctx string) ([]byte, error) {
	prefix := "sealed:" + ctx + ":"
	if !strings.HasPrefix(ct, prefix) {
		return nil, errors.New("wrong context")
	}
	return []byte(strings.TrimPrefix(ct, prefix)), nil
}

type fakeLimiter struct {
	n   int
	err error
}

func (l *fakeLimiter) Blocked(context.Context, string) (bool, time.Duration, error) {
	if l.err != nil {
		return false, 0, l.err
	}
	return l.n >= FailMax, time.Minute, nil
}

func (l *fakeLimiter) RecordFailure(context.Context, string) (bool, error) {
	if l.err != nil {
		return false, l.err
	}
	l.n++
	return l.n == FailMax, nil
}

func (l *fakeLimiter) Reset(context.Context, string) error { l.n = 0; return nil }

func newTestService() (*Service, *fakeRepo, *fakeLimiter, *time.Time) {
	repo, lim := &fakeRepo{}, &fakeLimiter{}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	svc := NewService(repo, fakeSealer{}, lim)
	svc.now = func() time.Time { return now }
	return svc, repo, lim, &now
}

func codeFor(t *testing.T, enr *Enrollment, at time.Time) string {
	t.Helper()
	secret, err := totp.DecodeSecret(enr.Secret)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return totp.Code(secret, at)
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestEnrollThenVerify(t *testing.T) {
	ctx := context.Background()
	svc, repo, _, now := newTestService()

	if ok, _ := svc.Enrolled(ctx, "u1"); ok {
		t.Fatal("enrolled before enrolment")
	}
	if _, _, err := svc.Verify(ctx, "u1", "123456"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("verify without an authenticator: %v, want ErrNotEnrolled", err)
	}

	enr, err := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com")
	if err != nil {
		t.Fatalf("start enrolment: %v", err)
	}
	if !strings.HasPrefix(enr.URI, "otpauth://totp/Zecurity:u1@example.com?") {
		t.Fatalf("provisioning URI = %q", enr.URI)
	}
	// M-1: the stored value is sealed, bound to the user, never the secret.
	if repo.rec.Ciphertext == enr.Secret || !strings.HasPrefix(repo.rec.Ciphertext, "sealed:workspace-user-totp:u1:") {
		t.Fatalf("stored secret is not sealed for the user: %q", repo.rec.Ciphertext)
	}
	// An unconfirmed authenticator does not count as enrolled.
	if ok, _ := svc.Enrolled(ctx, "u1"); ok {
		t.Fatal("enrolled before the first code was confirmed")
	}

	res, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now))
	if err != nil || !res.Enrolled {
		t.Fatalf("first code: res=%+v err=%v, want Enrolled", res, err)
	}
	if ok, _ := svc.Enrolled(ctx, "u1"); !ok {
		t.Fatal("not enrolled after the first code")
	}

	// A code is accepted once.
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("replayed code: %v, want ErrInvalidCode", err)
	}
	// The next step's code works and is a plain verification.
	*now = now.Add(30 * time.Second)
	res, _, err = svc.Verify(ctx, "u1", codeFor(t, enr, *now))
	if err != nil || res.Enrolled {
		t.Fatalf("second code: res=%+v err=%v, want a plain success", res, err)
	}

	// A confirmed authenticator can't be replaced by enrolling again.
	if _, err := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com"); !errors.Is(err, ErrAlreadyEnrolled) {
		t.Fatalf("re-enrol over a confirmed authenticator: %v, want ErrAlreadyEnrolled", err)
	}
}

func TestVerifyRateLimitAndTrip(t *testing.T) {
	ctx := context.Background()
	svc, _, lim, now := newTestService()
	enr, _ := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com")
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	for i := 1; i <= FailMax; i++ {
		_, failure, err := svc.Verify(ctx, "u1", "000000")
		if !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("wrong code %d: %v, want ErrInvalidCode", i, err)
		}
		if failure.Tripped != (i == FailMax) {
			t.Fatalf("wrong code %d: tripped=%v", i, failure.Tripped)
		}
	}
	// Locked out: even the right code is refused, and it is not consumed.
	*now = now.Add(30 * time.Second)
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("locked out: %v, want ErrRateLimited", err)
	}
	lim.n = 0
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); err != nil {
		t.Fatalf("after the lockout the same code must still work: %v", err)
	}
}

func TestVerifyFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc, repo, lim, now := newTestService()
	enr, _ := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com")
	good := codeFor(t, enr, *now)

	// Limiter down: refused although the code is right, and not consumed.
	lim.err = errors.New("valkey down")
	if _, _, err := svc.Verify(ctx, "u1", good); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("limiter down: %v, want ErrUnavailable", err)
	}
	lim.err = nil
	// Store down: refused.
	repo.err = errors.New("db down")
	if _, _, err := svc.Verify(ctx, "u1", good); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("store down: %v, want ErrUnavailable", err)
	}
	repo.err = nil
	if _, _, err := svc.Verify(ctx, "u1", good); err != nil {
		t.Fatalf("recovered: %v", err)
	}
	// Malformed codes never match.
	for _, c := range []string{"", "12345", "1234567", "12345a", " 12345"} {
		if _, _, err := svc.Verify(ctx, "u1", c); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("code %q: %v, want ErrInvalidCode", c, err)
		}
	}
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	svc, _, _, now := newTestService()
	enr, _ := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com")
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	had, err := svc.Reset(ctx, "ws1", "u1")
	if err != nil || !had {
		t.Fatalf("reset: had=%v err=%v", had, err)
	}
	if ok, _ := svc.Enrolled(ctx, "u1"); ok {
		t.Fatal("still enrolled after reset")
	}
	// The old authenticator's codes are dead; a new enrolment is allowed.
	*now = now.Add(30 * time.Second)
	if _, _, err := svc.Verify(ctx, "u1", codeFor(t, enr, *now)); !errors.Is(err, ErrNotEnrolled) {
		t.Fatalf("old code after reset: %v, want ErrNotEnrolled", err)
	}
	if _, err := svc.StartEnrollment(ctx, "u1", "ws1", "u1@example.com"); err != nil {
		t.Fatalf("enrol after reset: %v", err)
	}
}
