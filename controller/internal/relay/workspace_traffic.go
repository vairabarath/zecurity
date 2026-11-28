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
// know, and keeps the result in memory (D-02: one controller). Rates are
// taken on the heartbeat's database-write cadence. A relay restart (its
// uptime didn't grow by the gap) or a counter that went down gives no rate
// for that window, never a negative or invented one. The F23 fixes
// (2026-11-24) are documented on Record.
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
	// maxGap: a longer window between two readings has no rate (rule 5).
	// Zero means no limit.
	maxGap time.Duration
}

type relayTraffic struct {
	at     time.Time
	uptime uint64
	totals map[string]WorkspaceTrafficSample
	// domains is this reading's trust domain → workspace id, "" for a trust
	// domain that matched no active workspace (F23 R1-1 rule 4). In memory
	// only, for one reading; never stored. Replaced at
	// every reading, so it never holds more than one heartbeat's entries.
	domains map[string]string
	view    RelayWorkspaceTraffic
}

func NewWorkspaceTrafficTracker() *WorkspaceTrafficTracker {
	return &WorkspaceTrafficTracker{relays: map[string]*relayTraffic{}}
}

// WithMaxGap sets rule 5's limit: a window longer than d has no rate. main.go
// passes the sampler's TrafficMaxAge, so a gap is rejected exactly when the
// previous reading would already count as not reported.
func (t *WorkspaceTrafficTracker) WithMaxGap(d time.Duration) *WorkspaceTrafficTracker {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxGap = d
	return t
}

// restartSlackSeconds tolerates heartbeat delivery jitter and the relay's
// whole-second uptime: the same 5 s as B2's bytesRateValid.
const restartSlackSeconds = 5

// Record stores a relay's per-workspace counters taken at `at` and derives
// rates from its previous reading. A sample not newer than the stored one (a
// late heartbeat) changes nothing. domains maps every trust domain the relay
// reported to its workspace id, "" when it matched none (nil: not known).
//
// Rates (F23 R1-1, agreed 2026-11-23, option D):
//  1. In a continuous window, a workspace with a previous counter rates from
//     it; one absent from the previous reading rates from zero (the relay
//     reports every entry that ever carried bytes, so absent means zero).
//  2. A window that isn't continuous (first reading, relay restart) has no
//     rate for any workspace on the relay. Restart: U_now < U_prev + gap − 5 s.
//  3. A counter that went down (reset or re-created entry) has no rate.
//  4. A workspace whose trust domain was unmatched, or another workspace's,
//     in the previous reading gets no zero baseline: that counter may be
//     long-lived (a purged tenant's slug can be reused).
//  5. A window longer than maxGap has no rate. Every reading, rated or not,
//     is the baseline for the next one.
func (t *WorkspaceTrafficTracker) Record(relayID string, at time.Time, uptimeSeconds uint64, samples map[string]WorkspaceTrafficSample, domains map[string]string) {
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
		continuous = gap > 0 && !restarted(prev.uptime, uptimeSeconds, gap) &&
			(t.maxGap <= 0 || at.Sub(prev.at) <= t.maxGap)
	}
	if continuous {
		view.WindowSeconds = gap
	}
	totals := make(map[string]WorkspaceTrafficSample, len(samples))
	for ws, cur := range samples {
		totals[ws] = cur
		r := WorkspaceRate{ActiveStreams: cur.ActiveStreams}
		if continuous {
			base, ok := prev.totalsFor(ws)
			if !ok && !prev.zeroBaselineAllowed(ws, domains) {
				view.Workspaces[ws] = r
				continue
			}
			if cur.ToClient >= base.ToClient && cur.ToConnector >= base.ToConnector {
				toClient := float64(cur.ToClient-base.ToClient) / gap
				toConnector := float64(cur.ToConnector-base.ToConnector) / gap
				r.ToClientBytesPerSec, r.ToConnectorBytesPerSec = &toClient, &toConnector
			}
		}
		view.Workspaces[ws] = r
	}
	t.relays[relayID] = &relayTraffic{at: at, uptime: uptimeSeconds, totals: totals, domains: domains, view: view}
}

// restarted is the combined restart check (agreed 2026-11-24): the relay's
// uptime, a monotonic clock (Rust Instant), must have grown by the gap, less
// the tolerance. It catches a restart just after the previous reading, for a
// young relay too, and contains the older uptime + 5 >= gap test (prev = 0).
func restarted(prevUptime, uptime uint64, gapSeconds float64) bool {
	return float64(uptime) < float64(prevUptime)+gapSeconds-restartSlackSeconds
}

