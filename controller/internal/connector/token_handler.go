package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/netutil"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type regenerateResponse struct {
	InstallCommand string `json:"install_command"`
}

// RegenerateTokenHandler handles POST /connectors/{id}/token.
// Generates a fresh enrollment token for an existing pending connector.
// Requires JWT auth + workspace middleware already applied upstream.
func RegenerateTokenHandler(pool *pgxpool.Pool, cfg Config, rdb valkeycompat.Cmdable) http.Handler {
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

		caFingerprint, err := pki.IntermediateFingerprint(ctx, pool)
		if err != nil {
			http.Error(w, "ca cert not found", http.StatusInternalServerError)
			return
		}

		tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, tc.TenantID, workspaceSlug, caFingerprint)
		if err != nil {
			http.Error(w, "token generation failed", http.StatusInternalServerError)
			return
		}

		// Certificate audit bug 8: the row's enrollment_token_jti is the
		// authority on which token is current. Store the new token's entry
		// first (Enroll requires it), then make it current, only while the
		// connector is still pending. That swap supersedes every earlier
		// token at once; deleting the predecessor's entry is cleanup.
		if err := StoreEnrollmentJTI(ctx, rdb, jti, connectorID, cfg.EnrollmentTokenTTL); err != nil {
			http.Error(w, "failed to store token jti", http.StatusInternalServerError)
			return
		}

		// The row lock makes this wait for an enrollment in flight; a
		// connector that enrolled meanwhile is no longer pending.
		var previousJTI *string
		err = pool.QueryRow(ctx,
			`WITH current AS (
			     SELECT id, enrollment_token_jti AS previous FROM connectors
			      WHERE id = $2 AND tenant_id = $3 AND status = 'pending'
			        FOR UPDATE
			 )
			 UPDATE connectors c
			    SET enrollment_token_jti = $1, updated_at = NOW()
			   FROM current
			  WHERE c.id = current.id
			 RETURNING current.previous`,
			jti, connectorID, tc.TenantID,
		).Scan(&previousJTI)
		if err != nil {
			// The new token is not current, so it cannot enroll; drop its entry.
			if derr := DeleteEnrollmentJTI(ctx, rdb, jti); derr != nil {
				log.Printf("connector token: %s: unused token entry not deleted: %v", connectorID, derr)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "connector must be in pending state", http.StatusConflict)
				return
			}
			http.Error(w, "failed to persist token jti", http.StatusInternalServerError)
			return
		}
		if previousJTI != nil && *previousJTI != jti {
			if derr := DeleteEnrollmentJTI(ctx, rdb, *previousJTI); derr != nil {
				log.Printf("connector token: %s: superseded token entry not deleted: %v", connectorID, derr)
			}
		}

		grpcDefault, httpDefault := netutil.DetectLANAddr(cfg.GRPCPort, "8080")
		controllerAddr := os.Getenv("CONTROLLER_ADDR")
		if controllerAddr == "" || netutil.IsLocalhost(controllerAddr) {
			controllerAddr = grpcDefault
		}
		controllerHTTPAddr := os.Getenv("CONTROLLER_HTTP_ADDR")
		if controllerHTTPAddr == "" || netutil.IsLocalhost(controllerHTTPAddr) {
			controllerHTTPAddr = httpDefault
		}
		installCmd := fmt.Sprintf(
			"curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/connector/scripts/connector-install.sh | sudo CONTROLLER_ADDR=%s CONTROLLER_HTTP_ADDR=%s ENROLLMENT_TOKEN=%s bash",
			controllerAddr, controllerHTTPAddr, tokenString,
		)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(regenerateResponse{InstallCommand: installCmd})
	})
}
