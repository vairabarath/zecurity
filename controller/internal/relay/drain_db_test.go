package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
)

// Sprint 22 Phase D (D-32, D-31) over the real relay and provider stores on a
// throwaway database (PKI_TEST_DATABASE_URL). Drain removes a relay from the
// published list; resume restores it; the last eligible relay can't be drained.

func (e *adminEnv) eligibleRelay(t *testing.T, name string) string {
	t.Helper()
	id := e.relay(t, name)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE relays SET status = 'active', capacity_label = 'high', public_addr = $2 WHERE id = $1`,
		id, name+".relay.test:9093"); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *adminEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// published is the relay ids in the list connectors receive, and its version.
func (e *adminEnv) published(t *testing.T) ([]string, uint64) {
	t.Helper()
	list, err := e.store.BuildLabelledRelayList(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(list.Relays))
	for _, r := range list.Relays {
		ids = append(ids, r.RelayId)
	}
	sort.Strings(ids)
	return ids, list.Version
}

func (e *adminEnv) draining(t *testing.T, id string) bool {
	t.Helper()
	var draining bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT drain_started_at IS NOT NULL FROM relays WHERE id = $1`, id).Scan(&draining); err != nil {
		t.Fatal(err)
	}
	return draining
}

func (e *adminEnv) drainHooks() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.drains...)
}

func drainPath(id string) string  { return "/provider/relays/" + id + "/drain" }
func resumePath(id string) string { return "/provider/relays/" + id + "/resume" }

