package posture

// resource_policy_store.go -- PENDING-16 Phase 2.
//
// Resource Policies are the access-policy layer between a Resource and the
// Device Profiles that define device trust:
//
//	Resource -> exactly one Resource Policy -> zero or more Device Profiles
//
// A policy with zero profiles is a valid, intentional state meaning "Any
// Device". Multiple profiles on one policy are OR-evaluated by the
// authorization path, which a later phase wires up -- nothing here reads or
// changes ACL compilation.
//
// The Resource Policy model has no audit/enforce mode. Enforcement follows from
// a policy referencing a profile rather than from a switch on the profile, so
// none of the legacy device_profiles.mode guards are carried into these
// operations.
//
// The legacy direct binding model (resource_profile_bindings, operated on in
// store.go) is untouched and still authoritative. The two models coexist until
// the Phase 4 migration.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrInvalidPolicyName is returned when a resource policy name is blank.
	ErrInvalidPolicyName = errors.New("resource policy name is required")

	// ErrDuplicatePolicyName is returned when the workspace already has a
	// resource policy with the requested name.
	ErrDuplicatePolicyName = errors.New("duplicate resource policy name")

	// ErrDuplicateProfileBinding is returned when a device profile is already
	// attached to the resource policy. Kept distinct from ErrDuplicateBinding,
	// which belongs to the legacy resource_profile_bindings model.
	ErrDuplicateProfileBinding = errors.New("duplicate resource policy profile binding")

	// ErrResourceAlreadyAssigned is returned when a resource already carries a
	// different resource policy. A resource has exactly one policy, and an
	// existing assignment is never silently replaced.
	ErrResourceAlreadyAssigned = errors.New("resource already has a different resource policy")

	// ErrPolicyAssigned is returned when deleting a resource policy that is
	// still assigned to at least one resource. The caller must unassign first,
	// so deleting a policy can never silently strip a resource of its policy.
	ErrPolicyAssigned = errors.New("resource policy is still assigned to a resource")
)

// ResourcePolicy is a workspace-owned access policy that a Resource can be
// assigned to and that zero or more Device Profiles can be attached to.
type ResourcePolicy struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Name        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// resourcePolicyColumns is the shared column list for ResourcePolicy scans, so
// every query and its Scan call stay in the same order.
const resourcePolicyColumns = `id,
	        workspace_id,
	        name,
	        created_at,
	        updated_at`

// CreateResourcePolicy creates a resource policy owned by the workspace. The
// new policy starts with zero device profiles, which is valid and means
// "Any Device".
func (s *Store) CreateResourcePolicy(
	ctx context.Context,
	workspaceID uuid.UUID,
	name string,
) (*ResourcePolicy, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidPolicyName
	}

	policy := &ResourcePolicy{}

	err := s.pool.QueryRow(
		ctx,
		`INSERT INTO device_resource_policies (
		     workspace_id,
		     name
		 )
		 VALUES ($1, $2)
		 RETURNING `+resourcePolicyColumns,
		workspaceID,
		name,
	).Scan(
		&policy.ID,
		&policy.WorkspaceID,
		&policy.Name,
		&policy.CreatedAt,
		&policy.UpdatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicatePolicyName
		}
		return nil, fmt.Errorf("create resource policy: %w", err)
	}

	return policy, nil
}

// GetResourcePolicy fetches one workspace-scoped resource policy.
func (s *Store) GetResourcePolicy(
	ctx context.Context,
	workspaceID,
	policyID uuid.UUID,
) (*ResourcePolicy, error) {
	policy := &ResourcePolicy{}

	err := s.pool.QueryRow(
		ctx,
		`SELECT `+resourcePolicyColumns+`
		   FROM device_resource_policies
		  WHERE workspace_id = $1
		    AND id = $2`,
		workspaceID,
		policyID,
	).Scan(
		&policy.ID,
		&policy.WorkspaceID,
		&policy.Name,
		&policy.CreatedAt,
		&policy.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get resource policy: %w", err)
	}

	return policy, nil
}

