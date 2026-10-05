package providerquery

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// now is the fixed "current time" for every certificate test.
var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func day(n float64) time.Time { return now.Add(time.Duration(n * float64(24*time.Hour))) }

// makePEM returns a public certificate PEM with the given serial and expiry.
func makePEM(t *testing.T, serial int64, notAfter time.Time) string {
	t.Helper()
	der := makeDER(t, serial, notAfter)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func makeDER(t *testing.T, serial int64, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "test"},
		NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

type fakeCtlCert struct {
	serial   string
	notAfter time.Time
	ok       bool
}

func (f fakeCtlCert) CurrentCertInfo() (string, time.Time, time.Time, bool) {
	return f.serial, f.notAfter.Add(-time.Hour), f.notAfter, f.ok
}

// certFixture seeds one live certificate of every persisted kind plus
// revoked/deleted/certless rows that must never appear.
type certFixture struct {
	pool                                      *pgxpool.Pool
	tenantA, tenantB                          string
	root, inter, wsCA, r1, r5, c1, cB, s1, d1 string
	excluded                                  []string
	ctl                                       fakeCtlCert
}

func newCertFixture(t *testing.T) certFixture {
	pool := newTestDB(t)
	f := certFixture{pool: pool, ctl: fakeCtlCert{serial: "c0ffee", notAfter: now.Add(12 * time.Hour), ok: true}}
	ex := func(id string) { f.excluded = append(f.excluded, id) }

	f.root = mustScalar[string](t, pool, `INSERT INTO ca_root (encrypted_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ('ENCCANARY', 'NONCECANARY', $1, $2, $3) RETURNING id::text`, makePEM(t, 0xabc, day(300)), day(-1000), day(300))
	f.inter = mustScalar[string](t, pool, `INSERT INTO ca_intermediate (encrypted_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ('ENCCANARY2', 'NONCECANARY2', $1, $2, $3) RETURNING id::text`, makePEM(t, 0xdef, day(20)), day(-100), day(20))

	f.tenantA = seedWorkspace(t, pool, "acme", "active", t0)
	netA := seedNetwork(t, pool, f.tenantA, "hq", "active", t0)
	f.wsCA = mustScalar[string](t, pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ($1, 'ENCCANARY3', 'NONCECANARY3', $2, $3, $4) RETURNING id::text`, f.tenantA, makePEM(t, 0x123, day(5)), day(-30), day(5))

	gone := seedWorkspace(t, pool, "gone", "deleted", t0)
	netGone := seedNetwork(t, pool, gone, "hq", "active", t0)
	ex(mustScalar[string](t, pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
	   VALUES ($1, 'e', 'n', $2, $3, $4) RETURNING id::text`, gone, makePEM(t, 0x999, day(3)), day(-30), day(3)))
	cGone := seedConnector(t, pool, gone, netGone, "gone-c", "active", t0)
	mustExec(t, pool, `UPDATE connectors SET cert_not_after=$2 WHERE id=$1`, cGone, day(2))
	ex(cGone)

	relay := func(name, status string, notAfter any) string {
		id := seedRelay(t, pool, name, status, t0)
		mustExec(t, pool, `UPDATE relays SET cert_serial=$2, cert_not_after=$3 WHERE id=$1`, id, "rs-"+name, notAfter)
		return id
	}
	f.r1 = relay("r1", "active", now.Add(-time.Hour)) // already expired, still listed
	ex(relay("r2", "revoked", day(1)))
	ex(relay("r3", "deleted", day(1)))
	ex(relay("r4", "pending", nil)) // no certificate yet
	f.r5 = relay("r5", "inactive", day(3))

	conn := func(tenant, net, name, status string, notAfter any, revoked bool) string {
		id := seedConnector(t, pool, tenant, net, name, status, t0)
		var revokedAt any
		if revoked {
			revokedAt = t0
		}
		mustExec(t, pool, `UPDATE connectors SET cert_serial=$2, cert_not_after=$3, revoked_at=$4 WHERE id=$1`, id, "cs-"+name, notAfter, revokedAt)
		return id
	}
	f.c1 = conn(f.tenantA, netA, "c1", "active", day(2), false)
	ex(conn(f.tenantA, netA, "c2", "revoked", day(2), false))
	ex(conn(f.tenantA, netA, "c3", "active", day(2), true)) // revoked_at set, status not yet updated
	ex(conn(f.tenantA, netA, "c4", "pending", nil, false))

	shield := func(name, status string, notAfter time.Time) string {
		id := seedShield(t, pool, f.tenantA, netA, f.c1, name, status, t0)
		mustExec(t, pool, `UPDATE shields SET cert_serial=$2, cert_not_after=$3 WHERE id=$1`, id, "ss-"+name, notAfter)
		return id
	}
	f.s1 = shield("s1", "active", day(40))
	ex(shield("s2", "revoked", day(1)))

	u := seedUser(t, pool, f.tenantA, "owner", "active")
	device := func(name, serial string, notAfter time.Time, revoked bool) string {
		var revokedAt any
		if revoked {
			revokedAt = t0
		}
		return mustScalar[string](t, pool, `INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after, revoked_at)
		   VALUES ($1, $2, $3, 'linux', $4, $5, $6) RETURNING id::text`, u, f.tenantA, name, serial, notAfter, revokedAt)
	}
	f.d1 = device("OWNERCANARY laptop", "ds-d1", day(10), false)
	ex(device("revoked laptop", "ds-d2", day(10), true))

	f.tenantB = seedWorkspace(t, pool, "beta", "active", t0)
	netB := seedNetwork(t, pool, f.tenantB, "hq", "active", t0)
	f.cB = conn(f.tenantB, netB, "cB", "active", day(1), false)
	return f
}

