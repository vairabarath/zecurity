package relay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const defaultHeartbeatInterval = 30 * time.Second

type heartbeatStore interface {
	RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname string) error
}

func (s *Service) Heartbeat(ctx context.Context, req *relaypb.HeartbeatRequest) (*relaypb.HeartbeatResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	if spiffe.Role(ctx) != appmeta.SPIFFERoleRelay ||
		spiffe.TrustDomain(ctx) != appmeta.SPIFFEGlobalTrustDomain {
		return nil, status.Error(codes.PermissionDenied, "authenticated Relay identity required")
	}
	relayID, err := canonicalRelayID(spiffe.EntityID(ctx))
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "authenticated Relay ID is invalid")
	}
	leaf, err := authenticatedLeaf(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != appmeta.RelaySPIFFEID(relayID) {
		return nil, status.Error(codes.Unauthenticated, "authenticated Relay certificate identity mismatch")
	}
	if s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "Relay heartbeat store is not configured")
	}
	if err := s.store.RecordHeartbeat(
		ctx,
		relayID,
		leaf.SerialNumber.Text(16),
		leaf.NotAfter,
		req.Version,
		req.Hostname,
	); err != nil {
		if errors.Is(err, ErrRelayNotFound) {
			return nil, status.Error(codes.NotFound, "Relay registration not found")
		}
		return nil, status.Error(codes.Internal, "record Relay heartbeat")
	}
	return &relaypb.HeartbeatResponse{
		ServerTimeUnix:       time.Now().UTC().Unix(),
		NextHeartbeatSeconds: uint32(defaultHeartbeatInterval / time.Second),
	}, nil
}

func authenticatedLeaf(ctx context.Context) (*x509.Certificate, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("no peer info")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return nil, fmt.Errorf("no authenticated client certificate")
	}
	return tlsInfo.State.PeerCertificates[0], nil
}
