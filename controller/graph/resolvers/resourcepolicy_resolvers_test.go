package resolvers

// resourcepolicy_resolvers_test.go -- PENDING-16 Phase 3.
//
// Direct resolver tests for business behaviour, against a real PostgreSQL
// instance. Set PKI_TEST_DATABASE_URL to an admin DSN; the fixture creates and
// drops its own database, so it needs no pre-seeded fixture data.
//
// Presentation concerns (admin guard, error wording) live in
// resourcepolicy_guard_test.go and need no database.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type resourcePolicyFixture struct {
	mr    *mutationResolver
	qr    *queryResolver
	rp    *resourcePolicyResolver
	rr    *resourceResolver
	pool  *pgxpool.Pool
	fires *atomic.Int32

	ctx         context.Context
	workspaceID uuid.UUID
	networkID   uuid.UUID
	shieldID    uuid.UUID

	// otherWorkspaceID exists to prove cross-tenant operations are refused.
	otherWorkspaceID uuid.UUID
}

func newResourcePolicyFixture(t *testing.T) *resourcePolicyFixture {
	t.Helper()

	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	dbName := fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatalf("create test database: %v", err)
	}

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		adminPool.Close()
		t.Fatalf("parse DSN: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		adminPool.Close()
		t.Fatalf("connect test database: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		adminPool.Close()
	})

	applyResourcePolicyTestMigrations(t, ctx, pool)

	workspaceID := seedResourcePolicyWorkspace(t, ctx, pool, "rp-main")
	otherWorkspaceID := seedResourcePolicyWorkspace(t, ctx, pool, "rp-other")
	networkID, shieldID := seedResourcePolicyNetwork(t, ctx, pool, workspaceID, "main")

	fires := new(atomic.Int32)
	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	notifier.RegisterPushHook(func(string) { fires.Add(1) })

	r := &Resolver{
		Pool:           pool,
		ResourceCfg:    resource.Config{DB: pool},
		PolicyStore:    policy.NewStore(pool),
		PolicyNotifier: notifier,
		PostureStore:   posture.NewStore(pool),
	}

	tctx := tenant.Set(ctx, tenant.TenantContext{
		TenantID: workspaceID.String(),
		UserID:   uuid.NewString(),
		Role:     "admin",
		Email:    "resource-policy-test@example.com",
	})

	return &resourcePolicyFixture{
		mr:               &mutationResolver{r},
		qr:               &queryResolver{r},
		rp:               &resourcePolicyResolver{r},
		rr:               &resourceResolver{r},
		pool:             pool,
		fires:            fires,
		ctx:              tctx,
		workspaceID:      workspaceID,
		networkID:        networkID,
		shieldID:         shieldID,
		otherWorkspaceID: otherWorkspaceID,
	}
}

func applyResourcePolicyTestMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)
	for _, filename := range files {
		contents, err := os.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("execute %s: %v", filepath.Base(filename), err)
		}
	}
}

func seedResourcePolicyWorkspace(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	suffix string,
) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	slug := fmt.Sprintf("%s-%d", suffix, time.Now().UnixNano())
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func seedResourcePolicyNetwork(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID uuid.UUID,
	suffix string,
) (networkID, shieldID uuid.UUID) {
	t.Helper()

	if err := pool.QueryRow(
		ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, $2, 'other')
		 RETURNING id`,
		workspaceID,
		"network-"+suffix,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote network: %v", err)
	}

	var connectorID uuid.UUID
	if err := pool.QueryRow(
		ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		workspaceID,
		networkID,
		"connector-"+suffix,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	if err := pool.QueryRow(
		ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		workspaceID,
		networkID,
		connectorID,
		"shield-"+suffix,
	).Scan(&shieldID); err != nil {
		t.Fatalf("insert shield: %v", err)
	}

	return networkID, shieldID
}

// seedResource inserts a resource in the fixture's workspace.
func (f *resourcePolicyFixture) seedResource(t *testing.T, name, host string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO resources (
		     tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to
		 )
		 VALUES ($1, $2, $3, $4, $5, 'tcp', 443, 443)
		 RETURNING id`,
		f.workspaceID,
		f.networkID,
		f.shieldID,
		name,
		host,
	).Scan(&id); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	return id
}

// ------------------------------------------------------------------ queries --

