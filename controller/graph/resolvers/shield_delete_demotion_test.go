package resolvers

import (
	"context"
	"testing"
)

// Deleting a shield — or a connector, which cascades to its shields — used to leave
// its resources at status='protected' with shield_id NULL, because
// `resources.shield_id` is ON DELETE SET NULL and an FK action cannot touch another
// column. That row then failed the ENTIRE workspace ACL compile, so one deletion
// took every resource offline. The compiler now skips such a resource; these tests
// cover the other half — not creating it.
//
// Gated on RESOURCE_TEST_DATABASE_URL / RESOURCE_TEST_SHIELD_ID like its neighbours.

// status returns a resource's status and whether its shield_id is still set.
func (f *aclCoherenceFixture) status(t *testing.T, resourceID string) (string, bool) {
	t.Helper()
	var st string
	var shieldSet bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT status, shield_id IS NOT NULL FROM resources WHERE id = $1`, resourceID,
	).Scan(&st, &shieldSet); err != nil {
		t.Fatalf("read resource: %v", err)
	}
	return st, shieldSet
}

// seedShield creates a throwaway shield under the given connector so a delete can be
// exercised without destroying the fixture's own shield.
func (f *aclCoherenceFixture) seedShield(t *testing.T, connectorID string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
		 VALUES ($1,$2,$3,'demotion-test','revoked','ws-test.invalid')
		 RETURNING id`,
		f.tenantID, f.networkID, connectorID,
	).Scan(&id); err != nil {
		t.Skipf("cannot seed a shield (schema may differ): %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM shields WHERE id = $1`, id)
	})
	return id
}

// connectorOf returns the connector the fixture's shield belongs to.
func (f *aclCoherenceFixture) connectorOf(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT connector_id FROM shields WHERE id = $1`, f.shieldID,
	).Scan(&id); err != nil {
		t.Fatalf("read connector: %v", err)
	}
	return id
}

// bindResource points an existing seeded resource at a different shield.
func (f *aclCoherenceFixture) bindResource(t *testing.T, resourceID, shieldID, status string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE resources SET shield_id = $1, status = $2 WHERE id = $3`,
		shieldID, status, resourceID,
	); err != nil {
		t.Fatalf("bind resource: %v", err)
	}
}

// THE REGRESSION: deleting a shield must demote its resources, not orphan them.
func TestDeleteShield_DemotesBoundResources(t *testing.T) {
	f := newACLCoherenceFixture(t)
	victim := f.seedShield(t, f.connectorOf(t))
	res := f.seedResource(t, "10.77.0.11", "protected")
	f.bindResource(t, res, victim, "protected")

	if st, hasShield := f.status(t, res); st != "protected" || !hasShield {
		t.Fatalf("precondition: want protected with a shield, got %q shield=%v", st, hasShield)
	}

	f.fires.Store(0)
	ok, err := f.mr.DeleteShield(f.ctx, victim)
	if err != nil || !ok {
		t.Fatalf("DeleteShield: ok=%v err=%v", ok, err)
	}

	st, hasShield := f.status(t, res)
	if st != "unprotected" {
		t.Fatalf("resource must be demoted to unprotected, got %q — this is the row that failed the whole workspace compile", st)
	}
	if hasShield {
		t.Fatal("shield_id must be cleared")
	}
	// Deleting a shield changes the compiled ACL, so the snapshot must be invalidated.
	if f.fires.Load() == 0 {
		t.Fatal("DeleteShield must invalidate the ACL snapshot")
	}
}

// A refused delete must not demote anything: the shield still exists, so its
// resources are still legitimately shield-delivered.
func TestDeleteShield_RefusedDeleteLeavesResourcesAlone(t *testing.T) {
	f := newACLCoherenceFixture(t)
	victim := f.seedShield(t, f.connectorOf(t))
	// Only 'pending'/'revoked' shields may be deleted; make it undeletable.
	if _, err := f.pool.Exec(f.ctx, `UPDATE shields SET status='active' WHERE id=$1`, victim); err != nil {
		t.Fatalf("arrange: %v", err)
	}
	res := f.seedResource(t, "10.77.0.12", "protected")
	f.bindResource(t, res, victim, "protected")

	if ok, err := f.mr.DeleteShield(f.ctx, victim); err == nil || ok {
		t.Fatalf("delete of an active shield should be refused, got ok=%v err=%v", ok, err)
	}

	// The transaction must have rolled the demotion back.
	st, hasShield := f.status(t, res)
	if st != "protected" || !hasShield {
		t.Fatalf("a refused delete must not demote: got status=%q shield=%v", st, hasShield)
	}
}

// Deleting a connector cascades to its shields, so its resources must be demoted too.
func TestDeleteConnector_DemotesResourcesOfItsShields(t *testing.T) {
	f := newACLCoherenceFixture(t)

	// A throwaway connector so the fixture's own is untouched.
	var conn string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain)
		 VALUES ($1,$2,'demotion-test-conn','revoked','ws-test.invalid') RETURNING id`,
		f.tenantID, f.networkID,
	).Scan(&conn); err != nil {
		t.Skipf("cannot seed a connector (schema may differ): %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM connectors WHERE id = $1`, conn)
	})

	victim := f.seedShield(t, conn)
	res := f.seedResource(t, "10.77.0.13", "protected")
	f.bindResource(t, res, victim, "protected")

	f.fires.Store(0)
	ok, err := f.mr.DeleteConnector(f.ctx, conn)
	if err != nil || !ok {
		t.Fatalf("DeleteConnector: ok=%v err=%v", ok, err)
	}

	st, hasShield := f.status(t, res)
	if st != "unprotected" {
		t.Fatalf("resource of a cascaded shield must be demoted, got %q", st)
	}
	if hasShield {
		t.Fatal("shield_id must be cleared")
	}
	if f.fires.Load() == 0 {
		t.Fatal("DeleteConnector must invalidate the ACL snapshot (RevokeConnector already did; this path did not)")
	}
}
