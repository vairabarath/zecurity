package connector

import (
	"context"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Goodbye implements ConnectorService.Goodbye.
// Called by the Connector on clean shutdown (SIGTERM) to immediately mark
// itself DISCONNECTED rather than waiting for the disconnect watcher timeout.
func (h *EnrollmentHandler) Goodbye(ctx context.Context, req *pb.GoodbyeRequest) (*pb.GoodbyeResponse, error) {
	connectorID := SPIFFEEntityIDFromContext(ctx)
	trustDomain := TrustDomainFromContext(ctx)
	role := SPIFFERoleFromContext(ctx)

	if role != appmeta.SPIFFERoleConnector {
		return nil, status.Errorf(codes.PermissionDenied, "expected role %q, got %q", appmeta.SPIFFERoleConnector, role)
	}

	var tenantID string
	err := h.Pool.QueryRow(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE id = $1
		    AND trust_domain = $2
		    AND status = 'active'
		RETURNING tenant_id`,
		connectorID, trustDomain,
	).Scan(&tenantID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.Internal, "goodbye: update connector: %v", err)
		}
	} else {
		if h.PolicyNotifier != nil {
			if err := h.PolicyNotifier.NotifyPolicyChange(ctx, tenantID); err != nil {
				log.Printf("connector goodbye: notify policy change connector=%s: %v", connectorID, err)
			}
		}
		if h.TransportNotifier != nil {
			if err := h.TransportNotifier.NotifyTopologyChange(ctx, tenantID, []string{connectorID}); err != nil {
				log.Printf("connector goodbye: notify topology after connector disconnect connector=%s: %v", connectorID, err)
			}
		}
	}

	log.Printf("connector goodbye: connector_id=%s trust_domain=%s", connectorID, trustDomain)

	return &pb.GoodbyeResponse{Ok: true}, nil
}
