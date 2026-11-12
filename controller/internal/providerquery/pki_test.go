package providerquery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Quick-win F15/F16: the CA hierarchy and the CRL each CA signs.

var pkiValidity = PKIValidity{Relay: 10 * time.Minute, Workspace: 24 * time.Hour}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

// newCA issues a CA certificate signed by parent (self-signed when nil).
// x509.CreateCertificate fills SubjectKeyId for a CA and copies the parent's
// SubjectKeyId into AuthorityKeyId, as the controller's PKI does.
func newCA(t *testing.T, cn string, serial int64, parent *testCA) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Zecurity test"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	issuerCert, issuerKey := tmpl, key
	if parent != nil {
		issuerCert, issuerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, issuerCert, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

type pkiFixture struct {
	pool             *pgxpool.Pool
	root, inter      *testCA
	rootID, interID  string
	tenantA, tenantB string
	wsA, wsB         string
	caA, caB         *testCA
	deletedTenant    string
}

// newPKIFixture: root → intermediate → two workspace CAs (tenants A and B),
// plus a deleted workspace whose CA must not appear. Key columns hold
// canaries that must never reach a response.
func newPKIFixture(t *testing.T) pkiFixture {
	t.Helper()
	pool := newTestDB(t)
	f := pkiFixture{pool: pool}
	f.root = newCA(t, "Zecurity Root CA", 1, nil)
	f.inter = newCA(t, "Zecurity Intermediate CA", 2, f.root)
	f.rootID = mustScalar[string](t, pool, `INSERT INTO ca_root (encrypted_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ('ROOTKEYCANARY', 'ROOTNONCECANARY', $1, $2, $3) RETURNING id::text`, f.root.pem, f.root.cert.NotBefore, f.root.cert.NotAfter)
	f.interID = mustScalar[string](t, pool, `INSERT INTO ca_intermediate (encrypted_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ('INTERKEYCANARY', 'INTERNONCECANARY', $1, $2, $3) RETURNING id::text`, f.inter.pem, f.inter.cert.NotBefore, f.inter.cert.NotAfter)
	f.tenantA = seedWorkspace(t, pool, "acme", "active", t0)
	f.tenantB = seedWorkspace(t, pool, "beta", "active", t0.Add(time.Minute))
	f.deletedTenant = seedWorkspace(t, pool, "gone", "deleted", t0.Add(2*time.Minute))
	f.caA = newCA(t, "acme Workspace CA", 10, f.inter)
	f.caB = newCA(t, "beta Workspace CA", 11, f.inter)
	f.wsA = seedWorkspaceCA(t, pool, f.tenantA, f.caA, t0)
	f.wsB = seedWorkspaceCA(t, pool, f.tenantB, f.caB, t0.Add(time.Minute))
	seedWorkspaceCA(t, pool, f.deletedTenant, newCA(t, "gone Workspace CA", 12, f.inter), t0.Add(2*time.Minute))
	return f
}

func seedWorkspaceCA(t *testing.T, pool *pgxpool.Pool, tenant string, ca *testCA, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after, created_at)
	   VALUES ($1, 'WSKEYCANARY', 'WSNONCECANARY', $2, $3, $4, $5) RETURNING id::text`, tenant, ca.pem, ca.cert.NotBefore, ca.cert.NotAfter, createdAt)
}

func mustPKI(t *testing.T, q Querier, f PKIFilter) PKIHierarchy {
	t.Helper()
	r, err := PKI(context.Background(), q, f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byID(nodes []CANode) map[string]CANode {
	m := map[string]CANode{}
	for _, n := range nodes {
		m[n.ID] = n
	}
	return m
}

func TestPKI_TreeFollowsTheRealParents(t *testing.T) {
	f := newPKIFixture(t)
	r := mustPKI(t, f.pool, PKIFilter{Validity: pkiValidity})
	if len(r.Roots) != 1 || len(r.Intermediates) != 1 {
		t.Fatalf("roots %d, intermediates %d, want 1 and 1", len(r.Roots), len(r.Intermediates))
	}
	root, inter := r.Roots[0], r.Intermediates[0]
	if root.ID != f.rootID || root.Kind != "root" || root.ParentID != nil {
		t.Errorf("root = %+v", root)
	}
	if inter.ID != f.interID || inter.Kind != "intermediate" || inter.ParentID == nil || *inter.ParentID != f.rootID {
		t.Errorf("intermediate = %+v, want parent %s", inter, f.rootID)
	}
	ws := byID(r.WorkspaceCAs)
	if len(ws) != 2 || r.WorkspaceCAsTotal != 2 {
		t.Fatalf("workspace CAs = %d (total %d), want 2: a deleted workspace is excluded", len(ws), r.WorkspaceCAsTotal)
	}
	for id, c := range map[string]struct {
		tenant string
		ca     *testCA
	}{f.wsA: {f.tenantA, f.caA}, f.wsB: {f.tenantB, f.caB}} {
		n := ws[id]
		if n.Kind != "workspace_ca" || n.TenantID == nil || *n.TenantID != c.tenant {
			t.Errorf("workspace CA %s = %+v", id, n)
		}
		if n.ParentID == nil || *n.ParentID != f.interID {
			t.Errorf("workspace CA %s parent = %v, want the intermediate %s", id, n.ParentID, f.interID)
		}
		if n.Subject == nil || *n.Subject != c.ca.cert.Subject.String() || n.Issuer == nil || *n.Issuer != f.inter.cert.Subject.String() {
			t.Errorf("workspace CA %s subject/issuer = %v / %v", id, n.Subject, n.Issuer)
		}
		if n.Serial == nil || *n.Serial != c.ca.cert.SerialNumber.Text(16) {
			t.Errorf("workspace CA %s serial = %v", id, n.Serial)
		}
		if n.NotBefore == nil || !n.NotBefore.Equal(c.ca.cert.NotBefore) || !n.NotAfter.Equal(c.ca.cert.NotAfter) ||
			n.NotAfter.Location() != time.UTC {
			t.Errorf("workspace CA %s validity = %v → %v", id, n.NotBefore, n.NotAfter)
		}
	}
	if root.Name != "Root CA" || inter.Name != "Intermediate CA" || ws[f.wsA].Name != "Tenant acme" {
		t.Errorf("names = %q, %q, %q", root.Name, inter.Name, ws[f.wsA].Name)
	}
}

func TestPKI_CRLStatusBelongsToItsSigner(t *testing.T) {
	f := newPKIFixture(t)
	ctx := context.Background()
	relay := seedRelay(t, f.pool, "r1", "active", t0)
	for i, c := range []struct{ revoked, expired bool }{{true, false}, {true, false}, {true, true}, {false, false}} {
		mustExec(t, f.pool, `INSERT INTO relay_certificates (relay_id, serial, not_after, revoked_at)
		   VALUES ($1, $2, now() + CASE WHEN $3 THEN interval '-1 day' ELSE interval '1 day' END, CASE WHEN $4 THEN now() END)`,
			relay, fmt.Sprintf("r%d", i), c.expired, c.revoked)
	}
	// Tenant A's workspace CRL: 1 device + 2 connectors revoked and unexpired;
	// an expired one and an unrevoked one are not listed; B's rows are B's.
	seedRevoked := func(table, tenant, serial string, revoked, expired bool) {
		col := "device_id"
		if table == "connector_certificates" {
			col = "connector_id"
		}
		mustExec(t, f.pool, `INSERT INTO `+table+` (tenant_id, `+col+`, serial, not_after, revoked_at)
		   VALUES ($1, gen_random_uuid(), $2, now() + CASE WHEN $3 THEN interval '-1 day' ELSE interval '1 day' END, CASE WHEN $4 THEN now() END)`,
			tenant, serial, expired, revoked)
	}
	seedRevoked("client_device_certificates", f.tenantA, "d1", true, false)
	seedRevoked("client_device_certificates", f.tenantA, "d2", true, true)
	seedRevoked("client_device_certificates", f.tenantA, "d3", false, false)
	seedRevoked("connector_certificates", f.tenantA, "c1", true, false)
	seedRevoked("connector_certificates", f.tenantA, "c2", true, false)
	seedRevoked("connector_certificates", f.tenantB, "c3", true, false)

	r, err := PKI(ctx, f.pool, PKIFilter{Validity: pkiValidity})
	if err != nil {
		t.Fatal(err)
	}
	if r.Roots[0].CRL != nil {
		t.Errorf("the root signs no CRL here, got %+v", r.Roots[0].CRL)
	}
	want := &CRLStatus{URL: "/relay.crl", Covers: []string{"relays"}, Entries: 2, ValidForSeconds: 600}
	if got := r.Intermediates[0].CRL; !reflect.DeepEqual(got, want) {
		t.Errorf("intermediate CRL = %+v, want %+v", got, want)
	}
	ws := byID(r.WorkspaceCAs)
	for id, c := range map[string]struct {
		tenant  string
		entries int64
	}{f.wsA: {f.tenantA, 3}, f.wsB: {f.tenantB, 1}} {
		want := &CRLStatus{URL: "/ca.crl?workspace_id=" + c.tenant, Covers: []string{"client_devices", "connectors"},
			Entries: c.entries, ValidForSeconds: 86400}
		if got := ws[id].CRL; !reflect.DeepEqual(got, want) {
			t.Errorf("workspace CA %s CRL = %+v, want %+v", id, got, want)
		}
	}
}

// Pagination is explicit: every page carries the total, pages chain with
// next_cursor until null, and their union is every workspace CA once.
// Roots and intermediates are complete on every page.
func TestPKI_PaginationIsExplicit(t *testing.T) {
	f := newPKIFixture(t)
	for i := 0; i < 3; i++ {
		tenant := seedWorkspace(t, f.pool, fmt.Sprintf("more%d", i), "active", t0.Add(time.Duration(10+i)*time.Minute))
		seedWorkspaceCA(t, f.pool, tenant, newCA(t, fmt.Sprintf("more%d Workspace CA", i), int64(20+i), f.inter), t0.Add(time.Duration(10+i)*time.Minute))
	}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		r := mustPKI(t, f.pool, PKIFilter{Validity: pkiValidity, Limit: 2, Cursor: cursor})
		pages++
		if r.WorkspaceCAsTotal != 5 || len(r.Roots) != 1 || len(r.Intermediates) != 1 {
			t.Fatalf("page %d: total %d, roots %d, intermediates %d", pages, r.WorkspaceCAsTotal, len(r.Roots), len(r.Intermediates))
		}
		if len(r.WorkspaceCAs) > 2 {
			t.Fatalf("page %d has %d items, limit 2", pages, len(r.WorkspaceCAs))
		}
		for _, n := range r.WorkspaceCAs {
			if seen[n.ID] {
				t.Fatalf("workspace CA %s returned twice", n.ID)
			}
			seen[n.ID] = true
		}
		if r.NextCursor == nil {
			break
		}
		cursor = *r.NextCursor
	}
	if pages != 3 || len(seen) != 5 {
		t.Fatalf("pages %d, distinct CAs %d, want 3 and 5", pages, len(seen))
	}
	if _, err := PKI(context.Background(), f.pool, PKIFilter{Validity: pkiValidity, Cursor: "not-a-cursor"}); err == nil {
		t.Fatal("a bad cursor must be rejected")
	}
}

// A certificate that does not parse is still listed (nothing hidden), with
// no subject, issuer, serial or parent, and does not fail the read.
func TestPKI_UnparsableCertificateDegradesNotFails(t *testing.T) {
	f := newPKIFixture(t)
	tenant := seedWorkspace(t, f.pool, "broken", "active", t0.Add(time.Hour))
	id := mustScalar[string](t, f.pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after, created_at)
	   VALUES ($1, 'k', 'n', 'not a pem', $2, $3, $4) RETURNING id::text`, tenant, t0, t0.Add(time.Hour), t0.Add(time.Hour))
	r := mustPKI(t, f.pool, PKIFilter{Validity: pkiValidity})
	n, ok := byID(r.WorkspaceCAs)[id]
	if !ok {
		t.Fatal("the unparsable CA is missing")
	}
	if n.Subject != nil || n.Issuer != nil || n.Serial != nil || n.ParentID != nil || n.CRL == nil {
		t.Errorf("unparsable CA = %+v: want no subject/issuer/serial/parent, CRL still reported", n)
	}
}

