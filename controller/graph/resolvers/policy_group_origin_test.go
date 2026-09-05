package resolvers

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type policyResolverFixture struct {
	mr       *mutationResolver
	qr       *queryResolver
	pool     *pgxpool.Pool
	tenantID string
	ctx      context.Context
}

func newPolicyResolverFixture(t *testing.T) *policyResolverFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = os.Getenv("RESOURCE_TEST_DATABASE_URL")
	}
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL / RESOURCE_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := "resolvers_policy_test_" + strconv.Itoa(os.Getpid()) + "_" + uuid.NewString()[:8]

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer adminPool.Close()

	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cerr := pgxpool.New(context.Background(), adminDSN)
		if cerr == nil {
			_, _ = cleanupPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
			cleanupPool.Close()
		}
	})

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	testDSN := parsed.String()

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Apply migrations
	applyPolicyTestMigrations(t, ctx, pool)

	// Create workspace
	wsID := uuid.New()
	slug := "test-ws-" + wsID.String()[:8]
	_, err = pool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Policy Test Workspace', 'ACTIVE', 'td-policy-test')`,
		wsID, slug,
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	tctx := tenant.Set(ctx, tenant.TenantContext{
		TenantID: wsID.String(),
		Role:     "ADMIN",
	})

	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	notifier.RegisterPushHook(func(string) {})

	policyStore := policy.NewStore(pool)

	r := &Resolver{
		Pool:           pool,
		PolicyStore:    policyStore,
		PolicyNotifier: notifier,
	}

	return &policyResolverFixture{
		mr:       &mutationResolver{r},
		qr:       &queryResolver{r},
		pool:     pool,
		tenantID: wsID.String(),
		ctx:      tctx,
	}
}

func applyPolicyTestMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	dir := "../../migrations"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read migration %s: %v", e.Name(), rerr)
		}
		if _, xerr := pool.Exec(ctx, string(content)); xerr != nil {
			t.Fatalf("apply migration %s: %v", e.Name(), xerr)
		}
	}
}

func createTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, email string) string {
	t.Helper()
	var uid string
	err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, role, provider, status)
		 VALUES ($1, $2, 'MEMBER', 'manual', 'active')
		 RETURNING id`,
		workspaceID, email,
	).Scan(&uid)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	return uid
}

func createTestResource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, name string) string {
	t.Helper()
	// Insert remote network
	var netID string
	err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location, status)
		 VALUES ($1, 'Net-' || $2, 'us-east', 'ACTIVE')
		 RETURNING id`,
		workspaceID, name,
	).Scan(&netID)
	if err != nil {
		t.Fatalf("create remote network: %v", err)
	}

	// Insert shield
	var shieldID string
	err = pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, name, lan_ip, status, auth_token_hash)
		 VALUES ($1, $2, 'Shield-' || $3, '10.0.0.1', 'ACTIVE', 'dummy')
		 RETURNING id`,
		workspaceID, netID, name,
	).Scan(&shieldID)
	if err != nil {
		t.Fatalf("create shield: %v", err)
	}

	var resID string
	err = pool.QueryRow(ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status)
		 VALUES ($1, $2, $3, $4, 'internal.app', 'tcp', 8080, 8080, 'active')
		 RETURNING id`,
		workspaceID, netID, shieldID, name,
	).Scan(&resID)
	if err != nil {
		t.Fatalf("create resource: %v", err)
	}
	return resID
}

func createSCIMGroupInDB(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, name, externalID string) string {
	t.Helper()
	var gid string
	err := pool.QueryRow(ctx,
		`INSERT INTO groups (workspace_id, origin, name, external_id)
		 VALUES ($1, 'scim', $2, $3)
		 RETURNING id`,
		workspaceID, name, externalID,
	).Scan(&gid)
	if err != nil {
		t.Fatalf("create scim group in db: %v", err)
	}
	return gid
}

// ── SCIM Group Mutation Rejection Tests ──────────────────────────────────────