func mustCerts(t *testing.T, q Querier, ctl ControllerCertSource, f CertificateFilter) CertificateExpiryResult {
	t.Helper()
	if f.Now.IsZero() {
		f.Now = now
	}
	r, err := CertificateExpiry(context.Background(), q, ctl, f)
	if err != nil {
		t.Fatalf("CertificateExpiry(%+v): %v", f, err)
	}
	return r
}

func entityIDs(items []CertificateEntry) []string {
	out := make([]string, len(items))
	for i, e := range items {
		out[i] = e.EntityID
	}
	return out
}

func TestCertificateExpiry_AllKindsAndExclusions(t *testing.T) {
	f := newCertFixture(t)
	r := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin})

	type row struct{ kind, id, bucket string }
	var got []row
	for _, e := range r.Items {
		got = append(got, row{e.Kind, e.EntityID, e.Bucket})
	}
	want := []row{ // soonest first
		{"relay", f.r1, BucketExpired},
		{"controller_grpc", "controller", BucketLT24h},
		{"connector", f.cB, BucketLT24h},
		{"connector", f.c1, BucketLT7d},
		{"relay", f.r5, BucketLT7d},
		{"workspace_ca", f.wsCA, BucketLT7d},
		{"client_device", f.d1, BucketLT30d},
		{"intermediate", f.inter, BucketLT30d},
		{"shield", f.s1, BucketOK},
		{"root", f.root, BucketOK},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("items =\n%v\nwant\n%v", got, want)
	}
	kinds := map[string]bool{}
	for _, e := range r.Items {
		kinds[e.Kind] = true
	}
	for _, k := range CertificateKinds {
		if !kinds[k] {
			t.Errorf("kind %s missing", k)
		}
	}
	for _, id := range f.excluded {
		for _, e := range r.Items {
			if e.EntityID == id {
				t.Errorf("excluded entity %s (%s) listed", id, e.Kind)
			}
		}
	}
	wantSummary := CertificateSummary{Expired: 1, LT24h: 2, LT7d: 3, LT30d: 2, OK: 2}
	if r.Summary != wantSummary {
		t.Errorf("summary = %+v, want %+v", r.Summary, wantSummary)
	}
}

