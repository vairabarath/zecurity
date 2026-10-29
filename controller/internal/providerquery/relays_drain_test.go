package providerquery

import (
	"context"
	"testing"
	"time"
)

// Sprint 22 Phase D (P4): drain progress is decided by the server from the
// attached connectors and the relay's measured stream count, never by the
// console. Explicit timestamps; no sleeps.

func TestDrainState(t *testing.T) {
	start := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	after := start.Add(DrainWindow + 5*time.Second) // measured after the window
	now := after.Add(10 * time.Second)              // and fresh
	at := func(t time.Time) *time.Time { return &t }

	for _, tc := range []struct {
		name     string
		status   string
		drain    *time.Time
		attached int64
		count    int32
		countAt  *time.Time
		now      time.Time
		want     string // "" = null
	}{
		{"not draining", "active", nil, 0, 0, at(after), now, ""},
		{"revoked keeps no drain state", "revoked", at(start), 0, 0, at(after), now, ""},
		{"deleted keeps no drain state", "deleted", at(start), 0, 0, at(after), now, ""},
		{"connectors attached", "active", at(start), 2, 0, at(after), now, DrainMigrating},
		{"attached wins over a stale count", "active", at(start), 1, 0, nil, now, DrainMigrating},
		{"no measured count", "active", at(start), 0, 0, nil, now, DrainUnverified},
		{"zero count older than 90 s", "active", at(start), 0, 0, at(after), after.Add(DrainFreshness + time.Second), DrainUnverified},
		{"zero count exactly 90 s old is fresh", "active", at(start), 0, 0, at(after), after.Add(DrainFreshness), DrainDrained},
		{"stale non-zero count", "active", at(start), 0, 4, at(after), after.Add(DrainFreshness + time.Second), DrainUnverified},
		{"fresh non-zero count", "active", at(start), 0, 4, at(after), now, DrainStreamsOpen},
		{"fresh non-zero count inside the window", "active", at(start), 0, 4, at(start.Add(10 * time.Second)), start.Add(20 * time.Second), DrainStreamsOpen},
		{"zero count measured before the window ends", "active", at(start), 0, 0, at(start.Add(DrainWindow - time.Second)), start.Add(DrainWindow + time.Second), DrainUnverified},
		{"zero count measured before the drain", "active", at(start), 0, 0, at(start.Add(-time.Minute)), start.Add(10 * time.Second), DrainUnverified},
		{"zero count measured at the window's end", "active", at(start), 0, 0, at(start.Add(DrainWindow)), start.Add(DrainWindow + time.Second), DrainDrained},
		{"fresh zero after the window", "active", at(start), 0, 0, at(after), now, DrainDrained},
		{"inactive relay with a stale count", "inactive", at(start), 0, 0, at(after), after.Add(5 * time.Minute), DrainUnverified},
	} {
		got := drainState(tc.status, tc.drain, tc.attached, tc.count, tc.countAt, tc.now)
		gotS := ""
		if got != nil {
			gotS = *got
		}
		if gotS != tc.want {
			t.Errorf("%s: drain state %q, want %q", tc.name, gotS, tc.want)
		}
	}
}

// The read API: draining_since and drain_state on list and detail, nulls for
// terminal relays, and the eligibility fields from the shared predicate.
func TestRelayReadAPI_DrainFields(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	eligibleRelay := func(name string) string {
		id := seedRelay(t, pool, name, "active", t0)
		mustExec(t, pool, `UPDATE relays SET capacity_label = 'high', public_addr = $2 WHERE id = $1`, id, name+".relay.test:9093")
		return id
	}
	a, b := eligibleRelay("a"), eligibleRelay("b")
	low := eligibleRelay("low")
	mustExec(t, pool, `UPDATE relays SET capacity_label = 'low' WHERE id = $1`, low)
	noAddr := eligibleRelay("no-addr")
	mustExec(t, pool, `UPDATE relays SET public_addr = NULL, address_scope = 'public', observed_ip = NULL WHERE id = $1`, noAddr)
	revoked := eligibleRelay("revoked")
	mustExec(t, pool, `UPDATE relays SET status = 'revoked', drain_started_at = NOW() WHERE id = $1`, revoked)

	detail := func(id string) RelayDetail {
		t.Helper()
		d, err := GetRelay(ctx, pool, nil, id)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for id, wantIn := range map[string]bool{a: true, b: true, low: false, noAddr: false, revoked: false} {
		if d := detail(id); d.InRelayList != wantIn || d.EligibleRelayCount != 2 {
			t.Errorf("relay %s: in_relay_list=%v eligible_relay_count=%d, want %v and 2", d.Name, d.InRelayList, d.EligibleRelayCount, wantIn)
		}
	}
	if d := detail(revoked); d.DrainingSince != nil || d.DrainState != nil {
		t.Fatalf("revoked relay exposes drain fields: %v %v", d.DrainingSince, d.DrainState)
	}

	// Draining a relay moves it out of the list and shows its state. The
	// stored count was measured long ago: unverified, never drained.
	mustExec(t, pool, `UPDATE relays SET drain_started_at = NOW() - interval '10 minutes', connection_count = 0, connection_count_at = NOW() - interval '9 minutes' WHERE id = $1`, a)
	d := detail(a)
	if d.InRelayList || d.EligibleRelayCount != 1 || d.DrainingSince == nil || d.DrainState == nil || *d.DrainState != DrainUnverified {
		t.Fatalf("draining relay detail: in=%v count=%d since=%v state=%v", d.InRelayList, d.EligibleRelayCount, d.DrainingSince, d.DrainState)
	}
	// A fresh zero measured after the window: drained.
	mustExec(t, pool, `UPDATE relays SET connection_count_at = NOW() - interval '5 seconds' WHERE id = $1`, a)
	page, err := ListRelays(ctx, pool, nil, RelayFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range page.Items {
		if s.ID == a {
			found = true
			if s.DrainState == nil || *s.DrainState != DrainDrained || s.DrainingSince == nil {
				t.Fatalf("list drain fields: %v %v", s.DrainState, s.DrainingSince)
			}
		}
		if s.ID != a && (s.DrainState != nil || s.DrainingSince != nil) {
			t.Fatalf("relay %s not draining but has drain fields", s.Name)
		}
	}
	if !found {
		t.Fatal("draining relay missing from the list")
	}
	// A connector still attached: migrating, whatever the count says.
	tenant, network := seedTenant(t, pool, "acme")
	cid := mustScalar[string](t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name) VALUES ($1,$2,'c1') RETURNING id::text`, tenant, network)
	mustExec(t, pool, `INSERT INTO connector_relay_placement (connector_id, relay_id, source) VALUES ($1,$2,'heartbeat')`, cid, a)
	if d := detail(a); d.DrainState == nil || *d.DrainState != DrainMigrating {
		t.Fatalf("drain state with an attached connector: %v", d.DrainState)
	}
}
