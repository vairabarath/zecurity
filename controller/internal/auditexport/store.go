package auditexport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/audit"
)

// MaxSinksPerWorkspace is the P-8 limit.
const MaxSinksPerWorkspace = 3

// LagWarning is when the console flags a sink as behind (S-6): access events
// older than the 90-day floor may be purged before it catches up.
const LagWarning = 7 * 24 * time.Hour

// Sealer is pki.Service's secret sealing (AES-256-GCM under the master secret).
type Sealer interface {
	EncryptSecret(plaintext []byte, context string) (ciphertextB64, nonceB64 string, err error)
	DecryptSecret(ciphertextB64, nonceB64, context string) ([]byte, error)
}

func sealContext(sinkID string) string { return "audit-export-sink:" + sinkID }

var (
	// ErrNotFound: no such sink in this workspace.
	ErrNotFound = errors.New("audit export sink not found")
)

// InputError is a validation failure safe to show to the caller.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func inputErr(format string, args ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, args...)}
}

// ChangedBy is the console user making a change (for the audit row).
type ChangedBy struct {
	UserID string
	Email  string
}

// StreamStatus is one sink cursor's delivery state (P-5 console status).
type StreamStatus struct {
	Stream        string
	CursorSeq     int64
	HeadSeq       int64
	LastSuccessAt *time.Time
	LastError     *string
	FailingSince  *time.Time
	// OldestPendingAt is when the oldest undelivered event was recorded.
	OldestPendingAt *time.Time
	// Lagging: the oldest undelivered event is LagWarning old or older.
	Lagging bool
}

// Lag is the number of events not yet acknowledged.
func (s StreamStatus) Lag() int64 {
	if s.HeadSeq > s.CursorSeq {
		return s.HeadSeq - s.CursorSeq
	}
	return 0
}

// Sink is a sink as the API shows it: never any secret.
type Sink struct {
	ID             string
	WorkspaceID    string
	Name           string
	Kind           string
	URL            string
	Streams        []string
	Enabled        bool
	Config         json.RawMessage
	AuthHeaderName *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Status         []StreamStatus
}

// Store manages sinks and their cursors.
type Store struct {
	pool         *pgxpool.Pool
	sealer       Sealer
	registry     *Registry
	allowPrivate bool
	now          func() time.Time
}

// NewStore builds a Store. allowPrivate is the dev override (ClientOptions).
func NewStore(pool *pgxpool.Pool, sealer Sealer, registry *Registry, allowPrivate bool) *Store {
	return &Store{pool: pool, sealer: sealer, registry: registry, allowPrivate: allowPrivate, now: time.Now}
}

// Registry returns the adapter registry.
func (s *Store) Registry() *Registry { return s.registry }

// ── validation ─────────────────────────────────────────────────────────────

var headerName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// reservedHeaders can't be set as the extra auth header.
var reservedHeaders = []string{
	"host", "content-type", "content-length", "transfer-encoding", "connection",
	"keep-alive", "te", "trailer", "upgrade", "user-agent", "proxy-authorization",
	"expect", "cookie",
}

func validateHeader(name, value string) error {
	if !headerName.MatchString(name) {
		return inputErr("auth header name must be 1-64 letters, digits or '-'")
	}
	l := strings.ToLower(name)
	if slices.Contains(reservedHeaders, l) || strings.HasPrefix(l, "x-zecurity-") {
		return inputErr("auth header name %q is reserved", name)
	}
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return inputErr("auth header value must be 1-4096 characters on one line")
	}
	return nil
}

func validateName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" || len([]rune(n)) > 64 || n != name {
		return inputErr("name must be 1-64 characters without leading or trailing spaces")
	}
	return nil
}

func normalizeStreams(in []string) ([]string, error) {
	var out []string
	for _, s := range in {
		if !audit.Stream(s).Valid() {
			return nil, inputErr("unknown stream %q", s)
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, inputErr("choose at least one stream")
	}
	slices.Sort(out)
	return out, nil
}

// ── create ─────────────────────────────────────────────────────────────────

// StartFrom picks a new sink's first event (P-5).
type StartFrom string

const (
	StartAllHistory StartFrom = "all" // all retained history (default)
	StartNow        StartFrom = "now" // only events recorded from now on
)

// AuthHeader is the optional extra static header (sealed).
type AuthHeader struct {
	Name  string
	Value string
}

// CreateInput creates a sink.
type CreateInput struct {
	WorkspaceID string
	Name        string
	Kind        string
	URL         string
	Streams     []string
	Enabled     bool
	StartFrom   StartFrom
	Config      json.RawMessage
	AuthHeader  *AuthHeader
	Actor       ChangedBy
}

func newSigningSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "zsk_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Store) validateConfig(kind string, cfg json.RawMessage) (json.RawMessage, error) {
	a, ok := s.registry.Get(kind)
	if !ok {
		return nil, inputErr("unknown sink kind %q", kind)
	}
	if err := a.ValidateConfig(cfg); err != nil {
		return nil, inputErr("%v", err)
	}
	if len(strings.TrimSpace(string(cfg))) == 0 || string(cfg) == "null" {
		cfg = json.RawMessage(`{}`)
	}
	return cfg, nil
}

