package relay

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
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

const (
	relayHeartbeatLastPrefix     = "relay:heartbeat:last:"
	relayHeartbeatDBWritePrefix  = "relay:heartbeat:db-write:"
	relayHeartbeatMetadataPrefix = "relay:heartbeat:metadata:"
)

type heartbeatStore interface {
	RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname, observedIP string, observedPort int, addressScope, publicAddr string, connectionCount, maxConnections uint32) error
	MarkProvisioned(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname string) error
	InsertProvisionedRelay(ctx context.Context, id, name string, dnsAllowlist, ipAllowlist []string, certSerial string, certNotAfter time.Time, version, hostname string) error
	ListWorkspacesForRelay(ctx context.Context, relayID string) ([]string, error)
	EvaluateCapacityLabel(ctx context.Context, relayID string, holdDown time.Duration) (CapacityLabelTransition, error)
}

type policyChangeNotifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
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
	addr := observeRelayPeerAddress(ctx, req.ListenPort)
	log.Printf("relay heartbeat: received from relay=%s version=%s hostname=%s cert_serial=%s cert_not_after=%s",
		relayID, req.Version, req.Hostname, leaf.SerialNumber.Text(16), leaf.NotAfter.Format(time.RFC3339))

	var metadataChanged bool
	shouldWriteDB := true
	if s.redis != nil {
		var cacheErr error
		shouldWriteDB, metadataChanged, cacheErr = s.cacheRelayHeartbeat(
			ctx,
			relayID,
			leaf.SerialNumber.Text(16),
			leaf.NotAfter,
			req.Version,
			req.Hostname,
			addr,
		)
		if cacheErr != nil {
			log.Printf("relay heartbeat: cache %s: %v", relayID, cacheErr)
			shouldWriteDB = true
			metadataChanged = true // conservative: assume changed on cache failure
		}
	} else {
		metadataChanged = true // no cache: always notify
	}
	if !shouldWriteDB {
		return &relaypb.HeartbeatResponse{
			ServerTimeUnix:       time.Now().UTC().Unix(),
			NextHeartbeatSeconds: uint32(defaultHeartbeatInterval / time.Second),
		}, nil
	}

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
		req.ConnectionCount,
		req.MaxConnections,
	); err != nil {
		log.Printf("relay heartbeat: record %s: %v", relayID, err)
		if errors.Is(err, ErrRelayNotFound) {
			return nil, status.Error(codes.NotFound, "Relay registration not found")
		}
		return nil, status.Error(codes.Internal, "record Relay heartbeat")
	}
	if err := s.markRelayHeartbeatDBWritten(ctx, relayID); err != nil {
		log.Printf("relay heartbeat: mark db write %s: %v", relayID, err)
	}

	transition, evalErr := s.store.EvaluateCapacityLabel(ctx, relayID, s.labelHoldDown)
	if evalErr != nil {
		// A capacity-label failure must not fail the heartbeat itself —
		// the relay row is already updated and the worst case is a delayed
		// label promotion next heartbeat. Log and continue.
		log.Printf("relay heartbeat: evaluate capacity label %s: %v", relayID, evalErr)
	}
	if transition.Promoted {
		log.Printf("relay heartbeat: capacity label promoted relay=%s %s -> %s",
			relayID, transition.PreviousLabel, transition.NewLabel)
	}
	if metadataChanged || transition.Promoted {
		s.notifyRelayWorkspaces(ctx, relayID)
		// ADR-016 C5: pool change → push fresh LabelledRelayList to all
		// connected connectors. metadataChanged catches address/SPIFFE shifts
		// and first-time eligibility; Promoted catches tier transitions.
		if s.onPoolChange != nil {
			s.onPoolChange(ctx)
		}
	}
	return &relaypb.HeartbeatResponse{
		ServerTimeUnix:       time.Now().UTC().Unix(),
		NextHeartbeatSeconds: uint32(defaultHeartbeatInterval / time.Second),
	}, nil
}

func (s *Service) notifyRelayWorkspaces(ctx context.Context, relayID string) {
	if s.notifier == nil {
		return
	}
	workspaceIDs, err := s.store.ListWorkspacesForRelay(ctx, relayID)
	if err != nil {
		log.Printf("relay heartbeat: list workspaces for relay %s: %v", relayID, err)
		return
	}
	for _, wsID := range workspaceIDs {
		if err := s.notifier.NotifyPolicyChange(ctx, wsID); err != nil {
			log.Printf("relay heartbeat: notify workspace %s: %v", wsID, err)
		}
	}
}

func (s *Service) cacheRelayHeartbeat(ctx context.Context, relayID, certSerial string, certNotAfter time.Time, version, hostname string, addr relayAddressObservation) (shouldWriteDB bool, metadataChanged bool, err error) {
	now := time.Now().UTC()
	livenessTTL := 3 * defaultHeartbeatInterval
	if s.heartbeatDBWriteInterval > livenessTTL {
		livenessTTL = s.heartbeatDBWriteInterval + defaultHeartbeatInterval
	}

	if err := s.redis.Set(ctx, relayHeartbeatLastPrefix+relayID, strconv.FormatInt(now.Unix(), 10), livenessTTL).Err(); err != nil {
		return true, true, fmt.Errorf("set liveness key: %w", err)
	}

	metadataKey := relayHeartbeatMetadataPrefix + relayID
	metadataValue := relayHeartbeatMetadataValue(certSerial, certNotAfter, version, hostname, addr)
	previousMetadata, err := s.redis.Get(ctx, metadataKey).Result()
	if err != nil && err != valkeycompat.Nil {
		return true, true, fmt.Errorf("get metadata key: %w", err)
	}
	metadataChanged = err == valkeycompat.Nil || previousMetadata != metadataValue

	dbMarkerKey := relayHeartbeatDBWritePrefix + relayID
	_, err = s.redis.Get(ctx, dbMarkerKey).Result()
	if err != nil && err != valkeycompat.Nil {
		return true, true, fmt.Errorf("get db-write marker: %w", err)
	}
	dbWriteDue := err == valkeycompat.Nil

	if err := s.redis.Set(ctx, metadataKey, metadataValue, livenessTTL).Err(); err != nil {
		return true, true, fmt.Errorf("set metadata key: %w", err)
	}

	return metadataChanged || dbWriteDue, metadataChanged, nil
}

func (s *Service) markRelayHeartbeatDBWritten(ctx context.Context, relayID string) error {
	if s.redis == nil {
		return nil
	}
	interval := s.heartbeatDBWriteInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return s.redis.Set(ctx, relayHeartbeatDBWritePrefix+relayID, strconv.FormatInt(time.Now().UTC().Unix(), 10), interval).Err()
}

func relayHeartbeatMetadataValue(certSerial string, certNotAfter time.Time, version, hostname string, addr relayAddressObservation) string {
	parts := []string{
		certSerial,
		strconv.FormatInt(certNotAfter.UTC().Unix(), 10),
		version,
		hostname,
		addr.ObservedIP,
		strconv.Itoa(addr.ObservedPort),
		addr.Scope,
		addr.PublicAddr,
	}
	return strings.Join(parts, "\x00")
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

func observeRelayPeerAddress(ctx context.Context, listenPort uint32) relayAddressObservation {
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
		port := defaultRelayPublicPort
		if listenPort > 0 && listenPort <= 65535 {
			port = strconv.FormatUint(uint64(listenPort), 10)
		}
		observation.PublicAddr = net.JoinHostPort(ip.String(), port)
	default:
		observation.Scope = "unknown"
	}
	return observation
}
