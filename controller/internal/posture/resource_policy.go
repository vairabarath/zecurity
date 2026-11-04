package posture

// PENDING-16: Resource → Resource Policy → Device Profiles.
//
// Every resource has exactly one resource policy (resources.policy_id); every
// workspace has one Default policy (created with the workspace, migration 049).
// A policy requires zero or more device profiles: a device passes when it
// satisfies ANY of them, and a policy with none allows any device.
//
// Lock order for the races between these writes: device profile rows first
// (sorted by id when several), then the policy row. "Add a profile to a
// policy", "remove a profile's last requirement" and "delete a profile in use"
// all take the profile lock, so none can interleave with another.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

var (
	ErrPolicyNotFound       = apperr.UserErrorf("resource policy not found")
	ErrPolicyResourceGone   = apperr.UserErrorf("resource not found")
	ErrDefaultPolicyFixed   = apperr.UserErrorf("the Default resource policy can't be deleted or renamed")
	ErrPolicyNameTaken      = apperr.UserErrorf("a resource policy with this name already exists")
	ErrInvalidPolicyName    = apperr.UserErrorf("resource policy name must be 1 to 100 characters")
	ErrPolicyProfileMissing = apperr.UserErrorf("device profile not found")
	ErrEmptyProfileInPolicy = apperr.UserErrorf("a device profile with no requirements can't be added to a resource policy; it would block every device")
)

// ProfileInUseError refuses deleting a device profile that resource policies
// use, unless the caller confirmed it (D-4). It unwraps to a user-safe error,
// so the console shows the message with the policies' names.
type ProfileInUseError struct {
	Policies []PolicyRef
	user     error
}

func (e *ProfileInUseError) Error() string { return e.user.Error() }
func (e *ProfileInUseError) Unwrap() error { return e.user }

func newProfileInUseError(policies []PolicyRef) *ProfileInUseError {
	names := make([]string, len(policies))
	for i, p := range policies {
		names[i] = p.Name
	}
	return &ProfileInUseError{
		Policies: policies,
		user: apperr.UserErrorf(
			"device profile is used by resource policies: %s; confirm to remove it from them (a policy left with no profiles allows any device)",
			strings.Join(names, ", ")),
	}
}

type PolicyRef struct {
	ID   uuid.UUID
	Name string
}

