package alerting

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/auditexport"
	"github.com/yourorg/ztna/controller/internal/outbox"
)

// Mailer sends one plain-text email (the F14 SMTP sender,
// invitation.Emailer.SendPlain).
type Mailer interface {
	SendPlain(to, subject, body string) error
}

// Webhook headers (D-52). The signature is the SIEM export's scheme:
// t=<unix>,v1=<hex HMAC-SHA256(secret, "<unix>." + body)>. Receivers verify
// it and reject a t older than ReplayWindow; X-Zecurity-Event-Id lets them
// drop a redelivery (a retry after a lost response).
const (
	HeaderSignature = "X-Zecurity-Signature"
	HeaderEventID   = "X-Zecurity-Event-Id"
	ReplayWindow    = 5 * time.Minute
)

// Delivery attempt outcomes (alert_deliveries.status).
const (
	DeliveryDelivered = "delivered"
	DeliveryRetrying  = "retrying"
	DeliveryFailed    = "failed"
)

// DefaultRetryPolicy is the outbox's exponential backoff with alert-sized
// numbers: 5 s, 10 s, 20 s … capped at 5 min, 10 attempts (~25 min).
func DefaultRetryPolicy() outbox.RetryPolicy {
	return outbox.RetryPolicy{Base: 5 * time.Second, Max: outbox.MaxBackoff, Jitter: 0, MaxRetries: 10}
}

// Deliverer is the delivery worker (D-52, refined 2026-11-20: its own
// durable queue, alert_delivery_queue, with the outbox's retry policy). It
// also implements Notifier: a notification only wakes it early; the queue
// rows written with each event are the source of truth.
type Deliverer struct {
	Store      *Store
	Channels   *ChannelStore
	HTTP       *http.Client // auditexport.NewClient: SSRF guard at dial time, no redirects
	Mail       Mailer       // nil: email channels fail ("email not configured")
	ConsoleURL string       // provider console origin for console_url; "" = null
	Policy     outbox.RetryPolicy
	Now        func() time.Time
	Lease      time.Duration
	Batch      int
	// MailTimeout bounds one email send (the SMTP client takes no context).
	MailTimeout time.Duration

	wakeOnce sync.Once
	wake     chan struct{}
}

func (d *Deliverer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Deliverer) wakeCh() chan struct{} {
	d.wakeOnce.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

// Notify implements Notifier: wake the worker (non-blocking).
func (d *Deliverer) Notify(_ context.Context, events []Event) {
	for _, e := range events {
		log.Printf("alert %s: %s [%s/%s] %s", e.Transition, e.Kind, e.Severity, e.Scope, e.Summary)
	}
	select {
	case d.wakeCh() <- struct{}{}:
	default:
	}
}

// Run delivers due rows every interval (or when notified) until ctx ends.
func (d *Deliverer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	wake := d.wakeCh()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for d.RunOnce(ctx) > 0 && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-wake:
		}
	}
}

type queued struct {
	eventID, channelID string
	attempts           int
}

// RunOnce leases up to Batch due rows and delivers them; it returns how
// many it handled. A row whose lease expires (a crash mid-send) is picked
// up again.
func (d *Deliverer) RunOnce(ctx context.Context) int {
	lease, batch := d.Lease, d.Batch
	if lease <= 0 {
		lease = time.Minute
	}
	if batch <= 0 {
		batch = 20
	}
	now := d.now()
	rows, err := d.Store.pool.Query(ctx, `
UPDATE alert_delivery_queue q SET lease_until = $2
 WHERE (q.event_id, q.channel_id) IN (
       SELECT event_id, channel_id FROM alert_delivery_queue
        WHERE next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until < $1)
        ORDER BY next_attempt_at
        LIMIT $3
        FOR UPDATE SKIP LOCKED)
RETURNING q.event_id::text, q.channel_id::text, q.attempts`, now, now.Add(lease), batch)
	if err != nil {
		log.Printf("alert delivery: claim: %v", err)
		return 0
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (queued, error) {
		var q queued
		return q, r.Scan(&q.eventID, &q.channelID, &q.attempts)
	})
	if err != nil {
		log.Printf("alert delivery: claim: %v", err)
		return 0
	}
	for _, q := range items {
		if ctx.Err() != nil {
			break
		}
		d.deliver(ctx, q)
	}
	return len(items)
}

func (d *Deliverer) loadEvent(ctx context.Context, id string) (Event, error) {
	var e Event
	err := d.Store.pool.QueryRow(ctx, `
SELECT id::text, rule_id::text, kind, scope, tenant_id::text, target_type, target_id, target_name,
       transition, status, severity, summary, started_at, occurred_at
  FROM alert_events WHERE id = $1`, id).Scan(&e.ID, &e.RuleID, &e.Kind, &e.Scope, &e.TenantID, &e.TargetType,
		&e.TargetID, &e.TargetName, &e.Transition, &e.Status, &e.Severity, &e.Summary, &e.StartedAt, &e.OccurredAt)
	return e, err
}