// ListResourcePolicies lists every resource policy in the workspace.
func (s *Store) ListResourcePolicies(
	ctx context.Context,
	workspaceID uuid.UUID,
) ([]ResourcePolicy, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT `+resourcePolicyColumns+`
		   FROM device_resource_policies
		  WHERE workspace_id = $1
		  ORDER BY name`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resource policies: %w", err)
	}
	defer rows.Close()

	policies := make([]ResourcePolicy, 0)

	for rows.Next() {
		var policy ResourcePolicy

		if err := rows.Scan(
			&policy.ID,
			&policy.WorkspaceID,
			&policy.Name,
			&policy.CreatedAt,
			&policy.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan resource policy: %w", err)
		}

		policies = append(policies, policy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resource policies: %w", err)
	}

	return policies, nil
}

// UpdateResourcePolicy renames a workspace-scoped resource policy.
func (s *Store) UpdateResourcePolicy(
	ctx context.Context,
	workspaceID,
	policyID uuid.UUID,
	name string,
) (*ResourcePolicy, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrInvalidPolicyName
	}

	policy := &ResourcePolicy{}

	err := s.pool.QueryRow(
		ctx,
		`UPDATE device_resource_policies
		    SET name = $3,
		        updated_at = NOW()
		  WHERE workspace_id = $1
		    AND id = $2
		 RETURNING `+resourcePolicyColumns,
		workspaceID,
		policyID,
		name,
	).Scan(
		&policy.ID,
		&policy.WorkspaceID,
		&policy.Name,
		&policy.CreatedAt,
		&policy.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicatePolicyName
		}
		return nil, fmt.Errorf("update resource policy: %w", err)
	}

	return policy, nil
}

// DeleteResourcePolicy deletes a resource policy that no resource is using.
//
// Returns ErrPolicyAssigned while any resource still references the policy --
// deleting it would otherwise leave that resource without the policy its access
// depends on. Attached profile bindings are removed by the schema's cascade.
func (s *Store) DeleteResourcePolicy(
	ctx context.Context,
	workspaceID,
	policyID uuid.UUID,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the policy row so its assignment count cannot change between the
	// check below and the delete.
	var policyWorkspaceID uuid.UUID
	err = tx.QueryRow(
		ctx,
		`SELECT workspace_id
		   FROM device_resource_policies
		  WHERE id = $1
		    FOR UPDATE`,
		policyID,
	).Scan(&policyWorkspaceID)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock resource policy: %w", err)
	}
	if policyWorkspaceID != workspaceID {
		return ErrWorkspaceMismatch
	}

	var assignedCount int
	if err := tx.QueryRow(
		ctx,
		`SELECT COUNT(*)
		   FROM resources
		  WHERE device_resource_policy_id = $1`,
		policyID,
	).Scan(&assignedCount); err != nil {
		return fmt.Errorf("count resources for resource policy: %w", err)
	}
	if assignedCount > 0 {
		return ErrPolicyAssigned
	}

	cmdTag, err := tx.Exec(
		ctx,
		`DELETE FROM device_resource_policies
		  WHERE workspace_id = $1
		    AND id = $2`,
		workspaceID,
		policyID,
	)
	if err != nil {
		// ON DELETE NO ACTION on resources.device_resource_policy_id is the
		// authoritative guard; an assignment that raced the count above
		// surfaces here as the same domain error.
		if isForeignKeyViolation(err) {
			return ErrPolicyAssigned
		}
		return fmt.Errorf("delete resource policy: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete resource policy: %w", err)
	}

	return nil
}

// AssignResourcePolicy assigns a resource policy to a resource.
//
// A resource has exactly one resource policy, so assigning a second, different
// policy returns ErrResourceAlreadyAssigned rather than replacing the existing
// one. Re-assigning the policy the resource already carries succeeds without
// change.
func (s *Store) AssignResourcePolicy(
	ctx context.Context,
	workspaceID,
	resourceID,
	policyID uuid.UUID,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin assign resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	var policyWorkspaceID uuid.UUID
	err = tx.QueryRow(
		ctx,
		`SELECT workspace_id
		   FROM device_resource_policies
		  WHERE id = $1`,
		policyID,
	).Scan(&policyWorkspaceID)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup resource policy: %w", err)
	}
	if policyWorkspaceID != workspaceID {
		return ErrWorkspaceMismatch
	}

	// Lock the resource row so two concurrent assignments cannot both observe
	// an unassigned resource and both succeed.
	var (
		resourceTenantID uuid.UUID
		currentPolicyID  *uuid.UUID
	)
	err = tx.QueryRow(
		ctx,
		`SELECT tenant_id,
		        device_resource_policy_id
		   FROM resources
		  WHERE id = $1
		    FOR UPDATE`,
		resourceID,
	).Scan(
		&resourceTenantID,
		&currentPolicyID,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock resource: %w", err)
	}
	if resourceTenantID != workspaceID {
		return ErrWorkspaceMismatch
	}

	if currentPolicyID != nil {
		if *currentPolicyID != policyID {
			return ErrResourceAlreadyAssigned
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit assign resource policy: %w", err)
		}
		return nil
	}

	if _, err := tx.Exec(
		ctx,
		`UPDATE resources
		    SET device_resource_policy_id = $3
		  WHERE id = $2
		    AND tenant_id = $1`,
		workspaceID,
		resourceID,
		policyID,
	); err != nil {
		return fmt.Errorf("assign resource policy: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit assign resource policy: %w", err)
	}

	return nil
}

// UnassignResourcePolicy clears the resource's policy assignment. Unassigning a
// resource that carries no policy succeeds without change.
func (s *Store) UnassignResourcePolicy(
	ctx context.Context,
	workspaceID,
	resourceID uuid.UUID,
) error {
	cmdTag, err := s.pool.Exec(
		ctx,
		`UPDATE resources
		    SET device_resource_policy_id = NULL
		  WHERE id = $2
		    AND tenant_id = $1`,
		workspaceID,
		resourceID,
	)
	if err != nil {
		return fmt.Errorf("unassign resource policy: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return nil
}

// GetResourcePolicyForResource returns the policy assigned to one resource.
//
// Returns (nil, nil) when the resource exists but carries no policy, and
// ErrNotFound when the resource does not exist in this workspace -- so callers
// can tell "no policy" apart from "no such resource".
func (s *Store) GetResourcePolicyForResource(
	ctx context.Context,
	workspaceID,
	resourceID uuid.UUID,
) (*ResourcePolicy, error) {
	var (
		id         *uuid.UUID
		policyWSID *uuid.UUID
		name       *string
		createdAt  *time.Time
		updatedAt  *time.Time
	)

	err := s.pool.QueryRow(
		ctx,
		`SELECT p.id,
		        p.workspace_id,
		        p.name,
		        p.created_at,
		        p.updated_at
		   FROM resources r
		   LEFT JOIN device_resource_policies p
		     ON p.id = r.device_resource_policy_id
		  WHERE r.id = $2
		    AND r.tenant_id = $1`,
		workspaceID,
		resourceID,
	).Scan(
		&id,
		&policyWSID,
		&name,
		&createdAt,
		&updatedAt,
	)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get resource policy for resource: %w", err)
	}
	if id == nil {
		return nil, nil
	}

	return &ResourcePolicy{
		ID:          *id,
		WorkspaceID: *policyWSID,
		Name:        *name,
		CreatedAt:   *createdAt,
		UpdatedAt:   *updatedAt,
	}, nil
}

// ListResourceIDsForPolicy lists the resources assigned to one policy.
func (s *Store) ListResourceIDsForPolicy(
	ctx context.Context,
	workspaceID,
	policyID uuid.UUID,
) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT id
		   FROM resources
		  WHERE tenant_id = $1
		    AND device_resource_policy_id = $2
		  ORDER BY id`,
		workspaceID,
		policyID,
	)
	if err != nil {
		return nil, fmt.Errorf("list resources for resource policy: %w", err)
	}
	defer rows.Close()

	resourceIDs := make([]uuid.UUID, 0)

	for rows.Next() {
		var resourceID uuid.UUID
		if err := rows.Scan(&resourceID); err != nil {
			return nil, fmt.Errorf("scan resource for resource policy: %w", err)
		}
		resourceIDs = append(resourceIDs, resourceID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resources for resource policy: %w", err)
	}

	return resourceIDs, nil
}

