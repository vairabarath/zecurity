package resolvers

import (
	"strings"
	"testing"
)

// Sprint 20 Fix 16 (F46): revoking the last connector of a remote network
// that still has an enrolled shield attached is refused unless
// alsoRevokeShields is set, which revokes the shield too. Real Postgres via
// the certificate-history fixture (PKI_TEST_DATABASE_URL).

func (f *certHistoryFixture) shieldOn(t *testing.T, connectorID, name string) string {
	t.Helper()
	return f.scalar(t, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		VALUES ($1, $2, $3, $4, 'active') RETURNING id::text`, f.ws, f.rn, connectorID, name)
}

func TestRevokeConnector_LastConnectorWithShieldsIsRefused(t *testing.T) {
	f := newCertHistoryFixture(t)
	conn := f.connector(t, "only-conn", nil, valid)
	f.shieldOn(t, conn, "web-shield")

	ok, err := f.mr.RevokeConnector(f.ctx, conn, nil)
	if err == nil || ok {
		t.Fatalf("RevokeConnector = %v, %v; want refused", ok, err)
	}
	if !strings.Contains(err.Error(), "web-shield") || !strings.Contains(err.Error(), "alsoRevokeShields") {
		t.Errorf("error should name the shield and the way out: %v", err)
	}
	if got := f.scalar(t, `SELECT status FROM connectors WHERE id = $1`, conn); got != "active" {
		t.Errorf("connector status = %s, want active (nothing revoked)", got)
	}
}

func TestRevokeConnector_AlsoRevokeShieldsRevokesBoth(t *testing.T) {
	f := newCertHistoryFixture(t)
	conn := f.connector(t, "only-conn", nil, valid)
	sh := f.shieldOn(t, conn, "web-shield")

	yes := true
	if ok, err := f.mr.RevokeConnector(f.ctx, conn, &yes); err != nil || !ok {
		t.Fatalf("RevokeConnector(alsoRevokeShields): %v, %v", ok, err)
	}
	if got := f.scalar(t, `SELECT status FROM connectors WHERE id = $1`, conn); got != "revoked" {
		t.Errorf("connector status = %s, want revoked", got)
	}
	if got := f.scalar(t, `SELECT status FROM shields WHERE id = $1`, sh); got != "revoked" {
		t.Errorf("shield status = %s, want revoked", got)
	}
	if got := f.scalar(t, `SELECT count(*)::text FROM shield_revocations WHERE shield_id = $1`, sh); got != "1" {
		t.Errorf("shield tombstone rows = %s, want 1", got)
	}
}

func TestRevokeConnector_WithAPeerTheShieldIsNotStranded(t *testing.T) {
	f := newCertHistoryFixture(t)
	conn := f.connector(t, "conn-a", nil, valid)
	f.connector(t, "conn-b", nil, valid)
	sh := f.shieldOn(t, conn, "web-shield")

	if ok, err := f.mr.RevokeConnector(f.ctx, conn, nil); err != nil || !ok {
		t.Fatalf("RevokeConnector with a peer: %v, %v", ok, err)
	}
	if got := f.scalar(t, `SELECT status FROM shields WHERE id = $1`, sh); got != "active" {
		t.Errorf("shield status = %s, want active (it fails over to conn-b)", got)
	}
}
