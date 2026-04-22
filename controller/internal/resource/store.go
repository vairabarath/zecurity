package resource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Row holds a resource record with joined shield + network names.
type Row struct {
	ID              string
	Name            string
	Description     *string
	Host            string
	Protocol        string
	PortFrom        int
	PortTo          int
	Status          string
	ErrorMessage    *string
	AppliedAt       *time.Time
	LastVerifiedAt  *time.Time
	CreatedAt       time.Time
	ShieldID        string
	ShieldName      *string
	ShieldStatus    *string
	RemoteNetworkID string
	NetworkName     string
}

// PendingRow is a minimal resource record used in heartbeat delivery.
type PendingRow struct {
	ID       string
	Host     string
	Protocol string
	PortFrom int
	PortTo   int
	Status   string // "managing" or "removing"
}

// CreateInput holds fields provided by the admin when creating a resource.
type CreateInput struct {
	Name        string
	Description *string
	Host        string
	Protocol    string
	PortFrom    int
	PortTo      int
}

const resourceSelectCols = `
	r.id, r.name, r.description, r.host, r.protocol, r.port_from, r.port_to,
	r.status, r.error_message, r.applied_at, r.last_verified_at, r.created_at,
	r.shield_id, r.remote_network_id,
	s.name, s.status,
	rn.name`

const resourceJoins = `
	FROM resources r
	LEFT JOIN shields s ON s.id = r.shield_id
	JOIN  remote_networks rn ON rn.id = r.remote_network_id`

func scanRow(s interface{ Scan(...any) error }) (*Row, error) {
	var row Row
	if err := s.Scan(
		&row.ID, &row.Name, &row.Description, &row.Host, &row.Protocol,
		&row.PortFrom, &row.PortTo, &row.Status, &row.ErrorMessage,
		&row.AppliedAt, &row.LastVerifiedAt, &row.CreatedAt,
		&row.ShieldID, &row.RemoteNetworkID,
		&row.ShieldName, &row.ShieldStatus,
		&row.NetworkName,
	); err != nil {
		return nil, err
	}
	return &row, nil
}

// AutoMatchShield finds a shield whose lan_ip matches the given host.
func AutoMatchShield(ctx context.Context, db *pgxpool.Pool, host, tenantID string) (shieldID, remoteNetworkID string, err error) {
	err = db.QueryRow(ctx,
		`SELECT id, remote_network_id
		   FROM shields
		  WHERE lan_ip = $1
		    AND tenant_id = $2
		    AND status NOT IN ('revoked', 'deleted')
		  LIMIT 1`,
		host, tenantID,
	).Scan(&shieldID, &remoteNetworkID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("no shield installed on host %s", host)
		}
		return "", "", fmt.Errorf("auto-match shield: %w", err)
	}
	return shieldID, remoteNetworkID, nil
}

// Create inserts a new resource, auto-matching the shield by host IP.
func Create(ctx context.Context, db *pgxpool.Pool, tenantID string, input CreateInput) (*Row, error) {
	shieldID, remoteNetworkID, err := AutoMatchShield(ctx, db, input.Host, tenantID)
	if err != nil {
		return nil, err
	}

	var id string
	err = db.QueryRow(ctx,
		`INSERT INTO resources
		    (tenant_id, remote_network_id, shield_id, name, description, protocol, host, port_from, port_to)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		tenantID, remoteNetworkID, shieldID,
		input.Name, input.Description, input.Protocol, input.Host, input.PortFrom, input.PortTo,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	return GetByID(ctx, db, tenantID, id)
}

// GetByID fetches a single resource by id + tenant.
func GetByID(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	row, err := scanRow(db.QueryRow(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.id = $1 AND r.tenant_id = $2 AND r.deleted_at IS NULL`,
		id, tenantID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resource not found")
		}
		return nil, fmt.Errorf("get resource: %w", err)
	}
	return row, nil
}

