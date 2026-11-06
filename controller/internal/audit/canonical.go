package audit

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// The canonical encoding (PENDING-11 P-2), computed in Go from a row's raw
// column values. It must equal the database's audit_admin_canonical /
// audit_access_canonical byte for byte (TestCanonical_GoMatchesDatabase), so
// the verifier and the exporter don't trust the database's own hashing
// function, and a SIEM can recompute the hash from the exported fields.
//
// Each field is "<byte length>:<utf8 bytes>"; NULL is "-". Timestamps are UTC
// epoch microseconds. details is the exact jsonb text Postgres returns.

// CanonicalVersion is the stream encoding version (the first field).
const CanonicalVersion = "v1"

type canonicalBuf []byte

func (b canonicalBuf) str(v string) canonicalBuf {
	b = strconv.AppendInt(b, int64(len(v)), 10)
	b = append(b, ':')
	return append(b, v...)
}

func (b canonicalBuf) opt(v *string) canonicalBuf {
	if v == nil {
		return append(b, '-')
	}
	return b.str(*v)
}

func (b canonicalBuf) int(v int64) canonicalBuf { return b.str(strconv.FormatInt(v, 10)) }

func (b canonicalBuf) ts(t time.Time) canonicalBuf { return b.int(t.UnixMicro()) }

func (b canonicalBuf) optTS(t *time.Time) canonicalBuf {
	if t == nil {
		return append(b, '-')
	}
	return b.ts(*t)
}

// AdminRow is one audit_logs row (the admin stream), with uuids as text and
// details as the jsonb text.
type AdminRow struct {
	Seq         int64
	ID          string
	WorkspaceID string
	ActorUserID *string
	ActorEmail  string
	Action      string
	TargetType  string
	TargetID    string
	Details     *string // jsonb text, nil = NULL
	CreatedAt   time.Time
	PrevHash    []byte
	RowHash     []byte
}

// Canonical returns the row's hashed bytes.
func (r AdminRow) Canonical() []byte {
	var b canonicalBuf
	b = b.str(CanonicalVersion).str(string(StreamAdmin)).int(r.Seq).str(r.ID).str(r.WorkspaceID).
		opt(r.ActorUserID).str(r.ActorEmail).str(r.Action).str(r.TargetType).str(r.TargetID).
		opt(r.Details).ts(r.CreatedAt)
	return b
}

// AccessRow is one connector_logs row (the access stream).
type AccessRow struct {
	Seq            int64
	ID             string
	WorkspaceID    string
	ConnectorID    string
	Message        string
	ResourceID     *string
	ClientSpiffeID *string
	ClientDeviceID *string
	UserID         *string
	RouteType      *string
	Destination    *string
	Port           *int32
	Protocol       *string
	Action         *string
	Error          *string
	OccurredAt     *time.Time
	CreatedAt      time.Time
	PrevHash       []byte
	RowHash        []byte
}

// Canonical returns the row's hashed bytes.
func (r AccessRow) Canonical() []byte {
	var port *string
	if r.Port != nil {
		p := strconv.FormatInt(int64(*r.Port), 10)
		port = &p
	}
	var b canonicalBuf
	b = b.str(CanonicalVersion).str(string(StreamAccess)).int(r.Seq).str(r.ID).str(r.WorkspaceID).
		str(r.ConnectorID).str(r.Message).opt(r.ResourceID).opt(r.ClientSpiffeID).opt(r.ClientDeviceID).
		opt(r.UserID).opt(r.RouteType).opt(r.Destination).opt(port).opt(r.Protocol).opt(r.Action).
		opt(r.Error).optTS(r.OccurredAt).ts(r.CreatedAt)
	return b
}

const adminRowCols = `seq, id::text, tenant_id::text, actor_user_id::text, actor_email, action,
	target_type, target_id, details::text, created_at, prev_hash, row_hash`

const accessRowCols = `seq, id::text, workspace_id::text, connector_id, message, resource_id::text,
	client_spiffe_id, client_device_id::text, user_id::text, route_type, destination, port,
	protocol, action, error, occurred_at, created_at, prev_hash, row_hash`

// ReadAdminRows returns up to limit admin-stream rows with
// afterSeq < seq <= throughSeq, in seq order.
func ReadAdminRows(ctx context.Context, db Querier, workspaceID string, afterSeq, throughSeq int64, limit int) ([]AdminRow, error) {
	rows, err := db.Query(ctx,
		`SELECT `+adminRowCols+` FROM audit_logs
		  WHERE tenant_id = $1 AND seq > $2 AND seq <= $3 ORDER BY seq LIMIT $4`,
		workspaceID, afterSeq, throughSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: read admin rows: %w", err)
	}
	defer rows.Close()
	var out []AdminRow
	for rows.Next() {
		var r AdminRow
		if err := rows.Scan(&r.Seq, &r.ID, &r.WorkspaceID, &r.ActorUserID, &r.ActorEmail, &r.Action,
			&r.TargetType, &r.TargetID, &r.Details, &r.CreatedAt, &r.PrevHash, &r.RowHash); err != nil {
			return nil, fmt.Errorf("audit: scan admin row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReadAccessRows returns up to limit access-stream rows with
// afterSeq < seq <= throughSeq, in seq order.
func ReadAccessRows(ctx context.Context, db Querier, workspaceID string, afterSeq, throughSeq int64, limit int) ([]AccessRow, error) {
	rows, err := db.Query(ctx,
		`SELECT `+accessRowCols+` FROM connector_logs
		  WHERE workspace_id = $1 AND seq > $2 AND seq <= $3 ORDER BY seq LIMIT $4`,
		workspaceID, afterSeq, throughSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("audit: read access rows: %w", err)
	}
	defer rows.Close()
	var out []AccessRow
	for rows.Next() {
		var r AccessRow
		if err := rows.Scan(&r.Seq, &r.ID, &r.WorkspaceID, &r.ConnectorID, &r.Message, &r.ResourceID,
			&r.ClientSpiffeID, &r.ClientDeviceID, &r.UserID, &r.RouteType, &r.Destination, &r.Port,
			&r.Protocol, &r.Action, &r.Error, &r.OccurredAt, &r.CreatedAt, &r.PrevHash, &r.RowHash); err != nil {
			return nil, fmt.Errorf("audit: scan access row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Head is a stream's head row.
type Head struct {
	LastSeq           int64
	LastHash          []byte
	PurgedThroughSeq  int64
	PurgedThroughHash []byte // nil = nothing purged (chain starts at Genesis)
}

// ReadHead returns a workspace stream's head.
func ReadHead(ctx context.Context, db Querier, workspaceID string, s Stream) (Head, error) {
	var h Head
	err := db.QueryRow(ctx,
		`SELECT last_seq, last_hash, purged_through_seq, purged_through_hash
		   FROM audit_stream_heads WHERE workspace_id = $1 AND stream = $2`,
		workspaceID, string(s),
	).Scan(&h.LastSeq, &h.LastHash, &h.PurgedThroughSeq, &h.PurgedThroughHash)
	return h, err
}