func TestCertificateExpiry_FieldsAndSerials(t *testing.T) {
	f := newCertFixture(t)
	r := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin})
	by := map[string]CertificateEntry{}
	for _, e := range r.Items {
		by[e.EntityID] = e
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}
	checks := []struct {
		id                   string
		tenant, name, serial string
	}{
		{f.root, "<nil>", "Root CA", "abc"}, // parsed from the PEM
		{f.inter, "<nil>", "Intermediate CA", "def"},
		{f.wsCA, f.tenantA, "Tenant acme", "123"}, // tenant's display name, not identity
		{"controller", "<nil>", "Controller gRPC", "c0ffee"},
		{f.r1, "<nil>", "r1", "rs-r1"},
		{f.c1, f.tenantA, "c1", "cs-c1"},
		{f.s1, f.tenantA, "s1", "ss-s1"},
		{f.d1, f.tenantA, "<nil>", "ds-d1"}, // device names are never returned
	}
	for _, c := range checks {
		e := by[c.id]
		if str(e.TenantID) != c.tenant || str(e.EntityName) != c.name || str(e.Serial) != c.serial {
			t.Errorf("%s (%s): tenant=%s name=%s serial=%s; want %s %s %s", c.id, e.Kind, str(e.TenantID), str(e.EntityName), str(e.Serial), c.tenant, c.name, c.serial)
		}
		if e.NotAfter.Location() != time.UTC {
			t.Errorf("%s not_after not UTC", c.id)
		}
	}
}

func TestCertificateExpiry_WithinWindow(t *testing.T) {
	f := newCertFixture(t)
	full := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin}).Summary

	// Default 720h: root (300d) and shield (40d) fall outside the item list.
	def := mustCerts(t, f.pool, f.ctl, CertificateFilter{})
	if len(def.Items) != 8 {
		t.Fatalf("default within: %d items, want 8: %v", len(def.Items), entityIDs(def.Items))
	}
	for _, e := range def.Items {
		if e.EntityID == f.root || e.EntityID == f.s1 {
			t.Errorf("%s beyond 720h listed", e.Kind)
		}
	}

	// Custom 48h: expired r1, controller (12h), cB (1d), c1 (2d, exactly at the edge).
	custom := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: 48 * time.Hour})
	if got := entityIDs(custom.Items); !reflect.DeepEqual(got, []string{f.r1, "controller", f.cB, f.c1}) {
		t.Fatalf("within 48h = %v", got)
	}

	// The summary ignores within.
	for name, r := range map[string]CertificateExpiryResult{"default": def, "48h": custom} {
		if r.Summary != full {
			t.Errorf("%s summary = %+v, want %+v (summary must ignore within)", name, r.Summary, full)
		}
	}
}

func TestCertificateExpiry_WithinBounds(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	if _, err := CertificateExpiry(ctx, pool, nil, CertificateFilter{Within: MaxCertWithin}); err != nil {
		t.Errorf("8760h rejected: %v", err)
	}
	for _, w := range []time.Duration{MaxCertWithin + time.Hour, -time.Hour} {
		if _, err := CertificateExpiry(ctx, pool, nil, CertificateFilter{Within: w}); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("within %s: err = %v, want ErrInvalidFilter", w, err)
		}
	}
}

func TestCertificateExpiry_KindFilter(t *testing.T) {
	f := newCertFixture(t)
	for _, k := range CertificateKinds {
		r := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin, Kind: k})
		if len(r.Items) == 0 {
			t.Errorf("kind %s: no items", k)
		}
		var total int64
		for _, e := range r.Items {
			if e.Kind != k {
				t.Errorf("kind %s: got a %s", k, e.Kind)
			}
		}
		s := r.Summary
		total = s.Expired + s.LT24h + s.LT7d + s.LT30d + s.OK
		if total != int64(len(r.Items)) {
			t.Errorf("kind %s: summary total %d, items %d (summary must honour kind)", k, total, len(r.Items))
		}
	}
	for _, bad := range []string{"Root", "tls", "' OR 1=1 --"} {
		if _, err := CertificateExpiry(context.Background(), f.pool, f.ctl, CertificateFilter{Kind: bad}); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("kind %q: err = %v", bad, err)
		}
	}
}

