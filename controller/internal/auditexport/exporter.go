package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/outbox"
)

// Exporter limits (P-5).
const (
	PollInterval   = 2 * time.Second
	MaxBatchEvents = 500
	MaxBatchBytes  = 1 << 20
	// MaxBackoff caps retry delays for a failing sink (P-5: 15 min; the
	// outbox's own 5-minute cap is not used here).
	MaxBackoff = 15 * time.Minute
	workers    = 4
	// catchUpBudget bounds how long one worker stays on one cursor.
	catchUpBudget = 30 * time.Second
	maxDue        = 64
)

var retry = outbox.RetryPolicy{Base: 2 * time.Second, Max: MaxBackoff}

// Exporter is the single in-controller export worker (D-02).
type Exporter struct {
	pool   *pgxpool.Pool
	store  *Store
	client *http.Client
	now    func() time.Time

	mu       sync.Mutex
	inFlight map[cursorKey]bool
}

type cursorKey struct {
	sinkID string
	stream audit.Stream
}

// NewExporter builds the worker.
func NewExporter(pool *pgxpool.Pool, store *Store, client *http.Client) *Exporter {
	return &Exporter{pool: pool, store: store, client: client, now: time.Now, inFlight: map[cursorKey]bool{}}
}

// Run polls until ctx ends. A failing sink stalls only its own cursor: due
// cursors go to a bounded worker pool, and each delivery has a timeout.
func (e *Exporter) Run(ctx context.Context) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	for {
		due, err := e.dueCursors(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("audit export: list due cursors: %v", err)
		}
		for _, k := range due {
			if !e.claim(k) {
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				e.release(k)
				return
			}
			wg.Add(1)
			go func(k cursorKey) {
				defer wg.Done()
				defer func() { <-sem }()
				defer e.release(k)
				e.catchUp(ctx, k)
			}(k)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// catchUp keeps delivering one claimed cursor while it has a backlog, for up
// to catchUpBudget, so catch-up after an outage or from "all history" isn't
// limited to one batch per poll.
func (e *Exporter) catchUp(ctx context.Context, k cursorKey) {
	deadline := time.Now().Add(catchUpBudget)
	for time.Now().Before(deadline) {
		n, err := e.DeliverOnce(ctx, k.sinkID, k.stream)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("audit export: sink %s stream %s: %v", k.sinkID, k.stream, err)
			}
			return
		}
		if n == 0 {
			return
		}
	}
}

func (e *Exporter) claim(k cursorKey) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inFlight[k] {
		return false
	}
	e.inFlight[k] = true
	return true
}

func (e *Exporter) release(k cursorKey) {
	e.mu.Lock()
	delete(e.inFlight, k)
	e.mu.Unlock()
}

// dueCursors lists enabled sinks' cursors that are behind their stream head
// and not backing off.
func (e *Exporter) dueCursors(ctx context.Context) ([]cursorKey, error) {
	rows, err := e.pool.Query(ctx,
		`SELECT c.sink_id::text, c.stream
		   FROM audit_export_cursors c
		   JOIN audit_export_sinks s ON s.id = c.sink_id
		   JOIN audit_stream_heads h ON h.workspace_id = s.workspace_id AND h.stream = c.stream
		  WHERE s.enabled AND c.stream = ANY(s.streams)
		    AND h.last_seq > c.last_seq
		    AND (c.next_attempt_at <= now()
		         -- A batch-windowed cursor (S3, W-5) waiting for its window
		         -- is due at once when a full batch is pending; one in
		         -- backoff (attempts > 0) keeps waiting.
		         OR (c.attempts = 0 AND h.last_seq - c.last_seq >= $2))
		  ORDER BY c.next_attempt_at
		  LIMIT $1`, maxDue, MaxBatchEvents)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (cursorKey, error) {
		var k cursorKey
		var st string
		err := r.Scan(&k.sinkID, &st)
		k.stream = audit.Stream(st)
		return k, err
	})
}

