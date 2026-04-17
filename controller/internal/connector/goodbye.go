package connector

import (
	"context"
	"log"

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

	_, err := h.Pool.Exec(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE id = $1
		    AND trust_domain = $2`,
		connectorID, trustDomain,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "goodbye: update connector: %v", err)
	}

	log.Printf("connector goodbye: connector_id=%s trust_domain=%s", connectorID, trustDomain)

	return &pb.GoodbyeResponse{Ok: true}, nil
}