func TestResourcePolicyQueriesAndCRUDResolvers(t *testing.T) {
	f := newResourcePolicyFixture(t)

	// Empty workspace lists nothing rather than erroring.
	policies, err := f.qr.ResourcePolicies(f.ctx)
	if err != nil {
		t.Fatalf("ResourcePolicies on empty workspace: %v", err)
	}
	if len(policies) != 0 {
		t.Fatalf("policies = %#v, want none", policies)
	}

	created, err := f.mr.CreateResourcePolicy(f.ctx, "Engineering")
	if err != nil {
		t.Fatalf("CreateResourcePolicy: %v", err)
	}
	if created.Name != "Engineering" {
		t.Fatalf("created.Name = %q", created.Name)
	}
	if created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("timestamps not exposed: %+v", created)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("create fired notify %d times, want 1", got)
	}

	// Duplicate name is a user error and must not notify again.
	if _, err := f.mr.CreateResourcePolicy(f.ctx, "Engineering"); err == nil ||
		!strings.Contains(err.Error(), "createResourcePolicy: a resource policy with that name already exists") {
		t.Fatalf("duplicate create error = %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("rejected duplicate create fired notify, total %d, want 1", got)
	}

	fetched, err := f.qr.ResourcePolicy(f.ctx, created.ID)
	if err != nil {
		t.Fatalf("ResourcePolicy: %v", err)
	}
	if fetched == nil || fetched.ID != created.ID {
		t.Fatalf("fetched = %+v, want %s", fetched, created.ID)
	}

	// A well-formed but unknown id is a user error, not an internal one.
	if _, err := f.qr.ResourcePolicy(f.ctx, uuid.NewString()); err == nil ||
		!strings.Contains(err.Error(), "resourcePolicy: resource policy not found") {
		t.Fatalf("unknown policy query error = %v", err)
	}

	updated, err := f.mr.UpdateResourcePolicy(f.ctx, created.ID, "Engineering Prod")
	if err != nil {
		t.Fatalf("UpdateResourcePolicy: %v", err)
	}
	if updated.Name != "Engineering Prod" {
		t.Fatalf("updated.Name = %q", updated.Name)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("notify count after update = %d, want 2", got)
	}

	if _, err := f.mr.UpdateResourcePolicy(f.ctx, uuid.NewString(), "Nope"); err == nil ||
		!strings.Contains(err.Error(), "updateResourcePolicy: resource policy not found") {
		t.Fatalf("unknown policy update error = %v", err)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("rejected update fired notify, total %d, want 2", got)
	}

	policies, err = f.qr.ResourcePolicies(f.ctx)
	if err != nil {
		t.Fatalf("ResourcePolicies: %v", err)
	}
	if len(policies) != 1 || policies[0].ID != created.ID {
		t.Fatalf("policies = %#v, want just %s", policies, created.ID)
	}

	ok, err := f.mr.DeleteResourcePolicy(f.ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("DeleteResourcePolicy = %v, %v", ok, err)
	}
	if got := f.fires.Load(); got != 3 {
		t.Fatalf("notify count after delete = %d, want 3", got)
	}

	if _, err := f.mr.DeleteResourcePolicy(f.ctx, created.ID); err == nil ||
		!strings.Contains(err.Error(), "deleteResourcePolicy: resource policy not found") {
		t.Fatalf("re-delete error = %v", err)
	}
	if got := f.fires.Load(); got != 3 {
		t.Fatalf("rejected delete fired notify, total %d, want 3", got)
	}
}

// -------------------------------------------------------------- assignment --