// deliver makes one attempt and records it.
func (d *Deliverer) deliver(ctx context.Context, q queued) {
	attempt := q.attempts + 1
	policy := d.Policy
	if policy.MaxRetries == 0 {
		policy = DefaultRetryPolicy()
	}
	// failOrRetry: a failed attempt retries with backoff until the last one.
	failOrRetry := func(status *int, msg string) {
		if attempt >= policy.MaxRetries {
			d.finish(ctx, q, attempt, DeliveryFailed, status, msg)
			return
		}
		d.retry(ctx, q, attempt, status, msg, d.now().Add(policy.Backoff(attempt-1)))
	}
	ev, err := d.loadEvent(ctx, q.eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return // purged: the queue row went with it
	}
	if err != nil {
		// A transient read error (failover, timeout) is retried, never final.
		log.Printf("alert delivery: event %s: %v", q.eventID, err)
		failOrRetry(nil, "event unreadable (will retry)")
		return
	}
	ch, secret, err := d.Channels.loadChannel(ctx, d.Store.pool, q.channelID)
	if errors.Is(err, ErrNotFound) {
		return // deleted: the queue row went with it
	}
	if err != nil {
		log.Printf("alert delivery: channel %s: %v", q.channelID, err)
		failOrRetry(nil, "channel unreadable (will retry)")
		return
	}
	if !ch.Enabled {
		d.finish(ctx, q, attempt, DeliveryFailed, nil, "channel disabled")
		return
	}
	// The channel's filters may have changed since the event was queued
	// (D-50): never send a tenant alert to a channel now limited to
	// platform alerts, or below its current minimum severity.
	if !channelMatches(ch.Channel, ev) {
		d.finish(ctx, q, attempt, DeliveryFailed, nil, "channel filters no longer match")
		return
	}
	p := NewPayload(ev, d.ConsoleURL)
	status, sendErr := d.send(ctx, ch, secret, p)
	if sendErr == nil {
		d.finish(ctx, q, attempt, DeliveryDelivered, status, "")
		return
	}
	var partial *partialEmailError
	if errors.As(sendErr, &partial) {
		// Some recipients got it; retrying would duplicate theirs. Record the
		// shortfall so it is visible (A3).
		d.finish(ctx, q, attempt, DeliveryDelivered, nil, sendErr.Error())
		return
	}
	failOrRetry(status, sendErr.Error())
}

func severityRankOf(s string) int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	}
	return 2
}

// channelMatches is the routing rule of insertEvent, re-checked at send time.
func channelMatches(c Channel, e Event) bool {
	if c.ScopeFilter != FilterAll && c.ScopeFilter != e.Scope {
		return false
	}
	return severityRankOf(e.Severity) <= severityRankOf(c.MinSeverity)
}

// send delivers p over the channel. It returns the HTTP status (webhook)
// and an error when the attempt failed.
func (d *Deliverer) send(ctx context.Context, ch channelRow, secret []byte, p Payload) (*int, error) {
	switch ch.Type {
	case ChannelWebhook:
		if ch.url == nil {
			return nil, errors.New("webhook without a url")
		}
		return d.postWebhook(ctx, *ch.url, secret, p.EventID, p.JSON())
	case ChannelEmail:
		return nil, d.sendEmail(ctx, ch.Recipients, p.EmailSubject(), p.EmailBody())
	}
	return nil, fmt.Errorf("unknown channel type %q", ch.Type)
}

