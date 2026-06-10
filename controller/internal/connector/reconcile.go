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
	h.Recon.mu.Lock()
	defer h.Recon.mu.Unlock()
	h.Recon.ensure()
	metrics.ReconcileReport()

	desired, err := resource.GetDesiredForShield(ctx, db, report.ShieldId)
	if err != nil {
		return
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, d := range desired {
		desiredSet[d.ID] = true
	}
	reportedSet := make(map[string]bool, len(report.ActiveResourceIds))
	for _, id := range report.ActiveResourceIds {
		reportedSet[id] = true
	}

	// ── Drift: orphans (reported, not desired) + missing (desired, not reported)
	drift := false
	for id := range reportedSet {
		if !desiredSet[id] {
			drift = true
			metrics.DriftDetected("orphan")
			log.Printf("reconcile: shield %s enforcing ORPHAN resource %s", report.ShieldId, id)
		}
	}
	for id := range desiredSet {
		if !reportedSet[id] {
			drift = true
			metrics.DriftDetected("missing")
			log.Printf("reconcile: shield %s MISSING desired resource %s", report.ShieldId, id)
		}
	}
	if drift {
		h.Recon.drift[report.ShieldId]++
		if h.Recon.drift[report.ShieldId] >= driftReportsBeforeResync {
			log.Printf("reconcile: drift persisted %d reports on shield %s — re-pushing snapshot", h.Recon.drift[report.ShieldId], report.ShieldId)
			if msg, err := buildSnapshotMsg(ctx, db, report.ShieldId); err == nil {
				_ = client.send(msg)
				metrics.Resync()
			}
			h.Recon.drift[report.ShieldId] = 0
		}
	} else {
		h.Recon.drift[report.ShieldId] = 0
	}

	// ── Tombstone reap: confirmed-absent 'deleting' rows
	deleting, err := resource.GetDeletingForShield(ctx, db, report.ShieldId)
	if err != nil {
		return
	}
	for _, rid := range deleting {
		if reportedSet[rid] {
			h.Recon.absent[rid] = 0 // still enforced — snapshot will drop it; keep waiting
			continue
		}
		h.Recon.absent[rid]++
		if h.Recon.absent[rid] >= absentReportsBeforeReap {
			if reaped, _ := resource.ReapTombstone(ctx, db, client.tenantID, report.ShieldId, rid); reaped {
				metrics.TombstoneReaped()
				log.Printf("reconcile: tombstone %s confirmed absent x%d — reaped", rid, absentReportsBeforeReap)
			}
			delete(h.Recon.absent, rid)
		}
	}

	// Current-state gauges (under h.Recon.mu). drift entries reset to 0 stay in
	// the map, so count only the non-zero ones; every absent entry is a tombstone
	// still awaiting reap confirmation.
	drifting := 0
	for _, n := range h.Recon.drift {
		if n > 0 {
			drifting++
		}
	}
	metrics.SetReconcileGauges(drifting, len(h.Recon.absent))
}
