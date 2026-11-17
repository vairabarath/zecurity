package policy

// PENDING-06: DB-backed checks of the MFA store, Admin Console Security and
// the risk score. They live in this package because it owns the
// throwaway-database test harness (lifecycleTestPool) and the compiler that
// the risk score feeds.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/mfa"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/risk"
)

func TestMFAStore_EnrollAcceptReset(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "mfastore")
	other := newLifecycleFixture(t, pool, ctx, "mfaother")
	userID := mustInsertUser(t, ctx, pool, f.wsID)
	store := mfa.NewStore(pool)

	if rec, err := store.Get(ctx, userID); err != nil || rec != nil {
		t.Fatalf("get before enrolment: rec=%v err=%v", rec, err)
	}
	// A user can't be enrolled under another workspace.
	if err := store.BeginEnrollment(ctx, userID, other.wsID, "ct", "n"); !errors.Is(err, mfa.ErrAlreadyEnrolled) {
		t.Fatalf("enrol under a foreign workspace: %v, want refusal", err)
	}
	if err := store.BeginEnrollment(ctx, userID, f.wsID, "ct1", "n1"); err != nil {
		t.Fatalf("begin enrolment: %v", err)
	}
	// An unconfirmed secret can be replaced.
	if err := store.BeginEnrollment(ctx, userID, f.wsID, "ct2", "n2"); err != nil {
		t.Fatalf("replace unconfirmed: %v", err)
	}
	rec, err := store.Get(ctx, userID)
	if err != nil || rec == nil || rec.Ciphertext != "ct2" || rec.EnabledAt != nil {
		t.Fatalf("after replace: rec=%+v err=%v", rec, err)
	}

	// The same step, raced: exactly one request wins.
	var wg sync.WaitGroup
	wins := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.AcceptStep(ctx, userID, 100)
			if err != nil {
				t.Errorf("accept step: %v", err)
			}
			wins <- ok
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for ok := range wins {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d of 8 concurrent requests accepted the same code, want exactly 1", n)
	}
	if ok, _ := store.AcceptStep(ctx, userID, 99); ok {
		t.Fatal("an older step was accepted")
	}
	if ok, _ := store.AcceptStep(ctx, userID, 101); !ok {
		t.Fatal("a newer step was refused")
	}
	rec, _ = store.Get(ctx, userID)
	if rec.EnabledAt == nil || rec.LastStep == nil || *rec.LastStep != 101 {
		t.Fatalf("after accept: %+v", rec)
	}
	// Confirmed: can't be overwritten.
	if err := store.BeginEnrollment(ctx, userID, f.wsID, "ct3", "n3"); !errors.Is(err, mfa.ErrAlreadyEnrolled) {
		t.Fatalf("overwrite a confirmed authenticator: %v", err)
	}
	enrolled, err := store.EnrolledUsers(ctx, f.wsID)
	if err != nil || !enrolled[userID] || len(enrolled) != 1 {
		t.Fatalf("enrolled users: %v err=%v", enrolled, err)
	}

	// Reset is workspace-scoped.
	if had, err := store.Delete(ctx, other.wsID, userID); err != nil || had {
		t.Fatalf("delete through a foreign workspace: had=%v err=%v", had, err)
	}
	if had, err := store.Delete(ctx, f.wsID, userID); err != nil || !had {
		t.Fatalf("delete: had=%v err=%v", had, err)
	}
	if rec, _ := store.Get(ctx, userID); rec != nil {
		t.Fatal("authenticator survived the reset")
	}
}

func TestConsolePolicy(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "consolemfa")
	p := mfa.NewConsolePolicy(pool)

	if req, err := p.Required(ctx, f.wsID); err != nil || req {
		t.Fatalf("default: required=%v err=%v, want false", req, err)
	}
	if prev, err := p.Set(ctx, f.wsID, true); err != nil || prev {
		t.Fatalf("set true: previous=%v err=%v", prev, err)
	}
	// Set drops the cache: the change is visible at once.
	if req, err := p.Required(ctx, f.wsID); err != nil || !req {
		t.Fatalf("after set: required=%v err=%v, want true", req, err)
	}
	if prev, err := p.Set(ctx, f.wsID, false); err != nil || !prev {
		t.Fatalf("set false: previous=%v err=%v", prev, err)
	}
	// An unknown workspace is an error (the middleware fails closed on it).
	if _, err := p.Required(ctx, uuid.NewString()); err == nil {
		t.Fatal("unknown workspace reported a policy")
	}
}

type countingNotifier struct {
	mu sync.Mutex
	n  int
}

func (c *countingNotifier) NotifyPolicyChange(context.Context, string) error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return nil
}