func sorted(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func sameIDs(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

// P1: the list, the guard and the read API share one predicate. Rows the old
// SQL accepted but the Go loop dropped (empty address, public scope without an
// observed IP) must not count as eligible anywhere.
func TestDrainDB_SharedEligibilityPredicate(t *testing.T) {
	e := newAdminEnv(t, false)
	byAddr := e.eligibleRelay(t, "by-addr")
	byObserved := e.eligibleRelay(t, "by-observed")
	e.exec(t, `UPDATE relays SET public_addr = NULL, address_scope = 'public', observed_ip = '198.51.100.7', capacity_label = 'medium' WHERE id = $1`, byObserved)
	emptyAddr := e.eligibleRelay(t, "empty-addr")
	e.exec(t, `UPDATE relays SET public_addr = '', address_scope = 'private' WHERE id = $1`, emptyAddr)
	publicNoIP := e.eligibleRelay(t, "public-no-ip")
	e.exec(t, `UPDATE relays SET public_addr = NULL, address_scope = 'public', observed_ip = NULL WHERE id = $1`, publicNoIP)
	low := e.eligibleRelay(t, "low-tier")
	e.exec(t, `UPDATE relays SET capacity_label = 'low' WHERE id = $1`, low)
	inactive := e.eligibleRelay(t, "inactive")
	e.exec(t, `UPDATE relays SET status = 'inactive' WHERE id = $1`, inactive)
	drained := e.eligibleRelay(t, "drained")
	e.exec(t, `UPDATE relays SET drain_started_at = NOW() WHERE id = $1`, drained)

	want := sorted(byAddr, byObserved)
	got, _ := e.published(t)
	if !sameIDs(got, want) {
		t.Fatalf("published list %v, want %v", got, want)
	}
	rows, err := e.pool.Query(context.Background(), `SELECT id::text FROM relays WHERE `+eligible.Where+` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var viaPredicate []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		viaPredicate = append(viaPredicate, id)
	}
	rows.Close()
	if !sameIDs(viaPredicate, want) {
		t.Fatalf("eligible.Where selects %v, want the published list %v", viaPredicate, want)
	}

	// The guard agrees: with the two real candidates draining one is fine,
	// and the second is the last one even though five other rows exist.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(byAddr), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain with another eligible relay: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(byObserved), nil, nil); r.code != http.StatusConflict || errorCode(t, r.body) != "last_eligible_relay" {
		t.Fatalf("drain of the last published relay: %d %s (relays the list never offers must not count)", r.code, r.body)
	}
}

// AT-D1, AT-D4: drain leaves the list and changes the version; resume restores
// both. The connect-time push uses the same BuildLabelledRelayList, so a
// reconnecting connector gets the list without the drained relay (AT-D12).
func TestDrainDB_DrainAndResume(t *testing.T) {
	e := newAdminEnv(t, false)
	a, b := e.eligibleRelay(t, "relay-a"), e.eligibleRelay(t, "relay-b")
	before, v0 := e.published(t)
	if !sameIDs(before, sorted(a, b)) {
		t.Fatalf("setup: published %v", before)
	}

	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(a), map[string]string{"reason": "  kernel upgrade  "}, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain: %d %s", r.code, r.body)
	}
	during, v1 := e.published(t)
	if !sameIDs(during, []string{b}) || v1 == v0 {
		t.Fatalf("after drain: published %v version changed=%v, want only relay-b and a new version", during, v1 != v0)
	}
	if !e.draining(t, a) || e.status(t, a) != "active" {
		t.Fatalf("drained relay: draining=%v status=%s, want draining and still active", e.draining(t, a), e.status(t, a))
	}
	audits := e.auditRows(t, provider.ActionRelayDrain)
	if len(audits) != 1 {
		t.Fatalf("relay.drain audit rows: %d", len(audits))
	}
	if d := audits[0]["details"].(map[string]any); audits[0]["email"] != "admin@provider.test" || audits[0]["target"] != a ||
		audits[0]["ip"] != "192.0.2.10" || d["reason"] != "kernel upgrade" {
		t.Fatalf("relay.drain audit row: %+v", audits[0])
	}

	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, resumePath(a), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("resume: %d %s", r.code, r.body)
	}
	after, v2 := e.published(t)
	if !sameIDs(after, before) || v2 != v0 {
		t.Fatalf("after resume: published %v version %d, want %v and the pre-drain version %d", after, v2, before, v0)
	}
	if e.draining(t, a) {
		t.Fatal("resumed relay still draining")
	}
	resumes := e.auditRows(t, provider.ActionRelayResume)
	if len(resumes) != 1 || resumes[0]["target"] != a || resumes[0]["ip"] != "192.0.2.10" ||
		resumes[0]["details"].(map[string]any)["drained_since"] == nil {
		t.Fatalf("relay.resume audit rows: %+v", resumes)
	}
	if hooks := e.drainHooks(); !sameIDs(hooks, []string{"drain:" + a, "resume:" + a}) {
		t.Fatalf("drain hooks %v, want one drain then one resume", hooks)
	}
	if e.fanoutCount() != 0 {
		t.Fatalf("drain/resume ran the revoke fan-out %d times", e.fanoutCount())
	}
}

// AT-D2: the only eligible relay can't be drained; a non-eligible relay can.
func TestDrainDB_LastEligibleGuard(t *testing.T) {
	e := newAdminEnv(t, false)
	only := e.eligibleRelay(t, "only")
	low := e.eligibleRelay(t, "low")
	e.exec(t, `UPDATE relays SET capacity_label = 'low' WHERE id = $1`, low)
	inactive := e.eligibleRelay(t, "inactive")
	e.exec(t, `UPDATE relays SET status = 'inactive' WHERE id = $1`, inactive)

	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(only), map[string]string{"reason": "x"}, nil)
	if r.code != http.StatusConflict || errorCode(t, r.body) != "last_eligible_relay" {
		t.Fatalf("drain of the only eligible relay: %d %s", r.code, r.body)
	}
	if e.draining(t, only) || len(e.auditRows(t, provider.ActionRelayDrain)) != 0 || len(e.drainHooks()) != 0 {
		t.Fatal("a refused drain changed state, audited or ran the hook")
	}
	for _, id := range []string{low, inactive} {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(id), nil, nil); r.code != http.StatusNoContent {
			t.Fatalf("drain of a non-eligible relay: %d %s", r.code, r.body)
		}
	}
	if ids, _ := e.published(t); !sameIDs(ids, []string{only}) {
		t.Fatalf("published %v, want the one eligible relay", ids)
	}
}

// AT-D3: two operators drain the last two eligible relays at once. Exactly
// one wins, and the published list still holds one relay. Repeated to give
// the race several chances.
func TestDrainDB_ConcurrentDrainOfLastTwo(t *testing.T) {
	e := newAdminEnv(t, false)
	for i := 0; i < 10; i++ {
		e.exec(t, `UPDATE relays SET status = 'deleted' WHERE status <> 'deleted'`)
		a := e.eligibleRelay(t, fmt.Sprintf("race-%d-a", i))
		b := e.eligibleRelay(t, fmt.Sprintf("race-%d-b", i))
		start := make(chan struct{})
		var wg sync.WaitGroup
		out := make([]resp, 2)
		for j, id := range []string{a, b} {
			wg.Add(1)
			go func(j int, id string) {
				defer wg.Done()
				<-start
				role := provider.RoleSuperAdmin
				if j == 1 {
					role = provider.RoleRelayOps
				}
				out[j] = e.do(t, role, http.MethodPost, drainPath(id), map[string]string{"reason": "race"}, nil)
			}(j, id)
		}
		close(start)
		wg.Wait()
		ok, refused := 0, 0
		for _, r := range out {
			switch {
			case r.code == http.StatusNoContent:
				ok++
			case r.code == http.StatusConflict && errorCode(t, r.body) == "last_eligible_relay":
				refused++
			default:
				t.Fatalf("round %d: unexpected response %d %s", i, r.code, r.body)
			}
		}
		ids, _ := e.published(t)
		if ok != 1 || refused != 1 || len(ids) != 1 {
			t.Fatalf("round %d: %d drained, %d refused, published %v; want 1, 1 and one relay left", i, ok, refused, ids)
		}
	}
}

// AT-D7: state rules and no-ops.
func TestDrainDB_StateRules(t *testing.T) {
	e := newAdminEnv(t, false)
	e.eligibleRelay(t, "spare-1")
	e.eligibleRelay(t, "spare-2")
	pending := e.relay(t, "pending")
	revoked := e.eligibleRelay(t, "revoked")
	e.exec(t, `UPDATE relays SET status = 'revoked' WHERE id = $1`, revoked)
	deleted := e.eligibleRelay(t, "deleted")
	e.exec(t, `UPDATE relays SET status = 'deleted' WHERE id = $1`, deleted)
	revokedDraining := e.eligibleRelay(t, "revoked-draining")
	e.exec(t, `UPDATE relays SET status = 'revoked', drain_started_at = NOW() WHERE id = $1`, revokedDraining)

	for _, tc := range []struct {
		name, path, code string
		status           int
	}{
		{"drain pending", drainPath(pending), "invalid_state", http.StatusConflict},
		{"drain revoked", drainPath(revoked), "already_revoked", http.StatusConflict},
		{"drain deleted", drainPath(deleted), "already_deleted", http.StatusConflict},
		{"drain revoked+draining", drainPath(revokedDraining), "already_revoked", http.StatusConflict},
		{"resume revoked", resumePath(revoked), "already_revoked", http.StatusConflict},
		{"resume revoked+draining", resumePath(revokedDraining), "already_revoked", http.StatusConflict},
		{"resume deleted", resumePath(deleted), "already_deleted", http.StatusConflict},
		{"drain non-uuid", "/provider/relays/not-a-uuid/drain", "not_found", http.StatusNotFound},
		{"resume non-uuid", "/provider/relays/1;DROP/resume", "not_found", http.StatusNotFound},
		{"drain unknown", drainPath("00000000-0000-4000-8000-000000000000"), "not_found", http.StatusNotFound},
		{"resume unknown", resumePath("00000000-0000-4000-8000-000000000000"), "not_found", http.StatusNotFound},
	} {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, tc.path, nil, nil)
		if r.code != tc.status || errorCode(t, r.body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, r.code, r.body, tc.status, tc.code)
		}
	}
	if e.draining(t, pending) || e.draining(t, revoked) {
		t.Fatal("a refused drain changed state")
	}

	// Repeat drain and resume of a non-draining relay: 204, no audit, no hook.
	target := e.eligibleRelay(t, "target")
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, resumePath(target), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("resume of a non-draining relay: %d %s", r.code, r.body)
	}
	for i := 0; i < 2; i++ {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(target), nil, nil); r.code != http.StatusNoContent {
			t.Fatalf("drain #%d: %d %s", i+1, r.code, r.body)
		}
	}
	if n := len(e.auditRows(t, provider.ActionRelayDrain)); n != 1 {
		t.Fatalf("relay.drain audit rows %d, want 1 (the repeat is a no-op)", n)
	}
	if n := len(e.auditRows(t, provider.ActionRelayResume)); n != 0 {
		t.Fatalf("relay.resume audit rows %d, want 0", n)
	}
	if hooks := e.drainHooks(); !sameIDs(hooks, []string{"drain:" + target}) {
		t.Fatalf("drain hooks %v, want exactly one drain", hooks)
	}
	// The repeat keeps the original start time (the 120 s window runs from it).
	var started time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT drain_started_at FROM relays WHERE id = $1`, target).Scan(&started)
	e.exec(t, `UPDATE relays SET drain_started_at = drain_started_at - interval '1 hour' WHERE id = $1`, target)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(target), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain #3: %d %s", r.code, r.body)
	}
	var again time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT drain_started_at FROM relays WHERE id = $1`, target).Scan(&again)
	if !again.Equal(started.Add(-time.Hour)) {
		t.Fatalf("a repeat drain moved drain_started_at: %v -> %v", started.Add(-time.Hour), again)
	}
}

// AT-D5 (D-31): relay-ops may drain and resume; revoke/delete stay super-admin.
func TestDrainDB_RelayOpsMayDrainAndResume(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(a), map[string]string{"reason": "maintenance"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("relay-ops drain: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, resumePath(a), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("relay-ops resume: %d %s", r.code, r.body)
	}
	if rows := e.auditRows(t, provider.ActionRelayDrain); len(rows) != 1 || rows[0]["email"] != "ops@provider.test" {
		t.Fatalf("relay-ops drain audit: %+v", rows)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays/"+a+"/revoke", confirm("relay-a", "x"), nil); r.code != http.StatusForbidden {
		t.Fatalf("relay-ops revoke: %d", r.code)
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodDelete, "/provider/relays/"+a, confirm("relay-a", "x"), nil); r.code != http.StatusForbidden {
		t.Fatalf("relay-ops delete: %d", r.code)
	}
	if r := e.do(t, "auditor", http.MethodPost, drainPath(a), nil, nil); r.code != http.StatusForbidden || errorCode(t, r.body) != "forbidden" {
		t.Fatalf("unknown role drain: %d %s", r.code, r.body)
	}
}

func TestDrainDB_BodyValidation(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")
	for _, tc := range []struct {
		name, path string
		body       any
		code       string
	}{
		{"drain not json", drainPath(a), "reason=x", "invalid_request"},
		{"drain unknown field", drainPath(a), map[string]string{"reason": "x", "force": "1"}, "invalid_request"},
		{"drain long reason", drainPath(a), map[string]string{"reason": strings.Repeat("r", 501)}, "reason_too_long"},
		{"resume not json", resumePath(a), "x", "invalid_request"},
		{"resume unknown field", resumePath(a), map[string]string{"reason": "x"}, "invalid_request"},
	} {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, tc.path, tc.body, nil)
		if r.code != http.StatusBadRequest || errorCode(t, r.body) != tc.code {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, r.code, r.body, tc.code)
		}
	}
	if e.draining(t, a) {
		t.Fatal("an invalid request drained the relay")
	}
	// 500 code points of emoji are within the limit (counted like the console).
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(a), map[string]string{"reason": strings.Repeat("😀", 500)}, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain with a 500-code-point reason: %d %s", r.code, r.body)
	}
	// An empty object resumes; a blank reason drains without one.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, resumePath(a), map[string]string{}, nil); r.code != http.StatusNoContent {
		t.Fatalf("resume with {}: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(a), map[string]string{"reason": "   "}, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain with a blank reason: %d %s", r.code, r.body)
	}
	rows := e.auditRows(t, provider.ActionRelayDrain)
	if len(rows) != 2 {
		t.Fatalf("relay.drain audit rows: %d", len(rows))
	}
	if _, has := rows[1]["details"].(map[string]any)["reason"]; has {
		t.Fatalf("a drain without a reason stored one: %+v", rows[1])
	}
}

// D-14: a failed audit write rolls the drain/resume back; the 500 leaks nothing.
func TestDrainDB_AuditFailureRollsBack(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")
	draining := e.eligibleRelay(t, "relay-c")
	e.exec(t, `UPDATE relays SET drain_started_at = NOW() WHERE id = $1`, draining)
	e.exec(t, `ALTER TABLE provider_audit_logs ADD CONSTRAINT phase_d_test_block CHECK (action NOT IN ('relay.drain', 'relay.resume'))`)

	for _, path := range []string{drainPath(a), resumePath(draining)} {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, path, map[string]string{}, nil)
		if r.code != http.StatusInternalServerError || strings.TrimSpace(r.body) != `{"error":"server_error"}` {
			t.Fatalf("%s with failing audit: %d %q", path, r.code, r.body)
		}
	}
	if e.draining(t, a) || !e.draining(t, draining) || len(e.drainHooks()) != 0 {
		t.Fatalf("not rolled back: a draining=%v c draining=%v hooks=%v", e.draining(t, a), e.draining(t, draining), e.drainHooks())
	}
}

// The reconcile source: draining relays that may still heartbeat.
func TestDrainDB_ListDrainingRelayIDs(t *testing.T) {
	e := newAdminEnv(t, false)
	active := e.eligibleRelay(t, "active")
	inactive := e.eligibleRelay(t, "inactive")
	revoked := e.eligibleRelay(t, "revoked")
	e.eligibleRelay(t, "not-draining")
	e.exec(t, `UPDATE relays SET drain_started_at = NOW() WHERE id IN ($1, $2, $3)`, active, inactive, revoked)
	e.exec(t, `UPDATE relays SET status = 'inactive' WHERE id = $1`, inactive)
	e.exec(t, `UPDATE relays SET status = 'revoked' WHERE id = $1`, revoked)
	ids, err := e.store.ListDrainingRelayIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if !sameIDs(ids, sorted(active, inactive)) {
		t.Fatalf("draining relays %v, want the active and inactive ones", ids)
	}
}

// connection_count_at is the measured time passed in, not the write time.
func TestDrainDB_RecordHeartbeatStoresMeasuredAt(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.eligibleRelay(t, "relay-a")
	measured := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	if err := e.store.RecordHeartbeat(context.Background(), id, "s", time.Now().Add(time.Hour), "1.0", "h",
		"", 0, "", "relay-a.relay.test:9093", 7, 100, measured); err != nil {
		t.Fatal(err)
	}
	var count int
	var at time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT connection_count, connection_count_at FROM relays WHERE id = $1`, id).Scan(&count, &at); err != nil {
		t.Fatal(err)
	}
	if count != 7 || !at.Equal(measured) {
		t.Fatalf("stored count %d at %v, want 7 at %v", count, at, measured)
	}
}

