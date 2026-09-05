package auth

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/yourorg/ztna/controller/internal/idp"
)

// discoveryProvider is one selectable login option advertised to the client.
// ID is the identity_connections id — the client passes it back as connectionId
// to start login with this exact connection (uniform for Bootstrap + Enterprise).
type discoveryProvider struct {
	ID      string `json:"id"`
	Display string `json:"display"`
	Type    string `json:"type"` // protocol: "oidc" (saml later)
	Tier    string `json:"tier"` // "enterprise" | "bootstrap"
}

// discoveryResponse is the server-driven login configuration (ADR-024 §0). The
// client renders `providers` in order and styles "bootstrap" tiers as fallback.
type discoveryResponse struct {
	Workspace        string              `json:"workspace"`
	Providers        []discoveryProvider `json:"providers"`
	PlatformFallback bool                `json:"platformFallback"`
}

// DiscoveryHandler serves GET /workspaces/{slug}/auth — public, read-only. It
// never exposes client secrets (see auth invariants, doc.go #8).
func (s *serviceImpl) DiscoveryHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		slug := r.PathValue("slug")
		if slug == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		tenantID, err := s.idpStore.WorkspaceIDBySlug(ctx, slug)
		if err != nil {
			if errors.Is(err, idp.ErrWorkspaceNotFound) {
				http.Error(w, "workspace not found", http.StatusNotFound)
				return
			}
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		conns, err := s.idpStore.ListForWorkspace(ctx, tenantID)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		// Phase 7 platform login toggle (ADR-024 §5): when a workspace disables
		// platform login, the Bootstrap tier is no longer offered — its members
		// must use the workspace's own Enterprise IdP(s).
		platformEnabled, err := s.idpStore.PlatformLoginEnabled(ctx, tenantID)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}

		// Enterprise IdPs first, then Bootstrap IdPs as fallback (ADR-024 §0).
		var enterprise, bootstrap []discoveryProvider
		for i := range conns {
			c := conns[i]
			if c.Status != "active" {
				continue
			}
			dp := discoveryProvider{ID: c.ID, Display: c.DisplayName, Type: c.Protocol}
			if c.TenantID == nil {
				if !platformEnabled {
					continue // platform login disabled for this workspace
				}
				dp.Tier = "bootstrap"
				bootstrap = append(bootstrap, dp)
			} else {
				dp.Tier = "enterprise"
				enterprise = append(enterprise, dp)
			}
		}

		resp := discoveryResponse{
			Workspace:        slug,
			Providers:        append(enterprise, bootstrap...),
			PlatformFallback: len(bootstrap) > 0,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
}
