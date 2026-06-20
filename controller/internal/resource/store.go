package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNoMatchingShield = errors.New("no active shield installed on resource host")

// querier is the read surface shared by *pgxpool.Pool and pgx.Tx, so the
// single-source desired-state query can run either standalone or inside the
// snapshot transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

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
	PendingAction   string // "apply" or "remove" — meaningful only when status="protecting"
	ErrorMessage    *string
	AppliedAt       *time.Time
	LastVerifiedAt  *time.Time
	CreatedAt       time.Time
	ShieldID        string
	ConnectorID     string // connector that manages this shield — needed for stream push
	ShieldName      *string
	ShieldStatus    *string
	RemoteNetworkID string
	NetworkName     string
}

// PendingRow is a minimal resource record used in stream instruction delivery.
type PendingRow struct {
	ID            string
	Host          string
	Protocol      string
	PortFrom      int
	PortTo        int
	PendingAction string // "apply" or "remove"
}

// CreateInput holds fields provided by the admin when creating a resource.
type CreateInput struct {
	RemoteNetworkID string
	Name            string
	Description     *string
	Host            string
	Protocol        string
	PortFrom        int
	PortTo          int
}

const resourceSelectCols = `
	r.id, r.name, r.description, r.host, r.protocol, r.port_from, r.port_to,
	r.status, r.pending_action, r.error_message, r.applied_at, r.last_verified_at, r.created_at,
	COALESCE(r.shield_id::text, ''), r.remote_network_id,
	s.name, s.status, COALESCE(s.connector_id::text, ''),
	rn.name`

const resourceJoins = `
	FROM resources r
	LEFT JOIN shields s ON s.id = r.shield_id
	JOIN  remote_networks rn ON rn.id = r.remote_network_id`

func scanRow(s interface{ Scan(...any) error }) (*Row, error) {
	var row Row
	if err := s.Scan(
		&row.ID, &row.Name, &row.Description, &row.Host, &row.Protocol,
		&row.PortFrom, &row.PortTo, &row.Status, &row.PendingAction, &row.ErrorMessage,
		&row.AppliedAt, &row.LastVerifiedAt, &row.CreatedAt,
		&row.ShieldID, &row.RemoteNetworkID,
		&row.ShieldName, &row.ShieldStatus, &row.ConnectorID,
		&row.NetworkName,
	); err != nil {
		return nil, err
	}
	return &row, nil
}

// AutoMatchShield finds an active shield whose LAN IP matches the resource host
// within its remote network. The same private IP may exist in multiple networks.
func AutoMatchShield(ctx context.Context, db *pgxpool.Pool, host, tenantID, remoteNetworkID string) (shieldID string, err error) {
	err = db.QueryRow(ctx,
		`SELECT id
		   FROM shields
		  WHERE lan_ip = $1
		    AND tenant_id = $2
		    AND remote_network_id = $3
		    AND status = 'active'
		  LIMIT 1`,
		host, tenantID, remoteNetworkID,
	).Scan(&shieldID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: %s", ErrNoMatchingShield, host)
		}
		return "", fmt.Errorf("auto-match shield: %w", err)
	}
	return shieldID, nil
}

