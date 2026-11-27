package billing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu       sync.Mutex
	jobs     []EmailJob
	invoices map[string]EmailInvoice
	admins   map[string][]string
	adminErr error
	outcomes map[string]EmailOutcome
	stale    int
}

func key(j EmailJob) string { return j.InvoiceID + "/" + j.Kind }

func (s *fakeStore) ClaimDue(_ context.Context, _ time.Time, limit int) ([]EmailJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.jobs)
	if n > limit {
		n = limit
	}
	out := s.jobs[:n]
	s.jobs = s.jobs[n:]
	return out, nil
}
func (s *fakeStore) FailInterrupted(context.Context, time.Time) (int, error) { return s.stale, nil }
func (s *fakeStore) LoadInvoice(_ context.Context, id string) (EmailInvoice, error) {
	inv, ok := s.invoices[id]
	if !ok {
		return EmailInvoice{}, errors.New("no such invoice")
	}
	return inv, nil
}
func (s *fakeStore) TenantAdmins(_ context.Context, tenantID string) ([]string, error) {
	if s.adminErr != nil {
		return nil, s.adminErr
	}
	return s.admins[tenantID], nil
}
func (s *fakeStore) Finish(_ context.Context, j EmailJob, out EmailOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcomes == nil {
		s.outcomes = map[string]EmailOutcome{}
	}
	s.outcomes[key(j)] = out
	return nil
}

type fakeMailer struct {
	mu    sync.Mutex
	sent  []string // recipients
	fail  map[string]error
	block map[string]bool
}