type ResourcePolicy struct {
	ID            uuid.UUID
	WorkspaceID   uuid.UUID
	Name          string
	Description   *string
	IsDefault     bool
	ProfileIDs    []uuid.UUID
	ResourceCount int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ProfileDeleteResult says what deleting a device profile changed.
type ProfileDeleteResult struct {
	// RemovedFrom: the policies the profile was removed from.
	RemovedFrom []PolicyRef
	// NowAnyDevice: those of them left with no profiles (they now allow any device).
	NowAnyDevice []PolicyRef
}

// afterPolicyDeleteLock is a test seam: when set, DeleteResourcePolicy calls it
// after locking the policy row and before moving its resources, to widen the
// window a concurrent assignment races in. Always nil outside tests.
var afterPolicyDeleteLock func()

func validPolicyName(name string) (string, error) {
	n := strings.TrimSpace(name)
	if len(n) < 1 || len(n) > 100 {
		return "", ErrInvalidPolicyName
	}
	return n, nil
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

const policySelect = `
	SELECT p.id, p.workspace_id, p.name, p.description, p.is_default, p.created_at, p.updated_at,
	       COALESCE((SELECT array_agg(rpp.profile_id ORDER BY rpp.profile_id)
	                   FROM resource_policy_profiles rpp WHERE rpp.policy_id = p.id), '{}'),
	       (SELECT count(*) FROM resources r WHERE r.policy_id = p.id)
	  FROM resource_policies p`

func scanPolicy(row pgx.Row) (*ResourcePolicy, error) {
	var p ResourcePolicy
	if err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Description, &p.IsDefault,
		&p.CreatedAt, &p.UpdatedAt, &p.ProfileIDs, &p.ResourceCount); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListResourcePolicies returns the workspace's policies, Default first.
func (s *Store) ListResourcePolicies(ctx context.Context, workspaceID uuid.UUID) ([]ResourcePolicy, error) {
	rows, err := s.pool.Query(ctx, policySelect+`
	 WHERE p.workspace_id = $1
	 ORDER BY p.is_default DESC, lower(p.name)`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list resource policies: %w", err)
	}
	defer rows.Close()
	out := []ResourcePolicy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan resource policy: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetResourcePolicy returns one policy of the workspace; a foreign or missing
// one is ErrPolicyNotFound.
func (s *Store) GetResourcePolicy(ctx context.Context, workspaceID, policyID uuid.UUID) (*ResourcePolicy, error) {
	p, err := scanPolicy(s.pool.QueryRow(ctx, policySelect+`
	 WHERE p.workspace_id = $1 AND p.id = $2`, workspaceID, policyID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get resource policy: %w", err)
	}
	return p, nil
}

// PolicyOfResource returns the policy a resource of the workspace is in.
func (s *Store) PolicyOfResource(ctx context.Context, workspaceID, resourceID uuid.UUID) (*ResourcePolicy, error) {
	p, err := scanPolicy(s.pool.QueryRow(ctx, policySelect+`
	  JOIN resources res ON res.policy_id = p.id
	 WHERE p.workspace_id = $1 AND res.id = $2 AND res.tenant_id = $1`, workspaceID, resourceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPolicyResourceGone
	}
	if err != nil {
		return nil, fmt.Errorf("policy of resource: %w", err)
	}
	return p, nil
}

// ResourcesInPolicy lists the ids of the policy's resources.
func (s *Store) ResourcesInPolicy(ctx context.Context, workspaceID, policyID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM resources WHERE tenant_id = $1 AND policy_id = $2 ORDER BY name, id`,
		workspaceID, policyID)
	if err != nil {
		return nil, fmt.Errorf("resources in policy: %w", err)
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PoliciesUsingProfile lists the policies that require the profile.
func (s *Store) PoliciesUsingProfile(ctx context.Context, workspaceID, profileID uuid.UUID) ([]PolicyRef, error) {
	return policiesUsingProfile(ctx, s.pool, workspaceID, profileID)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func policiesUsingProfile(ctx context.Context, q querier, workspaceID, profileID uuid.UUID) ([]PolicyRef, error) {
	rows, err := q.Query(ctx,
		`SELECT p.id, p.name
		   FROM resource_policy_profiles rpp
		   JOIN resource_policies p ON p.id = rpp.policy_id
		  WHERE rpp.workspace_id = $1 AND rpp.profile_id = $2
		  ORDER BY lower(p.name)`, workspaceID, profileID)
	if err != nil {
		return nil, fmt.Errorf("policies using profile: %w", err)
	}
	defer rows.Close()
	out := []PolicyRef{}
	for rows.Next() {
		var r PolicyRef
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func profileInUse(ctx context.Context, q querier, profileID uuid.UUID) (bool, error) {
	var inUse bool
	if err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM resource_policy_profiles WHERE profile_id = $1)`, profileID,
	).Scan(&inUse); err != nil {
		return false, fmt.Errorf("profile in use: %w", err)
	}
	return inUse, nil
}

// ResourceProfilesForWorkspace maps each resource of the workspace whose
// policy requires profiles to those profile ids (one query, for the ACL
// compiler). A resource absent from the map allows any device.
func (s *Store) ResourceProfilesForWorkspace(ctx context.Context, workspaceID uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, rpp.profile_id
		   FROM resources r
		   JOIN resource_policy_profiles rpp ON rpp.policy_id = r.policy_id
		  WHERE r.tenant_id = $1
		  ORDER BY r.id, rpp.profile_id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("resource profiles for workspace: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]uuid.UUID{}
	for rows.Next() {
		var resourceID, profileID uuid.UUID
		if err := rows.Scan(&resourceID, &profileID); err != nil {
			return nil, err
		}
		out[resourceID] = append(out[resourceID], profileID)
	}
	return out, rows.Err()
}

// CreateResourcePolicy adds a policy that allows any device until profiles
// are set.
func (s *Store) CreateResourcePolicy(ctx context.Context, workspaceID uuid.UUID, name string, description *string) (*ResourcePolicy, error) {
	n, err := validPolicyName(name)
	if err != nil {
		return nil, err
	}
	var id uuid.UUID
	err = s.pool.QueryRow(ctx,
		`INSERT INTO resource_policies (workspace_id, name, description)
		 VALUES ($1, $2, NULLIF(btrim($3), ''))
		 RETURNING id`, workspaceID, n, description).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrPolicyNameTaken
	}
	if isForeignKeyViolation(err) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("create resource policy: %w", err)
	}
	return s.GetResourcePolicy(ctx, workspaceID, id)
}

// UpdateResourcePolicy renames a policy and/or sets its description. The
// Default can't be renamed.
func (s *Store) UpdateResourcePolicy(ctx context.Context, workspaceID, policyID uuid.UUID, name, description *string) (*ResourcePolicy, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin update resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	var isDefault bool
	var current string
	err = tx.QueryRow(ctx,
		`SELECT is_default, name FROM resource_policies WHERE workspace_id = $1 AND id = $2 FOR UPDATE`,
		workspaceID, policyID).Scan(&isDefault, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock resource policy: %w", err)
	}
	newName := current
	if name != nil {
		if newName, err = validPolicyName(*name); err != nil {
			return nil, err
		}
		if isDefault && newName != current {
			return nil, ErrDefaultPolicyFixed
		}
	}
	_, err = tx.Exec(ctx,
		`UPDATE resource_policies
		    SET name = $3,
		        description = CASE WHEN $4::boolean THEN NULLIF(btrim($5), '') ELSE description END,
		        updated_at = NOW()
		  WHERE workspace_id = $1 AND id = $2`,
		workspaceID, policyID, newName, description != nil, description)
	if isUniqueViolation(err) {
		return nil, ErrPolicyNameTaken
	}
	if err != nil {
		return nil, fmt.Errorf("update resource policy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit update resource policy: %w", err)
	}
	return s.GetResourcePolicy(ctx, workspaceID, policyID)
}

// SetResourcePolicyProfiles replaces the profiles a policy requires. Every
// profile must be the workspace's and have at least one requirement; an empty
// list means any device.
func (s *Store) SetResourcePolicyProfiles(ctx context.Context, workspaceID, policyID uuid.UUID, profileIDs []uuid.UUID) (*ResourcePolicy, error) {
	ids := dedupeSorted(profileIDs)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin set policy profiles: %w", err)
	}
	defer tx.Rollback(ctx)

	// Profiles first (lock order), sorted so two writers can't deadlock.
	for _, pid := range ids {
		var reqs int
		err := tx.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM device_profile_requirements r WHERE r.profile_id = p.id)
			   FROM device_profiles p
			  WHERE p.workspace_id = $1 AND p.id = $2
			  FOR UPDATE OF p`, workspaceID, pid).Scan(&reqs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPolicyProfileMissing
		}
		if err != nil {
			return nil, fmt.Errorf("lock device profile: %w", err)
		}
		if reqs == 0 {
			return nil, ErrEmptyProfileInPolicy
		}
	}
	var locked uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM resource_policies WHERE workspace_id = $1 AND id = $2 FOR UPDATE`,
		workspaceID, policyID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock resource policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM resource_policy_profiles WHERE policy_id = $1`, policyID); err != nil {
		return nil, fmt.Errorf("clear policy profiles: %w", err)
	}
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO resource_policy_profiles (policy_id, profile_id, workspace_id)
			 SELECT $1, unnest($2::uuid[]), $3`, policyID, ids, workspaceID); err != nil {
			return nil, fmt.Errorf("set policy profiles: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE resource_policies SET updated_at = NOW() WHERE id = $1`, policyID); err != nil {
		return nil, fmt.Errorf("touch resource policy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit set policy profiles: %w", err)
	}
	return s.GetResourcePolicy(ctx, workspaceID, policyID)
}

// DeleteResourcePolicy deletes a policy after moving its resources to the
// workspace's Default, in one transaction under the policy's row lock. A
// concurrent assignment to the policy waits on that lock (its FK check) and
// then fails as ErrPolicyNotFound. Returns the moved resources.
func (s *Store) DeleteResourcePolicy(ctx context.Context, workspaceID, policyID uuid.UUID) ([]uuid.UUID, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin delete resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	var isDefault bool
	err = tx.QueryRow(ctx,
		`SELECT is_default FROM resource_policies WHERE workspace_id = $1 AND id = $2 FOR UPDATE`,
		workspaceID, policyID).Scan(&isDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPolicyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock resource policy: %w", err)
	}
	if isDefault {
		return nil, ErrDefaultPolicyFixed
	}
	if afterPolicyDeleteLock != nil {
		afterPolicyDeleteLock()
	}
	rows, err := tx.Query(ctx,
		`UPDATE resources r
		    SET policy_id = d.id, updated_at = NOW()
		   FROM resource_policies d
		  WHERE d.workspace_id = $1 AND d.is_default
		    AND r.tenant_id = $1 AND r.policy_id = $2
		 RETURNING r.id`, workspaceID, policyID)
	if err != nil {
		return nil, fmt.Errorf("move resources to default policy: %w", err)
	}
	moved := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		moved = append(moved, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("move resources to default policy: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM resource_policies WHERE id = $1`, policyID); err != nil {
		return nil, fmt.Errorf("delete resource policy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit delete resource policy: %w", err)
	}
	return moved, nil
}

// SetResourcePolicy moves a resource of the workspace to a policy of the same
// workspace and returns the policy it was in. A foreign or missing resource
// or policy gets the same answer as a missing one.
func (s *Store) SetResourcePolicy(ctx context.Context, workspaceID, resourceID, policyID uuid.UUID) (previous uuid.UUID, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin set resource policy: %w", err)
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx,
		`SELECT policy_id FROM resources WHERE tenant_id = $1 AND id = $2 FOR UPDATE`,
		workspaceID, resourceID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrPolicyResourceGone
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock resource: %w", err)
	}
	// The same-workspace FK (policy_id, tenant_id) refuses a foreign policy;
	// it also waits on a concurrent delete of the policy, then fails.
	_, err = tx.Exec(ctx,
		`UPDATE resources SET policy_id = $3, updated_at = NOW() WHERE tenant_id = $1 AND id = $2`,
		workspaceID, resourceID, policyID)
	if isForeignKeyViolation(err) {
		return uuid.Nil, ErrPolicyNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("set resource policy: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit set resource policy: %w", err)
	}
	return previous, nil
}

// DeleteProfile deletes a device profile. If resource policies use it, the
// caller must confirm (D-4); the profile is then removed from them in the
// same transaction. The result names those policies and the ones left with no
// profiles (they now allow any device).
func (s *Store) DeleteProfile(ctx context.Context, workspaceID, profileID uuid.UUID, confirmInUse bool) (*ProfileDeleteResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin delete device profile: %w", err)
	}
	defer tx.Rollback(ctx)

	var locked uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id FROM device_profiles WHERE workspace_id = $1 AND id = $2 FOR UPDATE`,
		workspaceID, profileID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock device profile: %w", err)
	}
	policies, err := policiesUsingProfile(ctx, tx, workspaceID, profileID)
	if err != nil {
		return nil, err
	}
	if len(policies) > 0 && !confirmInUse {
		return nil, newProfileInUseError(policies)
	}
	result := &ProfileDeleteResult{RemovedFrom: policies, NowAnyDevice: []PolicyRef{}}
	for _, p := range policies {
		var others int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM resource_policy_profiles WHERE policy_id = $1 AND profile_id <> $2`,
			p.ID, profileID).Scan(&others); err != nil {
			return nil, fmt.Errorf("count policy profiles: %w", err)
		}
		if others == 0 {
			result.NowAnyDevice = append(result.NowAnyDevice, p)
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM device_profiles WHERE workspace_id = $1 AND id = $2`, workspaceID, profileID); err != nil {
		return nil, fmt.Errorf("delete device profile: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit delete device profile: %w", err)
	}
	return result, nil
}

func dedupeSorted(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
