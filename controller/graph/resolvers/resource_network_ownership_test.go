package resolvers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/resource"
)

// Resources may only reference the caller's own remote networks. Before this
// check, createResource/updateResource stored any remote_network_id they were
// given, and reads joined the network by ID alone, so a workspace-A admin could
// point a resource at workspace B's network and read B's network name back.
// Reuses the two-workspace fixture from policy_tenant_isolation_test.go.

// resourceRow is one resource row as "remote_network_id/name/updated_at".
func (f *isoFixture) resourceRow(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT remote_network_id::text || '/' || name || '/' || updated_at::text FROM resources WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *isoFixture) resourceCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM resources`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newResourceInput(rn, name string) graph.CreateResourceInput {
	return graph.CreateResourceInput{RemoteNetworkID: rn, Name: name, Host: "10.70.0.9", Protocol: "tcp", PortFrom: 443, PortTo: 443}
}

func TestResourceNetwork_CreateRejectsForeignAndMissingNetwork(t *testing.T) {
	f := newIsoFixture(t)
	before := f.resourceCount(t)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)

	var want string
	for _, c := range []struct{ name, rn string }{
		{"foreign network", f.b.rn},
		{"missing network", missingID},
		{"malformed id", "not-a-uuid"},
	} {
		res, err := f.mr.CreateResource(f.a.ctx, newResourceInput(c.rn, "probe-"+strings.ReplaceAll(c.name, " ", "-")))
		if err == nil {
			t.Errorf("%s: created resource %s on network %q", c.name, res.ID, res.RemoteNetwork.Name)
			continue
		}
		msg := presented(f.a.ctx, err)
		if want == "" {
			want = msg
		}
		if msg != want || msg != "remote network not found" {
			t.Errorf("%s: client sees %q, want %q for every case", c.name, msg, "remote network not found")
		}
	}
	if after := f.resourceCount(t); after != before {
		t.Errorf("rejected creates changed the resource count: %d -> %d", before, after)
	}
	if f.notifier.Version(f.a.id) != vA || f.notifier.Version(f.b.id) != vB {
		t.Errorf("rejected creates bumped a policy version")
	}
}

func TestResourceNetwork_UpdateRejectsForeignAndMissingNetwork(t *testing.T) {
	f := newIsoFixture(t)
	before := f.resourceRow(t, f.a.res)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)
	newName := "renamed"
	for _, c := range []struct{ name, rn string }{
		{"foreign network", f.b.rn},
		{"missing network", missingID},
		{"malformed id", "not-a-uuid"},
	} {
		rn := c.rn
		// Also change the name: a rejected update must not apply any field.
		res, err := f.mr.UpdateResource(f.a.ctx, f.a.res, graph.UpdateResourceInput{RemoteNetworkID: &rn, Name: &newName})
		if err == nil {
			t.Errorf("%s: update accepted; resource now on network %q", c.name, res.RemoteNetwork.Name)
			continue
		}
		if msg := presented(f.a.ctx, err); msg != "remote network not found" {
			t.Errorf("%s: client sees %q, want %q", c.name, msg, "remote network not found")
		}
	}
	if after := f.resourceRow(t, f.a.res); after != before {
		t.Errorf("rejected updates changed the resource: %s -> %s", before, after)
	}
	if f.notifier.Version(f.a.id) != vA || f.notifier.Version(f.b.id) != vB {
		t.Errorf("rejected updates bumped a policy version")
	}
}

func TestResourceNetwork_SameWorkspaceStillWorks(t *testing.T) {
	f := newIsoFixture(t)
	res, err := f.mr.CreateResource(f.a.ctx, newResourceInput(f.a.rn, "own-resource"))
	if err != nil {
		t.Fatalf("create on own network: %v", err)
	}
	if res.RemoteNetwork.ID != f.a.rn || res.RemoteNetwork.Name != "net-a" {
		t.Errorf("created resource network = %+v, want A's network", res.RemoteNetwork)
	}
	var second string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net-a2', 'other') RETURNING id`, f.a.id).Scan(&second); err != nil {
		t.Fatal(err)
	}
	vA := f.notifier.Version(f.a.id)
	upd, err := f.mr.UpdateResource(f.a.ctx, res.ID, graph.UpdateResourceInput{RemoteNetworkID: &second})
	if err != nil || upd.RemoteNetwork.ID != second || upd.RemoteNetwork.Name != "net-a2" {
		t.Fatalf("move to own second network: %v %+v", err, upd)
	}
	if f.notifier.Version(f.a.id) != vA+1 {
		t.Errorf("A's version after a network move: %d -> %d, want +1", vA, f.notifier.Version(f.a.id))
	}
	// An update that leaves the network alone still works.
	desc := "only the description"
	if _, err := f.mr.UpdateResource(f.a.ctx, res.ID, graph.UpdateResourceInput{Description: &desc}); err != nil {
		t.Fatalf("description-only update: %v", err)
	}
}

