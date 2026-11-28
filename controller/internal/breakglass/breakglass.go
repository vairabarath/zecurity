// Package breakglass makes every break-glass recovery loud (Sprint 27 G1,
// F20; Decision Record D-75).
//
// `zecurity-controller provider recover-admin --email --reason` on the
// controller host is the only break-glass path. It records a row in
// provider_break_glass_events in the same transaction as the recovery
// (provider.Store.RecoverAdmin). From that row the running controller:
//   - raises the critical platform alert provider.break_glass_used (the A1
//     evaluator, alerting.Sources.breakGlassUsed; A2 delivers it);
//   - emails every active super-admin (Job below; retried until at least one
//     recipient took it; the audit records the count, never the addresses);
//   - shows a console banner to super-admins until one acknowledges the event
//     with a note (Handler; audited break_glass.acknowledge), which also
//     resolves the alert.
//
// Nothing here ever sees the temporary password: the event holds the
// recovered operator's email, the time, the host and the reason.
package breakglass

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

const (
	// SystemActor is the audit actor of the email notification.
	SystemActor = "system:break-glass-alert"
	// Subject of the email to super-admins (D-75).
	Subject = "Zecurity: break-glass recovery used"
	// MaxNote bounds the acknowledgement note.
	MaxNote = 500
)

// Event is one recover-admin run, as the banner shows it.
type Event struct {
	ID            string     `json:"id"`
	Email         string     `json:"email"`
	Reason        string     `json:"reason"`
	Host          string     `json:"host"`
	CreatedAt     time.Time  `json:"created_at"`
	NotifiedAt    *time.Time `json:"notified_at"`
	NotifiedCount *int       `json:"notified_count"`
}

