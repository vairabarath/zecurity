package connector

import (
	"context"
	"log"
	"sync"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// transportPushTimeout bounds the background context for a single compile +
// fan-out so a stuck DB query cannot leak the push goroutine.
const transportPushTimeout = 10 * time.Second

// TransportPusher delivers a fresh workspace TransportSnapshot to only the
// connectors affected by a topology change (ADR-015/017 Track B), immediately
// after transport.Notifier.NotifyTopologyChange. It is the transport-plane
// counterpart to ACLPusher, but topology-scoped: a relay flap pushes to the
// handful of connectors on that relay, not the whole workspace — and it never
// touches the ACL plane. The connector control-stream open-push and the
// client GetTransportSnapshot poll remain the fallbacks.
//
// PushToConnectors is wired as transport.Notifier's push hook. Per workspace at
// most one goroutine runs; concurrent triggers coalesce, accumulating the union
// of affected connector IDs so none is dropped (latest snapshot, merged targets).
type TransportPusher struct {
	registry *ConnectorRegistry
	// compiler is *transport.Compiler in production; the interface keeps this
	// package decoupled from internal/transport and lets tests stub it.
	compiler TransportSnapshotSource

	mu       sync.Mutex
	inflight map[string]*transportPushState // workspaceID -> coalescing state
}

// transportPushState is the per-workspace coalescing latch. running means a
// runPush goroutine owns this workspace; pending is the set of affected
// connector IDs awaiting delivery (drained each push iteration).
type transportPushState struct {
	running bool
	pending map[string]struct{}
}

// NewTransportPusher builds a pusher over the live registry and a transport
// snapshot source (get-or-compile).
func NewTransportPusher(reg *ConnectorRegistry, compiler TransportSnapshotSource) *TransportPusher {
	return &TransportPusher{
		registry: reg,
		compiler: compiler,
		inflight: make(map[string]*transportPushState),
	}
}

// PushToConnectors schedules an immediate TransportSnapshot fan-out to the given
// connectors in workspaceID. Non-blocking — safe to call from the topology
// mutation path: it records intent (merging the affected set) and returns.
func (p *TransportPusher) PushToConnectors(workspaceID string, affectedConnectorIDs []string) {
	if workspaceID == "" || len(affectedConnectorIDs) == 0 {
		return
	}

	p.mu.Lock()
	st, ok := p.inflight[workspaceID]
	if !ok {
		st = &transportPushState{pending: make(map[string]struct{})}
		p.inflight[workspaceID] = st
	}
	for _, id := range affectedConnectorIDs {
		if id != "" {
			st.pending[id] = struct{}{}
		}
	}
	if st.running {
		p.mu.Unlock()
		return
	}
	st.running = true
	p.mu.Unlock()

	go p.runPush(workspaceID)
}

// runPush owns one workspace's compile+fan-out loop. It drains the pending
// affected set, pushes, and repeats while new triggers accumulated during the
// push — leaving no persistent goroutine behind.
func (p *TransportPusher) runPush(workspaceID string) {
	for {
		p.mu.Lock()
		st := p.inflight[workspaceID]
		if st == nil || len(st.pending) == 0 {
			delete(p.inflight, workspaceID)
			p.mu.Unlock()
			return
		}
		affected := st.pending
		st.pending = make(map[string]struct{}) // drain
		p.mu.Unlock()

		p.pushOnce(workspaceID, affected)
	}
}

// pushOnce compiles (or reuses the cached) workspace transport snapshot and
// sends it to each affected connector currently connected here. A compile error
// is logged and nothing is pushed (never fan out a partial snapshot). A connector
// not connected to this controller instance is skipped (it will pull on its next
// GetTransportSnapshot / receive it on reconnect). A per-connector send failure
// is logged and skipped.
func (p *TransportPusher) pushOnce(workspaceID string, affected map[string]struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), transportPushTimeout)
	defer cancel()

	snap, err := p.compiler.GetOrCompile(ctx, workspaceID)
	if err != nil {
		log.Printf("transport push: compile workspace %s: %v", workspaceID, err)
		return
	}

	sent := 0
	for id := range affected {
		c := p.registry.get(id)
		if c == nil {
			continue // not connected to this instance
		}
		if err := c.send(&pb.ConnectorControlMessage{
			Body: &pb.ConnectorControlMessage_TransportSnapshot{TransportSnapshot: snap},
		}); err != nil {
			log.Printf("transport push: send to connector %s (workspace %s): %v", id, workspaceID, err)
			continue
		}
		sent++
	}
	if sent > 0 {
		log.Printf("transport push: workspace %s version=%d connectors=%d", workspaceID, snap.Version, sent)
	}
}
