package relay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Relay pools PR 3 (D-40) over the real store: the policy a relay is sent,
// version bumps on every change to a pool's authorized set or residency, and
// versions that only increase for a relay, also across a move between pools.

func (e *adminEnv) policyVersion(t *testing.T, poolID string) int64 {
	t.Helper()
	var v int64
	if err := e.pool.QueryRow(context.Background(), `SELECT policy_version FROM relay_pools WHERE id = $1`, poolID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPolicyDB_AllowedSetAndVersions(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	india := e.createPool(t, "india", strPtr("in-south-1"), true)
	india2 := e.createPool(t, "india-2", strPtr("in-south-1"), true)
	plain := e.createPool(t, "plain", nil, false)
	r1, r2, rp := e.eligibleRelay(t, "relay-in1"), e.eligibleRelay(t, "relay-in2"), e.eligibleRelay(t, "relay-plain")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, r1, india)
	e.setRelayPool(t, r2, india2)
	e.setRelayPool(t, rp, plain)
	wsA, wsB := e.workspace(t, "tenant-a"), e.workspace(t, "tenant-b")

	policy := func(relayID string) WorkspacePolicy {
		t.Helper()
		p, err := e.store.RelayWorkspacePolicy(ctx, relayID)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := policy(rp); p.Enforced || len(p.Workspace) != 0 || p.Version == 0 {
		t.Fatalf("non-residency pool policy %+v, want not enforced, no list, a version", p)
	}
	if p := policy(r1); !p.Enforced || len(p.Workspace) != 0 {
		t.Fatalf("unused residency pool policy %+v, want enforced with an empty list", p)
	}

	// Assigning A to india (immediate) bumps india and lists A.
	v0 := e.policyVersion(t, india)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsA), map[string]any{"pool_id": india, "reason": "x", "migration": "immediate"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}
	if v := e.policyVersion(t, india); v <= v0 {
		t.Fatalf("assignment did not bump the pool: %d → %d", v0, v)
	}
	if p := policy(r1); !sameIDs(p.Workspace, []string{wsA}) {
		t.Fatalf("india allows %v, want [%s]", p.Workspace, wsA)
	}
	// B fails over to india: allowed there too.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsB), map[string]any{"pool_id": india2, "failover_pool_id": india, "reason": "x", "migration": "immediate"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign B: %d %s", r.code, r.body)
	}
	if p := policy(r1); !sameIDs(p.Workspace, sorted(wsA, wsB)) {
		t.Fatalf("india allows %v, want A (primary) and B (failover)", p.Workspace)
	}

	// Graceful move of A to india-2: india keeps A until the window closes.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsA), map[string]any{"pool_id": india2, "reason": "x"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("graceful move: %d %s", r.code, r.body)
	}
	if p := policy(r1); !sameIDs(p.Workspace, sorted(wsA, wsB)) {
		t.Fatalf("during the window india allows %v, want A still there", p.Workspace)
	}
	vBefore := e.policyVersion(t, india)
	if n, err := e.store.ExpireMigrationWindows(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("an open window must not close: n=%d err=%v", n, err)
	}
	if n, err := e.store.ExpireMigrationWindows(ctx, time.Now().Add(6*time.Minute)); err != nil || n != 1 {
		t.Fatalf("closing the ended window: n=%d err=%v", n, err)
	}
	if p := policy(r1); !sameIDs(p.Workspace, []string{wsB}) || p.Version <= uint64(vBefore) {
		t.Fatalf("after the window india allows %v (v%d, was v%d), want only B with a new version", p.Workspace, p.Version, vBefore)
	}

	// A relay moved between pools always sees a newer version.
	before := policy(rp).Version
	e.exec(t, `UPDATE relays SET drain_started_at = NULL WHERE id = $1`, rp)
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(r2), map[string]any{"pool_id": plain, "reason": "x"}, nil); r.code != http.StatusConflict {
		// r2 is india-2's only relay and india-2 is in use: the guard holds.
		t.Fatalf("moving the last relay of a used pool: %d %s", r.code, r.body)
	}
	extra := e.eligibleRelay(t, "relay-in1b")
	e.setRelayPool(t, extra, india)
	oldVersion := policy(extra).Version
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(extra), map[string]any{"pool_id": plain, "reason": "x"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("move: %d %s", r.code, r.body)
	}
	if p := policy(extra); p.Version <= oldVersion || p.Version <= before || p.Enforced {
		t.Fatalf("after a move: version %d (was %d in the old pool), enforced %v", p.Version, oldVersion, p.Enforced)
	}

	// Turning residency off (pool unused) bumps the version.
	unused := e.createPool(t, "unused", strPtr("in-south-1"), true)
	vu := e.policyVersion(t, unused)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(unused), map[string]any{"residency": false}, nil); r.code != http.StatusNoContent {
		t.Fatalf("residency off: %d %s", r.code, r.body)
	}
	if v := e.policyVersion(t, unused); v <= vu {
		t.Fatalf("residency change did not bump: %d → %d", vu, v)
	}

	// Acks are recorded only when they change.
	if err := e.store.RecordPolicyAck(ctx, r1, true, uint64(policy(r1).Version)); err != nil {
		t.Fatal(err)
	}
	var supported bool
	var acked int64
	if err := e.pool.QueryRow(ctx, `SELECT policy_supported, policy_version_acked FROM relays WHERE id = $1`, r1).Scan(&supported, &acked); err != nil || !supported || uint64(acked) != policy(r1).Version {
		t.Fatalf("ack stored supported=%v acked=%d err=%v", supported, acked, err)
	}
}