// zeroBaselineAllowed reports whether ws, absent from this (previous)
// reading, may rate from zero: none of its trust domains in the current
// reading was reported here as unmatched or as another workspace's (rule 4).
// Either way the relay entry existed and its counter may be long-lived.
func (r *relayTraffic) zeroBaselineAllowed(ws string, current map[string]string) bool {
	for td, id := range current {
		if id != ws {
			continue
		}
		if was, seen := r.domains[td]; seen && was != ws {
			return false
		}
	}
	return true
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
// It runs on the heartbeats that are written to the DB (the B2 cadence), so
// R1's rates keep their window.
//
// arrived keeps Go's monotonic reading for the R1 tracker's window (F23 fix);
// receivedAt is the same instant in UTC, which usage gets on every heartbeat
// (throttled ones too, recordWorkspaceUsage), so the collector always sees
// one kind of timestamp.
func (s *Service) recordWorkspaceTraffic(ctx context.Context, relayID string, arrived, receivedAt time.Time, req *relaypb.HeartbeatRequest) {
	if s.workspaceTraffic == nil || s.workspaceLookup == nil {
		return
	}
	samples, seen, ok := s.workspaceSamples(ctx, relayID, req)
	if !ok {
		return
	}
	s.workspaceTraffic.Record(relayID, arrived, req.GetUptimeSeconds(), samples, seen)
	s.recordUsage(ctx, relayID, receivedAt, req, samples)
}

// recordWorkspaceUsage passes a throttled heartbeat's counters to the usage
// collector only (Sprint 25 live finding F32-1): usage must see every
// heartbeat, not only the B2 DB-write cadence, so a relay restart loses at
// most the bytes since its last heartbeat (D-59). R1's rate tracker is not
// touched: its window stays the B2 cadence.
func (s *Service) recordWorkspaceUsage(ctx context.Context, relayID string, at time.Time, req *relaypb.HeartbeatRequest) {
	if s.usageSink == nil || s.workspaceLookup == nil {
		return
	}
	samples, _, ok := s.workspaceSamples(ctx, relayID, req)
	if !ok {
		return
	}
	s.recordUsage(ctx, relayID, at, req, samples)
}

// recordUsage hands the cumulative counters to the usage collector (Sprint 25
// U1, F32, D-59): exact bytes from the cumulative counters. A failed lookup
// skips this heartbeat; the counters are cumulative, so the next one counts
// the whole span once.
func (s *Service) recordUsage(ctx context.Context, relayID string, at time.Time, req *relaypb.HeartbeatRequest, samples map[string]WorkspaceTrafficSample) {
	if s.usageSink != nil {
		s.usageSink.RecordRelayCounters(ctx, relayID, at, req.GetUptimeSeconds(), samples)
	}
}

// workspaceSamples resolves a heartbeat's field 13 to workspace ids. ok is
// false when the lookup failed (nothing may be recorded for this heartbeat).
func (s *Service) workspaceSamples(ctx context.Context, relayID string, req *relaypb.HeartbeatRequest) (map[string]WorkspaceTrafficSample, map[string]string, bool) {
	entries := req.GetWorkspaceTraffic()
	domains := make([]string, 0, len(entries))
	for _, e := range entries {
		domains = append(domains, e.GetTrustDomain())
	}
	byDomain, err := s.workspaceLookup.ActiveWorkspacesByTrustDomain(ctx, domains)
	if err != nil {
		log.Printf("relay heartbeat: workspace traffic %s: %v — not recorded this window", relayID, err)
		return nil, nil, false
	}
	samples := make(map[string]WorkspaceTrafficSample, len(entries))
	seen := make(map[string]string, len(entries))
	for _, e := range entries {
		ws, ok := byDomain[e.GetTrustDomain()]
		if !ok {
			// Remembered for one reading as unmatched (R1-1 rule 4), never stored.
			seen[e.GetTrustDomain()] = ""
			log.Printf("relay heartbeat: workspace traffic %s: ignoring unknown or inactive trust domain %q", relayID, e.GetTrustDomain())
			continue
		}
		seen[e.GetTrustDomain()] = ws
		cur := samples[ws]
		cur.ToConnector += e.GetBytesClientToConnector()
		cur.ToClient += e.GetBytesConnectorToClient()
		cur.ActiveStreams += e.GetActiveStreams()
		samples[ws] = cur
	}
	return samples, seen, true
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