func TestGroupOrigin_UpdateGroup_RejectsSCIMGroup(t *testing.T) {
	f := newPolicyResolverFixture(t)
	scimGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, f.tenantID, "Engineering SCIM", "ext-eng")

	newName := "Renamed Eng"
	_, err := f.mr.UpdateGroup(f.ctx, scimGroupID, &newName, nil)
	if err == nil {
		t.Fatal("expected UpdateGroup to fail on SCIM group, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot edit a directory-managed (SCIM) group") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGroupOrigin_DeleteGroup_RejectsSCIMGroup(t *testing.T) {
	f := newPolicyResolverFixture(t)
	scimGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, f.tenantID, "DevOps SCIM", "ext-devops")

	ok, err := f.mr.DeleteGroup(f.ctx, scimGroupID)
	if err == nil {
		t.Fatal("expected DeleteGroup to fail on SCIM group, got nil error")
	}
	if ok {
		t.Fatal("expected DeleteGroup to return false on SCIM group")
	}
	if !strings.Contains(err.Error(), "cannot delete a directory-managed (SCIM) group") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGroupOrigin_AddGroupMember_RejectsSCIMGroup(t *testing.T) {
	f := newPolicyResolverFixture(t)
	scimGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, f.tenantID, "QA SCIM", "ext-qa")
	userID := createTestUser(t, f.ctx, f.pool, f.tenantID, "user1@example.com")

	_, err := f.mr.AddGroupMember(f.ctx, scimGroupID, userID)
	if err == nil {
		t.Fatal("expected AddGroupMember to fail on SCIM group, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot add members to a directory-managed (SCIM) group") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestGroupOrigin_RemoveGroupMember_RejectsSCIMGroup(t *testing.T) {
	f := newPolicyResolverFixture(t)
	scimGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, f.tenantID, "Security SCIM", "ext-sec")
	userID := createTestUser(t, f.ctx, f.pool, f.tenantID, "user2@example.com")

	// Directly insert member into DB (as SCIM sync would do)
	_, err := f.pool.Exec(f.ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`,
		scimGroupID, userID,
	)
	if err != nil {
		t.Fatalf("insert member: %v", err)
	}

	_, err = f.mr.RemoveGroupMember(f.ctx, scimGroupID, userID)
	if err == nil {
		t.Fatal("expected RemoveGroupMember to fail on SCIM group, got nil error")
	}
	if !strings.Contains(err.Error(), "cannot remove members from a directory-managed (SCIM) group") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ── SCIM Group Authorization Assignment Tests (MUST SUCCEED) ─────────────────

func TestGroupOrigin_AssignAndUnassignResource_AllowsSCIMGroup(t *testing.T) {
	f := newPolicyResolverFixture(t)
	scimGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, f.tenantID, "Infra SCIM", "ext-infra")
	resourceID := createTestResource(t, f.ctx, f.pool, f.tenantID, "Prod-DB")

	// 1. Assign Resource to SCIM Group -> MUST SUCCEED
	res, err := f.mr.AssignResourceToGroup(f.ctx, resourceID, scimGroupID)
	if err != nil {
		t.Fatalf("AssignResourceToGroup on SCIM group failed: %v", err)
	}
	if res == nil || res.ID != resourceID {
		t.Fatalf("unexpected resource: %+v", res)
	}

	// Verify group is associated with resource
	assignedGroupIDs, err := f.mr.PolicyStore.ListGroupsForResource(f.ctx, resourceID)
	if err != nil {
		t.Fatalf("ListGroupsForResource: %v", err)
	}
	found := false
	for _, gid := range assignedGroupIDs {
		if gid == scimGroupID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected scimGroupID %s in assigned groups, got %v", scimGroupID, assignedGroupIDs)
	}

	// 2. Unassign Resource from SCIM Group -> MUST SUCCEED
	resAfter, err := f.mr.UnassignResourceFromGroup(f.ctx, resourceID, scimGroupID)
	if err != nil {
		t.Fatalf("UnassignResourceFromGroup on SCIM group failed: %v", err)
	}
	if resAfter == nil || resAfter.ID != resourceID {
		t.Fatalf("unexpected resource after unassign: %+v", resAfter)
	}

	// Verify group is no longer associated
	assignedGroupIDsAfter, err := f.mr.PolicyStore.ListGroupsForResource(f.ctx, resourceID)
	if err != nil {
		t.Fatalf("ListGroupsForResource after unassign: %v", err)
	}
	for _, gid := range assignedGroupIDsAfter {
		if gid == scimGroupID {
			t.Fatalf("scimGroupID %s still assigned to resource after unassign", scimGroupID)
		}
	}
}

// ── Manual Group Operations Tests (MUST ALL SUCCEED) ─────────────────────────

func TestGroupOrigin_ManualGroup_AllOperationsSucceed(t *testing.T) {
	f := newPolicyResolverFixture(t)
	userID := createTestUser(t, f.ctx, f.pool, f.tenantID, "manual-user@example.com")
	resourceID := createTestResource(t, f.ctx, f.pool, f.tenantID, "Manual-App")

	// 1. Create manual group
	desc := "Initial Description"
	g, err := f.mr.CreateGroup(f.ctx, "Manual Group", &desc)
	if err != nil {
		t.Fatalf("CreateGroup manual failed: %v", err)
	}
	if g.Origin != "manual" {
		t.Fatalf("expected origin manual, got %s", g.Origin)
	}

	// 2. Update manual group
	updatedName := "Manual Group Updated"
	updatedDesc := "Updated Description"
	gUpd, err := f.mr.UpdateGroup(f.ctx, g.ID, &updatedName, &updatedDesc)
	if err != nil {
		t.Fatalf("UpdateGroup manual failed: %v", err)
	}
	if gUpd.Name != updatedName {
		t.Fatalf("expected name %s, got %s", updatedName, gUpd.Name)
	}

	// 3. Add member to manual group
	gMemAdd, err := f.mr.AddGroupMember(f.ctx, g.ID, userID)
	if err != nil {
		t.Fatalf("AddGroupMember manual failed: %v", err)
	}
	if len(gMemAdd.Members) != 1 || gMemAdd.Members[0].ID != userID {
		t.Fatalf("unexpected members: %+v", gMemAdd.Members)
	}

	// 4. Assign resource to manual group
	resAssigned, err := f.mr.AssignResourceToGroup(f.ctx, resourceID, g.ID)
	if err != nil {
		t.Fatalf("AssignResourceToGroup manual failed: %v", err)
	}
	if resAssigned == nil || resAssigned.ID != resourceID {
		t.Fatalf("unexpected resource assigned: %+v", resAssigned)
	}

	// 5. Unassign resource from manual group
	resUnassigned, err := f.mr.UnassignResourceFromGroup(f.ctx, resourceID, g.ID)
	if err != nil {
		t.Fatalf("UnassignResourceFromGroup manual failed: %v", err)
	}
	if resUnassigned == nil || resUnassigned.ID != resourceID {
		t.Fatalf("unexpected resource unassigned: %+v", resUnassigned)
	}

	// 6. Remove member from manual group
	gMemRm, err := f.mr.RemoveGroupMember(f.ctx, g.ID, userID)
	if err != nil {
		t.Fatalf("RemoveGroupMember manual failed: %v", err)
	}
	if len(gMemRm.Members) != 0 {
		t.Fatalf("expected 0 members after remove, got %d", len(gMemRm.Members))
	}

	// 7. Delete manual group
	ok, err := f.mr.DeleteGroup(f.ctx, g.ID)
	if err != nil {
		t.Fatalf("DeleteGroup manual failed: %v", err)
	}
	if !ok {
		t.Fatal("DeleteGroup manual returned false")
	}
}

// ── Cross-Workspace Isolation Test ───────────────────────────────────────────

func TestGroupOrigin_CrossWorkspace_RejectedAsNotFound(t *testing.T) {
	f := newPolicyResolverFixture(t)

	// Create a different workspace
	otherWS := uuid.New()
	_, err := f.pool.Exec(f.ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Other Workspace', 'ACTIVE', 'td-other')`,
		otherWS, "other-"+otherWS.String()[:8],
	)
	if err != nil {
		t.Fatalf("create other workspace: %v", err)
	}

	otherGroupID := createSCIMGroupInDB(t, f.ctx, f.pool, otherWS.String(), "Other SCIM", "ext-other")

	// UpdateGroup from fixture workspace against otherGroupID must fail as group not found
	name := "Hacked Name"
	_, err = f.mr.UpdateGroup(f.ctx, otherGroupID, &name, nil)
	if err == nil || !strings.Contains(err.Error(), "group not found") {
		t.Fatalf("expected 'group not found' for cross-workspace UpdateGroup, got: %v", err)
	}

	// DeleteGroup from fixture workspace against otherGroupID must fail as group not found
	_, err = f.mr.DeleteGroup(f.ctx, otherGroupID)
	if err == nil || !strings.Contains(err.Error(), "group not found") {
		t.Fatalf("expected 'group not found' for cross-workspace DeleteGroup, got: %v", err)
	}
}