func (m *fakeMailer) SendMultipart(to, subject, text, html string) error {
	if m.block[to] {
		time.Sleep(200 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail[to]; err != nil {
		return err
	}
	m.sent = append(m.sent, to)
	return nil
}

var t0 = time.Date(2026, 12, 2, 6, 0, 0, 0, time.UTC)

func newWorker(s *fakeStore, m Mailer) *EmailWorker {
	return &EmailWorker{Store: s, Mail: m, Now: func() time.Time { return t0 }, SendTimeout: 50 * time.Millisecond}
}

func storeWith(admins map[string][]string, jobs ...EmailJob) *fakeStore {
	inv := sampleInvoice()
	inv2 := sampleInvoice()
	inv2.ID, inv2.TenantID, inv2.Number = "inv-2", "t-2", "INV-2026-000002"
	return &fakeStore{jobs: jobs, admins: admins, invoices: map[string]EmailInvoice{"inv-1": inv, "inv-2": inv2}}
}

// Each tenant's email goes only to that tenant's own admins, once.
func TestEmailWorker_SentToOwnAdminsOnly(t *testing.T) {
	s := storeWith(map[string][]string{"t-1": {"a1@acme.test", "a2@acme.test"}, "t-2": {"b@other.test"}},
		EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	m := &fakeMailer{}
	n, err := newWorker(s, m).Tick(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("tick = %d, %v", n, err)
	}
	if strings.Join(m.sent, ",") != "a1@acme.test,a2@acme.test" {
		t.Fatalf("sent to %v", m.sent)
	}
	out := s.outcomes["inv-1/issued"]
	if out.Status != EmailSent || out.Recipients != 2 || out.Attempts != 1 || out.SentAt == nil {
		t.Fatalf("outcome %+v", out)
	}
	// Nothing left: a second tick sends nothing more.
	if n, _ := newWorker(s, m).Tick(context.Background()); n != 0 || len(m.sent) != 2 {
		t.Fatalf("second tick sent again: %d, %v", n, m.sent)
	}
}

func TestEmailWorker_NoAdminsNotDelivered(t *testing.T) {
	s := storeWith(map[string][]string{}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	m := &fakeMailer{}
	newWorker(s, m).Tick(context.Background())
	if out := s.outcomes["inv-1/issued"]; out.Status != EmailNotDelivered || len(m.sent) != 0 {
		t.Fatalf("outcome %+v, sent %v", out, m.sent)
	}
}

// Every recipient refused: retried with backoff; the stored error never
// contains an address; after MaxAttempts it gives up.
func TestEmailWorker_RetryThenFail(t *testing.T) {
	s := storeWith(map[string][]string{"t-1": {"a1@acme.test"}}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	m := &fakeMailer{fail: map[string]error{"a1@acme.test": errors.New("550 5.1.1 <a1@acme.test>: mailbox unavailable")}}
	w := newWorker(s, m)
	w.Tick(context.Background())
	out := s.outcomes["inv-1/issued"]
	if out.Status != EmailPending || out.NextAttemptAt == nil || !out.NextAttemptAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first failure outcome %+v", out)
	}
	if strings.Contains(out.LastError, "acme.test") || !strings.Contains(out.LastError, "<address>") {
		t.Fatalf("last error leaks the address: %q", out.LastError)
	}
	s.jobs = []EmailJob{{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1, Attempts: 11}}
	w.Tick(context.Background())
	if out := s.outcomes["inv-1/issued"]; out.Status != EmailFailed || out.Attempts != 12 {
		t.Fatalf("after max attempts %+v", out)
	}
}

// Some admins got it, some didn't: failed, not retried (no duplicates).
func TestEmailWorker_PartialIsNotRetried(t *testing.T) {
	s := storeWith(map[string][]string{"t-1": {"a1@acme.test", "a2@acme.test"}}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	m := &fakeMailer{fail: map[string]error{"a2@acme.test": errors.New("452 try later")}}
	newWorker(s, m).Tick(context.Background())
	out := s.outcomes["inv-1/issued"]
	if out.Status != EmailFailed || out.Recipients != 1 || out.NextAttemptAt != nil {
		t.Fatalf("partial outcome %+v", out)
	}
}

// A send that times out may still go out: failed, not retried.
func TestEmailWorker_TimeoutIsNotRetried(t *testing.T) {
	s := storeWith(map[string][]string{"t-1": {"slow@acme.test"}}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	m := &fakeMailer{block: map[string]bool{"slow@acme.test": true}}
	newWorker(s, m).Tick(context.Background())
	out := s.outcomes["inv-1/issued"]
	if out.Status != EmailFailed || out.NextAttemptAt != nil || !strings.Contains(out.LastError, "unknown") {
		t.Fatalf("timeout outcome %+v", out)
	}
}

func TestEmailWorker_TransientErrorsRetry(t *testing.T) {
	// No mailer configured, or the admin lookup failing: retried.
	s := storeWith(map[string][]string{"t-1": {"a1@acme.test"}}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	newWorker(s, nil).Tick(context.Background())
	if out := s.outcomes["inv-1/issued"]; out.Status != EmailPending || out.LastError != "email not configured" {
		t.Fatalf("no mailer outcome %+v", out)
	}
	s = storeWith(nil, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	s.adminErr = errors.New("db down")
	newWorker(s, &fakeMailer{}).Tick(context.Background())
	if out := s.outcomes["inv-1/issued"]; out.Status != EmailPending {
		t.Fatalf("admin lookup error outcome %+v", out)
	}
	// A content problem (no number) gives up at once.
	s = storeWith(map[string][]string{"t-1": {"a1@acme.test"}}, EmailJob{InvoiceID: "inv-1", Kind: EmailKindIssued, Seq: 1})
	inv := s.invoices["inv-1"]
	inv.Number = ""
	s.invoices["inv-1"] = inv
	newWorker(s, &fakeMailer{}).Tick(context.Background())
	if out := s.outcomes["inv-1/issued"]; out.Status != EmailFailed {
		t.Fatalf("content error outcome %+v", out)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	if got := Backoff(20); got != 6*time.Hour {
		t.Errorf("Backoff(20) = %s, want 6h", got)
	}
	// 12 attempts span roughly a day.
	var total time.Duration
	for i := 1; i < 12; i++ {
		total += Backoff(i)
	}
	if total < 18*time.Hour || total > 30*time.Hour {
		t.Errorf("11 backoffs = %s, want about 24 h", total)
	}
}