// Create inserts an unprotected resource. A matching active Shield is retained
// as a convenience for a later Protect operation, but is not required.
func Create(ctx context.Context, db *pgxpool.Pool, tenantID string, input CreateInput) (*Row, error) {
	shieldID, err := AutoMatchShield(ctx, db, input.Host, tenantID, input.RemoteNetworkID)
	if err != nil && !errors.Is(err, ErrNoMatchingShield) {
		return nil, err
	}
	var shieldArg any
	if shieldID != "" {
		shieldArg = shieldID
	}

	var id string
	err = db.QueryRow(ctx,
		`INSERT INTO resources
		    (tenant_id, remote_network_id, shield_id, name, description, protocol, host,
		     port_from, port_to, status, pending_action)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'unprotected', 'remove')
		 RETURNING id`,
		tenantID, input.RemoteNetworkID, shieldArg,
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
		  WHERE r.id = $1 AND r.tenant_id = $2`,
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

// GetByRemoteNetwork returns all resources for a remote network.
func GetByRemoteNetwork(ctx context.Context, db *pgxpool.Pool, tenantID, remoteNetworkID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.remote_network_id = $1 AND r.tenant_id = $2
		  ORDER BY r.created_at DESC`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetAll returns all resources for a tenant.
func GetAll(ctx context.Context, db *pgxpool.Pool, tenantID string) ([]*Row, error) {
	rows, err := db.Query(ctx,
		`SELECT `+resourceSelectCols+resourceJoins+`
		  WHERE r.tenant_id = $1
		  ORDER BY r.created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list all resources: %w", err)
	}
	defer rows.Close()
	return collectRows(rows)
}

// GetPendingForShield returns resources in the protecting and deleting state for a shield.
// Called on stream connect to deliver any queued instructions to a reconnecting connector.
func GetPendingForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]*PendingRow, error) {
	rows, err := db.Query(ctx,
		`SELECT id, host, protocol, port_from, port_to, pending_action
		   FROM resources
		  WHERE shield_id = $1
		    AND status IN ('protecting', 'deleting')`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get pending resources: %w", err)
	}
	defer rows.Close()

	var result []*PendingRow
	for rows.Next() {
		var r PendingRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Protocol, &r.PortFrom, &r.PortTo, &r.PendingAction); err != nil {
			return nil, err
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

// desiredForShield is the SINGLE definition of a shield's desired set — the
// complete set of resources that should be enforced on it right now. Everything
// that needs "what should this shield be enforcing" (snapshot build, reconciler
// drift detection) routes through here, so the predicate cannot drift between
// callers or into SQL elsewhere.
//
// Includes (fail-closed):
//   - protected
//   - failed — the admin's intent is "protected"; a failed resource (e.g. the
//     'port not listening' case, where the shield HAS applied the drop rule) must
//     keep its rule so a temporarily-down service stays protected and is enforced
//     the instant it returns. Re-applying a host-mismatch/nftables-error 'failed'
//     is a harmless no-op on the shield.
//   - protecting + apply (in-flight protect)
//
// 'deleting', 'unprotected', 'pending', and 'protecting/remove' are intentionally
// absent — the shield's replace-semantics drops anything not listed here.
func desiredForShield(ctx context.Context, q querier, shieldID string) ([]*PendingRow, error) {
	rows, err := q.Query(ctx,
		`SELECT id, host, protocol, port_from, port_to, pending_action
		   FROM resources
		  WHERE shield_id = $1
		    AND (status IN ('protected', 'failed')
		         OR (status = 'protecting' AND pending_action = 'apply'))`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get desired resources: %w", err)
	}
	defer rows.Close()

	var result []*PendingRow
	for rows.Next() {
		var r PendingRow
		if err := rows.Scan(&r.ID, &r.Host, &r.Protocol, &r.PortFrom, &r.PortTo, &r.PendingAction); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		result = append(result, &r)
	}
	return result, rows.Err()
}

// GetDesiredForShield returns the shield's desired set for read-only callers
// (the reconciler's drift detection). Snapshot delivery uses BuildShieldSnapshot
// instead, which pairs the same set with a generation.
func GetDesiredForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]*PendingRow, error) {
	return desiredForShield(ctx, db, shieldID)
}

// SnapshotResult is a shield's desired set paired with the monotonic generation
// that stamps it.
type SnapshotResult struct {
	Generation uint64
	Resources  []*PendingRow
}

// BuildShieldSnapshot returns the shield's desired set with a generation suitable
// for the shield's `generation <= last` staleness gate (ADR-004 Phase 2 / F11).
//
// Generation is opaque bookkeeping, never a desired-state rule: we hash the exact
// rows desiredForShield returns and bump the stored generation only when that
// fingerprint changes. Everything runs in one transaction with a row lock on the
// shield, so concurrent builders serialize and the (generation, fingerprint,
// content) triple stays consistent — a later-committed desired state always yields
// a higher generation, which is what lets the shield resolve out-of-order
// deliveries. Identical content reuses the generation, so the shield dedups it and
// metadata/audit writes never churn the value.
func BuildShieldSnapshot(ctx context.Context, db *pgxpool.Pool, shieldID string) (*SnapshotResult, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		gen      int64
		storedFP string
	)
	// Lock the shield row: without it two concurrent builders could read the same
	// generation and assign it to different content.
	if err := tx.QueryRow(ctx,
		`SELECT snapshot_generation, snapshot_fingerprint FROM shields WHERE id = $1 FOR UPDATE`,
		shieldID,
	).Scan(&gen, &storedFP); err != nil {
		return nil, fmt.Errorf("lock shield for snapshot: %w", err)
	}

	desired, err := desiredForShield(ctx, tx, shieldID)
	if err != nil {
		return nil, err
	}

	if fp := fingerprintDesired(desired); fp != storedFP {
		gen++
		if _, err := tx.Exec(ctx,
			`UPDATE shields SET snapshot_generation = $2, snapshot_fingerprint = $3 WHERE id = $1`,
			shieldID, gen, fp,
		); err != nil {
			return nil, fmt.Errorf("bump snapshot generation: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit snapshot tx: %w", err)
	}
	return &SnapshotResult{Generation: uint64(gen), Resources: desired}, nil
}

// fingerprintDesired is a stable content hash of a desired set. Sorted by resource
// ID so a reordered query result is not mistaken for a change.
//
// It hashes EXACTLY the fields the snapshot's ResourceInstruction carries on the
// wire — id, host, protocol, ports. pending_action is deliberately excluded: a
// snapshot has no per-row action (every listed row means "enforce this"), so
// hashing it would couple the generation to controller bookkeeping and churn the
// version without any change the shield can observe. A row entering or leaving the
// desired set is already reflected by its id appearing/disappearing here.
func fingerprintDesired(rows []*PendingRow) string {
	sorted := make([]*PendingRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	h := sha256.New()
	for _, r := range sorted {
		fmt.Fprintf(h, "%s|%s|%s|%d|%d\n", r.ID, r.Host, r.Protocol, r.PortFrom, r.PortTo)
	}
	return hex.EncodeToString(h.Sum(nil))
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
		  WHERE id = $1 AND tenant_id = $2
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

// MarkProtecting atomically matches an active Shield and transitions the
// resource to protected intent. ACL routing becomes Shield-based immediately.
func MarkProtecting(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`WITH candidate AS (
		     SELECT s.id
		       FROM resources r
		       JOIN shields s
		         ON s.tenant_id = r.tenant_id
		        AND s.remote_network_id = r.remote_network_id
		        AND s.lan_ip = r.host
		        AND s.status = 'active'
		      WHERE r.id = $1 AND r.tenant_id = $2
		        AND r.status = ANY($3)
		      ORDER BY CASE WHEN s.id = r.shield_id THEN 0 ELSE 1 END
		      LIMIT 1
		 )
		 UPDATE resources r
		    SET shield_id = candidate.id,
		        status = 'protecting', pending_action = 'apply',
		        error_message = NULL, updated_at = NOW()
		   FROM candidate
		  WHERE r.id = $1 AND r.tenant_id = $2
		 RETURNING r.id`,
		id, tenantID, []string{"pending", "failed", "unprotected"},
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("protect resource: resource not found, invalid state, or no active shield is installed on its host")
		}
		return nil, fmt.Errorf("protect resource: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// MarkUnprotecting removes protection from a resource.
//
// Normally it transitions to 'protecting'/pending_action='remove' and waits for
// the shield to ack the nftables removal. But if the bound shield is REVOKED or
// gone (no shields row), there is no shield left to apply the removal or ack it,
// and the firewall rule died with the shield — so parking in 'protecting' would
// hang forever waiting for an ack that can never arrive (Finding 7). In that case
// we resolve straight to the terminal 'unprotected' state in the same atomic
// UPDATE (deciding off the shield status, so there is no TOCTOU). A merely
// disconnected/offline-but-not-revoked shield still takes the normal path: it
// picks up the pending remove and acks it on reconnect.
//
// pending_action stays 'remove' in both branches — it is only meaningful while
// status='protecting', and the normal terminal path (RecordAck) also leaves it at
// 'remove' when it flips to 'unprotected', so the resolve-through end-state is
// identical: ('unprotected', 'remove').
func MarkUnprotecting(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
		    SET status = CASE
		            WHEN COALESCE(
		                     (SELECT s.status FROM shields s WHERE s.id = resources.shield_id),
		                     'revoked'
		                 ) = 'revoked'
		            THEN 'unprotected'   -- shield revoked or gone: nothing to ack (Finding 7)
		            ELSE 'protecting'    -- live/recoverable shield: normal remove + ack path
		        END,
		        pending_action = 'remove',
		        updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2
		    AND status = 'protected'
		 RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("unprotect resource: resource not found or not in protected state")
		}
		return nil, fmt.Errorf("unprotect resource: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// MarkDeleting transitions a resource to the 'deleting' tombstone with
// pending_action='remove'. The row is NOT removed here — it is reaped by
// RecordAck once the shield confirms the nftables rule is gone. Only states
// that may hold a rule (protected, failed) take this path.
func MarkDeleting(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (*Row, error) {
	var discardedID string
	err := db.QueryRow(ctx,
		`UPDATE resources
                  SET status = 'deleting', pending_action = 'remove', updated_at = NOW()
                WHERE id = $1 AND tenant_id = $2
                  AND status IN ('protected', 'failed')
               RETURNING id`,
		id, tenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("delete resource: resource not found or in invalid state")
		}
		return nil, fmt.Errorf("mark deleting: %w", err)
	}
	return GetByID(ctx, db, tenantID, id)
}

// DeleteRow hard-deletes a resource that cannot have a shield rule (pending or
// unprotected). Protected/failed resources must go through MarkDeleting first.
func DeleteRow(ctx context.Context, db *pgxpool.Pool, tenantID, id string) error {
	var discardedID string
	err := db.QueryRow(ctx,
		`DELETE FROM resources
                WHERE id = $1 AND tenant_id = $2
                  AND status IN ('pending', 'unprotected')
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

// ForceDeleteRow hard-deletes a resource in ANY state, tenant-scoped. This is the
// break-glass escape hatch (ADR-004 Phase 4): it bypasses the confirmation-gated
// tombstone path (MarkDeleting → ack-driven reap) for a resource permanently stuck
// because its shield is gone and will never ack removal. Because it removes the
// record of intent WITHOUT observing the rule's removal, callers must be admin-only
// and MUST audit-log the action, then best-effort re-push the shield snapshot so a
// still-connected shield drops any lingering rule. Returns true if a row was deleted.
func ForceDeleteRow(ctx context.Context, db *pgxpool.Pool, tenantID, id string) (bool, error) {
	ct, err := db.Exec(ctx,
		`DELETE FROM resources WHERE id = $1 AND tenant_id = $2`,
		id, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("force delete resource: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}

// RecordAck processes a ResourceAck from Shield and updates the resource status.
// The ack's port_reachable is intentionally not a parameter: it carries no
// information beyond `status` (protected ⟺ reachable; "port not listening" ⟺ not),
// and there is no port_reachable column to persist it to. verified_at IS persisted
// as last_verified_at.
func RecordAck(ctx context.Context, db *pgxpool.Pool, tenantID, resourceID, status, errMsg string, verifiedAt int64) (bool, error) {
	if status == "unprotected" {
		ct, err := db.Exec(ctx,
			`DELETE FROM resources
			 WHERE id = $1 AND tenant_id = $2
			   AND status = 'deleting'`,
			resourceID, tenantID,
		)
		if err != nil {
			return false, fmt.Errorf("record ack (reap): %w", err)
		}
		if ct.RowsAffected() > 0 {
			return true, nil
		}
	}
	ct, err := db.Exec(ctx,
		`UPDATE resources
		    SET status           = $2,
		        error_message    = NULLIF($3, ''),
		        last_verified_at = to_timestamp($4),
		        applied_at       = CASE WHEN $2 = 'protected' AND applied_at IS NULL THEN NOW() ELSE applied_at END,
		        updated_at       = NOW()
		  WHERE id = $1
		    AND tenant_id = $5
			AND status != 'deleting'
		    AND NOT (status = 'protecting' AND pending_action = 'remove' AND $2 != 'unprotected')
		    AND NOT (status = 'protecting' AND pending_action = 'apply'  AND $2 = 'unprotected')`,
		resourceID, status, errMsg, verifiedAt, tenantID,
	)
	if err != nil {
		return false, fmt.Errorf("record ack: %w", err)
	}
	return status == "unprotected" && ct.RowsAffected() > 0, nil
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

// GetDeletingForShield returns the ids of tombstoned ('deleting') resources on
// a shield. The Phase 3 reconciler reaps these once the shield's state reports
// confirm the rule is gone (ADR-004: confirmation-gated deletion).
func GetDeletingForShield(ctx context.Context, db *pgxpool.Pool, shieldID string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT id FROM resources WHERE shield_id = $1 AND status = 'deleting'`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("get deleting resources: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReapTombstone hard-deletes a 'deleting' tombstone after the shield's reported
// state confirmed the rule is absent. Returns true if a row was reaped.
func ReapTombstone(ctx context.Context, db *pgxpool.Pool, tenantID, shieldID, resourceID string) (bool, error) {
	ct, err := db.Exec(ctx,
		`DELETE FROM resources
		  WHERE id = $1 AND tenant_id = $2 AND shield_id = $3 AND status = 'deleting'`,
		resourceID, tenantID, shieldID,
	)
	if err != nil {
		return false, fmt.Errorf("reap tombstone: %w", err)
	}
	return ct.RowsAffected() > 0, nil
}