func TestResourcePolicyAssignmentResolvers(t *testing.T) {
	f := newResourcePolicyFixture(t)

	policy, err := f.mr.CreateResourcePolicy(f.ctx, "Assigned")
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	other, err := f.mr.CreateResourcePolicy(f.ctx, "Other")
	if err != nil {
		t.Fatalf("create other policy: %v", err)
	}
	resourceID := f.seedResource(t, "db", "10.10.0.5")
	f.fires.Store(0)

	// Resource.resourcePolicy is null before assignment.
	res := &graph.Resource{ID: resourceID.String()}
	attached, err := f.rr.ResourcePolicy(f.ctx, res)
	if err != nil {
		t.Fatalf("Resource.resourcePolicy before assign: %v", err)
	}
	if attached != nil {
		t.Fatalf("unassigned resource policy = %+v, want nil", attached)
	}

	assigned, err := f.mr.AssignResourcePolicy(f.ctx, resourceID.String(), policy.ID)
	if err != nil {
		t.Fatalf("AssignResourcePolicy: %v", err)
	}
	if assigned.ID != resourceID.String() {
		t.Fatalf("assigned resource = %+v, want %s", assigned, resourceID)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("assign fired notify %d times, want 1", got)
	}

	attached, err = f.rr.ResourcePolicy(f.ctx, res)
	if err != nil {
		t.Fatalf("Resource.resourcePolicy after assign: %v", err)
	}
	if attached == nil || attached.ID != policy.ID {
		t.Fatalf("attached policy = %+v, want %s", attached, policy.ID)
	}

	// ResourcePolicy.resources reverse relationship.
	resources, err := f.rp.Resources(f.ctx, policy)
	if err != nil {
		t.Fatalf("ResourcePolicy.resources: %v", err)
	}
	if len(resources) != 1 || resources[0].ID != resourceID.String() {
		t.Fatalf("policy resources = %#v, want [%s]", resources, resourceID)
	}

	// A second, different policy is refused and nothing changes.
	if _, err := f.mr.AssignResourcePolicy(f.ctx, resourceID.String(), other.ID); err == nil ||
		!strings.Contains(err.Error(), "assignResourcePolicy: resource already has a different resource policy") {
		t.Fatalf("second assign error = %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("rejected second assign fired notify, total %d, want 1", got)
	}
	attached, err = f.rr.ResourcePolicy(f.ctx, res)
	if err != nil {
		t.Fatalf("Resource.resourcePolicy after rejection: %v", err)
	}
	if attached == nil || attached.ID != policy.ID {
		t.Fatalf("policy changed despite rejection: %+v", attached)
	}

	// Deleting an assigned policy is refused, so access cannot vanish silently.
	if _, err := f.mr.DeleteResourcePolicy(f.ctx, policy.ID); err == nil ||
		!strings.Contains(err.Error(), "deleteResourcePolicy: resource policy is still assigned to a resource") {
		t.Fatalf("delete assigned policy error = %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("rejected delete fired notify, total %d, want 1", got)
	}

	// Unknown ids are user errors.
	if _, err := f.mr.AssignResourcePolicy(f.ctx, uuid.NewString(), policy.ID); err == nil ||
		!strings.Contains(err.Error(), "assignResourcePolicy: resource or resource policy not found") {
		t.Fatalf("unknown resource assign error = %v", err)
	}
	if _, err := f.mr.AssignResourcePolicy(f.ctx, resourceID.String(), uuid.NewString()); err == nil ||
		!strings.Contains(err.Error(), "assignResourcePolicy: resource or resource policy not found") {
		t.Fatalf("unknown policy assign error = %v", err)
	}

	unassigned, err := f.mr.UnassignResourcePolicy(f.ctx, resourceID.String())
	if err != nil {
		t.Fatalf("UnassignResourcePolicy: %v", err)
	}
	if unassigned.ID != resourceID.String() {
		t.Fatalf("unassigned resource = %+v", unassigned)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("notify count after unassign = %d, want 2", got)
	}

	attached, err = f.rr.ResourcePolicy(f.ctx, res)
	if err != nil {
		t.Fatalf("Resource.resourcePolicy after unassign: %v", err)
	}
	if attached != nil {
		t.Fatalf("policy still attached after unassign: %+v", attached)
	}

	if _, err := f.mr.UnassignResourcePolicy(f.ctx, uuid.NewString()); err == nil ||
		!strings.Contains(err.Error(), "unassignResourcePolicy: resource not found") {
		t.Fatalf("unknown resource unassign error = %v", err)
	}
}

// ---------------------------------------------------- profile relationship --

func TestResourcePolicyProfileResolvers(t *testing.T) {
	f := newResourcePolicyFixture(t)

	policy, err := f.mr.CreateResourcePolicy(f.ctx, "Profiles")
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	f.fires.Store(0)

	// Empty profile selection is valid and means Any Device.
	profiles, err := f.rp.DeviceProfiles(f.ctx, policy)
	if err != nil {
		t.Fatalf("DeviceProfiles on empty policy: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profiles = %#v, want none (Any Device)", profiles)
	}

	linux, err := f.mr.CreateDeviceProfile(f.ctx, "Corporate Linux", nil)
	if err != nil {
		t.Fatalf("create linux profile: %v", err)
	}
	windows, err := f.mr.CreateDeviceProfile(f.ctx, "Corporate Windows", nil)
	if err != nil {
		t.Fatalf("create windows profile: %v", err)
	}
	f.fires.Store(0)

	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, policy.ID, linux.ID); err != nil {
		t.Fatalf("AddProfileToResourcePolicy linux: %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("add profile fired notify %d times, want 1", got)
	}

	// Multiple profiles on one policy are allowed (OR semantics later).
	returned, err := f.mr.AddProfileToResourcePolicy(f.ctx, policy.ID, windows.ID)
	if err != nil {
		t.Fatalf("AddProfileToResourcePolicy windows: %v", err)
	}
	if returned.ID != policy.ID {
		t.Fatalf("returned policy = %+v, want %s", returned, policy.ID)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("notify count after second add = %d, want 2", got)
	}

	profiles, err = f.rp.DeviceProfiles(f.ctx, policy)
	if err != nil {
		t.Fatalf("DeviceProfiles: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles = %#v, want 2", profiles)
	}
	if profiles[0].ID != linux.ID || profiles[1].ID != windows.ID {
		t.Fatalf("profiles not name-ordered: %q, %q", profiles[0].Name, profiles[1].Name)
	}

	// Duplicate attachment refused, no extra notify.
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, policy.ID, linux.ID); err == nil ||
		!strings.Contains(err.Error(), "addProfileToResourcePolicy: device profile is already attached") {
		t.Fatalf("duplicate profile binding error = %v", err)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("rejected duplicate add fired notify, total %d, want 2", got)
	}

	// Unknown ids are user errors.
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, uuid.NewString(), linux.ID); err == nil ||
		!strings.Contains(err.Error(), "addProfileToResourcePolicy: resource policy or device profile not found") {
		t.Fatalf("unknown policy add error = %v", err)
	}
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, policy.ID, uuid.NewString()); err == nil ||
		!strings.Contains(err.Error(), "addProfileToResourcePolicy: resource policy or device profile not found") {
		t.Fatalf("unknown profile add error = %v", err)
	}

	if _, err := f.mr.RemoveProfileFromResourcePolicy(f.ctx, policy.ID, windows.ID); err != nil {
		t.Fatalf("RemoveProfileFromResourcePolicy: %v", err)
	}
	if got := f.fires.Load(); got != 3 {
		t.Fatalf("notify count after remove = %d, want 3", got)
	}

	if _, err := f.mr.RemoveProfileFromResourcePolicy(f.ctx, policy.ID, windows.ID); err == nil ||
		!strings.Contains(err.Error(), "removeProfileFromResourcePolicy: profile attachment not found") {
		t.Fatalf("re-remove error = %v", err)
	}
	if got := f.fires.Load(); got != 3 {
		t.Fatalf("rejected remove fired notify, total %d, want 3", got)
	}

	// Down to zero profiles again — back to Any Device, still valid.
	if _, err := f.mr.RemoveProfileFromResourcePolicy(f.ctx, policy.ID, linux.ID); err != nil {
		t.Fatalf("remove last profile: %v", err)
	}
	profiles, err = f.rp.DeviceProfiles(f.ctx, policy)
	if err != nil {
		t.Fatalf("DeviceProfiles after removal: %v", err)
	}
	if len(profiles) != 0 {
		t.Fatalf("profiles after removal = %#v, want none", profiles)
	}
}

