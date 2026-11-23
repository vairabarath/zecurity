package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"
)

// Invoice email delivery statuses (invoice_emails.status, D-68).
const (
	EmailPending      = "pending"       // waiting for its next attempt
	EmailSending      = "sending"       // claimed; a send may be in progress
	EmailSent         = "sent"          // every recipient accepted it
	EmailFailed       = "failed"        // gave up (retries exhausted, partial send, or interrupted)
	EmailNotDelivered = "not_delivered" // nobody to send to (no active admin, e.g. purged tenant)
)

// EmailJob is one queued invoice email (invoice_emails row).
type EmailJob struct {
	InvoiceID string
	Kind      string
	Seq       int
	Attempts  int // attempts made before this one
}

// EmailOutcome is what one attempt leaves on the row.
type EmailOutcome struct {
	Status        string
	Attempts      int        // including this attempt
	Recipients    int        // recipients the email was sent to (sent / failed partial)
	NextAttemptAt *time.Time // set when Status is pending (a retry)
	LastError     string     // sanitized: never contains an email address
	SentAt        *time.Time
}

// EmailStore is the delivery queue and the data an email needs. The SQL
// implementation works on invoice_emails (074_invoices, P1a).
type EmailStore interface {
	// ClaimDue moves up to limit pending rows whose next attempt is due to
	// sending, in one transaction, and returns them (oldest first).
	ClaimDue(ctx context.Context, now time.Time, limit int) ([]EmailJob, error)
	// FailInterrupted moves rows left in sending before cutoff (the
	// controller stopped during a send) to failed: whether the email went out
	// is unknown, so it is never retried automatically (at most once; a
	// super-admin can resend). Returns the number of rows moved.
	FailInterrupted(ctx context.Context, cutoff time.Time) (int, error)
	// LoadInvoice reads the invoice snapshot and its lines.
	LoadInvoice(ctx context.Context, invoiceID string) (EmailInvoice, error)
	// TenantAdmins returns the email addresses of the tenant's active admins
	// (users with role admin and status active), resolved at send time.
	TenantAdmins(ctx context.Context, tenantID string) ([]string, error)
	// Finish records the outcome of an attempt (sending → the new status).
	Finish(ctx context.Context, job EmailJob, out EmailOutcome) error
}

// Mailer sends one HTML + plain-text email (invitation.Emailer.SendMultipart).
type Mailer interface {
	SendMultipart(to, subject, text, html string) error
}

// EmailWorker delivers queued invoice emails (D-68). Process-local (D-02).
type EmailWorker struct {
	Store        EmailStore
	Mail         Mailer // nil: every attempt fails "email not configured" and is retried
	ProviderName string
	Now          func() time.Time // nil = time.Now
	// SendTimeout bounds one SMTP send (the SMTP client takes no context).
	SendTimeout time.Duration // 0 = 30 s
	// MaxAttempts before giving up (0 = 12: about 24 h with the backoff).
	MaxAttempts int
	// Batch is the number of rows claimed per tick (0 = 20).
	Batch int
}

