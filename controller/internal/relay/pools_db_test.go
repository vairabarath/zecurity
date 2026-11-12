package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Relay pools (D-37 … D-41, relay-pools PR 1) over the real relay and
// provider stores on a throwaway database (PKI_TEST_DATABASE_URL): pool CRUD
// and its authorization, per-pool lists, the per-pool guard, tenant
// assignment and residency placement checks.

func (e *adminEnv) workspace(t *testing.T, slug string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $1 || '.zecurity.test') RETURNING id::text`,
		slug).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// createPool creates a pool through the API as super-admin.
func (e *adminEnv) createPool(t *testing.T, name string, region *string, residency bool) string {
	t.Helper()
	body := map[string]any{"name": name, "residency": residency}
	if region != nil {
		body["region"] = *region
	}
	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", body, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("create pool %q: %d %s", name, r.code, r.body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.ID == "" {
		t.Fatalf("create pool body %s: %v", r.body, err)
	}
	return out.ID
}

func (e *adminEnv) setRelayPool(t *testing.T, relayID, poolID string) {
	t.Helper()
	e.exec(t, `UPDATE relays SET pool_id = $2 WHERE id = $1`, relayID, poolID)
}

func (e *adminEnv) poolOfRelay(t *testing.T, relayID string) string {
	t.Helper()
	var p string
	if err := e.pool.QueryRow(context.Background(), `SELECT pool_id::text FROM relays WHERE id = $1`, relayID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *adminEnv) poolOfWorkspace(t *testing.T, wsID string) string {
	t.Helper()
	var p string
	if err := e.pool.QueryRow(context.Background(), `SELECT relay_pool_id::text FROM workspaces WHERE id = $1`, wsID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *adminEnv) publishCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.publishes
}

func strPtr(s string) *string { return &s }

func assignPath(wsID string) string  { return "/provider/tenants/" + wsID + "/relay-pool" }
func movePath(relayID string) string { return "/provider/relays/" + relayID + "/pool" }
func poolPath(poolID string) string  { return "/provider/relay-pools/" + poolID }

func listIDs(ids []string) []string { return sorted(ids...) }

// relayIDsFor returns, sorted, the relay ids a workspace's connectors receive
// through the broadcast set and through the connect-time push, and fails if
// the two lists' versions differ (D-38).
func relayIDsFor(t *testing.T, e *adminEnv, wsID string) (broadcast, connectTime []string) {
	t.Helper()
	ctx := context.Background()
	set, err := e.store.BuildRelayLists(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range set.ForWorkspace(wsID).GetRelays() {
		broadcast = append(broadcast, r.RelayId)
	}
	l, err := e.store.BuildLabelledRelayListForWorkspace(ctx, wsID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range l.GetRelays() {
		connectTime = append(connectTime, r.RelayId)
	}
	if set.ForWorkspace(wsID).GetVersion() != l.GetVersion() {
		t.Fatalf("workspace %s: broadcast list v%d and connect-time list v%d differ", wsID, set.ForWorkspace(wsID).GetVersion(), l.GetVersion())
	}
	return listIDs(broadcast), listIDs(connectTime)
}

// The migration's defaults and constraints: everything starts in the default
// pool, which can't get a region, residency, a new name or be deleted.
func TestPoolsDB_DefaultPoolAndConstraints(t *testing.T) {
	e := newAdminEnv(t, false)
	relayID := e.relay(t, "relay-a")
	wsID := e.workspace(t, "acme")
	if p := e.poolOfRelay(t, relayID); p != relaypool.DefaultPoolID {
		t.Fatalf("new relay pool %s, want default", p)
	}
	if p := e.poolOfWorkspace(t, wsID); p != relaypool.DefaultPoolID {
		t.Fatalf("new workspace pool %s, want default", p)
	}
	// DB constraints hold even without the API.
	for _, sql := range []string{
		`UPDATE relay_pools SET region = 'in-south-1' WHERE id = '` + relaypool.DefaultPoolID + `'`,
		`INSERT INTO relay_pools (name, residency) VALUES ('no-region', true)`,
		`INSERT INTO relay_pools (name, region) VALUES ('bad-region', 'IN-South')`,
	} {
		if _, err := e.pool.Exec(context.Background(), sql); err == nil {
			t.Fatalf("constraint not enforced: %s", sql)
		}
	}
	for _, body := range []any{
		map[string]any{"name": "renamed"},
		map[string]any{"region": "in-south-1"},
	} {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(relaypool.DefaultPoolID), body, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "default_pool" {
			t.Fatalf("patch default pool %v: %d %s", body, r.code, r.body)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodDelete, poolPath(relaypool.DefaultPoolID), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "default_pool" {
		t.Fatalf("delete default pool: %d %s", r.code, r.body)
	}
}

// Region codes are validated free-form identifiers (D-37); residency needs a
// region; names are unique.
func TestPoolsDB_CreateValidation(t *testing.T) {
	e := newAdminEnv(t, false)
	for _, region := range []string{"IN-south-1", "in_south", " in-south-1", "in--south", "in-", "1in", "-in", strings.Repeat("a", 33), ""} {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", map[string]any{"name": "p-" + region, "region": region}, nil)
		if r.code != http.StatusBadRequest || errorCode(t, r.body) != "invalid_region" {
			t.Fatalf("region %q: %d %s, want 400 invalid_region", region, r.code, r.body)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", map[string]any{"name": "res", "residency": true}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "residency_needs_region" {
		t.Fatalf("residency without region: %d %s", r.code, r.body)
	}
	for _, name := range []string{"", "   ", strings.Repeat("n", 65), "bad\x01name"} {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", map[string]any{"name": name}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "invalid_name" {
			t.Fatalf("name %q: %d %s, want 400 invalid_name", name, r.code, r.body)
		}
	}
	id := e.createPool(t, "india", strPtr("in-south-1"), true)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", map[string]any{"name": "india"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "name_taken" {
		t.Fatalf("duplicate name: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relay-pools", map[string]any{"name": "x", "unknown": 1}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "invalid_request" {
		t.Fatalf("unknown field: %d %s", r.code, r.body)
	}
	rows := e.auditRows(t, provider.ActionRelayPoolCreate)
	if len(rows) != 1 || rows[0]["target"] != id {
		t.Fatalf("create audit rows %+v, want one for %s", rows, id)
	}
	if after := rows[0]["details"].(map[string]any)["after"].(map[string]any); after["region"] != "in-south-1" || after["residency"] != true {
		t.Fatalf("create audit after %v", after)
	}
}

// D-41: relay-ops manage pools and move relays; region/residency and tenant
// assignment are super-admin only. A refused request writes nothing.
func TestPoolsDB_RoleBoundaries(t *testing.T) {
	e := newAdminEnv(t, false)
	ws := e.workspace(t, "acme")
	relayA := e.eligibleRelay(t, "relay-a")
	_ = e.eligibleRelay(t, "relay-d") // keeps the default pool usable

	// relay-ops: create a plain pool, rename it, move a relay into it.
	r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relay-pools", map[string]any{"name": "ops-pool"}, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("relay-ops create plain pool: %d %s", r.code, r.body)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(r.body), &created)
	if r := e.do(t, provider.RoleRelayOps, http.MethodPatch, poolPath(created.ID), map[string]any{"name": "ops-pool-2"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("relay-ops rename: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(relayA), map[string]any{"pool_id": created.ID, "reason": "rebalance"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("relay-ops move: %d %s", r.code, r.body)
	}

	auditBefore := len(e.auditRows(t, provider.ActionRelayPoolCreate)) + len(e.auditRows(t, provider.ActionRelayPoolSetPlacement)) + len(e.auditRows(t, provider.ActionTenantRelayPoolAssign))
	refused := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/provider/relay-pools", map[string]any{"name": "eu", "region": "eu-central-1"}},
		{http.MethodPost, "/provider/relay-pools", map[string]any{"name": "eu", "region": "eu-central-1", "residency": true}},
		{http.MethodPatch, poolPath(created.ID), map[string]any{"region": "eu-central-1"}},
		{http.MethodPatch, poolPath(created.ID), map[string]any{"residency": false}},
		{http.MethodPut, assignPath(ws), map[string]any{"pool_id": created.ID, "reason": "move"}},
	}
	for _, c := range refused {
		if r := e.do(t, provider.RoleRelayOps, c.method, c.path, c.body, nil); r.code != http.StatusForbidden || errorCode(t, r.body) != "forbidden" {
			t.Fatalf("relay-ops %s %s %v: %d %s, want 403", c.method, c.path, c.body, r.code, r.body)
		}
	}
	if p := e.poolOfWorkspace(t, ws); p != relaypool.DefaultPoolID {
		t.Fatalf("refused assignment changed the workspace pool to %s", p)
	}
	auditAfter := len(e.auditRows(t, provider.ActionRelayPoolCreate)) + len(e.auditRows(t, provider.ActionRelayPoolSetPlacement)) + len(e.auditRows(t, provider.ActionTenantRelayPoolAssign))
	if auditAfter != auditBefore {
		t.Fatalf("refused requests wrote %d audit row(s)", auditAfter-auditBefore)
	}
	// super-admin may do all of it.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(created.ID), map[string]any{"region": "eu-central-1"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("super-admin set region: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": created.ID, "reason": "customer request"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("super-admin assign: %d %s", r.code, r.body)
	}
}

// D-38: a workspace's connectors receive exactly its pool's eligible relays,
// the same through the broadcast set and the connect-time push; an empty pool
// sends an empty list (no fallback).
func TestPoolsDB_ListsFollowPools(t *testing.T) {
	e := newAdminEnv(t, false)
	poolA := e.createPool(t, "pool-a", strPtr("in-south-1"), false)
	a1 := e.eligibleRelay(t, "relay-a1")
	d1 := e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, a1, poolA)
	wsA, wsD := e.workspace(t, "tenant-a"), e.workspace(t, "tenant-d")

	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsA), map[string]any{"pool_id": poolA, "reason": "latency"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}
	if e.publishCount() != 1 {
		t.Fatalf("publishes after assignment = %d, want 1", e.publishCount())
	}
	if b, c := relayIDsFor(t, e, wsA); !sameIDs(b, []string{a1}) || !sameIDs(c, []string{a1}) {
		t.Fatalf("pool-A tenant lists broadcast=%v connect=%v, want [%s]", b, c, a1)
	}
	if b, _ := relayIDsFor(t, e, wsD); !sameIDs(b, []string{d1}) {
		t.Fatalf("default tenant list %v, want [%s]", b, d1)
	}
	// Repeating the assignment is a no-op: no publish, no audit.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(wsA), map[string]any{"pool_id": poolA, "reason": "again"}, nil); r.code != http.StatusNoContent || e.publishCount() != 1 {
		t.Fatalf("repeat assignment: %d %s, publishes %d", r.code, r.body, e.publishCount())
	}
	if n := len(e.auditRows(t, provider.ActionTenantRelayPoolAssign)); n != 1 {
		t.Fatalf("assignment audit rows %d, want 1", n)
	}
	// A pool whose only relay stops being eligible (an outage, not an operator
	// action) sends an empty list, never another pool's relays.
	e.exec(t, `UPDATE relays SET status = 'inactive' WHERE id = $1`, a1)
	if b, c := relayIDsFor(t, e, wsA); len(b) != 0 || len(c) != 0 {
		t.Fatalf("exhausted pool lists broadcast=%v connect=%v, want empty", b, c)
	}
}

// D-41: the per-pool guard. Draining or moving the last eligible relay of a
// pool in use is refused; of an unused pool it is allowed; a tenant can't be
// assigned to a pool without an eligible relay; only an empty pool can be
// deleted.
func TestPoolsDB_PerPoolGuard(t *testing.T) {
	e := newAdminEnv(t, false)
	poolA := e.createPool(t, "pool-a", nil, false)
	poolB := e.createPool(t, "pool-b", nil, false)
	a1 := e.eligibleRelay(t, "relay-a1")
	b1 := e.eligibleRelay(t, "relay-b1")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, a1, poolA)
	e.setRelayPool(t, b1, poolB)
	ws := e.workspace(t, "tenant-a")
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": poolA, "reason": "setup"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}

	// Pool A is in use and a1 is its only eligible relay.
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(a1), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "last_eligible_relay" {
		t.Fatalf("drain last relay of a used pool: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(a1), map[string]any{"pool_id": poolB, "reason": "x"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "last_eligible_relay" {
		t.Fatalf("move last relay out of a used pool: %d %s", r.code, r.body)
	}
	// Pool B is not used by any tenant: its last relay may be drained.
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(b1), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain last relay of an unused pool: %d %s", r.code, r.body)
	}
	// Now B has no eligible relay: a tenant can't be assigned to it.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": poolB, "reason": "x"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_has_no_eligible_relay" {
		t.Fatalf("assign to a pool without eligible relays: %d %s", r.code, r.body)
	}
	// A pool with relays or tenants can't be deleted; an empty one can.
	for _, p := range []string{poolA, poolB} {
		if r := e.do(t, provider.RoleRelayOps, http.MethodDelete, poolPath(p), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_not_empty" {
			t.Fatalf("delete non-empty pool %s: %d %s", p, r.code, r.body)
		}
	}
	empty := e.createPool(t, "empty", nil, false)
	if r := e.do(t, provider.RoleRelayOps, http.MethodDelete, poolPath(empty), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("delete empty pool: %d %s", r.code, r.body)
	}
	if rows := e.auditRows(t, provider.ActionRelayPoolDelete); len(rows) != 1 || rows[0]["target"] != empty {
		t.Fatalf("delete audit %+v", rows)
	}
	// A pool whose only relays are deleted can be deleted; they move to default.
	gone := e.createPool(t, "gone", nil, false)
	g1 := e.relay(t, "relay-g1")
	e.setRelayPool(t, g1, gone)
	e.exec(t, `UPDATE relays SET status = 'deleted' WHERE id = $1`, g1)
	if r := e.do(t, provider.RoleRelayOps, http.MethodDelete, poolPath(gone), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("delete pool with only deleted relays: %d %s", r.code, r.body)
	}
	if p := e.poolOfRelay(t, g1); p != relaypool.DefaultPoolID {
		t.Fatalf("deleted relay pool after its pool was deleted: %s, want default", p)
	}
}

// Two concurrent operations on the last two eligible relays of a used pool (a
// drain of one and a move of the other) can't both pass: each round races
// them on a fresh pool and tenant.
func TestPoolsDB_ConcurrentDrainAndMove(t *testing.T) {
	e := newAdminEnv(t, false)
	_ = e.eligibleRelay(t, "relay-d1") // keeps the default pool usable
	for round := 0; round < 8; round++ {
		suffix := fmt.Sprintf("-%d", round)
		pool := e.createPool(t, "pool"+suffix, nil, false)
		x, y := e.eligibleRelay(t, "relay-x"+suffix), e.eligibleRelay(t, "relay-y"+suffix)
		e.setRelayPool(t, x, pool)
		e.setRelayPool(t, y, pool)
		ws := e.workspace(t, "tenant"+suffix)
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": pool, "reason": "setup"}, nil); r.code != http.StatusNoContent {
			t.Fatalf("round %d assign: %d %s", round, r.code, r.body)
		}
		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			codes[0] = e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(x), nil, nil).code
		}()
		go func() {
			defer wg.Done()
			codes[1] = e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(y), map[string]any{"pool_id": relaypool.DefaultPoolID, "reason": "race"}, nil).code
		}()
		wg.Wait()
		ok := 0
		for _, c := range codes {
			switch c {
			case http.StatusNoContent:
				ok++
			case http.StatusConflict:
			default:
				t.Fatalf("round %d: unexpected status %d", round, c)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d of 2 concurrent operations succeeded (%v), want exactly 1", round, ok, codes)
		}
		var eligibleLeft int
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM relays WHERE pool_id = $1 AND `+eligible.Where, pool).Scan(&eligibleLeft); err != nil {
			t.Fatal(err)
		}
		if eligibleLeft != 1 {
			t.Fatalf("round %d: pool left with %d eligible relays, want 1", round, eligibleLeft)
		}
	}
}

