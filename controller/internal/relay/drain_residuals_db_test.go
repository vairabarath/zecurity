package relay

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
)

// FU-7: the Phase D review residuals, on a throwaway database.

// An older heartbeat landing after a newer one keeps the newer count.
func TestDrainDB_OlderHeartbeatKeepsNewerCount(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.eligibleRelay(t, "relay-a")
	t1 := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	t2 := t1.Add(30 * time.Second)
	beat := func(count uint32, at time.Time) {
		t.Helper()
		if err := e.store.RecordHeartbeat(context.Background(), id, "s", time.Now().Add(time.Hour), "1.0", "h",
			"", 0, "", "relay-a.relay.test:9093", count, 100, at); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() (int, time.Time) {
		t.Helper()
		var count int
		var at time.Time
		if err := e.pool.QueryRow(context.Background(),
			`SELECT connection_count, connection_count_at FROM relays WHERE id = $1`, id).Scan(&count, &at); err != nil {
			t.Fatal(err)
		}
		return count, at
	}

	beat(0, t2)
	beat(9, t1) // measured earlier, written later
	if count, at := stored(); count != 0 || !at.Equal(t2) {
		t.Fatalf("after an older heartbeat: count %d at %v, want 0 at %v", count, at, t2)
	}
	beat(4, t2.Add(30*time.Second))
	if count, at := stored(); count != 4 || !at.Equal(t2.Add(30*time.Second)) {
		t.Fatalf("after a newer heartbeat: count %d at %v", count, at)
	}
}

// eligible.Where is false, never NULL, for a row with NULL columns: a relay
// with an observed IP but no address scope (the only nullable input) can be drained (it was a scan
// error, a 500, before).
func TestDrainDB_EligibleWhereNeverNull(t *testing.T) {
	e := newAdminEnv(t, false)
	e.eligibleRelay(t, "keeper")
	odd := e.eligibleRelay(t, "odd")
	e.exec(t, `UPDATE relays SET public_addr = NULL, address_scope = NULL, observed_ip = '203.0.113.7' WHERE id = $1`, odd)

	var isEligible bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT `+eligible.Where+` FROM relays WHERE id = $1`, odd).Scan(&isEligible); err != nil {
		t.Fatalf("eligible.Where on NULL columns: %v", err)
	}
	if isEligible {
		t.Fatal("relay without an address scope or label counted as eligible")
	}
	if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(odd), map[string]string{"reason": "maintenance"}, nil); r.code != http.StatusNoContent {
		t.Fatalf("drain: %d %s", r.code, r.body)
	}
}

// drain_started_at is the controller's clock after the row locks are held,
// not the transaction start: a drain that waited on a lock starts its window
// when it actually drains.
func TestDrainDB_DrainStartIsAfterLockWait(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")

	ctx := context.Background()
	holder, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM relays WHERE id = $1 FOR UPDATE`, a); err != nil {
		t.Fatal(err)
	}
	done := make(chan resp, 1)
	go func() {
		done <- e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(a), map[string]string{"reason": "maintenance"}, nil)
	}()
	time.Sleep(500 * time.Millisecond)
	released := time.Now()
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.code != http.StatusNoContent {
		t.Fatalf("drain: %d %s", r.code, r.body)
	}
	var started time.Time
	if err := e.pool.QueryRow(ctx, `SELECT drain_started_at FROM relays WHERE id = $1`, a).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if started.Before(released.Add(-50 * time.Millisecond)) {
		t.Fatalf("drain_started_at %v is before the lock was released (%v)", started, released)
	}
}