// GetByRemoteNetwork returns all non-deleted resources for a remote network.
func GetByRemoteNetwork(ctx context.Context, db *pgxpool.Pool, tenantID, remoteNetworkID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.remote_network_id = $1 AND r.tenant_id = $2 AND r.deleted_at IS NULL
		  ORDER BY r.created_at DESC`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetAll returns all non-deleted resources for a tenant.
func GetAll(ctx context.Context, db *pgxpool.Pool, tenantID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.tenant_id = $1 AND r.deleted_at IS NULL
		  ORDER BY r.created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list all resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetPendingForShield returns resources in managing/removing state for a shield.
// Called from the heartbeat handler — no tenant context needed (shield_id is tenant-scoped).
func GetPendingForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]*PendingRow, error) {
	rows, err := db.Query(ctx,
		`SELECT id, host, protocol, port_from, port_to, status
		   FROM resources
		  WHERE shield_id = $1
		    AND status IN ('managing', 'removing')
		    AND deleted_at IS NULL`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending resources: %w", err)
	}
	defer rows.Close()

	var result []*PendingRow
	for rows.Next() {
		var r PendingRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Protocol, &r.PortFrom, &r.PortTo, &r.Status); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

// UpdateInput holds the fields that can be changed on an existing resource.
// Only non-nil fields are written to the database.
type UpdateInput struct {
	RemoteNetworkID *string
	Name            *string
	Description     *string
	Protocol        *string
	PortFrom        *int
	PortTo          *int
}

// Update modifies editable fields on a resource. Only non-nil fields are applied.
func Update(ctx context.Context, db *pgxpool.Pool, tenantID, id string, input UpdateInput) (*Row, error) {
	args := []any{id, tenantID}
	sets := []string{"updated_at = NOW()"}

	add := func(col string, val any) {
		args = append(args, val)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}

	if input.RemoteNetworkID != nil {
		add("remote_network_id", *input.RemoteNetworkID)
	}
	if input.Name != nil {
		add("name", *input.Name)
	}
	if input.Description != nil {
		add("description", *input.Description)
	}
	if input.Protocol != nil {
		add("protocol", *input.Protocol)
	}
	if input.PortFrom != nil {
		add("port_from", *input.PortFrom)
	}
	if input.PortTo != nil {
		add("port_to", *input.PortTo)
	}

	query := fmt.Sprintf(
		`UPDATE resources SET %s
		  WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL
		  RETURNING id`,
		joinSets(sets),
	)

	var discardedID string
	err := db.QueryRow(ctx, query, args...).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resource not found")
		}
		return nil, fmt.Errorf("update resource: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

func joinSets(sets []string) string {
	var b strings.Builder
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s)
	}
	return b.String()
}

// MarkManaging transitions a resource to managing status (Shield will apply nftables).
func MarkManaging(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	return updateStatus(ctx, db, tenantID, id, "managing", nil,
		[]string{"pending", "failed"}, "protect resource")
}

// MarkRemoving transitions a resource to removing status (Shield will remove nftables rule).
func MarkRemoving(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	return updateStatus(ctx, db, tenantID, id, "removing", nil,
		[]string{"protected"}, "unprotect resource")
}

// SoftDelete marks a resource as deleted.
func SoftDelete(ctx context.Context, db *pgxpool.Pool, tenantID, id string) error {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
		    SET deleted_at = NOW(), updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2
		    AND status NOT IN ('managing', 'protecting', 'removing')
		 RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("resource not found or must be unprotected before deleting")
		}
		return fmt.Errorf("delete resource: %w", err)
	}
	return nil
}

// RecordAck processes a ResourceAck from Shield and updates the resource status.
func RecordAck(ctx context.Context, db *pgxpool.Pool, resourceID, status, errMsg string, verifiedAt int64, portReachable bool) error {
	_, err := db.Exec(ctx,
		`UPDATE resources
		    SET status          = $2,
		        error_message   = NULLIF($3, ''),
		        last_verified_at = to_timestamp($4),
		        applied_at      = CASE WHEN $2 = 'protected' AND applied_at IS NULL THEN NOW() ELSE applied_at END,
		        updated_at      = NOW()
		  WHERE id = $1 AND deleted_at IS NULL`,
		resourceID, status, errMsg, verifiedAt,
	)
	if err != nil {
		return fmt.Errorf("record ack: %w", err)
	}
	return nil
}

// updateStatus is the shared helper for MarkManaging / MarkRemoving.
func updateStatus(ctx context.Context, db *pgxpool.Pool, tenantID, id, newStatus string, errMsg *string, allowedFrom []string, op string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
		    SET status = $3, error_message = $4, updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2
		    AND status = ANY($5)
		 RETURNING id`,
		id, tenantID, newStatus, errMsg, allowedFrom,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%s: resource not found or invalid status transition", op)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return GetByID(ctx, db, tenantID, id)
}

func collectRows(rows pgx.Rows) ([]*Row, error) {
	var result []*Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		result = []*Row{}
	}
	return result, nil
}
