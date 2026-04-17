package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type regenerateResponse struct {
	InstallCommand string `json:"install_command"`
}

// RegenerateTokenHandler handles POST /connectors/{id}/token.
// Generates a fresh enrollment token for an existing pending connector.
// Requires JWT auth + workspace middleware already applied upstream.
func RegenerateTokenHandler(pool *pgxpool.Pool, cfg Config, rdb *redis.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Path is /api/connectors/{id}/token
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "api" || parts[1] != "connectors" || parts[3] != "token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		connectorID := parts[2]

		tc := tenant.MustGet(r.Context())
		ctx := r.Context()

		var status string
		err := pool.QueryRow(ctx,
			`SELECT status FROM connectors WHERE id = $1 AND tenant_id = $2`,
			connectorID, tc.TenantID,
		).Scan(&status)
		if err != nil {
			http.Error(w, "connector not found", http.StatusNotFound)
			return
		}
		if status != "pending" {
			http.Error(w, "connector must be in pending state", http.StatusConflict)
			return
		}

		var workspaceSlug string
		err = pool.QueryRow(ctx,
			`SELECT slug FROM workspaces WHERE id = $1`,
			tc.TenantID,
		).Scan(&workspaceSlug)
		if err != nil {
			http.Error(w, "workspace not found", http.StatusInternalServerError)
			return
		}

		var certPEM string
		err = pool.QueryRow(ctx,
			`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
		).Scan(&certPEM)
		if err != nil {
			http.Error(w, "ca cert not found", http.StatusInternalServerError)
			return
		}
		block, _ := pem.Decode([]byte(certPEM))
		if block == nil {
			http.Error(w, "invalid ca cert pem", http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(block.Bytes)
		caFingerprint := hex.EncodeToString(sum[:])

		tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, tc.TenantID, workspaceSlug, caFingerprint)
		if err != nil {
			http.Error(w, "token generation failed", http.StatusInternalServerError)
			return
		}

		if err := StoreEnrollmentJTI(ctx, rdb, jti, connectorID, cfg.EnrollmentTokenTTL); err != nil {
			http.Error(w, "failed to store token jti", http.StatusInternalServerError)
			return
		}

		_, err = pool.Exec(ctx,
			`UPDATE connectors SET enrollment_token_jti = $1, updated_at = NOW()
			  WHERE id = $2 AND tenant_id = $3`,
			jti, connectorID, tc.TenantID,
		)
		if err != nil {
			http.Error(w, "failed to persist token jti", http.StatusInternalServerError)
			return
		}

		controllerAddr := os.Getenv("CONTROLLER_ADDR")
		if controllerAddr == "" {
			controllerAddr = "localhost:" + cfg.GRPCPort
		}
		controllerHTTPAddr := os.Getenv("CONTROLLER_HTTP_ADDR")
		if controllerHTTPAddr == "" {
			if i := strings.LastIndex(controllerAddr, ":"); i != -1 {
				controllerHTTPAddr = controllerAddr[:i] + ":8080"
			} else {
				controllerHTTPAddr = "localhost:8080"
			}
		}
		installCmd := fmt.Sprintf(
			"curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/connector/scripts/connector-install.sh | sudo CONTROLLER_ADDR=%s CONTROLLER_HTTP_ADDR=%s ENROLLMENT_TOKEN=%s bash",
			controllerAddr, controllerHTTPAddr, tokenString,
		)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(regenerateResponse{InstallCommand: installCmd})
	})
}
