package providerquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// RelayStatuses is the relays.status vocabulary (CHECK constraint, 028).
var RelayStatuses = []string{"pending", "active", "inactive", "revoked", "deleted"}

// HeartbeatSource reads the Valkey liveness key relay:heartbeat:last:<id>,
// which every heartbeat refreshes (TTL 330s by default) while the
// relays.last_heartbeat_at column is written at most every 5 minutes.
// *relay.Service satisfies it; the interface keeps this package free of the
// relay package and its net/http import (D-04). ok=false means no signal.
type HeartbeatSource interface {
	LastHeartbeat(ctx context.Context, relayID string) (time.Time, bool, error)
}

// RelaySummary is one row of GET /provider/relays. It is the whole allowed
// response surface: never enrollment_token_jti, observed_ip or observed_port.
type RelaySummary struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Status             string     `json:"status"`
	Version            *string    `json:"version"`
	Hostname           *string    `json:"hostname"`
	PublicAddr         *string    `json:"public_addr"`
	AddressScope       *string    `json:"address_scope"`
	CapacityLabel      string     `json:"capacity_label"`
	ConnectionCount    int32      `json:"connection_count"`
	MaxConnections     int32      `json:"max_connections"`
	CertSerial         *string    `json:"cert_serial"`
	CertNotAfter       *time.Time `json:"cert_not_after"`
	LastHeartbeatAt    *time.Time `json:"last_heartbeat_at"`
	AttachedConnectors int64      `json:"attached_connectors"`
	CreatedAt          time.Time  `json:"created_at"`
}

// RelayCertificate is one row of a relay's certificate history.
type RelayCertificate struct {
	Serial           string     `json:"serial"`
	IssuedAt         time.Time  `json:"issued_at"`
	NotAfter         time.Time  `json:"not_after"`
	RevokedAt        *time.Time `json:"revoked_at"`
	RevocationReason *string    `json:"revocation_reason"`
	// IsCurrent: this serial is relays.cert_serial (there is no is_current
	// column; the relay row names its current certificate).
	IsCurrent bool `json:"is_current"`
}

// RelayAttachment is one connector currently placed on the relay.
type RelayAttachment struct {
	ConnectorID   string    `json:"connector_id"`
	TenantID      string    `json:"tenant_id"`
	AttachedAt    time.Time `json:"attached_at"`
	LastConfirmed time.Time `json:"last_confirmed"`
	Source        string    `json:"source"`
}

// RelayDetail is GET /provider/relays/{id}.
type RelayDetail struct {
	RelaySummary
	DNSAllowlist          []string           `json:"dns_allowlist"`
	IPAllowlist           []string           `json:"ip_allowlist"`
	Certificates          []RelayCertificate `json:"certificates"`
	CertificatesTruncated bool               `json:"certificates_truncated"`
	Attachments           []RelayAttachment  `json:"attachments"`
	AttachmentsTruncated  bool               `json:"attachments_truncated"`
}

// RelayFilter narrows ListRelays. Status "" means every status except
// deleted; "deleted" must be asked for explicitly.
type RelayFilter struct {
	Status string
	Limit  int
	Cursor string
}

// The summary column list, shared by list and detail so both expose the same
// surface. Order matches scanRelaySummary.
const relaySummaryColumns = `
       r.id::text, r.name, r.status, r.version, r.hostname, r.public_addr, r.address_scope,
       r.capacity_label, r.connection_count, r.max_connections, r.cert_serial, r.cert_not_after,
       r.last_heartbeat_at,
       (SELECT count(*) FROM connector_relay_placement p WHERE p.relay_id = r.id),
       r.created_at`