func (d *Deliverer) postWebhook(ctx context.Context, rawURL string, secret []byte, eventID string, body []byte) (*int, error) {
	if d.HTTP == nil {
		return nil, errors.New("webhook client not configured")
	}
	rctx, cancel := context.WithTimeout(ctx, auditexport.DeliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid webhook url")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Zecurity-Alerts/1")
	req.Header.Set(HeaderEventID, eventID)
	req.Header.Set(HeaderSignature, auditexport.Signature(secret, d.now(), body))
	resp, err := d.HTTP.Do(req)
	if err != nil {
		// Never echo the URL: its path may be a credential.
		return nil, fmt.Errorf("post to %s failed: %s", auditexport.RedactURL(rawURL), auditexport.DescribeTransportError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	code := resp.StatusCode
	if code < 200 || code > 299 {
		return &code, fmt.Errorf("receiver answered HTTP %d", code)
	}
	return &code, nil
}

func (d *Deliverer) sendEmail(ctx context.Context, recipients []string, subject, body string) error {
	mailer := d.Mail // captured: an abandoned send keeps using this one
	if mailer == nil {
		return errors.New("email not configured")
	}
	to := recipients
	if to == nil {
		var err error
		if to, err = activeSuperAdmins(ctx, d.Store); err != nil {
			return err
		}
	}
	if len(to) == 0 {
		return errors.New("no recipients")
	}
	timeout := d.MailTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	sent := 0
	var last error
	for _, addr := range to {
		if err := sendWithTimeout(ctx, timeout, func() error { return mailer.SendPlain(addr, subject, body) }); err != nil {
			last = err
			continue
		}
		sent++
	}
	switch {
	case sent == 0:
		return fmt.Errorf("email to %d recipients failed: %s", len(to), truncate(last.Error(), 200))
	case sent < len(to):
		return &partialEmailError{failed: len(to) - sent, total: len(to), last: truncate(last.Error(), 200)}
	}
	return nil
}

// partialEmailError: some recipients got the email, some didn't.
type partialEmailError struct {
	failed, total int
	last          string
}

func (e *partialEmailError) Error() string {
	return fmt.Sprintf("email: %d of %d recipients failed: %s", e.failed, e.total, e.last)
}

// sendWithTimeout runs send (an SMTP call that takes no context) and gives up
// after timeout, so one stalled mail server can't block every other
// delivery. A send that is abandoned finishes (or fails) in the background.
func sendWithTimeout(ctx context.Context, timeout time.Duration, send func() error) error {
	done := make(chan error, 1)
	go func() { done <- send() }()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return errors.New("email send timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func activeSuperAdmins(ctx context.Context, s *Store) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT email FROM provider_users WHERE role = 'super-admin' AND disabled_at IS NULL ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("list super-admins: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// finish records the last attempt and removes the queue row.
func (d *Deliverer) finish(ctx context.Context, q queued, attempt int, status string, httpStatus *int, errText string) {
	err := d.Store.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertDelivery(ctx, tx, q, attempt, status, httpStatus, errText); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM alert_delivery_queue WHERE event_id = $1 AND channel_id = $2`, q.eventID, q.channelID)
		return err
	})
	if err != nil {
		log.Printf("alert delivery: record %s: %v", status, err)
	}
	if status == DeliveryFailed {
		log.Printf("alert delivery: event %s to channel %s failed after %d attempts: %s", q.eventID, q.channelID, attempt, errText)
	}
}

// retry records a failed attempt and schedules the next one.
func (d *Deliverer) retry(ctx context.Context, q queued, attempt int, httpStatus *int, errText string, next time.Time) {
	err := d.Store.inTx(ctx, func(tx pgx.Tx) error {
		if err := insertDelivery(ctx, tx, q, attempt, DeliveryRetrying, httpStatus, errText); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE alert_delivery_queue SET attempts = $3, next_attempt_at = $4, lease_until = NULL
		                          WHERE event_id = $1 AND channel_id = $2`, q.eventID, q.channelID, attempt, next)
		return err
	})
	if err != nil {
		log.Printf("alert delivery: schedule retry: %v", err)
	}
}

func insertDelivery(ctx context.Context, tx pgx.Tx, q queued, attempt int, status string, httpStatus *int, errText string) error {
	var e *string
	if errText != "" {
		t := truncate(errText, 500)
		e = &t
	}
	_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (event_id, channel_id, attempt, status, http_status, error)
	                        VALUES ($1, $2, $3, $4, $5, $6)`, q.eventID, q.channelID, attempt, status, httpStatus, e)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// TestResult is the outcome of a channel test send.
type TestResult struct {
	Delivered  bool   `json:"delivered"`
	HTTPStatus *int   `json:"http_status"`
	Error      string `json:"error,omitempty"`
}

// SendTest sends a synthetic "test" event over a channel now (not queued,
// not recorded as an alert).
func (d *Deliverer) SendTest(ctx context.Context, channelID string) (TestResult, error) {
	ch, secret, err := d.Channels.loadChannel(ctx, d.Store.pool, channelID)
	if err != nil {
		return TestResult{}, err
	}
	now := d.now().UTC()
	p := Payload{
		Version: PayloadVersion, EventID: "test-" + now.Format("20060102T150405Z"), EventType: "test",
		Status: "firing", Severity: SeverityInfo, Scope: ScopePlatform,
		Target:    PayloadTarget{Type: "channel", ID: ch.ID, Name: ch.Name},
		Summary:   "Test alert from Zecurity: this channel receives alerts",
		StartedAt: now.Format(time.RFC3339), Timestamp: now.Format(time.RFC3339),
	}
	status, err := d.send(ctx, ch, secret, p)
	if err != nil {
		return TestResult{HTTPStatus: status, Error: err.Error()}, nil
	}
	return TestResult{Delivered: true, HTTPStatus: status}, nil
}