// Create validates and stores a new sink, its cursors and its audit row in
// one transaction. It returns the signing secret, shown once.
func (s *Store) Create(ctx context.Context, in CreateInput) (Sink, string, error) {
	if err := validateName(in.Name); err != nil {
		return Sink{}, "", err
	}
	if err := ValidateURL(in.URL, s.allowPrivate); err != nil {
		return Sink{}, "", inputErr("%v", err)
	}
	streams, err := normalizeStreams(in.Streams)
	if err != nil {
		return Sink{}, "", err
	}
	cfg, err := s.validateConfig(in.Kind, in.Config)
	if err != nil {
		return Sink{}, "", err
	}
	if in.Kind == "splunk_hec" && in.AuthHeader == nil {
		return Sink{}, "", inputErr("a Splunk HEC sink needs its HEC token")
	}
	if in.AuthHeader != nil {
		if err := validateHeader(in.AuthHeader.Name, in.AuthHeader.Value); err != nil {
			return Sink{}, "", err
		}
	}
	switch in.StartFrom {
	case "", StartAllHistory:
		in.StartFrom = StartAllHistory
	case StartNow:
	default:
		return Sink{}, "", inputErr("startFrom must be all or now")
	}

	id := uuid.NewString()
	secret, err := newSigningSecret()
	if err != nil {
		return Sink{}, "", fmt.Errorf("signing secret: %w", err)
	}
	keyCT, keyNonce, err := s.sealer.EncryptSecret([]byte(secret), sealContext(id))
	if err != nil {
		return Sink{}, "", fmt.Errorf("seal signing secret: %w", err)
	}
	var hName, hCT, hNonce *string
	if in.AuthHeader != nil {
		ct, n, err := s.sealer.EncryptSecret([]byte(in.AuthHeader.Value), sealContext(id))
		if err != nil {
			return Sink{}, "", fmt.Errorf("seal auth header: %w", err)
		}
		hName, hCT, hNonce = &in.AuthHeader.Name, &ct, &n
	}

	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialise creates per workspace for the limit. NO KEY UPDATE doesn't
		// block concurrent audit inserts (they take FOR KEY SHARE).
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workspaces WHERE id = $1 FOR NO KEY UPDATE`, in.WorkspaceID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_export_sinks WHERE workspace_id = $1`, in.WorkspaceID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxSinksPerWorkspace {
			return inputErr("a workspace can have at most %d audit export sinks", MaxSinksPerWorkspace)
		}
		var createdBy any
		if in.Actor.UserID != "" {
			createdBy = in.Actor.UserID
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_export_sinks
			   (id, workspace_id, name, kind, url, streams, enabled, config,
			    signing_key_ct, signing_key_nonce, auth_header_name, auth_header_ct, auth_header_nonce, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			id, in.WorkspaceID, in.Name, in.Kind, in.URL, streams, in.Enabled, cfg,
			keyCT, keyNonce, hName, hCT, hNonce, createdBy,
		); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return inputErr("a sink named %q already exists", in.Name)
			}
			return err
		}
		if err := addCursors(ctx, tx, id, in.WorkspaceID, streams, in.StartFrom); err != nil {
			return err
		}
		return audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    in.WorkspaceID,
			ActorUserID: in.Actor.UserID,
			ActorEmail:  in.Actor.Email,
			Action:      "audit_export.sink.create",
			TargetType:  "audit_export_sink",
			TargetID:    id,
			Details: map[string]any{
				"name": in.Name, "kind": in.Kind, "url": RedactURL(in.URL), "streams": streams,
				"enabled": in.Enabled, "start_from": string(in.StartFrom),
				"auth_header_name": hName, "config": json.RawMessage(cfg),
			},
		})
	})
	if err != nil {
		return Sink{}, "", err
	}
	sink, err := s.Get(ctx, in.WorkspaceID, id)
	return sink, secret, err
}