// Drain racing eviction: both lock rows in id order, so neither deadlocks.
// Every drain ends in 204 or a 409, never a 500.
func TestDrainDB_DrainRacesEviction(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		e.exec(t, `UPDATE relays SET status = 'deleted' WHERE status <> 'deleted'`)
		ids := make([]string, 4)
		for i := range ids {
			ids[i] = e.eligibleRelay(t, fmt.Sprintf("evict-%d-%d", round, i))
		}
		e.exec(t, `UPDATE relays SET last_heartbeat_at = NOW() - interval '10 minutes' WHERE id = ANY($1::uuid[])`, ids)
		// Evict in descending id order: the order the old statement could
		// lock in, opposite to the drain's.
		desc := sorted(ids...)
		for i, j := 0, len(desc)-1; i < j; i, j = i+1, j-1 {
			desc[i], desc[j] = desc[j], desc[i]
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var evictErr error
		drains := make([]resp, len(ids))
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, evictErr = e.store.EvictRelays(ctx, desc, time.Now())
		}()
		for i, id := range ids {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				<-start
				drains[i] = e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(id), map[string]string{"reason": "race"}, nil)
			}(i, id)
		}
		close(start)
		wg.Wait()
		if evictErr != nil {
			t.Fatalf("round %d: evict: %v", round, evictErr)
		}
		for i, r := range drains {
			if r.code != http.StatusNoContent && r.code != http.StatusConflict {
				t.Fatalf("round %d: drain %d: %d %s", round, i, r.code, r.body)
			}
		}
	}
}

// Drain racing revoke: the revoke always wins its row; the drain either
// committed first (204) or sees the revoked relay (409 already_revoked).
func TestDrainDB_DrainRacesRevoke(t *testing.T) {
	e := newAdminEnv(t, false)
	for round := 0; round < 10; round++ {
		e.exec(t, `UPDATE relays SET status = 'deleted' WHERE status <> 'deleted'`)
		name := fmt.Sprintf("rv-%d", round)
		id := e.eligibleRelay(t, name)
		e.eligibleRelay(t, name+"-keeper")

		start := make(chan struct{})
		var wg sync.WaitGroup
		var drain, revoke resp
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			drain = e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(id), map[string]string{"reason": "race"}, nil)
		}()
		go func() {
			defer wg.Done()
			<-start
			revoke = e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relays/"+id+"/revoke", confirm(name, "race"), nil)
		}()
		close(start)
		wg.Wait()
		if revoke.code != http.StatusNoContent {
			t.Fatalf("round %d: revoke: %d %s", round, revoke.code, revoke.body)
		}
		if drain.code != http.StatusNoContent && !(drain.code == http.StatusConflict && errorCode(t, drain.body) == "already_revoked") {
			t.Fatalf("round %d: drain: %d %s", round, drain.code, drain.body)
		}
		if e.status(t, id) != "revoked" {
			t.Fatalf("round %d: status %s, want revoked", round, e.status(t, id))
		}
	}
}

// Drain racing resume on a draining relay: both serialise on the row lock;
// the stored state matches whichever committed last, and neither fails.
func TestDrainDB_DrainRacesResume(t *testing.T) {
	e := newAdminEnv(t, false)
	a := e.eligibleRelay(t, "relay-a")
	e.eligibleRelay(t, "relay-b")
	ctx := context.Background()
	for round := 0; round < 10; round++ {
		if r := e.do(t, provider.RoleRelayOps, http.MethodPost, drainPath(a), map[string]string{"reason": "setup"}, nil); r.code != http.StatusNoContent {
			t.Fatalf("round %d: setup drain: %d %s", round, r.code, r.body)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		out := make([]resp, 2)
		for j, path := range []string{resumePath(a), drainPath(a)} {
			var body any
			if path == drainPath(a) {
				body = map[string]string{"reason": "race"}
			}
			wg.Add(1)
			go func(j int, path string, body any) {
				defer wg.Done()
				<-start
				out[j] = e.do(t, provider.RoleRelayOps, http.MethodPost, path, body, nil)
			}(j, path, body)
		}
		close(start)
		wg.Wait()
		for j, r := range out {
			if r.code != http.StatusNoContent {
				t.Fatalf("round %d: call %d: %d %s", round, j, r.code, r.body)
			}
		}
		// Bring the relay back to a known state for the next round.
		if _, err := e.store.ResumeRelay(ctx, a, func(context.Context, pgx.Tx, time.Time) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
}