// pending is one batch read from a consistent snapshot.
type pending struct {
	workspaceID string
	fromSeq     int64 // cursor at read time
	toSeq       int64 // cursor after the batch is acknowledged
	events      []Event
	more        bool // the stream has events past this batch (it is full)
	gap         bool // a synthesized export.gap batch
}

// DeliverOnce sends the next batch for one sink stream and moves its cursor.
// It returns the number of events acknowledged (0 = nothing to send).
func (e *Exporter) DeliverOnce(ctx context.Context, sinkID string, s audit.Stream) (int, error) {
	t, err := e.store.loadTarget(ctx, sinkID)
	if errors.Is(err, ErrNotFound) {
		return 0, nil // deleted meanwhile
	}
	if err != nil {
		return 0, err
	}
	if !t.Enabled {
		return 0, nil
	}
	adapter, ok := e.store.registry.Get(t.Kind)
	if !ok {
		return 0, e.fail(ctx, sinkID, s, &DeliveryError{Msg: "sink kind is not supported"})
	}
	p, err := e.read(ctx, sinkID, t.WorkspaceID, s)
	if err != nil || p == nil {
		return 0, err
	}
	full := p.more || len(p.events) >= MaxBatchEvents
	if w, ok := adapter.(Windowed); ok && !p.gap && !full {
		if due, wait := windowDue(p.events[0], w.BatchWindow(), e.now()); !due {
			// Not a full batch and the oldest event is still young: come back
			// when it is old enough (W-5). Only the schedule moves.
			_, err := e.pool.Exec(ctx,
				`UPDATE audit_export_cursors SET next_attempt_at = now() + make_interval(secs => $3)
				  WHERE sink_id = $1 AND stream = $2 AND last_seq = $4 AND attempts = 0`,
				sinkID, string(s), wait.Seconds(), p.fromSeq)
			return 0, err
		}
	}
	b := Batch{DeliveryID: uuid.NewString(), WorkspaceID: p.workspaceID, Stream: string(s), Events: p.events}
	dctx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	derr := adapter.Deliver(dctx, e.client, t.Destination, b)
	cancel()
	if derr != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err() // shutting down: not the sink's fault
		}
		return 0, e.fail(ctx, sinkID, s, derr)
	}
	// Advance only from the cursor this batch was read at; a concurrent
	// change (stream removed and re-added) wins.
	if _, err := e.pool.Exec(ctx,
		`UPDATE audit_export_cursors
		    SET last_seq = $3, last_success_at = now(), last_error = NULL, failing_since = NULL,
		        attempts = 0, next_attempt_at = now()
		  WHERE sink_id = $1 AND stream = $2 AND last_seq = $4`,
		sinkID, string(s), p.toSeq, p.fromSeq); err != nil {
		return 0, err
	}
	return len(p.events), nil
}

