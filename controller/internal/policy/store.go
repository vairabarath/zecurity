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

// NewStore creates a Store. Relay discovery now comes from the relays table
// at ACL-compile time (see compiler.go), not from env-var config.
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
	ResourceID        string
	Name              string
	Address           string
	Port              uint32
	Protocol          string
	GroupID           string
	ShieldID          string
	Status            string
	RemoteNetworkID   string
	RemoteNetworkName string
}

// ListEnabledRulesWithResources returns all enabled access_rules joined with
// resource host/port/name for a workspace. Used by the ACL compiler.
func (s *Store) ListEnabledRulesWithResources(ctx context.Context, workspaceID string) ([]*CompilerResourceRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT ar.resource_id, r.name, r.host, r.port_from, r.protocol,
		        ar.group_id, COALESCE(r.shield_id::text, ''), r.status,
		        r.remote_network_id::text, rn.name
		 FROM access_rules ar
		 JOIN resources r ON r.id = ar.resource_id
		 JOIN remote_networks rn ON rn.id = r.remote_network_id
		 WHERE ar.workspace_id = $1
		   AND ar.enabled = TRUE
		   AND r.status != 'deleting'`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list enabled rules: %w", err)
	}
	defer rows.Close()

	var out []*CompilerResourceRow
	for rows.Next() {
		r := &CompilerResourceRow{}
		if err := rows.Scan(&r.ResourceID, &r.Name, &r.Address, &r.Port, &r.Protocol, &r.GroupID, &r.ShieldID, &r.Status, &r.RemoteNetworkID, &r.RemoteNetworkName); err != nil {
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
		   AND cd.spiffe_id IS NOT NULL
		   AND cd.revoked_at IS NULL`,
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

// ListActiveDeviceSPIFFEsForGroups returns non-revoked client device SPIFFE IDs
// for all supplied group IDs in a single query. The returned map is keyed by
// group ID; groups with no active devices are absent from the map.
func (s *Store) ListActiveDeviceSPIFFEsForGroups(ctx context.Context, workspaceID string, groupIDs []string) (map[string][]string, error) {
	if len(groupIDs) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT gm.group_id::text, cd.spiffe_id
		 FROM group_members gm
		 JOIN client_devices cd ON cd.user_id = gm.user_id
		 WHERE gm.group_id = ANY($1::uuid[])
		   AND cd.workspace_id = $2
		   AND cd.spiffe_id IS NOT NULL
		   AND cd.revoked_at IS NULL`,
		groupIDs, workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list device spiffes for groups: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]string)
	for rows.Next() {
		var groupID, spiffeID string
		if err := rows.Scan(&groupID, &spiffeID); err != nil {
			return nil, fmt.Errorf("scan spiffe row: %w", err)
		}
		out[groupID] = append(out[groupID], spiffeID)
	}
	return out, rows.Err()
}

// RemoteNetworkConnectorsRow is one active connector row for a remote network.
type RemoteNetworkConnectorsRow struct {
	RemoteNetworkID string
	ConnectorID     string
	LanAddr         string
	TrustDomain     string
	RelayAddr       string // empty if connector has no placement or relay has no public addr
	RelayID         string // empty if no placement; used to build SPIFFE ID
}

// GetConnectorsForRemoteNetworks returns all active connectors for the given
// remote network IDs. RNs with no active connector are simply absent from the
// result — callers must seed entries for every RN first and then populate
// connectors from this result to preserve partial-availability semantics.
// Each row includes per-connector relay coordinates from connector_relay_placement
// (LEFT JOIN so connectors without a placement still appear as direct-only).
func (s *Store) GetConnectorsForRemoteNetworks(ctx context.Context, remoteNetworkIDs []string) ([]*RemoteNetworkConnectorsRow, error) {
	if len(remoteNetworkIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT c.remote_network_id::text,
		        c.id::text,
		        COALESCE(c.lan_addr, ''),
		        COALESCE(c.trust_domain, ''),
		        COALESCE(
		          CASE
		            WHEN r.public_addr IS NOT NULL AND r.public_addr != ''
		              THEN r.public_addr
		            WHEN r.address_scope = 'public' AND r.observed_ip IS NOT NULL
		              THEN r.observed_ip::text || ':9093'
		            ELSE ''
		          END, ''
		        ),
		        COALESCE(r.id::text, '')
		   FROM connectors c
		   LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
		   LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
		  WHERE c.remote_network_id = ANY($1::uuid[])
		    AND c.status = 'active'
		  ORDER BY c.remote_network_id, c.last_heartbeat_at DESC NULLS LAST`,
		remoteNetworkIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("get connectors for remote networks: %w", err)
	}
	defer rows.Close()

	var out []*RemoteNetworkConnectorsRow
	for rows.Next() {
		r := &RemoteNetworkConnectorsRow{}
		if err := rows.Scan(&r.RemoteNetworkID, &r.ConnectorID, &r.LanAddr, &r.TrustDomain, &r.RelayAddr, &r.RelayID); err != nil {
			return nil, fmt.Errorf("scan connector row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RelayDiscoveryRow holds the minimal relay data needed for ACL snapshot generation.
type RelayDiscoveryRow struct {
	ID           string
	PublicAddr   string
	ObservedIP   string
	AddressScope string
}

// GetActiveRelay returns the most-recently-heartbeating active relay that has a
// discoverable public address, or nil if none exists.
func (s *Store) GetActiveRelay(ctx context.Context) (*RelayDiscoveryRow, error) {
	row := &RelayDiscoveryRow{}
	err := s.pool.QueryRow(ctx,
		`SELECT id::text,
		        COALESCE(public_addr, ''),
		        COALESCE(observed_ip::text, ''),
		        COALESCE(address_scope, '')
		   FROM relays
		  WHERE status = 'active'
		    AND (public_addr IS NOT NULL OR address_scope = 'public')
		  ORDER BY last_heartbeat_at DESC NULLS LAST
		  LIMIT 1`,
	).Scan(&row.ID, &row.PublicAddr, &row.ObservedIP, &row.AddressScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active relay: %w", err)
	}
	return row, nil
}
