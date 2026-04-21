package shield

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yourorg/ztna/controller/internal/tenant"
)

type tokenResponse struct {
	InstallCommand string `json:"install_command"`
}

// TokenHandler handles POST /api/shields/{id}/token.
// Generates a fresh enrollment token for an existing pending shield.
// Requires JWT auth + workspace middleware already applied upstream.
func (s *service) TokenHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Path is /api/shields/{id}/token
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "api" || parts[1] != "shields" || parts[3] != "token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		shieldID := parts[2]

		tc := tenant.MustGet(r.Context())
		ctx := r.Context()

		var status, remoteNetworkID, shieldName string
		err := s.db.QueryRow(ctx,
			`SELECT status, remote_network_id, name FROM shields WHERE id = $1 AND tenant_id = $2`,
			shieldID, tc.TenantID,
		).Scan(&status, &remoteNetworkID, &shieldName)
		if err != nil {
			http.Error(w, "shield not found", http.StatusNotFound)
			return
		}
		if status != "pending" {
			http.Error(w, "shield must be in pending state", http.StatusConflict)
			return
		}

		_, installCmd, err := s.GenerateShieldToken(ctx, remoteNetworkID, tc.TenantID, tc.TenantID, shieldID, shieldName)
		if err != nil {
			http.Error(w, "token generation failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(tokenResponse{InstallCommand: installCmd})
	})
}