// read builds the next batch from one REPEATABLE READ snapshot, so the
// cursor, the head's retention checkpoint and the rows agree even while a
// purge runs. If retention removed rows past the cursor, the batch is a
// single export.gap event (P-4) and the cursor moves to the checkpoint.
func (e *Exporter) read(ctx context.Context, sinkID, workspaceID string, s audit.Stream) (*pending, error) {
	var p *pending
	err := pgx.BeginTxFunc(ctx, e.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var cursor int64
		err := tx.QueryRow(ctx, `SELECT last_seq FROM audit_export_cursors WHERE sink_id = $1 AND stream = $2`,
			sinkID, string(s)).Scan(&cursor)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // stream removed from the sink
		}
		if err != nil {
			return err
		}
		head, err := audit.ReadHead(ctx, tx, workspaceID, s)
		if err != nil {
			return err
		}
		if cursor < head.PurgedThroughSeq {
			p = &pending{workspaceID: workspaceID, fromSeq: cursor, toSeq: head.PurgedThroughSeq, gap: true,
				events: []Event{GapEvent(workspaceID, s, cursor+1, head.PurgedThroughSeq, head.PurgedThroughHash, e.now())}}
			return nil
		}
		if cursor >= head.LastSeq {
			return nil
		}
		var events []Event
		switch s {
		case audit.StreamAdmin:
			rows, err := audit.ReadAdminRows(ctx, tx, workspaceID, cursor, head.LastSeq, MaxBatchEvents)
			if err != nil {
				return err
			}
			for _, r := range rows {
				events = append(events, AdminEvent(r))
			}
		case audit.StreamAccess:
			rows, err := audit.ReadAccessRows(ctx, tx, workspaceID, cursor, head.LastSeq, MaxBatchEvents)
			if err != nil {
				return err
			}
			for _, r := range rows {
				events = append(events, AccessEvent(r))
			}
		}
		events = capBytes(events, MaxBatchBytes)
		if len(events) == 0 {
			return nil
		}
		last := events[len(events)-1].Seq
		p = &pending{workspaceID: workspaceID, fromSeq: cursor, toSeq: last, events: events, more: head.LastSeq > last}
		return nil
	})
	return p, err
}

// windowDue reports whether a batch starting with first may be written now
// under a batch window, and otherwise how long to wait. An event whose
// recorded_at can't be read is treated as due.
func windowDue(first Event, window time.Duration, now time.Time) (bool, time.Duration) {
	at, err := time.Parse(time.RFC3339Nano, first.RecordedAt)
	if err != nil {
		return true, 0
	}
	wait := at.Add(window).Sub(now)
	if wait <= 0 {
		return true, 0
	}
	return false, wait
}

// capBytes keeps a prefix of events whose JSON fits in limit (always at
// least one event, so an oversized row can't wedge the cursor).
func capBytes(events []Event, limit int) []Event {
	total := 0
	for i, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			return events[:i]
		}
		total += len(b) + 1
		if total > limit && i > 0 {
			return events[:i]
		}
	}
	return events
}

// fail records a failed delivery and schedules the retry.
func (e *Exporter) fail(ctx context.Context, sinkID string, s audit.Stream, derr error) error {
	var attempts int
	if err := e.pool.QueryRow(ctx,
		`SELECT attempts FROM audit_export_cursors WHERE sink_id = $1 AND stream = $2`,
		sinkID, string(s)).Scan(&attempts); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	delay := retry.Backoff(attempts)
	_, err := e.pool.Exec(ctx,
		`UPDATE audit_export_cursors
		    SET attempts = attempts + 1, last_error = $3,
		        failing_since = COALESCE(failing_since, now()),
		        next_attempt_at = now() + make_interval(secs => $4)
		  WHERE sink_id = $1 AND stream = $2`,
		sinkID, string(s), ErrorMessage(derr), delay.Seconds())
	if err != nil {
		return err
	}
	return derr
}

// SendTest delivers one signed export.test event to a sink, synchronously,
// without touching its cursors. It returns a console-safe error message, or
// "" on success.
func (e *Exporter) SendTest(ctx context.Context, workspaceID, sinkID string) (string, error) {
	t, err := e.store.loadTarget(ctx, sinkID)
	if errors.Is(err, ErrNotFound) || (err == nil && t.WorkspaceID != workspaceID) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	adapter, ok := e.store.registry.Get(t.Kind)
	if !ok {
		return "sink kind is not supported", nil
	}
	b := Batch{
		DeliveryID:  uuid.NewString(),
		WorkspaceID: workspaceID,
		Stream:      string(audit.StreamAdmin),
		Events:      []Event{TestEvent(workspaceID, sinkID, e.now())},
	}
	dctx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	defer cancel()
	if err := adapter.Deliver(dctx, e.client, t.Destination, b); err != nil {
		return ErrorMessage(err), nil
	}
	return "", nil
}