// D-37: region and residency can't change while a workspace uses the pool;
// the name can.
func TestPoolsDB_PlacementImmutableWhileUsed(t *testing.T) {
	e := newAdminEnv(t, false)
	poolA := e.createPool(t, "pool-a", strPtr("in-south-1"), false)
	a1 := e.eligibleRelay(t, "relay-a1")
	e.setRelayPool(t, a1, poolA)
	ws := e.workspace(t, "tenant-a")
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(ws), map[string]any{"pool_id": poolA, "reason": "setup"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}
	for _, body := range []any{
		map[string]any{"region": "in-west-1"},
		map[string]any{"region": nil},
		map[string]any{"residency": true},
	} {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(poolA), body, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "pool_in_use" {
			t.Fatalf("patch %v of a used pool: %d %s, want 409 pool_in_use", body, r.code, r.body)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, poolPath(poolA), map[string]any{"name": "pool-a-renamed"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("rename used pool: %d %s", r.code, r.body)
	}
	rows := e.auditRows(t, provider.ActionRelayPoolUpdate)
	if len(rows) != 1 {
		t.Fatalf("rename audit rows %d, want 1", len(rows))
	}
	d := rows[0]["details"].(map[string]any)
	if d["before"].(map[string]any)["name"] != "pool-a" || d["after"].(map[string]any)["name"] != "pool-a-renamed" {
		t.Fatalf("rename before/after %v -> %v", d["before"], d["after"])
	}
}

// D-40: for a residency tenant, a placement is allowed only on a relay of an
// authorized pool: its primary pool, or its previous pool while a graceful
// migration window is open. Non-residency tenants are not checked.
func TestPoolsDB_PlacementAllowed(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	india := e.createPool(t, "india-1", strPtr("in-south-1"), true)
	india2 := e.createPool(t, "india-2", strPtr("in-south-1"), true)
	other := e.createPool(t, "eu", strPtr("eu-central-1"), false)
	r1 := e.eligibleRelay(t, "relay-in1")
	r2 := e.eligibleRelay(t, "relay-in2")
	o1 := e.eligibleRelay(t, "relay-eu1")
	_ = e.eligibleRelay(t, "relay-d1")
	e.setRelayPool(t, r1, india)
	e.setRelayPool(t, r2, india2)
	e.setRelayPool(t, o1, other)
	resTenant, plainTenant := e.workspace(t, "res-tenant"), e.workspace(t, "plain-tenant")
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(resTenant), map[string]any{"pool_id": india, "reason": "residency"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("assign: %d %s", r.code, r.body)
	}
	allowed := func(ws, relayID string) bool {
		t.Helper()
		ok, err := e.store.PlacementAllowed(ctx, ws, relayID)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !allowed(resTenant, r1) || allowed(resTenant, o1) || allowed(resTenant, "not-a-relay") {
		t.Fatal("residency tenant: in-pool relay must be allowed, out-of-pool and unknown relays refused")
	}
	if !allowed(plainTenant, o1) || !allowed(plainTenant, r1) {
		t.Fatal("non-residency tenant placements must not be checked")
	}

	// Graceful move india → india2: india stays authorized during the window.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(resTenant), map[string]any{"pool_id": india2, "reason": "rebalance", "migration": "graceful"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("graceful move: %d %s", r.code, r.body)
	}
	if !allowed(resTenant, r1) || !allowed(resTenant, r2) {
		t.Fatal("during a graceful migration both the old and the new pool must be authorized")
	}
	// The window closes.
	e.exec(t, `UPDATE workspaces SET relay_pool_previous_until = NOW() - interval '1 second' WHERE id = $1`, resTenant)
	if allowed(resTenant, r1) || !allowed(resTenant, r2) {
		t.Fatal("after the window only the new pool may be authorized")
	}
	// An immediate move revokes the old pool at once.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(resTenant), map[string]any{"pool_id": india, "reason": "revoke", "migration": "immediate"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("immediate move: %d %s", r.code, r.body)
	}
	if !allowed(resTenant, r1) || allowed(resTenant, r2) {
		t.Fatal("an immediate move must authorize only the new pool")
	}
	rows := e.auditRows(t, provider.ActionTenantRelayPoolAssign)
	last := rows[len(rows)-1]["details"].(map[string]any)
	if last["migration"] != "immediate" || last["reason"] != "revoke" || last["after"].(map[string]any)["previous_pool_id"] != nil {
		t.Fatalf("immediate move audit %v", last)
	}
	graceful := rows[len(rows)-2]["details"].(map[string]any)
	if graceful["migration"] != "graceful" || graceful["after"].(map[string]any)["previous_pool_id"] != india {
		t.Fatalf("graceful move audit %v", graceful)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(resTenant), map[string]any{"pool_id": india2, "reason": "x", "migration": "sometime"}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "invalid_migration" {
		t.Fatalf("invalid migration mode: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath(resTenant), map[string]any{"pool_id": india2}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "reason_required" {
		t.Fatalf("missing reason: %d %s", r.code, r.body)
	}
}

// A relay move is audited with both pools and the reason; moving a deleted
// relay or into an unknown pool is refused; unknown ids are 404.
func TestPoolsDB_MoveRelayAuditAndErrors(t *testing.T) {
	e := newAdminEnv(t, false)
	poolA := e.createPool(t, "pool-a", nil, false)
	a1 := e.eligibleRelay(t, "relay-a1")
	_ = e.eligibleRelay(t, "relay-d1")
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(a1), map[string]any{"pool_id": poolA, "reason": "new region"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("move: %d %s", r.code, r.body)
	}
	rows := e.auditRows(t, provider.ActionRelayMove)
	if len(rows) != 1 {
		t.Fatalf("move audit rows %d", len(rows))
	}
	d := rows[0]["details"].(map[string]any)
	if d["reason"] != "new region" || d["before"].(map[string]any)["pool_id"] != relaypool.DefaultPoolID || d["after"].(map[string]any)["pool_id"] != poolA {
		t.Fatalf("move audit %v", d)
	}
	if e.publishCount() != 1 {
		t.Fatalf("publishes after a move = %d, want 1", e.publishCount())
	}
	// Moving to the pool it is in: no-op.
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(a1), map[string]any{"pool_id": poolA, "reason": "again"}, nil); r.code != http.StatusNoContent || e.publishCount() != 1 {
		t.Fatalf("no-op move: %d %s publishes=%d", r.code, r.body, e.publishCount())
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(a1), map[string]any{"pool_id": "00000000-0000-0000-0000-0000000000ff", "reason": "x"}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "pool_not_found" {
		t.Fatalf("move to unknown pool: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath("00000000-0000-0000-0000-0000000000ee"), map[string]any{"pool_id": poolA, "reason": "x"}, nil); r.code != http.StatusNotFound {
		t.Fatalf("move unknown relay: %d %s", r.code, r.body)
	}
	gone := e.relay(t, "relay-gone")
	e.exec(t, `UPDATE relays SET status = 'deleted' WHERE id = $1`, gone)
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(gone), map[string]any{"pool_id": poolA, "reason": "x"}, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_deleted" {
		t.Fatalf("move deleted relay: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, movePath(a1), map[string]any{"pool_id": poolA}, nil); r.code != http.StatusBadRequest || errorCode(t, r.body) != "reason_required" {
		t.Fatalf("move without reason: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPut, assignPath("00000000-0000-0000-0000-0000000000dd"), map[string]any{"pool_id": poolA, "reason": "x"}, nil); r.code != http.StatusNotFound {
		t.Fatalf("assign unknown tenant: %d %s", r.code, r.body)
	}
}