// The post-commit hooks (drain/resume and revoke/delete) run after the
// commit, and on a context detached from the request: a client that
// disconnects right after the commit can't cancel the broadcast.
func TestDrainDB_HooksRunAfterCommitOnDetachedContext(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")
	c := e.eligibleRelay(t, "relay-c")

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	type seen struct {
		committed bool
		ctxErr    error
	}
	var got []seen
	check := func(ctx context.Context, relayID string, wantDraining bool) {
		// A separate pool connection sees only committed data.
		var draining bool
		if err := e.pool.QueryRow(context.Background(), `SELECT drain_started_at IS NOT NULL FROM relays WHERE id = $1`, relayID).Scan(&draining); err != nil {
			t.Error(err)
		}
		cancelReq() // the client goes away while the hook runs
		got = append(got, seen{committed: draining == wantDraining, ctxErr: ctx.Err()})
	}
	e.handler.OnRelayDrainChanged = func(ctx context.Context, relayID string, draining bool) { check(ctx, relayID, draining) }
	e.handler.OnRelayRevoked = func(ctx context.Context, relayID string) {
		var status string
		_ = e.pool.QueryRow(context.Background(), `SELECT status FROM relays WHERE id = $1`, relayID).Scan(&status)
		cancelReq()
		got = append(got, seen{committed: status == "revoked", ctxErr: ctx.Err()})
	}

	send := func(method, path string, body string) int {
		reqCtx, cancelReq = context.WithCancel(context.Background())
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(provider.WithActor(reqCtx, e.actor(provider.RoleSuperAdmin)))
		req.RemoteAddr = "192.0.2.10:54321"
		rec := httptest.NewRecorder()
		e.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(http.MethodPost, drainPath(a), ``); code != http.StatusNoContent {
		t.Fatalf("drain: %d", code)
	}
	if code := send(http.MethodPost, resumePath(a), ``); code != http.StatusNoContent {
		t.Fatalf("resume: %d", code)
	}
	if code := send(http.MethodPost, "/provider/relays/"+c+"/revoke", `{"reason":"x","confirm_name":"relay-c"}`); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if len(got) != 3 {
		t.Fatalf("hook calls: %d, want 3 (drain, resume, revoke)", len(got))
	}
	for i, s := range got {
		if !s.committed {
			t.Errorf("hook %d ran before its change was committed", i)
		}
		if s.ctxErr != nil {
			t.Errorf("hook %d: context ended with the request: %v", i, s.ctxErr)
		}
	}
}

// AT-D12: the real drain fan-out with a failing broadcast. The drain stays
// committed, the failure is logged with the relay id, and the list a
// (re)connecting connector receives at connect time leaves the relay out.
func TestDrainDB_BroadcastFailureKeepsDrain(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	b := e.eligibleRelay(t, "relay-b")
	flags := &recordingFlags{}
	e.handler.OnRelayDrainChanged = DrainFanout(flags, func(context.Context) error { return errors.New("registry unavailable") })
	logs := captureLog(t)

	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, drainPath(a), nil, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain with a failing broadcast: %d %s", r.code, r.body)
	}
	if !e.draining(t, a) || len(e.auditRows(t, provider.ActionRelayDrain)) != 1 {
		t.Fatal("a failed broadcast undid the committed drain or its audit row")
	}
	if !strings.Contains(logs.String(), "relay "+a) || !strings.Contains(logs.String(), "registry unavailable") {
		t.Fatalf("broadcast failure not logged with the relay id: %q", logs.String())
	}
	if !sameIDs(flags.marked, []string{a}) {
		t.Fatalf("draining flag calls %v, want one for the drained relay", flags.marked)
	}
	// Connect-time push (control_stream.go) uses BuildLabelledRelayList.
	if ids, _ := e.published(t); !sameIDs(ids, []string{b}) {
		t.Fatalf("connect-time list %v, want only relay-b", ids)
	}
}