// ListUnacknowledged returns the events no super-admin has acknowledged yet,
// oldest first.
func ListUnacknowledged(ctx context.Context, q providerquery.Querier) ([]Event, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, email, reason, host, created_at, notified_at, notified_count
		  FROM provider_break_glass_events
		 WHERE acknowledged_at IS NULL
		 ORDER BY created_at, id
		 LIMIT 500`)
	if err != nil {
		return nil, fmt.Errorf("list break-glass events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Email, &e.Reason, &e.Host, &e.CreatedAt, &e.NotifiedAt, &e.NotifiedCount); err != nil {
			return nil, fmt.Errorf("scan break-glass event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

var (
	ErrNotFound            = errors.New("break-glass event not found")
	ErrAlreadyAcknowledged = errors.New("break-glass event already acknowledged")
)

// Acknowledge marks one event acknowledged by a super-admin with a note,
// with its audit row, in one transaction. The alert resolves on the next
// evaluator tick.
func Acknowledge(ctx context.Context, pool *pgxpool.Pool, store *provider.Store, actor provider.Actor, ip, id, note string) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var email string
		var ackedAt *time.Time
		err := tx.QueryRow(ctx, `SELECT email, acknowledged_at FROM provider_break_glass_events WHERE id = $1 FOR UPDATE`, id).
			Scan(&email, &ackedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock break-glass event: %w", err)
		}
		if ackedAt != nil {
			return ErrAlreadyAcknowledged
		}
		if _, err := tx.Exec(ctx, `
			UPDATE provider_break_glass_events
			   SET acknowledged_at = NOW(), acknowledged_by = $2, ack_note = $3
			 WHERE id = $1`, id, actor.UserID, note); err != nil {
			return fmt.Errorf("acknowledge break-glass event: %w", err)
		}
		uid := actor.UserID
		return store.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &uid, ProviderEmail: actor.Email, IPAddress: ip,
			Action: provider.ActionBreakGlassAcknowledge, TargetType: "break_glass_event", TargetID: id,
			Details: provider.WithChange(map[string]any{"note": note, "recovered": email},
				map[string]any{"acknowledged": false}, map[string]any{"acknowledged": true}),
		})
	})
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

// Handler serves the banner routes (super-admin only, D-75):
//
//	GET  /provider/break-glass              → {items: [Event]}   (unacknowledged)
//	POST /provider/break-glass/{id}/ack     {note} → 204         (audited break_glass.acknowledge)
type Handler struct {
	DB    *pgxpool.Pool
	Store *provider.Store
	Authz *provider.Authz
}

func (h *Handler) begin(w http.ResponseWriter, r *http.Request, action string) (provider.Actor, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return provider.Actor{}, false
	}
	if h.Authz.CanManageBreakGlass(actor, action) != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return provider.Actor{}, false
	}
	return actor, true
}

// List: GET /provider/break-glass.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.begin(w, r, provider.ActionBreakGlassRead); !ok {
		return
	}
	items, err := ListUnacknowledged(r.Context(), h.DB)
	if err != nil {
		log.Printf("break-glass: %v", err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type ackRequest struct {
	Note string `json:"note"`
}

// Ack: POST /provider/break-glass/{id}/ack.
func (h *Handler) Ack(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.begin(w, r, provider.ActionBreakGlassAcknowledge)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !providerquery.IsUUID(id) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_id")
		return
	}
	var req ackRequest
	if !provider.DecodeJSON(w, r, &req) {
		return
	}
	note := strings.TrimSpace(req.Note)
	switch {
	case note == "":
		provider.WriteError(w, http.StatusBadRequest, "note_required")
		return
	case utf8.RuneCountInString(note) > MaxNote:
		provider.WriteError(w, http.StatusBadRequest, "note_too_long")
		return
	}
	err := Acknowledge(r.Context(), h.DB, h.Store, actor, remoteIP(r), id, note)
	switch {
	case errors.Is(err, ErrNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrAlreadyAcknowledged):
		provider.WriteError(w, http.StatusConflict, "already_acknowledged")
	case err != nil:
		log.Printf("break-glass: acknowledge: %v", err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func remoteIP(r *http.Request) string {
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return strings.Trim(r.RemoteAddr[:i], "[]")
	}
	return r.RemoteAddr
}

// ── email ────────────────────────────────────────────────────────────────────

// Sender sends one plain-text email. *invitation.Emailer satisfies it.
type Sender interface {
	SendPlain(to, subject, body string) error
}

// Job emails every active super-admin about each break-glass event (D-75).
// An event is marked notified (with the recipient count, audited) once at
// least one recipient took the email; otherwise it is retried on the next
// check. Single controller (D-02).
type Job struct {
	DB     *pgxpool.Pool
	Store  *provider.Store
	Sender Sender
}

// Run checks every interval until ctx ends.
func (j *Job) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := j.Check(ctx); err != nil {
			log.Printf("break-glass email: %v", err)
		} else if n > 0 {
			log.Printf("break-glass email: %d event(s) notified", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check emails the events not yet notified and returns how many were marked
// notified.
func (j *Job) Check(ctx context.Context) (int, error) {
	pending, err := j.pending(ctx)
	if err != nil || len(pending) == 0 {
		return 0, err
	}
	recipients, err := activeSuperAdmins(ctx, j.DB)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, e := range pending {
		sent, failed := 0, 0
		subject, body := Compose(e)
		for _, to := range recipients {
			if err := j.Sender.SendPlain(to, subject, body); err != nil {
				failed++
				log.Printf("break-glass email: event %s: a send failed: %v", e.ID, err)
				continue
			}
			sent++
		}
		if sent == 0 && len(recipients) > 0 {
			continue // every send failed: retried on the next check
		}
		if err := j.markNotified(ctx, e.ID, sent, failed); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func (j *Job) pending(ctx context.Context) ([]Event, error) {
	rows, err := j.DB.Query(ctx, `
		SELECT id::text, email, reason, host, created_at
		  FROM provider_break_glass_events
		 WHERE notified_at IS NULL
		 ORDER BY created_at, id
		 LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("pending break-glass events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Email, &e.Reason, &e.Host, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (j *Job) markNotified(ctx context.Context, id string, sent, failed int) error {
	return pgx.BeginFunc(ctx, j.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE provider_break_glass_events SET notified_at = NOW(), notified_count = $2
			 WHERE id = $1 AND notified_at IS NULL`, id, sent)
		if err != nil {
			return fmt.Errorf("mark break-glass event notified: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return j.Store.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderEmail: SystemActor,
			Action:        provider.ActionBreakGlassNotified,
			TargetType:    "break_glass_event",
			TargetID:      id,
			Details:       map[string]any{"recipients": sent, "failed": failed},
		})
	})
}

func activeSuperAdmins(ctx context.Context, q providerquery.Querier) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT email FROM provider_users WHERE role = $1 AND disabled_at IS NULL ORDER BY email`, provider.RoleSuperAdmin)
	if err != nil {
		return nil, fmt.Errorf("super-admins: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Compose builds the email: the recovered operator, the time, the host and
// the reason. Never a password or secret (the event holds none).
func Compose(e Event) (subject, body string) {
	var b strings.Builder
	b.WriteString("Break-glass recovery was used on the Zecurity controller host.\n\n")
	fmt.Fprintf(&b, "Operator recovered: %s\n", clean(e.Email))
	fmt.Fprintf(&b, "Time (UTC):         %s\n", e.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Host:               %s\n", clean(e.Host))
	fmt.Fprintf(&b, "Reason:             %s\n\n", clean(e.Reason))
	b.WriteString("The operator's password was reset and their TOTP cleared; they must set a new password and enrol TOTP at next sign-in.\n")
	b.WriteString("If you did not expect this, investigate now. Acknowledge it in the provider console (a note is required).\n")
	return Subject, b.String()
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
