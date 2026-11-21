package relay

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
)

// Sprint 24 R1 (Decision Record D-54): per-workspace relay traffic.
//
// A relay reports, per trust domain, the bytes it bridged in each direction
// since it started and its open streams (heartbeat field 13). The controller
// maps each trust domain to its workspace, ignores (and logs) any it doesn't
// know, and keeps the result in memory (D-02: one controller). Rates use the
// same window and restart rules as the relay totals (B2, F5): they are taken
// on the heartbeat's database-write cadence, a counter that went down or an
// uptime shorter than the gap means a restart, and then there is no rate for
// that window, never a negative or invented one.
//
// Pre-production refinement (2026-11-21): every relay is rebuilt with R1, so
// an empty field 13 means the relay carried no tenant traffic (0). "Not
// reported" is a relay that isn't reporting at all: silent, revoked, or a
// sample older than the reader accepts (see MeasuredAt).

// WorkspaceTrafficSample is one workspace's cumulative counters as a relay
// reported them.
type WorkspaceTrafficSample struct {
	ToConnector   uint64
	ToClient      uint64
	ActiveStreams uint32
}

// WorkspaceRate is one workspace's traffic through one relay over the last
// window. A nil rate means none could be derived this window (first sample,
// restart, counter reset); ActiveStreams is always the latest value.
type WorkspaceRate struct {
	ToClientBytesPerSec    *float64
	ToConnectorBytesPerSec *float64
	ActiveStreams          uint32
}

// RelayWorkspaceTraffic is the latest per-workspace view of one relay. A
// workspace missing from Workspaces had no traffic on this relay.
type RelayWorkspaceTraffic struct {
	RelayID string
	// MeasuredAt is when the heartbeat carrying the sample arrived. Readers
	// treat an old value as not reported (the relay may have gone silent).
	MeasuredAt time.Time
	// WindowSeconds is the time the rates cover; 0 when there are none.
	WindowSeconds float64
	// Workspaces by workspace id.
	Workspaces map[string]WorkspaceRate
}

// WorkspaceTrafficTracker holds every relay's latest per-workspace view.
// Safe for concurrent use.
type WorkspaceTrafficTracker struct {
	mu     sync.Mutex
	relays map[string]*relayTraffic
}

type relayTraffic struct {
	at     time.Time
	totals map[string]WorkspaceTrafficSample
	view   RelayWorkspaceTraffic
}

func NewWorkspaceTrafficTracker() *WorkspaceTrafficTracker {
	return &WorkspaceTrafficTracker{relays: map[string]*relayTraffic{}}
}

// restartSlackSeconds matches B2's bytesRateValid: heartbeat delivery jitter.
const restartSlackSeconds = 5

// Record stores a relay's per-workspace counters taken at `at` and derives
// rates from its previous sample. A sample not newer than the stored one (a
// late heartbeat) changes nothing.
func (t *WorkspaceTrafficTracker) Record(relayID string, at time.Time, uptimeSeconds uint64, samples map[string]WorkspaceTrafficSample) {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.relays[relayID]
	if prev != nil && !at.After(prev.at) {
		return
	}
	view := RelayWorkspaceTraffic{RelayID: relayID, MeasuredAt: at, Workspaces: make(map[string]WorkspaceRate, len(samples))}
	var gap float64
	continuous := false
	if prev != nil {
		gap = at.Sub(prev.at).Seconds()
		continuous = gap > 0 && float64(uptimeSeconds)+restartSlackSeconds >= gap
	}
	if continuous {
		view.WindowSeconds = gap
	}
	totals := make(map[string]WorkspaceTrafficSample, len(samples))
	for ws, cur := range samples {
		totals[ws] = cur
		r := WorkspaceRate{ActiveStreams: cur.ActiveStreams}
		if p, ok := prev.totalsFor(ws); continuous && ok && cur.ToClient >= p.ToClient && cur.ToConnector >= p.ToConnector {
			toClient := float64(cur.ToClient-p.ToClient) / gap
			toConnector := float64(cur.ToConnector-p.ToConnector) / gap
			r.ToClientBytesPerSec, r.ToConnectorBytesPerSec = &toClient, &toConnector
		}
		view.Workspaces[ws] = r
	}
	t.relays[relayID] = &relayTraffic{at: at, totals: totals, view: view}
}

func (r *relayTraffic) totalsFor(ws string) (WorkspaceTrafficSample, bool) {
	if r == nil {
		return WorkspaceTrafficSample{}, false
	}
	s, ok := r.totals[ws]
	return s, ok
}