func TestRisk_ScoreThresholdAndCompile(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "risk")
	rnID, _ := mustInsertRNWithConnector(t, ctx, pool, f.wsID, "rn-risk", "td-risk", "10.44.0.1")
	rID := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-risk", "10.44.0.10", 443)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, rID, f.grpID)
	userID := mustInsertUser(t, ctx, pool, f.wsID)
	mustAddGroupMember(t, ctx, pool, f.grpID, userID)
	spiffe := "spiffe://td/client/" + userID
	deviceID := mustInsertClientDevice(t, ctx, pool, f.wsID, userID, spiffe)
	mustExec(t, ctx, pool, `UPDATE client_devices SET session_auth_at = NOW() - INTERVAL '5 minutes' WHERE id = $1`, deviceID)

	ws := uuid.MustParse(f.wsID)
	pstore := posture.NewStore(pool)
	def, err := pstore.PolicyOfResource(ctx, ws, uuid.MustParse(rID))
	if err != nil {
		t.Fatalf("policy of resource: %v", err)
	}
	day := 24 * 60
	if _, err := pstore.SetResourcePolicyAuthentication(ctx, ws, def.ID, &day, false); err != nil {
		t.Fatalf("set authentication: %v", err)
	}
	store := NewStore(pool)
	aclNotifier := NewNotifier(NewSnapshotCache())
	allowed := func() bool {
		t.Helper()
		compiled, err := CompileACLSnapshot(ctx, store, pstore, aclNotifier, f.wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, e := range compiled.Snapshot.Entries {
			if e.ResourceId == rID {
				return contains(e.AllowedSpiffeIds, spiffe)
			}
		}
		return false
	}
	pending := func() int {
		t.Helper()
		st, err := store.GetDeviceAuthState(ctx, f.wsID, deviceID)
		if err != nil {
			t.Fatalf("device auth state: %v", err)
		}
		return st.Pending(time.Now())
	}
	if !allowed() || pending() != 0 {
		t.Fatal("freshly authenticated device is not allowed")
	}

	notifier := &countingNotifier{}
	svc := risk.NewService(pool, notifier)

	// +30 stays under the threshold: nothing changes.
	if demanded, err := svc.Add(ctx, f.wsID, deviceID, risk.PostureRegressed); err != nil || demanded {
		t.Fatalf("posture regression: demanded=%v err=%v, want false", demanded, err)
	}
	if !allowed() || notifier.n != 0 {
		t.Fatalf("under the threshold: allowed=%v notifications=%d", allowed(), notifier.n)
	}
	// +30 again reaches 60: re-authentication demanded, exactly once.
	if demanded, err := svc.Add(ctx, f.wsID, deviceID, risk.PostureRegressed); err != nil || !demanded {
		t.Fatalf("second regression: demanded=%v err=%v, want true", demanded, err)
	}
	if demanded, _ := svc.Add(ctx, f.wsID, deviceID, risk.CountryChanged); demanded {
		t.Fatal("an already-flagged device was demanded again")
	}
	if notifier.n != 1 {
		t.Fatalf("notifications = %d, want 1", notifier.n)
	}
	var score int
	var reasons []string
	if err := pool.QueryRow(ctx, `SELECT risk_score, risk_reasons FROM client_devices WHERE id = $1`, deviceID).Scan(&score, &reasons); err != nil {
		t.Fatal(err)
	}
	if score != 110 || len(reasons) != 2 {
		t.Fatalf("score=%d reasons=%v, want 110 and two distinct reasons", score, reasons)
	}
	if allowed() || pending() != 1 {
		t.Fatalf("flagged device: allowed=%v pending=%d, want false 1", allowed(), pending())
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = $2`,
		risk.ActionReauthRequired, deviceID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("device.reauth_required audit rows = %d, want 1", audits)
	}

	// Concurrent signals on a fresh device: exactly one demand.
	device2 := mustInsertClientDevice(t, ctx, pool, f.wsID, userID, spiffe+"-2")
	var wg sync.WaitGroup
	results := make(chan bool, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := svc.Add(ctx, f.wsID, device2, risk.CountryChanged)
			if err != nil {
				t.Errorf("add: %v", err)
			}
			results <- d
		}()
	}
	wg.Wait()
	close(results)
	demands := 0
	for d := range results {
		if d {
			demands++
		}
	}
	if demands != 1 {
		t.Fatalf("%d concurrent signals reported the demand, want exactly 1", demands)
	}

	// Unknown, foreign and revoked devices are no-ops.
	if d, err := svc.Add(ctx, f.wsID, uuid.NewString(), risk.CountryChanged); err != nil || d {
		t.Fatalf("unknown device: demanded=%v err=%v", d, err)
	}
	if d, err := svc.Add(ctx, uuid.NewString(), deviceID, risk.CountryChanged); err != nil || d {
		t.Fatalf("foreign workspace: demanded=%v err=%v", d, err)
	}
}
