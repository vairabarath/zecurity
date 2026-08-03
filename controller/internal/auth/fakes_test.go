package auth

import (
	"context"
	"net/url"
	"sort"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/idp"
)

// fakeConnStore is a connectionStore for unit tests — no database.
type fakeConnStore struct {
	byID        map[string]*idp.Connection
	platform    map[string]*idp.Connection
	workspaces  map[string]string // slug -> tenantID
	connections []idp.Connection  // used by ListForWorkspace
}

func (f *fakeConnStore) GetByID(_ context.Context, id string) (*idp.Connection, error) {
	if c, ok := f.byID[id]; ok {
		return c, nil
	}
	return nil, idp.ErrConnectionNotFound
}

func (f *fakeConnStore) GetPlatformByProvider(_ context.Context, provider string) (*idp.Connection, error) {
	if c, ok := f.platform[provider]; ok {
		return c, nil
	}
	return nil, idp.ErrConnectionNotFound
}

func (f *fakeConnStore) WorkspaceIDBySlug(_ context.Context, slug string) (string, error) {
	if id, ok := f.workspaces[slug]; ok {
		return id, nil
	}
	return "", idp.ErrWorkspaceNotFound
}

// ListForWorkspace returns platform connections (TenantID nil) plus any scoped to
// tenantID, mirroring idp.Store's `ORDER BY (tenant_id IS NOT NULL), display_name`
// so the fake is a faithful stand-in for ordering assertions.
func (f *fakeConnStore) ListForWorkspace(_ context.Context, tenantID string) ([]idp.Connection, error) {
	var out []idp.Connection
	for _, c := range f.connections {
		if c.TenantID == nil || *c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := out[i].TenantID != nil, out[j].TenantID != nil
		if pi != pj {
			return !pi // platform (TenantID nil) first
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	return out, nil
}

// googleConnStore returns a fake store holding one active managed Google
// connection, resolvable both by id ("google-conn") and by platform provider.
func googleConnStore() *fakeConnStore {
	g := &idp.Connection{ID: "google-conn", Provider: "google", Managed: true, Status: "active", Issuer: "https://accounts.google.com"}
	return &fakeConnStore{
		byID:     map[string]*idp.Connection{g.ID: g},
		platform: map[string]*idp.Connection{"google": g},
	}
}

// fakeProvider is a providers.IdentityProvider for tests — no network.
type fakeProvider struct {
	result *providers.AuthenticationContext
	err    error
}

func (f *fakeProvider) AuthURL(_ context.Context, p providers.AuthURLParams) (string, error) {
	return "https://fake-idp/authorize?state=" + url.QueryEscape(p.State), nil
}

func (f *fakeProvider) Authenticate(_ context.Context, _, _, _, _ string) (*providers.AuthenticationContext, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}
