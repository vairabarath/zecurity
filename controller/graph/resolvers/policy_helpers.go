package resolvers

import (
	"context"
	"errors"
	"fmt"
	"time"

	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/models"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// errGroupOrUserNotFound answers addGroupMember for a group or user that is
// missing or belongs to another workspace, with one message for all four
// cases so the response reveals nothing about other workspaces (B-1).
var errGroupOrUserNotFound = apperr.UserErrorf("addGroupMember: group or user not found")

// loadUser fetches a single user row of tenantID by ID.
func loadUser(ctx context.Context, pool *pgxpool.Pool, tenantID, userID string) (*models.User, error) {
	var u models.User
	var lastLoginAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT id, tenant_id, email, provider, provider_sub, role, status, last_login_at, created_at, updated_at,
		        provisioned_by, provisioning_owner
		 FROM users WHERE id = $1 AND tenant_id = $2`,
		userID, tenantID,
	).Scan(&u.ID, &u.TenantID, &u.Email, &u.Provider, &u.ProviderSub,
		&u.Role, &u.Status, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt,
		&u.ProvisionedBy, &u.ProvisioningOwner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("user %s not found", userID)
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	u.LastLoginAt = lastLoginAt
	return &u, nil
}

// loadResourceByID fetches a minimal record of one of tenantID's resources for
// policy responses. Another workspace's resource reads as not found (B-1).
func loadResourceByID(ctx context.Context, pool *pgxpool.Pool, tenantID, resourceID string) (*graph.Resource, error) {
	var (
		res          graph.Resource
		description  *string
		errorMessage *string
		appliedAt    *time.Time
		lastVerified *time.Time
		createdAt    time.Time
		networkID    string
		networkName  string
	)
	err := pool.QueryRow(ctx,
		`SELECT r.id, r.name, r.description, r.host, r.protocol, r.port_from, r.port_to,
		        r.status, r.error_message, r.applied_at, r.last_verified_at, r.created_at,
		        r.remote_network_id, COALESCE(rn.name, '')
		 FROM resources r
		 LEFT JOIN remote_networks rn ON rn.id = r.remote_network_id AND rn.tenant_id = r.tenant_id
		 WHERE r.id = $1 AND r.tenant_id = $2`,
		resourceID, tenantID,
	).Scan(
		&res.ID, &res.Name, &description, &res.Host, &res.Protocol, &res.PortFrom, &res.PortTo,
		&res.Status, &errorMessage, &appliedAt, &lastVerified, &createdAt,
		&networkID, &networkName,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("resource %s not found", resourceID)
	}
	if err != nil {
		return nil, fmt.Errorf("load resource: %w", err)
	}
	res.Description = description
	res.ErrorMessage = errorMessage
	res.AppliedAt = fmtTimePtr(appliedAt)
	res.LastVerifiedAt = fmtTimePtr(lastVerified)
	res.CreatedAt = fmtTime(createdAt)
	res.RemoteNetwork = &graph.RemoteNetwork{ID: networkID, Name: networkName}
	res.Groups = []*graph.Group{}
	return &res, nil
}

// groupRowToGQL converts a policy.GroupRow to the GraphQL Group type.
func groupRowToGQL(row *policy.GroupRow, members []*models.User, resources []*graph.Resource) *graph.Group {
	if members == nil {
		members = []*models.User{}
	}
	if resources == nil {
		resources = []*graph.Resource{}
	}
	return &graph.Group{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		Members:     members,
		Resources:   resources,
		CreatedAt:   fmtTime(row.CreatedAt),
		UpdatedAt:   fmtTime(row.UpdatedAt),
		// ADR-025 §7 provenance. Origin is NOT NULL in the DB, but default to
		// "manual" rather than emitting "" if a row somehow predates the column
		// — an empty origin would read as "unknown" in the UI and defeat the
		// point of the label.
		Origin:       originOrManual(row.Origin),
		ExternalID:   row.ExternalID,
		ConnectionID: row.ConnectionID,
	}
}

// loadGroup fetches one of tenantID's groups with its members and assigned
// resources. A group of another workspace reads as not found, the same as a
// missing one (B-1).
func (r *Resolver) loadGroup(ctx context.Context, tenantID, groupID string) (*graph.Group, error) {
	row, err := r.PolicyStore.GetGroup(ctx, groupID)
	if errors.Is(err, policy.ErrNotFound) || (err == nil && row.WorkspaceID != tenantID) {
		return nil, apperr.UserErrorf("group not found")
	}
	if err != nil {
		return nil, fmt.Errorf("loadGroup: %w", err)
	}

	memberIDs, err := r.PolicyStore.ListGroupMembers(ctx, tenantID, groupID)
	if err != nil {
		return nil, fmt.Errorf("loadGroup members: %w", err)
	}
	members := make([]*models.User, 0, len(memberIDs))
	for _, uid := range memberIDs {
		u, err := loadUser(ctx, r.Pool, tenantID, uid)
		if err != nil {
			return nil, err
		}
		members = append(members, u)
	}

	resourceIDs, err := r.PolicyStore.ListResourcesForGroup(ctx, tenantID, groupID)
	if err != nil {
		return nil, fmt.Errorf("loadGroup resources: %w", err)
	}
	resources := make([]*graph.Resource, 0, len(resourceIDs))
	for _, rid := range resourceIDs {
		res, err := loadResourceByID(ctx, r.Pool, tenantID, rid)
		if err != nil {
			return nil, err
		}
		resources = append(resources, res)
	}

	return groupRowToGQL(row, members, resources), nil
}

// attachResourceGroups fills Groups on resource list results with one query
// (Sprint 20 F25). Before, resources/allResources always returned no groups.
func (r *Resolver) attachResourceGroups(ctx context.Context, tenantID string, list []*graph.Resource) error {
	byResource, err := r.PolicyStore.ListGroupsByResource(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("resource groups: %w", err)
	}
	for _, res := range list {
		rows := byResource[res.ID]
		groups := make([]*graph.Group, 0, len(rows))
		for _, g := range rows {
			groups = append(groups, groupRowToGQL(g, nil, nil))
		}
		res.Groups = groups
	}
	return nil
}

// loadResourceWithGroups fetches one of tenantID's resources and populates its
// Groups field with that workspace's groups only (B-1).
func (r *Resolver) loadResourceWithGroups(ctx context.Context, tenantID, resourceID string) (*graph.Resource, error) {
	res, err := loadResourceByID(ctx, r.Pool, tenantID, resourceID)
	if err != nil {
		return nil, err
	}

	groupIDs, err := r.PolicyStore.ListGroupsForResource(ctx, tenantID, resourceID)
	if err != nil {
		return nil, fmt.Errorf("loadResourceWithGroups: %w", err)
	}
	groups := make([]*graph.Group, 0, len(groupIDs))
	for _, gid := range groupIDs {
		grow, err := r.PolicyStore.GetGroup(ctx, gid)
		if err != nil {
			return nil, fmt.Errorf("loadResourceWithGroups group: %w", err)
		}
		groups = append(groups, groupRowToGQL(grow, nil, nil))
	}
	res.Groups = groups
	return res, nil
}

// originOrManual guarantees a non-empty group origin. The column is NOT NULL
// with a 'manual' default (migration 034), so this only guards against a zero
// value reaching the API from an unpopulated struct.
func originOrManual(o string) string {
	if o == "" {
		return "manual"
	}
	return o
}