func TestCertificateExpiry_TenantFilter(t *testing.T) {
	f := newCertFixture(t)
	r := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin, TenantID: f.tenantA})
	if got := entityIDs(r.Items); !sameSet(got, []string{f.wsCA, f.c1, f.s1, f.d1}) {
		t.Fatalf("tenant A = %v", got)
	}
	if r.Summary != (CertificateSummary{LT7d: 2, LT30d: 1, OK: 1}) {
		t.Errorf("tenant A summary = %+v", r.Summary)
	}
	rb := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin, TenantID: f.tenantB})
	if got := entityIDs(rb.Items); !reflect.DeepEqual(got, []string{f.cB}) {
		t.Fatalf("tenant B = %v", got)
	}
	// Platform certificates have no tenant: a tenant filter plus kind=root is empty.
	if rr := mustCerts(t, f.pool, f.ctl, CertificateFilter{Within: MaxCertWithin, TenantID: f.tenantA, Kind: "root"}); len(rr.Items) != 0 {
		t.Fatalf("tenant + root = %v", entityIDs(rr.Items))
	}
	for _, bad := range []string{"acme", "1' OR '1'='1"} {
		if _, err := CertificateExpiry(context.Background(), f.pool, f.ctl, CertificateFilter{TenantID: bad}); !errors.Is(err, ErrInvalidID) {
			t.Errorf("tenant %q: err = %v, want ErrInvalidID", bad, err)
		}
	}
}