// ------------------------------------------------- cross-workspace refusal --

func TestResourcePolicyResolversRejectCrossWorkspace(t *testing.T) {
	f := newResourcePolicyFixture(t)

	store := posture.NewStore(f.pool)

	// Objects owned by a different workspace than the caller's tenant.
	foreignPolicy, err := store.CreateResourcePolicy(f.ctx, f.otherWorkspaceID, "Foreign Policy")
	if err != nil {
		t.Fatalf("create foreign policy: %v", err)
	}
	foreignProfile, err := store.CreateProfile(f.ctx, f.otherWorkspaceID, "Foreign Profile", true)
	if err != nil {
		t.Fatalf("create foreign profile: %v", err)
	}

	localPolicy, err := f.mr.CreateResourcePolicy(f.ctx, "Local Policy")
	if err != nil {
		t.Fatalf("create local policy: %v", err)
	}
	localResource := f.seedResource(t, "local", "10.10.0.9")
	f.fires.Store(0)

	// Reading another workspace's policy looks like "not found".
	if _, err := f.qr.ResourcePolicy(f.ctx, foreignPolicy.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "resourcePolicy: resource policy not found") {
		t.Fatalf("cross-workspace query error = %v", err)
	}
	if _, err := f.mr.UpdateResourcePolicy(f.ctx, foreignPolicy.ID.String(), "Hijack"); err == nil ||
		!strings.Contains(err.Error(), "updateResourcePolicy: resource policy not found") {
		t.Fatalf("cross-workspace update error = %v", err)
	}
	if _, err := f.mr.DeleteResourcePolicy(f.ctx, foreignPolicy.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "deleteResourcePolicy: resource policy not found") {
		t.Fatalf("cross-workspace delete error = %v", err)
	}

	// Assigning another workspace's policy to a local resource is refused.
	if _, err := f.mr.AssignResourcePolicy(f.ctx, localResource.String(), foreignPolicy.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "assignResourcePolicy: resource or resource policy not found") {
		t.Fatalf("cross-workspace assign error = %v", err)
	}

	// Attaching another workspace's profile to a local policy is refused.
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, localPolicy.ID, foreignProfile.ID.String()); err == nil ||
		!strings.Contains(err.Error(), "addProfileToResourcePolicy: resource policy or device profile not found") {
		t.Fatalf("cross-workspace profile attach error = %v", err)
	}

	// The listing only ever shows the caller's own workspace.
	policies, err := f.qr.ResourcePolicies(f.ctx)
	if err != nil {
		t.Fatalf("ResourcePolicies: %v", err)
	}
	if len(policies) != 1 || policies[0].ID != localPolicy.ID {
		t.Fatalf("policies = %#v, want only the local policy", policies)
	}

	// Not one rejected cross-workspace mutation may notify the policy plane.
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected cross-workspace mutations fired notify %d times, want 0", got)
	}
}

