package connector

import (
	"context"
	"log"
	"sync"
	"time"
)

// recordRelayPlacement records a connector's reported relay placement (from a
// heartbeat or a relay event) and, when the placement changed, fires a
// transport-plane topology change for this connector only (never the ACL,
// ADR-017 Track B).
//
// D-40: for a residency workspace the placement is first checked against the
// workspace's authorized pools. A placement outside them is not recorded, any
// placement already stored for the connector is removed (so the transport
// snapshot stops publishing a relay path through it), and the refusal is
// recorded once per connector and relay. This is supplementary: the relay-side
// check (relay-pools PR 3) is the enforcement. A failed check is fail-closed:
// nothing is recorded.
func (h *EnrollmentHandler) recordRelayPlacement(ctx context.Context, client *connectorStreamClient,
	connectorID, relayID string, attachedAt time.Time, source string) {
	allowed, err := h.RelayStore.PlacementAllowed(ctx, client.tenantID, relayID)
	if err != nil {
		log.Printf("control stream: check relay placement from %s connector=%s relay=%s: %v (not recorded)", source, connectorID, relayID, err)
		return
	}
	if !allowed {
		h.rejectRelayPlacement(ctx, client, connectorID, relayID, source)
		return
	}
	h.placementRejects.clear(connectorID)
	changed, err := h.RelayStore.UpsertPlacement(ctx, connectorID, relayID, attachedAt, source)
	if err != nil {
		log.Printf("control stream: upsert relay placement from %s connector=%s relay=%s: %v", source, connectorID, relayID, err)
		return
	}
	if changed {
		h.notifyPlacementChange(ctx, client, connectorID, source)
	}
}

// rejectRelayPlacement handles a placement outside the authorized pools.
func (h *EnrollmentHandler) rejectRelayPlacement(ctx context.Context, client *connectorStreamClient,
	connectorID, relayID, source string) {
	first := h.placementRejects.record(connectorID, relayID)
	if first {
		log.Printf("control stream: relay placement refused (D-40, outside the workspace's authorized relay pools) source=%s workspace=%s connector=%s relay=%s",
			source, client.tenantID, connectorID, relayID)
		if h.OnPlacementRejected != nil {
			h.OnPlacementRejected(ctx, client.tenantID, connectorID, relayID)
		}
	}
	changed, err := h.RelayStore.DeletePlacement(ctx, connectorID)
	if err != nil {
		log.Printf("control stream: remove refused relay placement connector=%s: %v", connectorID, err)
		return
	}
	if changed {
		h.notifyPlacementChange(ctx, client, connectorID, source)
	}
}

func (h *EnrollmentHandler) notifyPlacementChange(ctx context.Context, client *connectorStreamClient, connectorID, source string) {
	if h.TransportNotifier == nil {
		return
	}
	if err := h.TransportNotifier.NotifyTopologyChange(ctx, client.tenantID, []string{connectorID}); err != nil {
		log.Printf("control stream: notify topology after relay placement (%s) connector=%s: %v", source, connectorID, err)
	}
}

// placementRejectLog remembers the last refused relay per connector so a
// refusal repeated by every heartbeat is logged and audited once (single
// controller, D-02; a restart may record one more).
type placementRejectLog struct {
	mu   sync.Mutex
	last map[string]string // connector id → refused relay id
}

// record notes a refusal and reports whether it is new for this connector.
func (l *placementRejectLog) record(connectorID, relayID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]string{}
	}
	if l.last[connectorID] == relayID {
		return false
	}
	l.last[connectorID] = relayID
	return true
}

// clear forgets a connector's refusal once it reports an allowed placement.
func (l *placementRejectLog) clear(connectorID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, connectorID)
}