func TestResourceNetwork_ReadsHideForeignNetworkName(t *testing.T) {
	f := newIsoFixture(t)
	// A legacy row the old code could write: A's resource on B's network. The
	// schema refuses it (FU-2), so drop that check here.
	allowCrossWorkspaceRows(t, f.pool)
	if _, err := f.pool.Exec(context.Background(), `UPDATE resources SET remote_network_id = $1 WHERE id = $2`, f.b.rn, f.a.res); err != nil {
		t.Fatal(err)
	}
	q := &queryResolver{f.mr.Resolver}
	all, err := q.AllResources(f.a.ctx)
	if err != nil {
		t.Fatalf("allResources: %v", err)
	}
	found := false
	for _, r := range all {
		if r.ID != f.a.res {
			continue
		}
		found = true
		// The row's own stored network ID is shown (it is A's data), but the
		// foreign network's name is not.
		if r.RemoteNetwork == nil || r.RemoteNetwork.ID != f.b.rn || r.RemoteNetwork.Name != "" {
			t.Errorf("allResources legacy row network = %+v, want ID %s with an empty name", r.RemoteNetwork, f.b.rn)
		}
	}
	if !found {
		t.Errorf("A's legacy resource disappeared from allResources (A must still be able to see and fix it)")
	}
	// Policy responses (B-1 helpers) load the row too, without the name. A read
	// error fails the test: it must not pass by not reading.
	res, err := f.mr.loadResourceWithGroups(f.a.ctx, f.a.id, f.a.res)
	if err != nil {
		t.Fatalf("policy read of A's legacy resource: %v", err)
	}
	if res.ID != f.a.res || res.RemoteNetwork == nil || res.RemoteNetwork.ID != f.b.rn || res.RemoteNetwork.Name != "" {
		t.Errorf("policy read = id %s network %+v, want A's resource with an empty network name", res.ID, res.RemoteNetwork)
	}
	// A can move it back onto its own network.
	if _, err := f.mr.UpdateResource(f.a.ctx, f.a.res, graph.UpdateResourceInput{RemoteNetworkID: &f.a.rn}); err != nil {
		t.Fatalf("repair legacy resource: %v", err)
	}
}

// The check lives in the store's own SQL, so calling the store directly can't
// get a foreign network through. There is no separate validation query to
// race: the ownership condition is part of the same INSERT/UPDATE statement,
// and no code moves a remote network to another tenant, so concurrency adds
// no coverage and this test makes no concurrency claim.
func TestResourceNetwork_StoreEnforcesOwnership(t *testing.T) {
	f := newIsoFixture(t)
	before := f.resourceCount(t)
	if _, err := resource.Create(context.Background(), f.pool, f.a.id, resource.CreateInput{
		RemoteNetworkID: f.b.rn, Name: "direct", Host: "10.70.0.10", Protocol: "tcp", PortFrom: 80, PortTo: 80,
	}); !errors.Is(err, resource.ErrRemoteNetworkNotFound) {
		t.Errorf("direct store create with a foreign network: err = %v, want ErrRemoteNetworkNotFound", err)
	}
	rn := f.b.rn
	if _, err := resource.Update(context.Background(), f.pool, f.a.id, f.a.res, resource.UpdateInput{RemoteNetworkID: &rn}); !errors.Is(err, resource.ErrRemoteNetworkNotFound) {
		t.Errorf("direct store update with a foreign network: err = %v", err)
	}
	if after := f.resourceCount(t); after != before {
		t.Errorf("resource count %d -> %d", before, after)
	}
}