func TestCertificateExpiry_ControllerSource(t *testing.T) {
	pool := newTestDB(t)
	// No source, or a source without a certificate: no controller row.
	for name, ctl := range map[string]ControllerCertSource{"nil": nil, "not ok": fakeCtlCert{ok: false}} {
		if r := mustCerts(t, pool, ctl, CertificateFilter{Within: MaxCertWithin}); len(r.Items) != 0 || r.Summary != (CertificateSummary{}) {
			t.Errorf("%s source: %+v", name, r)
		}
	}
	// The real rotator: the row reports exactly the certificate it serves,
	// and nothing about it is written to the database.
	notAfter := now.Add(90 * time.Minute).Truncate(time.Second)
	der := makeDER(t, 0x5eed, notAfter)
	rot := pki.NewControllerCertRotator(nil, &tls.Certificate{Certificate: [][]byte{der}}, now.Add(-time.Hour), notAfter, nil)
	r := mustCerts(t, pool, rot, CertificateFilter{Within: MaxCertWithin})
	if len(r.Items) != 1 {
		t.Fatalf("items = %+v", r.Items)
	}
	e := r.Items[0]
	if e.Kind != "controller_grpc" || e.EntityID != "controller" || *e.Serial != "5eed" || !e.NotAfter.Equal(notAfter) || e.Bucket != BucketLT24h || e.TenantID != nil {
		t.Fatalf("controller row = %+v", e)
	}
	var stored int64
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM relay_certificates`).Scan(&stored)
	if stored != 0 {
		t.Fatal("controller certificate was persisted")
	}
}

func TestCertificateExpiry_PaginationAndTies(t *testing.T) {
	pool := newTestDB(t)
	tie := day(3)
	// Five relays and the controller share one not_after: order by kind, then entity_id.
	for i := 0; i < 5; i++ {
		id := seedRelay(t, pool, "tie", "active", t0)
		mustExec(t, pool, `UPDATE relays SET cert_not_after=$2 WHERE id=$1`, id, tie)
	}
	early := seedRelay(t, pool, "early", "active", t0)
	mustExec(t, pool, `UPDATE relays SET cert_not_after=$2 WHERE id=$1`, early, day(1))
	ctl := fakeCtlCert{serial: "c", notAfter: tie, ok: true}

	all := mustCerts(t, pool, ctl, CertificateFilter{Within: MaxCertWithin, Limit: 200})
	var expected []string
	expected = append(expected, early, "controller") // controller_grpc sorts before relay at the tie
	rows, err := pool.Query(context.Background(), `SELECT id::text FROM relays WHERE cert_not_after = $1 ORDER BY id::text`, tie)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		expected = append(expected, id)
	}
	rows.Close()
	if got := entityIDs(all.Items); !reflect.DeepEqual(got, expected) {
		t.Fatalf("order = %v\nwant    %v", got, expected)
	}

	var paged []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		r := mustCerts(t, pool, ctl, CertificateFilter{Within: MaxCertWithin, Limit: 2, Cursor: cursor})
		paged = append(paged, entityIDs(r.Items)...)
		if r.Summary != all.Summary {
			t.Fatalf("summary changed across pages: %+v vs %+v", r.Summary, all.Summary)
		}
		if r.NextCursor == nil {
			break
		}
		cursor = *r.NextCursor
	}
	if !reflect.DeepEqual(paged, expected) {
		t.Fatalf("paged = %v\nwant   %v", paged, expected)
	}
}

func TestCertificateExpiry_LimitsAndCursorErrors(t *testing.T) {
	pool := newTestDB(t)
	mustExec(t, pool, `INSERT INTO relays (name, status, cert_not_after)
	                   SELECT 'bulk-' || g, 'active', $1::timestamptz + g * interval '1 minute' FROM generate_series(1, 205) g`, now)
	if r := mustCerts(t, pool, nil, CertificateFilter{}); len(r.Items) != DefaultLimit || r.NextCursor == nil {
		t.Fatalf("default: %d", len(r.Items))
	}
	if r := mustCerts(t, pool, nil, CertificateFilter{Limit: 1000}); len(r.Items) != MaxLimit || r.NextCursor == nil {
		t.Fatalf("clamped: %d", len(r.Items))
	}
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"garbage":       "%%%",
		"relay cursor":  EncodeCursor(now, "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"), // a list cursor has no kind
		"unknown kind":  enc(`{"t":"2026-10-05T12:00:00Z","k":"ssh","id":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}`),
		"bad entity id": enc(`{"t":"2026-10-05T12:00:00Z","k":"relay","id":"x'; --"}`),
		"bad time":      enc(`{"t":"soon","k":"relay","id":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}`),
	} {
		if _, err := CertificateExpiry(context.Background(), pool, nil, CertificateFilter{Cursor: c}); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
}

func TestBucketBoundaries(t *testing.T) {
	cases := map[time.Duration]string{
		-time.Second: BucketExpired, 0: BucketExpired,
		time.Second: BucketLT24h, 24 * time.Hour: BucketLT24h,
		24*time.Hour + time.Second: BucketLT7d, 7 * 24 * time.Hour: BucketLT7d,
		7*24*time.Hour + time.Second: BucketLT30d, 30 * 24 * time.Hour: BucketLT30d,
		30*24*time.Hour + time.Second: BucketOK,
	}
	for d, want := range cases {
		if got := bucketFor(now.Add(d), now); got != want {
			t.Errorf("now%+v → %s, want %s", d, got, want)
		}
	}
}

func TestSerialFromPEM(t *testing.T) {
	if s := serialFromPEM(makePEM(t, 0xabc, now)); s == nil || *s != "abc" {
		t.Fatalf("serial = %v", s)
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"} {
		if s := serialFromPEM(bad); s != nil {
			t.Errorf("serialFromPEM(%q) = %v, want nil", bad, *s)
		}
	}
}

func TestCertificateExpiry_NoSecretsAndTwoStatements(t *testing.T) {
	f := newCertFixture(t)
	cq := &countingQuerier{Querier: f.pool}
	r := mustCerts(t, cq, f.ctl, CertificateFilter{Within: MaxCertWithin})
	if cq.n != 2 {
		t.Fatalf("ran %d statements, want 2 (summary, page)", cq.n)
	}
	b, _ := json.Marshal(r)
	body := strings.ToLower(string(b))
	for _, bad := range []string{"enccanary", "noncecanary", "begin", "certificate-----", "private", "encrypted", "nonce", "pem",
		"ownercanary", "user_id", "email", "jti", "token"} {
		if strings.Contains(body, bad) {
			t.Errorf("certificate response contains %q", bad)
		}
	}
}

func TestCertificateDTOSurface(t *testing.T) {
	for name, c := range map[string]struct {
		v    any
		want []string
	}{
		"CertificateEntry":        {CertificateEntry{}, []string{"bucket", "entity_id", "entity_name", "kind", "not_after", "serial", "tenant_id"}},
		"CertificateSummary":      {CertificateSummary{}, []string{"expired", "lt_24h", "lt_30d", "lt_7d", "ok"}},
		"CertificateExpiryResult": {CertificateExpiryResult{}, []string{"items", "next_cursor", "summary"}},
	} {
		if got := jsonKeys(t, c.v); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s keys = %v\nwant %v", name, got, c.want)
		}
	}
	kinds := append([]string{}, CertificateKinds...)
	sort.Strings(kinds)
	if !reflect.DeepEqual(kinds, CertificateKinds) || len(kinds) != 8 {
		t.Fatalf("CertificateKinds = %v, want the eight kinds sorted", CertificateKinds)
	}
}
