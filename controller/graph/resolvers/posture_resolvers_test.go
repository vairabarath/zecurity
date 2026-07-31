package resolvers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

func TestPostureMutationRequiresAdmin(t *testing.T) {
	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: uuid.NewString(),
		UserID:   uuid.NewString(),
		Role:     "member",
	})

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: &Resolver{},
		Directives: graph.DirectiveRoot{
			HasRole: HasRole,
		},
	}))
	body := []byte(`{"query":"mutation { bindResourceToProfile(profileId: \"00000000-0000-0000-0000-000000000001\", resourceId: \"00000000-0000-0000-0000-000000000002\") { id } }"}`)
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, req)

	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, recorder.Body.String())
	}
	if len(response.Errors) != 1 || !strings.Contains(response.Errors[0].Message, "forbidden") {
		t.Fatalf("GraphQL errors = %#v, want forbidden", response.Errors)
	}
}

func TestCreateDeviceProfileBlankNameReturnsUserError(t *testing.T) {
	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: uuid.NewString(),
		UserID:   uuid.NewString(),
		Role:     "admin",
	})

	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers: &Resolver{PostureStore: posture.NewStore(nil)},
		Directives: graph.DirectiveRoot{
			HasRole: HasRole,
		},
	}))
	srv.SetErrorPresenter(ErrorPresenter)
	body := []byte(`{"query":"mutation { createDeviceProfile(name: \"   \" ) { id } }"}`)
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, req)

	var response struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GraphQL response: %v; body=%s", err, recorder.Body.String())
	}
	if len(response.Errors) != 1 || response.Errors[0].Message != "createDeviceProfile: name is required" {
		t.Fatalf("GraphQL errors = %#v, want safe blank-name error", response.Errors)
	}
}

func TestPostureBindingResolversNotifyAndLoadBoundResources(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"posture-resolver-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	resourceID := f.seedResource(t, "10.0.1.11", "unprotected")

	gotProfile, err := f.mr.BindResourceToProfile(f.ctx, profile.ID.String(), resourceID)
	if err != nil {
		t.Fatalf("BindResourceToProfile: %v", err)
	}
	if gotProfile.ID != profile.ID.String() {
		t.Fatalf("bound profile ID = %q, want %q", gotProfile.ID, profile.ID)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("bind push hook fired %d times, want 1", got)
	}

	dpr := &deviceProfileResolver{f.mr.Resolver}
	resources, err := dpr.BoundResources(f.ctx, gotProfile)
	if err != nil {
		t.Fatalf("BoundResources: %v", err)
	}
	if len(resources) != 1 || resources[0].ID != resourceID {
		t.Fatalf("bound resources = %#v, want resource %s", resources, resourceID)
	}

	if _, err := f.mr.BindResourceToProfile(f.ctx, profile.ID.String(), resourceID); err == nil ||
		!strings.Contains(err.Error(), "duplicate resource binding") {
		t.Fatalf("duplicate bind error = %v", err)
	} else if presented := ErrorPresenter(f.ctx, err); presented.Message != "bindResourceToProfile: duplicate resource binding" {
		t.Fatalf("presented duplicate error = %q", presented.Message)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("failed duplicate bind fired push hook; count = %d, want 1", got)
	}

	if _, err := f.mr.UnbindResourceFromProfile(f.ctx, profile.ID.String(), resourceID); err != nil {
		t.Fatalf("UnbindResourceFromProfile: %v", err)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("unbind push hook fired total %d times, want 2", got)
	}

	resources, err = dpr.BoundResources(f.ctx, gotProfile)
	if err != nil {
		t.Fatalf("BoundResources after unbind: %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("bound resources after unbind = %#v, want empty", resources)
	}

	if _, err := f.mr.UnbindResourceFromProfile(f.ctx, profile.ID.String(), resourceID); err == nil ||
		!strings.Contains(err.Error(), "binding not found") {
		t.Fatalf("missing unbind error = %v", err)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("failed unbind fired push hook; count = %d, want 2", got)
	}
}

func TestBindResourceToProfileRejectsEmptyEnforcedProfile(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)
	workspaceID := uuid.MustParse(f.tenantID)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		workspaceID,
		"posture-empty-enforce-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(context.Background(), workspaceID, profile.ID)
	})

	if _, err := f.pool.Exec(
		f.ctx,
		`UPDATE device_profiles SET mode = 'enforce' WHERE id = $1`,
		profile.ID,
	); err != nil {
		t.Fatalf("force invalid enforce profile: %v", err)
	}

	resourceID := f.seedResource(t, "10.0.1.12", "unprotected")
	_, err = f.mr.BindResourceToProfile(f.ctx, profile.ID.String(), resourceID)
	if err == nil || !strings.Contains(err.Error(), "requires at least one requirement") {
		t.Fatalf("empty enforce bind error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected empty enforce bind fired push hook %d times, want 0", got)
	}
}

func TestBindResourceToProfileRejectsCrossWorkspaceProfile(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	var otherWorkspaceID uuid.UUID
	slug := "posture-cross-workspace-" + uuid.NewString()
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, otherWorkspaceID)
	})

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		otherWorkspaceID,
		"cross-workspace-profile",
	)
	if err != nil {
		t.Fatalf("create other-workspace profile: %v", err)
	}

	resourceID := f.seedResource(t, "10.0.1.13", "unprotected")
	_, err = f.mr.BindResourceToProfile(f.ctx, profile.ID.String(), resourceID)
	if err == nil || !strings.Contains(err.Error(), "profile or resource not found") {
		t.Fatalf("cross-workspace bind error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected cross-workspace bind fired push hook %d times, want 0", got)
	}
}
