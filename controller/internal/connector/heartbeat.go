package connector

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Heartbeat implements the ConnectorService.Heartbeat gRPC handler.
// Called by: gRPC server (registered via proto-generated service definition)
//
// NOTE: The SPIFFE interceptor has ALREADY validated the mTLS certificate and
// injected identity into context before this code runs.
//
// Flow:
//
//  1. Read identity from context (injected by interceptor in spiffe.go)
//  2. Verify role == appmeta.SPIFFERoleConnector
//  3. Resolve tenant: SELECT tenant_id FROM connectors WHERE id = $1 AND trust_domain = $2
//  4. Verify not revoked: check connector status != 'revoked'
//  5. Update connector: last_heartbeat_at=NOW(), version, hostname, public_ip, status='active'
//  6. Return HeartbeatResponse{Ok: true, ReEnroll: false}
//
// re_enroll is ALWAYS false this sprint.
func (h *EnrollmentHandler) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	// Step 1 — Read identity from context.
	// Called: spiffe.go → context accessor helpers
	trustDomain := TrustDomainFromContext(ctx)
	role := SPIFFERoleFromContext(ctx)
	connectorID := SPIFFEEntityIDFromContext(ctx)

	// Step 2 — Verify role is "connector".
	if role != appmeta.SPIFFERoleConnector {
		return nil, status.Errorf(codes.PermissionDenied, "expected role %q, got %q", appmeta.SPIFFERoleConnector, role)
	}

	// Step 3 — Resolve tenant and load connector status + cert expiry.
	var connStatus, tenantID string
	var certNotAfter *time.Time
	err := h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id, cert_not_after FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&connStatus, &tenantID, &certNotAfter)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "connector not found: %v", err)
	}

	// Step 4 — Verify not revoked.
	if connStatus == "revoked" {
		return nil, status.Error(codes.PermissionDenied, "connector is revoked")
	}

	// Step 4b — Check if cert is expiring soon (within renewal window).
	reEnroll := false
	if certNotAfter != nil {
		renewBy := time.Now().Add(h.Cfg.RenewalWindow)
		if certNotAfter.Before(renewBy) {
			reEnroll = true
			log.Printf("connector %s: cert expiring soon (not_after=%v), requesting renewal", connectorID, *certNotAfter)
		}
	}

	// Step 5 — Update connector row.
	_, err = h.Pool.Exec(ctx,
		`UPDATE connectors
		    SET last_heartbeat_at = NOW(),
		        version = $1,
		        hostname = $2,
		        public_ip = $3,
		        agent_addr = NULLIF($5, ''),
		        status = 'active',
		        updated_at = NOW()
		  WHERE id = $4`,
		req.Version,
		req.Hostname,
		req.PublicIp,
		connectorID,
		req.AgentAddr,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update connector: %v", err)
	}

	// Step 6 — Process shield health reports from this connector.
	for _, sh := range req.Shields {
		if err := h.ShieldSvc.UpdateShieldHealth(ctx, sh.ShieldId, connectorID, sh.Status, sh.Version, sh.LastHeartbeatAt); err != nil {
			log.Printf("heartbeat: update shield health shield_id=%s: %v", sh.ShieldId, err)
		}
	}

	// Step 7 — Return success.
	// re_enroll = true when cert expiring within the renewal window.
	return &pb.HeartbeatResponse{
		Ok:       true,
		ReEnroll: reEnroll,
	}, nil
}

// ── Phase 5 — Disconnect Watcher ────────────────────────────────────────────

// RunDisconnectWatcher runs as a background goroutine, started alongside the gRPC server.
// Called by: main.go (Member 2 starts this with `go connector.RunDisconnectWatcher(ctx, pool, cfg)`)
//
// Behavior:
//   - Ticks every cfg.HeartbeatInterval
//   - Marks connectors DISCONNECTED where:
//     status='active' AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold
//   - Only affects connectors in active workspaces
//   - Respects context cancellation for graceful shutdown
func RunDisconnectWatcher(ctx context.Context, pool *pgxpool.Pool, cfg Config) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := markDisconnected(ctx, pool, cfg.DisconnectThreshold)
			if err != nil {
				log.Printf("disconnect watcher: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("disconnect watcher: marked %d connector(s) disconnected", n)
			}
		}
	}
}

// markDisconnected marks stale active connectors as disconnected.
// Only affects connectors in active workspaces.
// Called by: RunDisconnectWatcher() above (on every tick)
func markDisconnected(ctx context.Context, pool *pgxpool.Pool, threshold time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < NOW() - $1::interval
		    AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')`,
		fmt.Sprintf("%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return 0, fmt.Errorf("mark disconnected: %w", err)
	}
	return tag.RowsAffected(), nil
}
