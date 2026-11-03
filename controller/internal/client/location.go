package client

import (
	"context"
	"errors"
	"log"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/peer"

	"github.com/yourorg/ztna/controller/internal/geoip"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// observationRefresh bounds how long a poll may skip writing an unchanged
// sighting. It is well inside posture.MaxReportAge, so a device that keeps
// polling never has a stale observation.
const observationRefresh = 2 * time.Minute

type sighting struct {
	country string
	written time.Time
}

// locationObserver records where the controller sees client devices connect
// from (PENDING-20 G1). Process-local cache (D-02).
type locationObserver struct {
	geo  geoip.Resolver
	now  func() time.Time
	last sync.Map // uuid.UUID -> sighting
}

// WithGeoIP enables country observation. Without it no sighting is recorded,
// and country rules deny (unknown location, G-4).
func (s *Service) WithGeoIP(geo geoip.Resolver) *Service {
	s.location = &locationObserver{geo: geo, now: time.Now}
	return s
}

// peerAddr returns the gRPC peer's address: the client's public (or NAT
// egress) address, since the controller terminates client TLS itself.
func peerAddr(ctx context.Context) (netip.Addr, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return netip.Addr{}, false
	}
	ap, err := netip.ParseAddrPort(p.Addr.String())
	if err != nil {
		return netip.Addr{}, false
	}
	return ap.Addr().Unmap(), true
}

// observeLocation records the caller's country for deviceID.
//
// PRECONDITION: the caller has already verified that deviceID belongs to the
// authenticated user in workspaceID and is not revoked — GetACLSnapshot via
// deviceGate (client_devices WHERE id AND user_id, workspace match, directive
// NONE), ReportDevicePosture via verifyPostureDevice. Never call it with an
// unverified device id: it writes the device's location, which gates access.
//
// It reports
// whether the country changed. Best effort: an error is logged, never
// returned, because a missing sighting already fails closed (stale or unknown
// location never satisfies a country rule).
//
// force skips the write-only-on-change cache (posture reports always write,
// so the observation is at least as fresh as the report being evaluated).
func (s *Service) observeLocation(ctx context.Context, workspaceID, deviceID uuid.UUID, force bool) bool {
	lo := s.location
	if lo == nil || s.postureStore == nil {
		return false
	}
	addr, ok := peerAddr(ctx)
	if !ok {
		return false
	}
	lookup := lo.geo.Resolve(addr)
	now := lo.now()
	if !force {
		if v, ok := lo.last.Load(deviceID); ok {
			prev := v.(sighting)
			if prev.country == lookup.Country && now.Sub(prev.written) < observationRefresh {
				return false
			}
		}
	}
	changed, err := s.postureStore.RecordNetworkObservation(ctx, workspaceID, deviceID, lookup, now)
	if err != nil {
		log.Printf("client: record network observation for device %s: %v", deviceID, err)
		lo.last.Delete(deviceID)
		return false
	}
	lo.last.Store(deviceID, sighting{country: lookup.Country, written: now})
	if changed {
		log.Printf("client: device %s seen in country %q", deviceID, lookup.Country)
	}
	return changed
}

// reevaluateAfterMove re-evaluates a device whose country changed, so access
// follows within one poll. A device that hasn't reported posture yet has
// nothing to evaluate.
func (s *Service) reevaluateAfterMove(ctx context.Context, workspaceID, deviceID uuid.UUID) {
	if s.postureEvaluator == nil {
		return
	}
	if _, err := s.postureEvaluator.EvaluateDevice(ctx, workspaceID, deviceID); err != nil && !errors.Is(err, posture.ErrNotFound) {
		log.Printf("client: re-evaluate device %s after a country change: %v", deviceID, err)
	}
}