// AddProfileToPolicy attaches a device profile to a resource policy.
//
// Any number of profiles may be attached; duplicates are rejected. No
// audit/enforce or requirement-count guard applies here -- a profile's
// requirements are its own concern, and enforcement follows from this
// attachment rather than from a mode on the profile.
func (s *Store) AddProfileToPolicy(
	ctx context.Context,
	workspaceID,
	policyID,
	profileID uuid.UUID,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin add profile to resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	var policyWorkspaceID uuid.UUID
	err = tx.QueryRow(
		ctx,
		`SELECT workspace_id
		   FROM device_resource_policies
		  WHERE id = $1`,
		policyID,
	).Scan(&policyWorkspaceID)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup resource policy: %w", err)
	}
	if policyWorkspaceID != workspaceID {
		return ErrWorkspaceMismatch
	}

	var profileWorkspaceID uuid.UUID
	err = tx.QueryRow(
		ctx,
		`SELECT workspace_id
		   FROM device_profiles
		  WHERE id = $1`,
		profileID,
	).Scan(&profileWorkspaceID)

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lookup device profile: %w", err)
	}
	if profileWorkspaceID != workspaceID {
		return ErrWorkspaceMismatch
	}

	if _, err := tx.Exec(
		ctx,
		`INSERT INTO resource_policy_profile_bindings (
		     device_resource_policy_id,
		     profile_id,
		     workspace_id
		 )
		 VALUES ($2, $3, $1)`,
		workspaceID,
		policyID,
		profileID,
	); err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateProfileBinding
		}
		return fmt.Errorf("add profile to resource policy: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit add profile to resource policy: %w", err)
	}

	return nil
}

