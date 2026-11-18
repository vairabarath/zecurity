package connector

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

func (f *bindingFixture) setSuspended(suspended bool) {
	f.t.Helper()
	sql := `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`
	if suspended {
		sql = `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`
	}
	if _, err := f.pool.Exec(f.ctx, sql, f.wsID); err != nil {
		f.t.Fatal(err)
	}
}

// Sprint 23 S1 (D-45): a new connector can't enroll while its tenant is
// suspended; nothing is issued and the connector stays pending. After resume
// it can.
func TestEnrollConnector_RefusedWhileTenantSuspended(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	f.setSuspended(true)
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(token))
	if status.Code(err) != codes.FailedPrecondition || resp != nil || !strings.Contains(err.Error(), tenantstate.Message) {
		t.Fatalf("enroll while suspended: resp %v err %v, want FailedPrecondition %q", resp, err, tenantstate.Message)
	}
	var st string
	if err := f.pool.QueryRow(f.ctx, `SELECT status FROM connectors WHERE id = $1`, f.connID).Scan(&st); err != nil || st != "pending" {
		t.Fatalf("connector status %q (err %v), want pending", st, err)
	}
	f.setSuspended(false)
	if _, err := f.handler(f.pki).Enroll(f.ctx, f.request(f.mustReissue())); err != nil {
		t.Fatalf("enroll after resume: %v", err)
	}
}

// D-08 preserved: an existing connector keeps renewing its certificate while
// its tenant is suspended (it gets a deny-all ACL instead, see policy).
func TestRenewConnector_UnaffectedByTenantSuspension(t *testing.T) {
	f := newBindingFixture(t)
	leaf := f.enrollWithKey(newConnectorKey(t))
	f.setSuspended(true)
	resp, err := f.renewWith(f.handler(f.pki), leaf, f.key)
	if err != nil {
		t.Fatalf("renewal while the tenant is suspended refused: %v (D-08)", err)
	}
	renewed := parsePEMCert(t, resp.CertificatePem)
	if got := f.certState().serial; got != renewed.SerialNumber.Text(16) {
		t.Fatalf("recorded serial %s, want the renewed %s", got, renewed.SerialNumber.Text(16))
	}
}
