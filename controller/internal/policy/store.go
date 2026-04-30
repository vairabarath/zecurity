package policy

import (
	"context"
	"errors"
	"fmt"
	"time"

	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("not found")

// GroupRow is a group record with pre-loaded members and resources.
type GroupRow struct {
	ID          string
	WorkspaceID string
	Name        string
	Description *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AccessRuleRow represents a single access_rules record.
type AccessRuleRow struct {
	ID          string
	WorkspaceID string
	ResourceID  string
	GroupID     string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store wraps a pgxpool and provides policy DB operations.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a Store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ── Groups ────────────────────────────────────────────────────────────────

func (s *Store) CreateGroup(ctx context.Context, workspaceID, name string, description *string) (*GroupRow, error) {
	row := &GroupRow{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO groups (workspace_id, name, description)
		 VALUES ($1, $2, $3)
		 RETURNING id, workspace_id, name, description, created_at, updated_at`,
		workspaceID, name, description,
	).Scan(&row.ID, &row.WorkspaceID, &row.Name, &row.Description, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}
	return row, nil
}

func (s *Store) UpdateGroup(ctx context.Context, id string, name *string, description *string) (*GroupRow, error) {
	row := &GroupRow{}
	err := s.pool.QueryRow(ctx,
		`UPDATE groups
		 SET name        = COALESCE($2, name),
		     description = COALESCE($3, description),
		     updated_at  = NOW()
		 WHERE id = $1
		 RETURNING id, workspace_id, name, description, created_at, updated_at`,
		id, name, description,
	).Scan(&row.ID, &row.WorkspaceID, &row.Name, &row.Description, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update group: %w", err)
	}
	return row, nil
}

func (s *Store) DeleteGroup(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetGroup(ctx context.Context, id string) (*GroupRow, error) {
	row := &GroupRow{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, description, created_at, updated_at
		 FROM groups WHERE id = $1`,
		id,
	).Scan(&row.ID, &row.WorkspaceID, &row.Name, &row.Description, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get group: %w", err)
	}
	return row, nil
}

func (s *Store) ListGroups(ctx context.Context, workspaceID string) ([]*GroupRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, description, created_at, updated_at
		 FROM groups WHERE workspace_id = $1 ORDER BY created_at`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var out []*GroupRow
	for rows.Next() {
		r := &GroupRow{}
		if err := rows.Scan(&r.ID, &r.WorkspaceID, &r.Name, &r.Description, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Members ───────────────────────────────────────────────────────────────

func (s *Store) AddGroupMember(ctx context.Context, groupID, userID string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`,
		groupID, userID,
	)
	if err != nil {
		return fmt.Errorf("add group member: %w", err)
	}
	return nil
}

func (s *Store) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`,
		groupID, userID,
	)
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGroupMembers returns user IDs that belong to a group.
func (s *Store) ListGroupMembers(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id FROM group_members WHERE group_id = $1`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── Access Rules ──────────────────────────────────────────────────────────

func (s *Store) AssignResourceToGroup(ctx context.Context, workspaceID, resourceID, groupID string) (*AccessRuleRow, error) {
	row := &AccessRuleRow{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO access_rules (workspace_id, resource_id, group_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (resource_id, group_id) DO UPDATE SET enabled = TRUE, updated_at = NOW()
		 RETURNING id, workspace_id, resource_id, group_id, enabled, created_at, updated_at`,
		workspaceID, resourceID, groupID,
	).Scan(&row.ID, &row.WorkspaceID, &row.ResourceID, &row.GroupID, &row.Enabled, &row.CreatedAt, &row.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("assign resource to group: %w", err)
	}
	return row, nil
}

func (s *Store) UnassignResourceFromGroup(ctx context.Context, resourceID, groupID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM access_rules WHERE resource_id = $1 AND group_id = $2`,
		resourceID, groupID,
	)
	if err != nil {
		return fmt.Errorf("unassign resource from group: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetRuleEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE access_rules SET enabled = $2, updated_at = NOW() WHERE id = $1`,
		id, enabled,
	)
	if err != nil {
		return fmt.Errorf("set rule enabled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGroupsForResource returns group IDs that have an enabled access rule for a resource.
func (s *Store) ListGroupsForResource(ctx context.Context, resourceID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT group_id FROM access_rules WHERE resource_id = $1 AND enabled = TRUE`,
		resourceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list groups for resource: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan group id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListResourcesForGroup returns resource IDs assigned to a group.
func (s *Store) ListResourcesForGroup(ctx context.Context, groupID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT resource_id FROM access_rules WHERE group_id = $1 AND enabled = TRUE`,
		groupID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resources for group: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan resource id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── Compiler queries ──────────────────────────────────────────────────────

// CompilerResourceRow is the minimal resource data the compiler needs.
type CompilerResourceRow struct {
	ResourceID string
	Address    string
	Port       uint32
	Protocol   string
	GroupID    string
}

// ListEnabledRulesWithResources returns all enabled access_rules joined with
// resource host/port for a workspace. Used by the ACL compiler.
func (s *Store) ListEnabledRulesWithResources(ctx context.Context, workspaceID string) ([]*CompilerResourceRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ar.resource_id, r.host, r.port_from, r.protocol, ar.group_id
		 FROM access_rules ar
		 JOIN resources r ON r.id = ar.resource_id
		 WHERE ar.workspace_id = $1
		   AND ar.enabled = TRUE
		   AND r.deleted_at IS NULL`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list enabled rules: %w", err)
	}
	defer rows.Close()

	var out []*CompilerResourceRow
	for rows.Next() {
		r := &CompilerResourceRow{}
		if err := rows.Scan(&r.ResourceID, &r.Address, &r.Port, &r.Protocol, &r.GroupID); err != nil {
			return nil, fmt.Errorf("scan rule row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListActiveDeviceSPIFFEsForGroup returns non-revoked client device SPIFFE IDs
// for all users in a group, within the workspace. Used by the ACL compiler.
func (s *Store) ListActiveDeviceSPIFFEsForGroup(ctx context.Context, workspaceID, groupID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT cd.spiffe_id
		 FROM group_members gm
		 JOIN client_devices cd ON cd.user_id = gm.user_id
		 WHERE gm.group_id = $1
		   AND cd.workspace_id = $2
		   AND cd.spiffe_id IS NOT NULL`,
		groupID, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list device spiffes: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan spiffe id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