// Snapshot returns a copy of every relay's latest view, ordered by relay id.
func (t *WorkspaceTrafficTracker) Snapshot() []RelayWorkspaceTraffic {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RelayWorkspaceTraffic, 0, len(t.relays))
	for _, r := range t.relays {
		v := r.view
		v.Workspaces = make(map[string]WorkspaceRate, len(r.view.Workspaces))
		for ws, rate := range r.view.Workspaces {
			v.Workspaces[ws] = copyRate(rate)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelayID < out[j].RelayID })
	return out
}

func copyRate(r WorkspaceRate) WorkspaceRate {
	if r.ToClientBytesPerSec != nil {
		v := *r.ToClientBytesPerSec
		r.ToClientBytesPerSec = &v
	}
	if r.ToConnectorBytesPerSec != nil {
		v := *r.ToConnectorBytesPerSec
		r.ToConnectorBytesPerSec = &v
	}
	return r
}

// workspaceLookup maps trust domains to workspace ids (Store implements it).
type workspaceLookup interface {
	ActiveWorkspacesByTrustDomain(ctx context.Context, trustDomains []string) (map[string]string, error)
}

// ActiveWorkspacesByTrustDomain returns trust domain → workspace id for the
// active workspaces among trustDomains. Unknown, deleted or otherwise not
// active trust domains are simply absent. A suspended tenant (D-45:
// suspended_at, status stays 'active') is included: its traffic is real.
func (s *Store) ActiveWorkspacesByTrustDomain(ctx context.Context, trustDomains []string) (map[string]string, error) {
	out := make(map[string]string, len(trustDomains))
	if len(trustDomains) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT trust_domain, id::text FROM workspaces WHERE trust_domain = ANY($1) AND status = 'active'`,
		trustDomains)
	if err != nil {
		return nil, fmt.Errorf("look up workspaces by trust domain: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var td, id string
		if err := rows.Scan(&td, &id); err != nil {
			return nil, fmt.Errorf("scan workspace by trust domain: %w", err)
		}
		out[td] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read workspaces by trust domain: %w", err)
	}
	return out, nil
}

// WithWorkspaceTraffic enables R1: heartbeats record per-workspace traffic
// into tracker, with workspaces resolved through lookup.
func (s *Service) WithWorkspaceTraffic(tracker *WorkspaceTrafficTracker, lookup workspaceLookup) *Service {
	s.workspaceTraffic = tracker
	s.workspaceLookup = lookup
	return s
}

// recordWorkspaceTraffic maps a heartbeat's field 13 to workspaces and
// records it. Unknown trust domains are logged and dropped, never stored. A
// lookup failure records nothing for this window, so the previous view ages
// (a reader sees it as not reported) instead of being replaced by a guess.
func (s *Service) recordWorkspaceTraffic(ctx context.Context, relayID string, at time.Time, req *relaypb.HeartbeatRequest) {
	if s.workspaceTraffic == nil || s.workspaceLookup == nil {
		return
	}
	entries := req.GetWorkspaceTraffic()
	domains := make([]string, 0, len(entries))
	for _, e := range entries {
		domains = append(domains, e.GetTrustDomain())
	}
	byDomain, err := s.workspaceLookup.ActiveWorkspacesByTrustDomain(ctx, domains)
	if err != nil {
		log.Printf("relay heartbeat: workspace traffic %s: %v — not recorded this window", relayID, err)
		return
	}
	samples := make(map[string]WorkspaceTrafficSample, len(entries))
	for _, e := range entries {
		ws, ok := byDomain[e.GetTrustDomain()]
		if !ok {
			log.Printf("relay heartbeat: workspace traffic %s: ignoring unknown or inactive trust domain %q", relayID, e.GetTrustDomain())
			continue
		}
		cur := samples[ws]
		cur.ToConnector += e.GetBytesClientToConnector()
		cur.ToClient += e.GetBytesConnectorToClient()
		cur.ActiveStreams += e.GetActiveStreams()
		samples[ws] = cur
	}
	s.workspaceTraffic.Record(relayID, at, req.GetUptimeSeconds(), samples)
	if s.usageSink != nil {
		// Sprint 25 U1 (F32, D-59): the usage collector counts exact bytes
		// from the cumulative counters. A failed lookup above skips this
		// heartbeat; the counters are cumulative, so the next one counts the
		// whole span once.
		s.usageSink.RecordRelayCounters(ctx, relayID, at, req.GetUptimeSeconds(), samples)
	}
}

// WorkspaceUsageSink receives every heartbeat's per-workspace cumulative
// counters (Sprint 25 U1, F32). Implemented by the usage collector; an
// interface so this package doesn't depend on it.
type WorkspaceUsageSink interface {
	RecordRelayCounters(ctx context.Context, relayID string, at time.Time, uptimeSeconds uint64, counters map[string]WorkspaceTrafficSample)
}

// WithUsageSink sets the usage collector (Sprint 25 U1).
func (s *Service) WithUsageSink(sink WorkspaceUsageSink) *Service {
	s.usageSink = sink
	return s
}
