package relay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
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
const defaultRelayPublicPort = "9093"

type heartbeatStore interface {
	RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname, observedIP string, observedPort int, addressScope, publicAddr string) error
	MarkProvisioned(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname string) error
	InsertProvisionedRelay(ctx context.Context, id, name string, dnsAllowlist, ipAllowlist []string, certSerial string, certNotAfter time.Time, version, hostname string) error
}

type relayAddressObservation struct {
	ObservedIP   string
	ObservedPort int
	Scope        string
	PublicAddr   string
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
	addr := observeRelayPeerAddress(ctx)
	log.Printf("relay heartbeat: received from relay=%s version=%s hostname=%s cert_serial=%s cert_not_after=%s",
		relayID, req.Version, req.Hostname, leaf.SerialNumber.Text(16), leaf.NotAfter.Format(time.RFC3339))
	if err := s.store.RecordHeartbeat(
		ctx,
		relayID,
		leaf.SerialNumber.Text(16),
		leaf.NotAfter,
		req.Version,
		req.Hostname,
		addr.ObservedIP,
		addr.ObservedPort,
		addr.Scope,
		addr.PublicAddr,
	); err != nil {
		log.Printf("relay heartbeat: record %s: %v", relayID, err)
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

func observeRelayPeerAddress(ctx context.Context) relayAddressObservation {
	observation := relayAddressObservation{Scope: "unknown"}
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return observation
	}

	host, portText, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return observation
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return observation
	}

	observation.ObservedIP = ip.String()
	if port, err := strconv.Atoi(portText); err == nil {
		observation.ObservedPort = port
	}

	switch {
	case ip.IsLoopback():
		observation.Scope = "loopback"
	case ip.IsPrivate():
		observation.Scope = "private"
	case ip.IsLinkLocalUnicast():
		observation.Scope = "link_local"
	case ip.IsGlobalUnicast():
		observation.Scope = "public"
		observation.PublicAddr = net.JoinHostPort(ip.String(), defaultRelayPublicPort)
	default:
		observation.Scope = "unknown"
	}
	return observation
}
