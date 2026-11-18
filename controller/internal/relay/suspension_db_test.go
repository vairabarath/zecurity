package relay

import (
	"context"
	"net/http"
	"testing"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 23 S1 (D-45) over the real store: a suspended workspace gets an
// empty relay list on both delivery paths (never the default pool's), leaves
// its residency pools' allowlists, and its pools get new policy versions;
// resume restores both. tenantlifecycle drives the same transitions through
// the API; here the suspension is written directly (that package imports
// this one).

func (e *adminEnv) setSuspended(t *testing.T, wsID string, suspended bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if suspended {
		_, err = tx.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, wsID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, wsID)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := BumpWorkspacePoolPoliciesTx(ctx, tx, wsID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSuspensionDB_RelayListsAndAllowlists(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	india := e.createPool(t, "india", strPtr("in-south-1"), true)
	r1 := e.eligibleRelay(t, "relay-in1")
	d1 := e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, r1, india)
	wsA, wsB, wsD := e.workspace(t, "tenant-a"), e.workspace(t, "tenant-b"), e.workspace(t, "tenant-d")
	for _, ws := range []string{wsA, wsB} {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": india, "reason": "x", "migration": "immediate"}, nil); r.code != http.StatusNoContent {
			t.Fatalf("assign %s: %d %s", ws, r.code, r.body)
		}
	}
	allowed := func() []string {
		t.Helper()
		p, err := e.store.RelayWorkspacePolicy(ctx, r1)
		if err != nil {
			t.Fatal(err)
		}
		return p.Workspace
	}
	if got := allowed(); !sameIDs(got, sorted(wsA, wsB)) {
		t.Fatalf("india allows %v, want A and B", got)
	}

	// Suspend A (residency pool) and D (default pool).
	v0 := e.policyVersion(t, india)
	e.setSuspended(t, wsA, true)
	e.setSuspended(t, wsD, true)
	if v := e.policyVersion(t, india); v <= v0 {
		t.Fatalf("suspension did not bump the pool's policy version: %d → %d", v0, v)
	}
	if got := allowed(); !sameIDs(got, []string{wsB}) {
		t.Fatalf("india allows %v while A is suspended, want only B", got)
	}
	for _, ws := range []string{wsA, wsD} {
		if b, c := relayIDsFor(t, e, ws); len(b) != 0 || len(c) != 0 {
			t.Fatalf("suspended %s lists broadcast=%v connect=%v, want empty (never the default pool)", ws, b, c)
		}
	}
	if b, c := relayIDsFor(t, e, wsB); !sameIDs(b, []string{r1}) || !sameIDs(c, []string{r1}) {
		t.Fatalf("active B lists broadcast=%v connect=%v, want [%s]", b, c, r1)
	}

	// Resume restores the lists and the allowlist, with a new version.
	v1 := e.policyVersion(t, india)
	e.setSuspended(t, wsA, false)
	e.setSuspended(t, wsD, false)
	if v := e.policyVersion(t, india); v <= v1 {
		t.Fatalf("resume did not bump the pool's policy version: %d → %d", v1, v)
	}
	if got := allowed(); !sameIDs(got, sorted(wsA, wsB)) {
		t.Fatalf("india allows %v after resume, want A and B", got)
	}
	if b, c := relayIDsFor(t, e, wsA); !sameIDs(b, []string{r1}) || !sameIDs(c, []string{r1}) {
		t.Fatalf("resumed A lists broadcast=%v connect=%v, want [%s]", b, c, r1)
	}
	if b, _ := relayIDsFor(t, e, wsD); !sameIDs(b, []string{d1}) {
		t.Fatalf("resumed D list %v, want the default pool's [%s]", b, d1)
	}
}