// RemoveProfileFromPolicy detaches a device profile from a resource policy.
// Removing the last profile is allowed -- the policy then means "Any Device".
func (s *Store) RemoveProfileFromPolicy(
	ctx context.Context,
	workspaceID,
	policyID,
	profileID uuid.UUID,
) error {
	cmdTag, err := s.pool.Exec(
		ctx,
		`DELETE FROM resource_policy_profile_bindings
		  WHERE workspace_id = $1
		    AND device_resource_policy_id = $2
		    AND profile_id = $3`,
		workspaceID,
		policyID,
		profileID,
	)
	if err != nil {
		return fmt.Errorf("remove profile from resource policy: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}

	return nil
}

// ListProfilesForPolicy lists the device profiles attached to a resource
// policy. An empty result is valid and means the policy applies to any device.
func (s *Store) ListProfilesForPolicy(
	ctx context.Context,
	workspaceID,
	policyID uuid.UUID,
) ([]Profile, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT p.id,
		        p.workspace_id,
		        p.name,
		        p.mode,
		        p.revision,
		        p.manual_trust_enabled,
		        p.created_at,
		        p.updated_at
		   FROM resource_policy_profile_bindings b
		   JOIN device_profiles p
		     ON p.id = b.profile_id
		  WHERE b.workspace_id = $1
		    AND b.device_resource_policy_id = $2
		  ORDER BY p.name`,
		workspaceID,
		policyID,
	)
	if err != nil {
		return nil, fmt.Errorf("list profiles for resource policy: %w", err)
	}
	defer rows.Close()

	profiles := make([]Profile, 0)

	for rows.Next() {
		var profile Profile

		if err := rows.Scan(
			&profile.ID,
			&profile.WorkspaceID,
			&profile.Name,
			&profile.Mode,
			&profile.Revision,
			&profile.ManualTrustEnabled,
			&profile.CreatedAt,
			&profile.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan profile for resource policy: %w", err)
		}

		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate profiles for resource policy: %w", err)
	}

	return profiles, nil
}

// isForeignKeyViolation reports whether err is a Postgres foreign-key
// violation. Mirrors isUniqueViolation in store.go.
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
