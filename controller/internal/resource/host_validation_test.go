package resource

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

// PENDING-14 F0/F1: a resource host is a usable IPv4 address or a valid DNS
// name. Anything else used to be stored and then dropped by every client.
func TestNormalizeHost(t *testing.T) {
	type want struct {
		host string
		kind HostKind
	}
	ok := map[string]want{
		"10.0.0.5":                      {"10.0.0.5", HostIPv4},
		" 10.0.0.5 ":                    {"10.0.0.5", HostIPv4},
		"\t192.168.1.33\n":              {"192.168.1.33", HostIPv4},
		"127.0.0.1":                     {"127.0.0.1", HostIPv4}, // a shield may protect a loopback-bound service
		"8.8.8.8":                       {"8.8.8.8", HostIPv4},
		"db.internal":                   {"db.internal", HostName},
		"DB.Internal.Acme.COM":          {"db.internal.acme.com", HostName},
		"db.internal.acme.com.":         {"db.internal.acme.com", HostName},
		" api-1.svc.cluster.local ":     {"api-1.svc.cluster.local", HostName},
		"10-0-0-5.nip.io":               {"10-0-0-5.nip.io", HostName},
		"123.example":                   {"123.example", HostName},
		"x.y2k":                         {"x.y2k", HostName}, // last label needs a letter, not only letters
		strings.Repeat("a", 63) + ".io": {strings.Repeat("a", 63) + ".io", HostName},
	}
	for in, w := range ok {
		got, kind, err := NormalizeHost(in)
		if err != nil || got != w.host || kind != w.kind {
			t.Errorf("NormalizeHost(%q) = %q, %v, %v; want %q, %v, nil", in, got, kind, err, w.host, w.kind)
		}
		if IsNameHost(got) != (w.kind == HostName) {
			t.Errorf("IsNameHost(%q) = %v", got, IsNameHost(got))
		}
	}

	bad := []string{
		"", "   ", ".",
		"localhost", "fileserver", // single label
		"*.acme.com", "db.*.acme.com", // no wildcards (F-4)
		"db_1.acme.com", "db .acme.com", "db..acme.com", ".acme.com", "acme.com..",
		"-db.acme.com", "db-.acme.com", "db.acme.123", "1.2.3.4.5", "10.0.0.256", "010.0.0.5", "10.0.0",
		strings.Repeat("a", 64) + ".io", strings.Repeat("a.", 127) + "io",
		"10.0.0.5:22", "10.0.0.0/24", "db.acme.com:5432", "http://db.acme.com",
		"::1", "fd00::5", "::ffff:10.0.0.5", "fe80::1%eth0",
		"0.0.0.0", "255.255.255.255", "224.0.0.1", "239.255.255.250",
	}
	for _, in := range bad {
		got, _, err := NormalizeHost(in)
		if !errors.Is(err, ErrInvalidHost) {
			t.Errorf("NormalizeHost(%q) = %q, %v; want ErrInvalidHost", in, got, err)
		}
	}

	for _, e := range []error{ErrInvalidHost, ErrNameNeedsTCP, ErrNameCollision, ErrNameProtectedMove, ErrProtectNoShield, ErrProtectNameNeedsShield} {
		var ue *apperr.UserError
		if !errors.As(e, &ue) {
			t.Errorf("%v must be a user-safe error so the console shows its message", e)
		}
	}
}

func TestCreateUpdateRefuseInvalidHost(t *testing.T) {
	db := newResourceTestDB(t)
	ctx := context.Background()
	slug := fmt.Sprintf("f0-%d", time.Now().UnixNano())

	var tenantID, networkID string
	if err := db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in").Scan(&tenantID); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`,
		tenantID).Scan(&networkID); err != nil {
		t.Fatalf("network: %v", err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM resources WHERE tenant_id = $1`, tenantID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	in := func(host string) CreateInput {
		return CreateInput{RemoteNetworkID: networkID, Name: "r-" + host, Host: host, Protocol: "tcp", PortFrom: 5432, PortTo: 5432}
	}

	t.Run("create refuses an invalid host and writes nothing", func(t *testing.T) {
		for _, h := range []string{"localhost", "*.acme.com", "fd00::5", "0.0.0.0", "10.0.0.5:22", ""} {
			if _, err := Create(ctx, db, tenantID, in(h)); !errors.Is(err, ErrInvalidHost) {
				t.Fatalf("host %q: err = %v, want ErrInvalidHost", h, err)
			}
		}
		if n := count(); n != 0 {
			t.Fatalf("%d resources written, want 0", n)
		}
	})

	t.Run("create stores the normalised host", func(t *testing.T) {
		for h, want := range map[string]string{" 10.0.0.7 ": "10.0.0.7", "DB.Acme.Example.": "db.acme.example"} {
			row, err := Create(ctx, db, tenantID, in(h))
			if err != nil {
				t.Fatalf("host %q: %v", h, err)
			}
			if row.Host != want {
				t.Fatalf("host = %q, want %q", row.Host, want)
			}
		}
	})

	t.Run("update refuses an invalid host and changes nothing", func(t *testing.T) {
		row, err := Create(ctx, db, tenantID, in("10.0.0.8"))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"localhost", "::1", "255.255.255.255", "  "} {
			name := "renamed"
			_, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Host: &h, Name: &name})
			if !errors.Is(err, ErrInvalidHost) {
				t.Fatalf("host %q: err = %v, want ErrInvalidHost", h, err)
			}
		}
		got, err := GetByID(ctx, db, tenantID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Host != "10.0.0.8" || got.Name != row.Name {
			t.Fatalf("resource changed: host=%q name=%q", got.Host, got.Name)
		}
	})
}