// The relay CRL is signed by the intermediate the PKI service loads. With
// more than one intermediate row that one cannot be named from the data, so
// no intermediate claims it rather than a guessed one.
func TestPKI_RelayCRLNeedsASingleIntermediate(t *testing.T) {
	f := newPKIFixture(t)
	other := newCA(t, "Second Intermediate", 3, f.root)
	mustExec(t, f.pool, `INSERT INTO ca_intermediate (encrypted_key, nonce, certificate_pem, not_before, not_after) VALUES ('k','n',$1,$2,$3)`,
		other.pem, other.cert.NotBefore, other.cert.NotAfter)
	r := mustPKI(t, f.pool, PKIFilter{Validity: pkiValidity})
	if len(r.Intermediates) != 2 {
		t.Fatalf("intermediates = %d", len(r.Intermediates))
	}
	for _, n := range r.Intermediates {
		if n.CRL != nil {
			t.Errorf("intermediate %s claims the relay CRL with two intermediates present", n.ID)
		}
		if n.ParentID == nil || *n.ParentID != f.rootID {
			t.Errorf("intermediate %s parent = %v", n.ID, n.ParentID)
		}
	}
}

func TestPKI_NoSecretsAndFixedStatements(t *testing.T) {
	f := newPKIFixture(t)
	cq := &countingQuerier{Querier: f.pool}
	r := mustPKI(t, cq, PKIFilter{Validity: pkiValidity})
	if cq.n != 5 {
		t.Fatalf("PKI ran %d statements, want 5 (CAs, relay CRL, total, page, workspace CRLs)", cq.n)
	}
	b, _ := json.Marshal(r)
	body := strings.ToLower(string(b))
	for _, bad := range []string{"canary", "begin", "-----", "private", "encrypted", "nonce", "pem"} {
		if strings.Contains(body, bad) {
			t.Errorf("PKI response contains %q", bad)
		}
	}
}

func TestPKIDTOSurface(t *testing.T) {
	for name, c := range map[string]struct {
		v    any
		want []string
	}{
		"PKIHierarchy": {PKIHierarchy{}, []string{"intermediates", "next_cursor", "roots", "workspace_cas", "workspace_cas_total"}},
		"CANode": {CANode{}, []string{"crl", "id", "issuer", "kind", "name", "not_after", "not_before", "parent_id",
			"serial", "subject", "tenant_id"}},
		"CRLStatus": {CRLStatus{}, []string{"covers", "entries", "url", "valid_for_seconds"}},
	} {
		if got := jsonKeys(t, c.v); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s keys = %v\nwant %v", name, got, c.want)
		}
	}
}