func scanRelaySummary(row pgx.Row, extra ...any) (RelaySummary, error) {
	var s RelaySummary
	dest := append([]any{
		&s.ID, &s.Name, &s.Status, &s.Version, &s.Hostname, &s.PublicAddr, &s.AddressScope,
		&s.CapacityLabel, &s.ConnectionCount, &s.MaxConnections, &s.CertSerial, &s.CertNotAfter,
		&s.LastHeartbeatAt, &s.AttachedConnectors, &s.CreatedAt,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return RelaySummary{}, err
	}
	s.CertNotAfter = utcPtr(s.CertNotAfter)
	s.LastHeartbeatAt = utcPtr(s.LastHeartbeatAt)
	s.CreatedAt = s.CreatedAt.UTC()
	return s, nil
}

// effectiveHeartbeat returns the fresher of the persisted heartbeat and the
// Valkey liveness key. A Valkey error or missing key falls back to the
// database value: a read API shouldn't fail because the cache is down.
func effectiveHeartbeat(ctx context.Context, hb HeartbeatSource, relayID string, db *time.Time) *time.Time {
	if hb == nil {
		return db
	}
	t, ok, err := hb.LastHeartbeat(ctx, relayID)
	if err != nil || !ok {
		return db
	}
	t = t.UTC()
	if db == nil || t.After(*db) {
		return &t
	}
	return db
}

func validRelayStatus(s string) bool {
	for _, v := range RelayStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// ListRelays returns one page of relays, newest first. The attached-connector
// count is a correlated subquery in the same statement (one query per page);
// liveness takes one Valkey read per row, bounded by the page size.
func ListRelays(ctx context.Context, q Querier, hb HeartbeatSource, f RelayFilter) (Page[RelaySummary], error) {
	if f.Status != "" && !validRelayStatus(f.Status) {
		return Page[RelaySummary]{}, fmt.Errorf("%w: status %q", ErrInvalidFilter, f.Status)
	}
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return Page[RelaySummary]{}, err
	}
	limit := ClampLimit(f.Limit)
	var status any
	if f.Status != "" {
		status = f.Status
	}
	ct, cid := keysetArgs(cur)

	rows, err := q.Query(ctx, `
SELECT`+relaySummaryColumns+`
  FROM relays r
 WHERE (($1::text IS NULL AND r.status <> 'deleted') OR r.status = $1)
   AND ($2::timestamptz IS NULL OR (r.created_at, r.id) < ($2, $3::uuid))
 ORDER BY r.created_at DESC, r.id DESC
 LIMIT $4`, status, ct, cid, limit+1)
	if err != nil {
		return Page[RelaySummary]{}, fmt.Errorf("list relays: %w", err)
	}
	defer rows.Close()

	items := make([]RelaySummary, 0, limit)
	for rows.Next() {
		s, err := scanRelaySummary(rows)
		if err != nil {
			return Page[RelaySummary]{}, fmt.Errorf("scan relay: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return Page[RelaySummary]{}, fmt.Errorf("list relays: %w", err)
	}

	page := Page[RelaySummary]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := EncodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &next
	}
	for i := range page.Items {
		page.Items[i].LastHeartbeatAt = effectiveHeartbeat(ctx, hb, page.Items[i].ID, page.Items[i].LastHeartbeatAt)
	}
	return page, nil
}

// GetRelay returns one relay with its allowlists, certificate history (newest
// first) and current connector attachments. Deleted relays are still
// readable by id. Each nested list is capped at DetailCap and flagged when cut.
func GetRelay(ctx context.Context, q Querier, hb HeartbeatSource, id string) (RelayDetail, error) {
	if !IsUUID(id) {
		return RelayDetail{}, ErrInvalidID
	}
	var d RelayDetail
	s, err := scanRelaySummary(q.QueryRow(ctx, `
SELECT`+relaySummaryColumns+`, r.dns_allowlist, r.ip_allowlist
  FROM relays r
 WHERE r.id = $1`, id), &d.DNSAllowlist, &d.IPAllowlist)
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayDetail{}, ErrNotFound
	}
	if err != nil {
		return RelayDetail{}, fmt.Errorf("get relay: %w", err)
	}
	s.LastHeartbeatAt = effectiveHeartbeat(ctx, hb, s.ID, s.LastHeartbeatAt)
	d.RelaySummary = s

	if d.Certificates, d.CertificatesTruncated, err = relayCertificates(ctx, q, id); err != nil {
		return RelayDetail{}, err
	}
	if d.Attachments, d.AttachmentsTruncated, err = relayAttachments(ctx, q, id); err != nil {
		return RelayDetail{}, err
	}
	return d, nil
}

func relayCertificates(ctx context.Context, q Querier, relayID string) ([]RelayCertificate, bool, error) {
	rows, err := q.Query(ctx, `
SELECT c.serial, c.issued_at, c.not_after, c.revoked_at, c.revocation_reason,
       (r.cert_serial IS NOT NULL AND c.serial = r.cert_serial)
  FROM relay_certificates c
  JOIN relays r ON r.id = c.relay_id
 WHERE c.relay_id = $1
 ORDER BY c.issued_at DESC, c.id DESC
 LIMIT $2`, relayID, DetailCap+1)
	if err != nil {
		return nil, false, fmt.Errorf("relay certificates: %w", err)
	}
	defer rows.Close()
	out := make([]RelayCertificate, 0)
	for rows.Next() {
		var c RelayCertificate
		if err := rows.Scan(&c.Serial, &c.IssuedAt, &c.NotAfter, &c.RevokedAt, &c.RevocationReason, &c.IsCurrent); err != nil {
			return nil, false, fmt.Errorf("scan relay certificate: %w", err)
		}
		c.IssuedAt, c.NotAfter, c.RevokedAt = c.IssuedAt.UTC(), c.NotAfter.UTC(), utcPtr(c.RevokedAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("relay certificates: %w", err)
	}
	if len(out) > DetailCap {
		return out[:DetailCap], true, nil
	}
	return out, false, nil
}

func relayAttachments(ctx context.Context, q Querier, relayID string) ([]RelayAttachment, bool, error) {
	// connector_relay_placement has no tenant column; the connector row has it.
	rows, err := q.Query(ctx, `
SELECT p.connector_id::text, c.tenant_id::text, p.attached_at, p.last_confirmed, p.source
  FROM connector_relay_placement p
  JOIN connectors c ON c.id = p.connector_id
 WHERE p.relay_id = $1
 ORDER BY p.attached_at DESC, p.connector_id DESC
 LIMIT $2`, relayID, DetailCap+1)
	if err != nil {
		return nil, false, fmt.Errorf("relay attachments: %w", err)
	}
	defer rows.Close()
	out := make([]RelayAttachment, 0)
	for rows.Next() {
		var a RelayAttachment
		if err := rows.Scan(&a.ConnectorID, &a.TenantID, &a.AttachedAt, &a.LastConfirmed, &a.Source); err != nil {
			return nil, false, fmt.Errorf("scan relay attachment: %w", err)
		}
		a.AttachedAt, a.LastConfirmed = a.AttachedAt.UTC(), a.LastConfirmed.UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("relay attachments: %w", err)
	}
	if len(out) > DetailCap {
		return out[:DetailCap], true, nil
	}
	return out, false, nil
}