// addCursors creates cursors for streams: at the retention checkpoint ("all
// retained history", so no export.gap for rows purged before the sink
// existed) or at the head ("now"; the sink's own audit_export.sink.create row
// is written after this, so it is the first admin event delivered). A row
// inserted but not yet committed by another transaction already holds a seq
// above the head read here, so it is never skipped. Existing cursors are
// kept.
func addCursors(ctx context.Context, tx pgx.Tx, sinkID, workspaceID string, streams []string, from StartFrom) error {
	col := "purged_through_seq"
	if from == StartNow {
		col = "last_seq"
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO audit_export_cursors (sink_id, stream, last_seq)
		 SELECT $1, h.stream, h.`+col+`
		   FROM audit_stream_heads h
		  WHERE h.workspace_id = $2 AND h.stream = ANY($3)
		 ON CONFLICT (sink_id, stream) DO NOTHING`,
		sinkID, workspaceID, streams)
	return err
}

// ── update ─────────────────────────────────────────────────────────────────

// UpdateInput changes a sink; nil fields are unchanged.
type UpdateInput struct {
	WorkspaceID string
	ID          string
	Name        *string
	URL         *string
	Streams     []string // nil = unchanged
	Enabled     *bool
	Config      json.RawMessage // nil = unchanged
	AuthHeader  *AuthHeader     // set or replace
	ClearAuth   bool            // remove the auth header
	Actor       ChangedBy
}

type sinkRow struct {
	Name       string
	Kind       string
	URL        string
	Streams    []string
	Enabled    bool
	Config     json.RawMessage
	HeaderName *string
}

func lockSink(ctx context.Context, tx pgx.Tx, workspaceID, id string) (sinkRow, error) {
	var r sinkRow
	err := tx.QueryRow(ctx,
		`SELECT name, kind, url, streams, enabled, config, auth_header_name
		   FROM audit_export_sinks WHERE id = $1 AND workspace_id = $2 FOR UPDATE`,
		id, workspaceID,
	).Scan(&r.Name, &r.Kind, &r.URL, &r.Streams, &r.Enabled, &r.Config, &r.HeaderName)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// Update applies in. A stream that is added starts from all retained
// history; a stream that is removed loses its cursor.
func (s *Store) Update(ctx context.Context, in UpdateInput) (Sink, error) {
	if _, err := uuid.Parse(in.ID); err != nil {
		return Sink{}, ErrNotFound
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockSink(ctx, tx, in.WorkspaceID, in.ID)
		if err != nil {
			return err
		}
		next := cur
		changed := map[string]any{}
		if in.Name != nil && *in.Name != cur.Name {
			if err := validateName(*in.Name); err != nil {
				return err
			}
			next.Name, changed["name"] = *in.Name, *in.Name
		}
		if in.URL != nil && *in.URL != cur.URL {
			if err := ValidateURL(*in.URL, s.allowPrivate); err != nil {
				return inputErr("%v", err)
			}
			// A stored credential must not follow the sink to a new host: an
			// editor could otherwise capture a token they never saw.
			if cur.HeaderName != nil && in.AuthHeader == nil && !in.ClearAuth && !sameOrigin(cur.URL, *in.URL) {
				return inputErr("changing the destination host requires entering the auth header or Splunk token again")
			}
			next.URL, changed["url"] = *in.URL, RedactURL(*in.URL)
		}
		if in.Streams != nil {
			st, err := normalizeStreams(in.Streams)
			if err != nil {
				return err
			}
			if !slices.Equal(st, cur.Streams) {
				next.Streams, changed["streams"] = st, st
			}
		}
		if in.Enabled != nil && *in.Enabled != cur.Enabled {
			next.Enabled, changed["enabled"] = *in.Enabled, *in.Enabled
		}
		if in.Config != nil {
			cfg, err := s.validateConfig(cur.Kind, in.Config)
			if err != nil {
				return err
			}
			next.Config, changed["config"] = cfg, json.RawMessage(cfg)
		}
		var hName, hCT, hNonce *string
		authChange := false
		switch {
		case in.AuthHeader != nil && in.ClearAuth:
			return inputErr("set the auth header or clear it, not both")
		case in.AuthHeader != nil:
			if err := validateHeader(in.AuthHeader.Name, in.AuthHeader.Value); err != nil {
				return err
			}
			ct, n, err := s.sealer.EncryptSecret([]byte(in.AuthHeader.Value), sealContext(in.ID))
			if err != nil {
				return fmt.Errorf("seal auth header: %w", err)
			}
			hName, hCT, hNonce, authChange = &in.AuthHeader.Name, &ct, &n, true
			changed["auth_header"] = "replaced"
			changed["auth_header_name"] = in.AuthHeader.Name
		case in.ClearAuth:
			if cur.Kind == "splunk_hec" {
				return inputErr("a Splunk HEC sink needs its HEC token")
			}
			authChange = true
			changed["auth_header"] = "removed"
		}
		if len(changed) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE audit_export_sinks
			    SET name = $3, url = $4, streams = $5, enabled = $6, config = $7,
			        auth_header_name  = CASE WHEN $8 THEN $9  ELSE auth_header_name  END,
			        auth_header_ct    = CASE WHEN $8 THEN $10 ELSE auth_header_ct    END,
			        auth_header_nonce = CASE WHEN $8 THEN $11 ELSE auth_header_nonce END,
			        updated_at = now()
			  WHERE id = $1 AND workspace_id = $2`,
			in.ID, in.WorkspaceID, next.Name, next.URL, next.Streams, next.Enabled, next.Config,
			authChange, hName, hCT, hNonce,
		); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23505" {
				return inputErr("a sink named %q already exists", next.Name)
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM audit_export_cursors WHERE sink_id = $1 AND NOT (stream = ANY($2))`,
			in.ID, next.Streams); err != nil {
			return err
		}
		if err := addCursors(ctx, tx, in.ID, in.WorkspaceID, next.Streams, StartAllHistory); err != nil {
			return err
		}
		changed["name_before"] = cur.Name
		return audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    in.WorkspaceID,
			ActorUserID: in.Actor.UserID,
			ActorEmail:  in.Actor.Email,
			Action:      "audit_export.sink.update",
			TargetType:  "audit_export_sink",
			TargetID:    in.ID,
			Details:     changed,
		})
	})
	if err != nil {
		return Sink{}, err
	}
	return s.Get(ctx, in.WorkspaceID, in.ID)
}

// RotateSecret replaces the signing secret and returns the new one (shown once).
func (s *Store) RotateSecret(ctx context.Context, workspaceID, id string, actor ChangedBy) (Sink, string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Sink{}, "", ErrNotFound
	}
	secret, err := newSigningSecret()
	if err != nil {
		return Sink{}, "", fmt.Errorf("signing secret: %w", err)
	}
	ct, n, err := s.sealer.EncryptSecret([]byte(secret), sealContext(id))
	if err != nil {
		return Sink{}, "", fmt.Errorf("seal signing secret: %w", err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockSink(ctx, tx, workspaceID, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE audit_export_sinks SET signing_key_ct = $2, signing_key_nonce = $3, updated_at = now() WHERE id = $1`,
			id, ct, n); err != nil {
			return err
		}
		return audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    workspaceID,
			ActorUserID: actor.UserID,
			ActorEmail:  actor.Email,
			Action:      "audit_export.sink.rotate_secret",
			TargetType:  "audit_export_sink",
			TargetID:    id,
			Details:     map[string]any{"name": cur.Name},
		})
	})
	if err != nil {
		return Sink{}, "", err
	}
	sink, err := s.Get(ctx, workspaceID, id)
	return sink, secret, err
}

