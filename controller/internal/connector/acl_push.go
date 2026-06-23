package connector

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// aclPushTimeout bounds the background context used for a single proactive
// compile+fan-out, so a stuck DB query can't leak the push goroutine.
const aclPushTimeout = 10 * time.Second

// ACLPusher delivers a fresh workspace ACL snapshot to all connected connectors
// in a workspace immediately after a policy change, closing the connector-side
// revocation latency gap. It is the proactive counterpart to the heartbeat
// reconciliation path (EnrollmentHandler.pushACLSnapshot), which it leaves
// untouched and which remains the correctness fallback for offline/missed
// connectors.
//
// PushWorkspace is wired as policy.Notifier's push hook. Each workspace is
// served by at most one in-flight goroutine; concurrent triggers coalesce into a
// single trailing recompile (latest-wins), so a burst of mutations does not
// spawn a goroutine or DB compile per mutation.
type ACLPusher struct {
	registry *ConnectorRegistry
	store    *policy.Store
	cache    *policy.SnapshotCache
	notifier *policy.Notifier
	pool     *pgxpool.Pool

	// compile is policy.CompileACLSnapshot in production; overridable in tests.
	compile func(context.Context, *policy.Store, *policy.Notifier, *pgxpool.Pool, string) (*clientv1.ACLSnapshot, error)

	mu       sync.Mutex
	inflight map[string]*wsPushState // workspaceID -> coalescing latch state
}

// wsPushState is the per-workspace coalescing latch. running means a runPush
// goroutine owns this workspace; pending means at least one trigger arrived
// while it was running and one more compile+push must follow.
type wsPushState struct {
	running bool
	pending bool
}

// NewACLPusher builds a pusher over the live registry and policy subsystem.
func NewACLPusher(reg *ConnectorRegistry, store *policy.Store, cache *policy.SnapshotCache, notifier *policy.Notifier, pool *pgxpool.Pool) *ACLPusher {
	return &ACLPusher{
		registry: reg,
		store:    store,
		cache:    cache,
		notifier: notifier,
		pool:     pool,
		compile:  policy.CompileACLSnapshot,
		inflight: make(map[string]*wsPushState),
	}
}

// PushWorkspace schedules an immediate ACL fan-out to every connected connector
// in workspaceID. It is non-blocking — safe to call directly from the policy
// mutation path: it records intent and returns. If a push for this workspace is
// already running, it sets the pending flag so exactly one trailing recompile
// happens after the current one finishes; otherwise it starts the worker.
func (p *ACLPusher) PushWorkspace(workspaceID string) {
	if workspaceID == "" {
		return
	}

	p.mu.Lock()
	if st, ok := p.inflight[workspaceID]; ok && st.running {
		st.pending = true
		p.mu.Unlock()
		return
	}
	p.inflight[workspaceID] = &wsPushState{running: true}
	p.mu.Unlock()

	go p.runPush(workspaceID)
}

// runPush owns one workspace's compile+fan-out loop. It pushes once, then drains
// any trigger that arrived during the push, and exits when none is pending —
// leaving no persistent goroutine behind.
func (p *ACLPusher) runPush(workspaceID string) {
	for {
		p.pushOnce(workspaceID)

		p.mu.Lock()
		st := p.inflight[workspaceID]
		if st != nil && st.pending {
			st.pending = false
			p.mu.Unlock()
			continue
		}
		delete(p.inflight, workspaceID)
		p.mu.Unlock()
		return
	}
}

// pushOnce compiles (or reuses the cached) snapshot for the workspace and sends
// it directly to each connected connector. It deliberately does NOT apply the
// heartbeat path's version-equality gate: a policy change just occurred, so
// connected connectors must receive the new snapshot regardless of the version
// they last reported. A compile error is logged and nothing is pushed
// (default-deny — never fan out a partial snapshot). A send failure on one
// connector (wedged mailbox or mid-disconnect) is logged and skipped; that
// connector recovers on its next heartbeat.
func (p *ACLPusher) pushOnce(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), aclPushTimeout)
	defer cancel()

	// GetOrCompile captures the cache epoch before compiling and stores via an
	// epoch CAS, so a snapshot built from a now-superseded view is dropped rather
	// than poisoning the freshly-invalidated slot (ADR-013).
	snap, err := p.cache.GetOrCompile(workspaceID, func() (*clientv1.ACLSnapshot, error) {
		return p.compile(ctx, p.store, p.notifier, p.pool, workspaceID)
	})
	if err != nil {
		log.Printf("acl push: compile workspace %s: %v", workspaceID, err)
		return
	}

	clients := p.registry.ClientsForWorkspace(workspaceID)
	for _, c := range clients {
		if err := c.send(&pb.ConnectorControlMessage{
			Body: &pb.ConnectorControlMessage_AclSnapshot{AclSnapshot: snap},
		}); err != nil {
			log.Printf("acl push: send to connector %s (workspace %s): %v", c.connectorID, workspaceID, err)
		}
	}
	if len(clients) > 0 {
		log.Printf("acl push: workspace %s version=%d entries=%d connectors=%d",
			workspaceID, snap.Version, len(snap.Entries), len(clients))
	}
}