// ------------------------------------------------------- legacy coexistence --

// The legacy bind/unbind surface must keep working untouched alongside the new
// Resource Policy API.
func TestLegacyProfileBindingStillWorksAlongsideResourcePolicy(t *testing.T) {
	f := newResourcePolicyFixture(t)

	profile, err := f.mr.CreateDeviceProfile(f.ctx, "Legacy Profile", nil)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	resourceID := f.seedResource(t, "legacy", "10.10.0.11")

	// Legacy direct binding.
	if _, err := f.mr.BindResourceToProfile(f.ctx, profile.ID, resourceID.String()); err != nil {
		t.Fatalf("legacy BindResourceToProfile: %v", err)
	}

	// New model, same resource and profile, independently.
	policy, err := f.mr.CreateResourcePolicy(f.ctx, "Coexistence")
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, policy.ID, profile.ID); err != nil {
		t.Fatalf("AddProfileToResourcePolicy: %v", err)
	}
	if _, err := f.mr.AssignResourcePolicy(f.ctx, resourceID.String(), policy.ID); err != nil {
		t.Fatalf("AssignResourcePolicy: %v", err)
	}

	// The legacy row is still there and still readable through the legacy path.
	bindings, err := posture.NewStore(f.pool).ListResourceBindings(f.ctx, f.workspaceID, uuid.MustParse(profile.ID))
	if err != nil {
		t.Fatalf("legacy ListResourceBindings: %v", err)
	}
	if len(bindings) != 1 || bindings[0].ResourceID != resourceID {
		t.Fatalf("legacy bindings = %#v, want the seeded binding", bindings)
	}

	// And legacy unbind still works.
	if _, err := f.mr.UnbindResourceFromProfile(f.ctx, profile.ID, resourceID.String()); err != nil {
		t.Fatalf("legacy UnbindResourceFromProfile: %v", err)
	}

	// Removing the legacy binding leaves the new-model assignment intact.
	attached, err := f.rr.ResourcePolicy(f.ctx, &graph.Resource{ID: resourceID.String()})
	if err != nil {
		t.Fatalf("Resource.resourcePolicy: %v", err)
	}
	if attached == nil || attached.ID != policy.ID {
		t.Fatalf("new-model assignment disturbed by legacy unbind: %+v", attached)
	}

	// Sanity: the two models really are separate tables.
	var legacyCount, newCount int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM resource_profile_bindings WHERE workspace_id = $1`,
		f.workspaceID,
	).Scan(&legacyCount); err != nil {
		t.Fatalf("count legacy bindings: %v", err)
	}
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM resource_policy_profile_bindings WHERE workspace_id = $1`,
		f.workspaceID,
	).Scan(&newCount); err != nil {
		t.Fatalf("count new bindings: %v", err)
	}
	if legacyCount != 0 || newCount != 1 {
		t.Fatalf("legacy=%d new=%d, want legacy=0 new=1", legacyCount, newCount)
	}

	// Legacy error semantics are unchanged too: unbinding twice is not-found.
	if _, err := f.mr.UnbindResourceFromProfile(f.ctx, profile.ID, resourceID.String()); err == nil ||
		!strings.Contains(err.Error(), "unbindResourceFromProfile: binding not found") {
		t.Fatalf("legacy re-unbind error = %v, want a not-found user error", err)
	}
}
