package relay

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Controller-driven failover (D-39, relay-pools PR 2) over the real store on a
// throwaway database. The reconciler is driven with an explicit clock, so the
// state machine is deterministic.

type switchLog struct {
	mu  sync.Mutex
	all []PoolTransition
}

func (l *switchLog) hook(_ context.Context, _ pgx.Tx, t PoolTransition) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, t)
	return nil
}

func (l *switchLog) actions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.all))
	for i, t := range l.all {
		out[i] = t.Action
	}
	return out
}

func (e *adminEnv) poolState(t *testing.T, poolID string) string {
	t.Helper()
	var ex, since *time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT exhausted_at, eligible_since FROM relay_pools WHERE id = $1`, poolID).Scan(&ex, &since); err != nil {
		t.Fatal(err)
	}
	return relaypool.State(ex, since)
}

func (e *adminEnv) assign(t *testing.T, wsID, primary string, failover *string) {
	t.Helper()
	body := map[string]any{"pool_id": primary, "reason": "test"}
	if failover != nil {
		body["failover_pool_id"] = *failover
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsID), body, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign %s → %s (failover %v): %d %s", wsID, primary, failover, r.code, r.body)
	}
}

func (e *adminEnv) setActive(t *testing.T, relayID string, active bool) {
	t.Helper()
	status := "inactive"
	if active {
		status = "active"
	}
	e.exec(t, `UPDATE relays SET status = $2 WHERE id = $1`, relayID, status)
}

// The full state machine: immediate failover on exhaustion, idempotent
// repeats, a hold-down that restarts on any dip, failback after it, and the
// lists following the effective pool throughout.
func TestFailoverDB_StateMachine(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	hold := 5 * time.Minute
	primary := e.createPool(t, "primary", strPtr("in-south-1"), false)
	failover := e.createPool(t, "failover", strPtr("in-south-1"), false)
	p1, f1 := e.eligibleRelay(t, "relay-p1"), e.eligibleRelay(t, "relay-f1")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, p1, primary)
	e.setRelayPool(t, f1, failover)
	ws := e.workspace(t, "tenant")
	e.assign(t, ws, primary, &failover)
	log := &switchLog{}
	reconcile := func(at time.Time) bool {
		t.Helper()
		changed, err := e.store.ReconcilePools(ctx, at, hold, log.hook)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	lists := func() []string {
		t.Helper()
		b, c := relayIDsFor(t, e, ws)
		if !sameIDs(b, c) {
			t.Fatalf("broadcast %v and connect-time %v lists differ", b, c)
		}
		return b
	}
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	if reconcile(t0) || e.poolState(t, primary) != relaypool.StateHealthy || !sameIDs(lists(), []string{p1}) {
		t.Fatal("healthy primary: no change, tenant on the primary")
	}

	// Primary exhausted → failover at once.
	e.setActive(t, p1, false)
	if !reconcile(t0) {
		t.Fatal("exhaustion must report a change")
	}
	if e.poolState(t, primary) != relaypool.StateFailedOver || !sameIDs(lists(), []string{f1}) {
		t.Fatalf("after exhaustion: state %s lists %v, want failed_over on [%s]", e.poolState(t, primary), lists(), f1)
	}
	if reconcile(t0) || reconcile(t0.Add(time.Minute)) {
		t.Fatal("repeated reconciles must be no-ops")
	}

	// Eligible again → recovering; still on failover.
	e.setActive(t, p1, true)
	t1 := t0.Add(2 * time.Minute)
	if reconcile(t1) || e.poolState(t, primary) != relaypool.StateRecovering || !sameIDs(lists(), []string{f1}) {
		t.Fatal("recovery starts the hold-down without switching back")
	}
	if reconcile(t1.Add(hold - time.Second)) {
		t.Fatal("no failback inside the hold-down")
	}
	// A dip during the hold-down restarts it.
	e.setActive(t, p1, false)
	if reconcile(t1.Add(4*time.Minute)) || e.poolState(t, primary) != relaypool.StateFailedOver {
		t.Fatal("a dip during recovery returns to failed_over without a switch")
	}
	e.setActive(t, p1, true)
	t2 := t1.Add(5 * time.Minute)
	reconcile(t2)
	if reconcile(t2.Add(hold-time.Second)) || !sameIDs(lists(), []string{f1}) {
		t.Fatal("the restarted hold-down must run its full length")
	}

	// A controller restart: a new store reads the persisted state.
	restarted := NewStore(e.pool)
	set, err := restarted.BuildRelayLists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ids := set.ForWorkspace(ws).GetRelays(); len(ids) != 1 || ids[0].RelayId != f1 {
		t.Fatalf("after a restart the tenant must stay on failover, got %v", ids)
	}

	// Failback after the full hold-down.
	if !reconcile(t2.Add(hold)) {
		t.Fatal("failback must report a change")
	}
	if e.poolState(t, primary) != relaypool.StateHealthy || !sameIDs(lists(), []string{p1}) {
		t.Fatalf("after failback: state %s lists %v", e.poolState(t, primary), lists())
	}
	if got := log.actions(); len(got) != 2 || got[0] != ActionPoolFailover || got[1] != ActionPoolFailback {
		t.Fatalf("switches %v, want [failover failback]", got)
	}
	if log.all[0].Tenants != 1 || log.all[1].ExhaustedAt == nil || !log.all[1].ExhaustedAt.Equal(t0) {
		t.Fatalf("switch details %+v", log.all)
	}
}

// No failover pool: an exhausted primary gives an empty list (fail closed);
// no state is kept, so the pool is never "recovering". Both pools exhausted:
// the failover's empty list.
func TestFailoverDB_NoFallbackWhenNoneOrBothExhausted(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	alone := e.createPool(t, "alone", nil, false)
	a1 := e.eligibleRelay(t, "relay-a1")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, a1, alone)
	ws := e.workspace(t, "tenant-alone")
	e.assign(t, ws, alone, nil)
	e.setActive(t, a1, false)
	if changed, err := e.store.ReconcilePools(ctx, time.Now(), time.Minute, nil); err != nil || changed {
		t.Fatalf("pool without failover tenants: changed=%v err=%v", changed, err)
	}
	if e.poolState(t, alone) != relaypool.StateHealthy {
		t.Fatal("a pool nobody fails over from keeps no state")
	}
	if b, _ := relayIDsFor(t, e, ws); len(b) != 0 {
		t.Fatalf("exhausted pool without failover: list %v, want empty", b)
	}

	primary := e.createPool(t, "primary", nil, false)
	failover := e.createPool(t, "failover", nil, false)
	p1, f1 := e.eligibleRelay(t, "relay-p1"), e.eligibleRelay(t, "relay-f1")
	e.setRelayPool(t, p1, primary)
	e.setRelayPool(t, f1, failover)
	ws2 := e.workspace(t, "tenant-both")
	e.assign(t, ws2, primary, &failover)
	e.setActive(t, p1, false)
	e.setActive(t, f1, false)
	if _, err := e.store.ReconcilePools(ctx, time.Now(), time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if b, c := relayIDsFor(t, e, ws2); len(b) != 0 || len(c) != 0 {
		t.Fatalf("both pools exhausted: lists %v %v, want empty (never the default pool)", b, c)
	}
	// Removing the failover pool clears the primary's state.
	e.setActive(t, f1, true)
	e.exec(t, `UPDATE workspaces SET relay_failover_pool_id = NULL WHERE id = $1`, ws2)
	if _, err := e.store.ReconcilePools(ctx, time.Now(), time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if e.poolState(t, primary) != relaypool.StateHealthy {
		t.Fatal("state must clear once no tenant fails over from the pool")
	}
}

// D-39 assignment rules: different pools, same region, residency needs a
// residency failover in the same region, and the failover pool must have an
// eligible relay. Omitting the failover pool removes it.
func TestFailoverDB_AssignmentRules(t *testing.T) {
	e := newAdminEnv(t, false)
	inA := e.createPool(t, "in-a", strPtr("in-south-1"), false)
	inB := e.createPool(t, "in-b", strPtr("in-south-1"), false)
	eu := e.createPool(t, "eu", strPtr("eu-central-1"), false)
	resA := e.createPool(t, "res-a", strPtr("in-south-1"), true)
	resB := e.createPool(t, "res-b", strPtr("in-south-1"), true)
	empty := e.createPool(t, "in-empty", strPtr("in-south-1"), false)
	for name, pool := range map[string]string{"a": inA, "b": inB, "eu": eu, "ra": resA, "rb": resB} {
		e.setRelayPool(t, e.eligibleRelay(t, "relay-"+name), pool)
	}
	_ = e.eligibleRelay(t, "relay-d1")
	ws := e.workspace(t, "tenant")

	refused := []struct {
		primary, failover, code string
		status                  int
	}{
		{inA, inA, "failover_same_as_primary", http.StatusBadRequest},
		{inA, eu, "failover_region_mismatch", http.StatusBadRequest},
		{inA, relaypool.DefaultPoolID, "failover_region_mismatch", http.StatusBadRequest},
		{resA, inB, "failover_needs_residency", http.StatusBadRequest},
		{inA, empty, "pool_has_no_eligible_relay", http.StatusConflict},
		{inA, "00000000-0000-0000-0000-0000000000ff", "pool_not_found", http.StatusBadRequest},
	}
	for _, c := range refused {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws),
			map[string]any{"pool_id": c.primary, "failover_pool_id": c.failover, "reason": "x"}, nil)
		if r.code != c.status || errorCode(t, r.body) != c.code {
			t.Fatalf("primary %s failover %s: %d %s, want %d %s", c.primary, c.failover, r.code, r.body, c.status, c.code)
		}
	}
	if p := e.poolOfWorkspace(t, ws); p != relaypool.DefaultPoolID {
		t.Fatalf("refused assignments changed the pool to %s", p)
	}
	// Allowed: same region; residency → residency in the same region; a
	// non-residency primary may fail over to a residency pool of its region.
	e.assign(t, ws, inA, &inB)
	e.assign(t, ws, resA, &resB)
	e.assign(t, ws, inA, &resA)
	// Changing only the failover pool is a change (audited); its migration
	// mode doesn't apply (the primary didn't move).
	rows := e.auditRows(t, provider.ActionTenantRelayPoolAssign)
	last := rows[len(rows)-1]["details"].(map[string]any)
	if last["after"].(map[string]any)["failover_pool_id"] != resA {
		t.Fatalf("assignment audit after %v", last["after"])
	}
	// Omitting the failover pool removes it.
	e.assign(t, ws, inA, nil)
	var fo *string
	if err := e.pool.QueryRow(context.Background(), `SELECT relay_failover_pool_id::text FROM workspaces WHERE id = $1`, ws).Scan(&fo); err != nil || fo != nil {
		t.Fatalf("failover pool after omitting it: %v %v", fo, err)
	}
	// The DB constraint holds without the API too.
	if _, err := e.pool.Exec(context.Background(), `UPDATE workspaces SET relay_failover_pool_id = relay_pool_id WHERE id = $1`, ws); err == nil {
		t.Fatal("failover = primary must be refused by the database")
	}
}

// The per-pool guard counts failover use, and failover doesn't excuse
// draining a primary's last relay (D-41). Placement authorization includes
// the failover pool.
func TestFailoverDB_GuardAndPlacement(t *testing.T) {
	e := newAdminEnv(t, false)
	primary := e.createPool(t, "primary", strPtr("in-south-1"), true)
	failover := e.createPool(t, "failover", strPtr("in-south-1"), true)
	p1, f1 := e.eligibleRelay(t, "relay-p1"), e.eligibleRelay(t, "relay-f1")
	other := e.eligibleRelay(t, "relay-other")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, p1, primary)
	e.setRelayPool(t, f1, failover)
	ws := e.workspace(t, "tenant")
	e.assign(t, ws, primary, &failover)

	for _, id := range []string{p1, f1} {
		if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(id), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "last_eligible_relay" {
			t.Fatalf("drain last relay of a primary/failover pool %s: %d %s", id, r.code, r.body)
		}
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodDelete, poolPath(failover), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_not_empty" {
		t.Fatalf("delete a pool used as failover: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(failover), map[string]any{"region": "in-west-1"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_in_use" {
		t.Fatalf("change region of a pool used as failover: %d %s", r.code, r.body)
	}
	ctx := context.Background()
	// The graceful assignment keeps the previous (default) pool authorized for
	// its window; close it so only the primary and failover pools remain.
	e.exec(t, `UPDATE workspaces SET relay_pool_previous_until = NOW() - interval '1 second' WHERE id = $1`, ws)
	for relayID, want := range map[string]bool{p1: true, f1: true, other: false} {
		if ok, err := e.store.PlacementAllowed(ctx, ws, relayID); err != nil || ok != want {
			t.Fatalf("placement on %s: %v %v, want %v", relayID, ok, err, want)
		}
	}
}

// A failover pool can be added to a tenant whose primary is already down:
// only newly assigned pools need an eligible relay. The tenant then fails over
// at the next reconcile.
func TestFailoverDB_AddFailoverWhilePrimaryIsDown(t *testing.T) {
	e := newAdminEnv(t, false)
	primary := e.createPool(t, "primary", nil, false)
	backup := e.createPool(t, "backup", nil, false)
	p1, b1 := e.eligibleRelay(t, "relay-p1"), e.eligibleRelay(t, "relay-b1")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, p1, primary)
	e.setRelayPool(t, b1, backup)
	ws := e.workspace(t, "tenant")
	e.assign(t, ws, primary, nil)
	e.setActive(t, p1, false) // the primary goes down

	e.assign(t, ws, primary, &backup)
	if _, err := e.store.ReconcilePools(context.Background(), time.Now(), time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := relayIDsFor(t, e, ws); !sameIDs(b, []string{b1}) {
		t.Fatalf("after adding a failover to a down primary: list %v, want [%s]", b, b1)
	}
	// A newly chosen primary without an eligible relay is still refused.
	down := e.createPool(t, "down", nil, false)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": down, "failover_pool_id": backup, "reason": "x"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_has_no_eligible_relay" {
		t.Fatalf("new primary without relays: %d %s", r.code, r.body)
	}
}