// Delete removes a sink and its cursors.
func (s *Store) Delete(ctx context.Context, workspaceID, id string, actor ChangedBy) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockSink(ctx, tx, workspaceID, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM audit_export_sinks WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    workspaceID,
			ActorUserID: actor.UserID,
			ActorEmail:  actor.Email,
			Action:      "audit_export.sink.delete",
			TargetType:  "audit_export_sink",
			TargetID:    id,
			Details:     map[string]any{"name": cur.Name, "kind": cur.Kind, "url": RedactURL(cur.URL)},
		})
	})
}

// ── reads ──────────────────────────────────────────────────────────────────

// List returns a workspace's sinks with their status.
func (s *Store) List(ctx context.Context, workspaceID string) ([]Sink, error) {
	return s.list(ctx, workspaceID, "")
}

// Get returns one sink.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (Sink, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Sink{}, ErrNotFound
	}
	sinks, err := s.list(ctx, workspaceID, id)
	if err != nil {
		return Sink{}, err
	}
	if len(sinks) == 0 {
		return Sink{}, ErrNotFound
	}
	return sinks[0], nil
}

func (s *Store) list(ctx context.Context, workspaceID, id string) ([]Sink, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, workspace_id::text, name, kind, url, streams, enabled, config,
		        auth_header_name, created_at, updated_at
		   FROM audit_export_sinks
		  WHERE workspace_id = $1 AND ($2 = '' OR id::text = $2)
		  ORDER BY created_at, id`,
		workspaceID, id)
	if err != nil {
		return nil, fmt.Errorf("list audit export sinks: %w", err)
	}
	sinks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sink, error) {
		var k Sink
		err := r.Scan(&k.ID, &k.WorkspaceID, &k.Name, &k.Kind, &k.URL, &k.Streams, &k.Enabled, &k.Config,
			&k.AuthHeaderName, &k.CreatedAt, &k.UpdatedAt)
		return k, err
	})
	if err != nil {
		return nil, fmt.Errorf("list audit export sinks: %w", err)
	}
	if len(sinks) == 0 {
		return sinks, nil
	}
	ids := make([]string, len(sinks))
	byID := map[string]*Sink{}
	for i := range sinks {
		ids[i] = sinks[i].ID
		byID[sinks[i].ID] = &sinks[i]
	}
	srows, err := s.pool.Query(ctx,
		`SELECT c.sink_id::text, c.stream, c.last_seq, h.last_seq, c.last_success_at, c.last_error, c.failing_since,
		        CASE c.stream
		          WHEN 'admin'  THEN (SELECT a.created_at FROM audit_logs a
		                               WHERE a.tenant_id = s.workspace_id AND a.seq > c.last_seq ORDER BY a.seq LIMIT 1)
		          ELSE               (SELECT l.created_at FROM connector_logs l
		                               WHERE l.workspace_id = s.workspace_id AND l.seq > c.last_seq ORDER BY l.seq LIMIT 1)
		        END
		   FROM audit_export_cursors c
		   JOIN audit_export_sinks s ON s.id = c.sink_id
		   JOIN audit_stream_heads h ON h.workspace_id = s.workspace_id AND h.stream = c.stream
		  WHERE c.sink_id::text = ANY($1)
		  ORDER BY c.stream`,
		ids)
	if err != nil {
		return nil, fmt.Errorf("audit export status: %w", err)
	}
	defer srows.Close()
	now := s.now()
	for srows.Next() {
		var (
			sinkID string
			st     StreamStatus
		)
		if err := srows.Scan(&sinkID, &st.Stream, &st.CursorSeq, &st.HeadSeq, &st.LastSuccessAt, &st.LastError,
			&st.FailingSince, &st.OldestPendingAt); err != nil {
			return nil, fmt.Errorf("audit export status: %w", err)
		}
		st.Lagging = st.OldestPendingAt != nil && now.Sub(*st.OldestPendingAt) >= LagWarning
		if k := byID[sinkID]; k != nil {
			k.Status = append(k.Status, st)
		}
	}
	return sinks, srows.Err()
}

// target is everything the exporter needs to deliver to a sink.
type target struct {
	Destination
	WorkspaceID string
	Kind        string
	Enabled     bool
	Streams     []string
}

// loadTarget reads and decrypts a sink's delivery settings.
func (s *Store) loadTarget(ctx context.Context, sinkID string) (target, error) {
	var (
		t                  target
		keyCT, keyNonce    string
		hName, hCT, hNonce *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id::text, kind, url, streams, enabled, config,
		        signing_key_ct, signing_key_nonce, auth_header_name, auth_header_ct, auth_header_nonce
		   FROM audit_export_sinks WHERE id = $1`, sinkID,
	).Scan(&t.WorkspaceID, &t.Kind, &t.URL, &t.Streams, &t.Enabled, &t.Config,
		&keyCT, &keyNonce, &hName, &hCT, &hNonce)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, err
	}
	t.SinkID = sinkID
	key, err := s.sealer.DecryptSecret(keyCT, keyNonce, sealContext(sinkID))
	if err != nil {
		return t, fmt.Errorf("open signing secret: %w", err)
	}
	t.SigningKey = key
	if hName != nil && hCT != nil && hNonce != nil {
		v, err := s.sealer.DecryptSecret(*hCT, *hNonce, sealContext(sinkID))
		if err != nil {
			return t, fmt.Errorf("open auth header: %w", err)
		}
		t.AuthHeaderName, t.AuthHeaderValue = *hName, string(v)
	}
	return t, nil
}

// sameOrigin reports whether two https URLs share host and port.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && strings.EqualFold(ua.Host, ub.Host)
}