func (w *EmailWorker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run fails rows interrupted by an earlier stop, then delivers due emails
// every interval until ctx ends.
func (w *EmailWorker) Run(ctx context.Context, interval time.Duration) {
	if n, err := w.Store.FailInterrupted(ctx, w.now()); err != nil {
		log.Printf("invoice email: recover interrupted sends: %v", err)
	} else if n > 0 {
		log.Printf("invoice email: %d send(s) interrupted by a restart marked failed (resend from the console)", n)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Printf("invoice email: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick claims the due rows and attempts each once. It returns the number of
// rows attempted.
func (w *EmailWorker) Tick(ctx context.Context) (int, error) {
	batch := w.Batch
	if batch <= 0 {
		batch = 20
	}
	jobs, err := w.Store.ClaimDue(ctx, w.now(), batch)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	for _, j := range jobs {
		out := w.attempt(ctx, j)
		if err := w.Store.Finish(ctx, j, out); err != nil {
			// The row stays in sending; FailInterrupted marks it failed on
			// the next start (never re-sent automatically).
			log.Printf("invoice email %s/%s/%d: record outcome: %v", j.InvoiceID, j.Kind, j.Seq, err)
		}
	}
	return len(jobs), nil
}

// attempt sends one job and decides the row's next state.
func (w *EmailWorker) attempt(ctx context.Context, j EmailJob) EmailOutcome {
	attempts := j.Attempts + 1
	retry := func(err error) EmailOutcome {
		max := w.MaxAttempts
		if max <= 0 {
			max = 12
		}
		msg := SanitizeEmailError(err)
		if attempts >= max {
			return EmailOutcome{Status: EmailFailed, Attempts: attempts, LastError: msg}
		}
		next := w.now().Add(Backoff(attempts))
		return EmailOutcome{Status: EmailPending, Attempts: attempts, NextAttemptAt: &next, LastError: msg}
	}

	inv, err := w.Store.LoadInvoice(ctx, j.InvoiceID)
	if err != nil {
		return retry(fmt.Errorf("load invoice: %w", err))
	}
	if inv.ProviderName == "" {
		inv.ProviderName = w.ProviderName
	}
	subject, text, html, err := ComposeInvoiceEmail(j.Kind, inv)
	if err != nil {
		// A content problem won't fix itself: give up now.
		return EmailOutcome{Status: EmailFailed, Attempts: attempts, LastError: SanitizeEmailError(err)}
	}
	to, err := w.Store.TenantAdmins(ctx, inv.TenantID)
	if err != nil {
		return retry(fmt.Errorf("tenant admins: %w", err))
	}
	if len(to) == 0 {
		return EmailOutcome{Status: EmailNotDelivered, Attempts: attempts, LastError: "no active tenant admin"}
	}
	if w.Mail == nil {
		return retry(errors.New("email not configured"))
	}
	timeout := w.SendTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	sent, unknown := 0, 0
	var last error
	for _, addr := range to {
		addr := addr
		if err := sendWithTimeout(ctx, timeout, func() error { return w.Mail.SendMultipart(addr, subject, text, html) }); err != nil {
			last = err
			if errors.Is(err, errSendUnknown) {
				unknown++
			}
			continue
		}
		sent++
	}
	switch {
	case unknown > 0:
		// A send that timed out (or was cut off by shutdown) may still be
		// delivered in the background: retrying could send it twice, so stop
		// (at most once). A super-admin can resend.
		return EmailOutcome{Status: EmailFailed, Attempts: attempts, Recipients: sent,
			LastError: SanitizeEmailError(fmt.Errorf("email: delivery to %d of %d recipient(s) unknown: %w", unknown, len(to), last))}
	case sent == len(to):
		at := w.now()
		return EmailOutcome{Status: EmailSent, Attempts: attempts, Recipients: sent, SentAt: &at}
	case sent == 0:
		return retry(fmt.Errorf("email to %d recipient(s) failed: %w", len(to), last))
	default:
		// Some admins have it: a retry would send it to them twice, so stop
		// here (at most once); the console shows the partial failure and a
		// super-admin can resend.
		return EmailOutcome{Status: EmailFailed, Attempts: attempts, Recipients: sent,
			LastError: SanitizeEmailError(fmt.Errorf("email: %d of %d recipient(s) failed: %w", len(to)-sent, len(to), last))}
	}
}

// Backoff before attempt n+1: 1 min doubling, at most 6 h.
func Backoff(attempt int) time.Duration {
	d := time.Minute
	for i := 1; i < attempt && d < 6*time.Hour; i++ {
		d *= 2
	}
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return d
}

var emailAddressRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+`)

// SanitizeEmailError removes email addresses (an SMTP error often quotes the
// recipient) and caps the length, so last_error can be stored and shown.
func SanitizeEmailError(err error) string {
	if err == nil {
		return ""
	}
	s := emailAddressRE.ReplaceAllString(err.Error(), "<address>")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// errSendUnknown: the send was abandoned (timeout or shutdown); it may still
// complete in the background.
var errSendUnknown = errors.New("email send abandoned; delivery unknown")

// sendWithTimeout runs send (an SMTP call that takes no context) and gives up
// after timeout; an abandoned send finishes or fails in the background.
func sendWithTimeout(ctx context.Context, timeout time.Duration, send func() error) error {
	done := make(chan error, 1)
	go func() { done <- send() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return fmt.Errorf("timed out after %s: %w", timeout, errSendUnknown)
	case <-ctx.Done():
		return fmt.Errorf("%v: %w", ctx.Err(), errSendUnknown)
	}
}
