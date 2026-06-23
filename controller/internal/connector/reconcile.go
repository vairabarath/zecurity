package connector

import (
	"context"
	"log"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/metrics"
	"github.com/yourorg/ztna/controller/internal/resource"
)

// Hysteresis thresholds (ADR-004 Phase 3): never act on a single report — an
// in-flight apply/remove that simply hasn't reflected yet must not be "fixed".
const (
	driftReportsBeforeResync = 2 // consecutive drifting reports before snapshot re-push
	absentReportsBeforeReap  = 3 // consecutive reports without a tombstone before reap
)

// reconcileState holds in-memory hysteresis counters. Controller restart
// resets them — harmless: the loop just waits N more reports before acting.
type reconcileState struct {
	mu     sync.Mutex
	drift  map[string]int // shield_id   → consecutive reports with drift
	absent map[string]int // resource_id → consecutive reports without it
}

func (s *reconcileState) ensure() {
	if s.drift == nil {
		s.drift = make(map[string]int)
		s.absent = make(map[string]int)
	}
}

// handleResourceState reconciles a batch of shield actual-state reports against
// desired state. Corrective action for drift is a fresh desired-state snapshot
// (Phase 2 replace-semantics drops orphans and applies missing in one shot).
func (h *EnrollmentHandler) handleResourceState(ctx context.Context, client *connectorStreamClient, batch *pb.ResourceStateBatch) {
	for _, report := range batch.Reports {
		// Security scope: the reporting connector must own this shield, in this tenant.
		var owned bool
		err := h.Pool.QueryRow(ctx,
			`SELECT true FROM shields WHERE id = $1 AND connector_id = $2 AND tenant_id = $3`,
			report.ShieldId, client.connectorID, client.tenantID,
		).Scan(&owned)
		if err != nil {
			log.Printf("reconcile: report for unowned/unknown shield %s from connector %s — ignored", report.ShieldId, client.connectorID)
			continue
		}
		h.reconcileShield(ctx, h.Pool, client, report)
	}
}

func (h *EnrollmentHandler) reconcileShield(ctx context.Context, db *pgxpool.Pool, client *connectorStreamClient, report *shieldpb.ResourceStateReport) {
	metrics.ReconcileReport()

	desired, err := resource.GetDesiredForShield(ctx, db, report.ShieldId)
	if err != nil {
		return
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, d := range desired {
		desiredSet[d.ID] = true
	}

	// Known tombstones ('deleting'). They are EXPECTED to still be enforced for a
	// few reports while the shield processes the removal, so they are NOT orphans —
	// the tombstone-reap pass below owns them. Excluding them from orphan
	// classification keeps drift_detected{orphan} meaning a TRUE zombie (enforced,
	// never desired, not mid-delete). On a load error, proceed with an empty set so
	// drift detection still runs.
	deleting, err := resource.GetDeletingForShield(ctx, db, report.ShieldId)
	if err != nil {
		log.Printf("reconcile: load deleting set for shield %s: %v", report.ShieldId, err)
	}
	deletingSet := make(map[string]bool, len(deleting))
	for _, id := range deleting {
		deletingSet[id] = true
	}

	reportedSet := make(map[string]bool, len(report.ActiveResourceIds))
	for _, id := range report.ActiveResourceIds {
		reportedSet[id] = true
	}

	// ── Drift: orphans (reported, not desired) + missing (desired, not reported)
	drift := false
	for id := range reportedSet {
		if !desiredSet[id] {
			// Still drift — a resync re-pushes the desired set, which drops a true
			// zombie AND re-sends a removal whose instruction may have been lost
			// (backstop for a stuck tombstone). But only a NON-tombstone counts as an
			// orphan: a 'deleting' row reported here is just mid-removal, owned by the
			// reap pass below — counting it would conflate normal deletes with zombies.
			drift = true
			if !deletingSet[id] {
				metrics.DriftDetected("orphan")
				log.Printf("reconcile: shield %s enforcing ORPHAN resource %s", report.ShieldId, id)
			}
		}
	}
	for id := range desiredSet {
		if !reportedSet[id] {
			drift = true
			metrics.DriftDetected("missing")
			log.Printf("reconcile: shield %s MISSING desired resource %s", report.ShieldId, id)
		}
	}

	// ── Counter bookkeeping under the lock. h.Recon.mu guards ONLY the in-memory
	// hysteresis maps, so we hold it just long enough to update counters and decide
	// what to do — capturing the decisions (resync? which tombstones to reap?) —
	// then release it BEFORE any DB query or connector send. Holding it across I/O
	// would serialize reconciliation for every connector in the controller behind a
	// single slow query or a stalled stream send. Per-key access is single-writer
	// (a shield is owned by exactly one connector, whose stream is processed by one
	// goroutine), so splitting the lock from the I/O introduces no per-shield race.
	var (
		shouldResync    bool
		resyncDriftRuns int
		toReap          []string
		drifting        int
		absent          int
	)
	h.Recon.mu.Lock()
	h.Recon.ensure()
	if drift {
		h.Recon.drift[report.ShieldId]++
		if h.Recon.drift[report.ShieldId] >= driftReportsBeforeResync {
			shouldResync = true
			resyncDriftRuns = h.Recon.drift[report.ShieldId]
			h.Recon.drift[report.ShieldId] = 0
		}
	} else {
		h.Recon.drift[report.ShieldId] = 0
	}
	for _, rid := range deleting {
		if reportedSet[rid] {
			h.Recon.absent[rid] = 0 // still enforced — snapshot will drop it; keep waiting
			continue
		}
		h.Recon.absent[rid]++
		if h.Recon.absent[rid] >= absentReportsBeforeReap {
			toReap = append(toReap, rid)
			delete(h.Recon.absent, rid)
		}
	}
	// Current-state gauges, snapshotted under the lock. drift entries reset to 0 stay
	// in the map, so count only the non-zero ones; every remaining absent entry is a
	// tombstone still awaiting reap confirmation.
	for _, n := range h.Recon.drift {
		if n > 0 {
			drifting++
		}
	}
	absent = len(h.Recon.absent)
	h.Recon.mu.Unlock()

	// ── I/O outside the lock: snapshot re-push for persistent drift, then reap the
	// tombstones the bookkeeping pass confirmed absent.
	if shouldResync {
		log.Printf("reconcile: drift persisted %d reports on shield %s — re-pushing snapshot", resyncDriftRuns, report.ShieldId)
		if msg, err := buildSnapshotMsg(ctx, db, report.ShieldId); err == nil {
			_ = client.send(msg)
			metrics.Resync()
		}
	}
	var anyReaped bool
	for _, rid := range toReap {
		if reaped, _ := resource.ReapTombstone(ctx, db, client.tenantID, report.ShieldId, rid); reaped {
			anyReaped = true
			metrics.TombstoneReaped()
			log.Printf("reconcile: tombstone %s confirmed absent x%d — reaped", rid, absentReportsBeforeReap)
		}
	}
	// A reap physically removed a tombstoned resource from the ACL compiler's
	// output — invalidate the workspace ACL snapshot once for this report.
	if anyReaped && h.PolicyNotifier != nil {
		if err := h.PolicyNotifier.NotifyPolicyChange(ctx, client.tenantID); err != nil {
			log.Printf("reconcile: notify after reap tenant=%s: %v", client.tenantID, err)
		}
	}

	metrics.SetReconcileGauges(drifting, absent)
}
