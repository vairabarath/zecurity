package resource

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

// PENDING-14 F0: a resource host must be a usable IPv4 address. Anything else
// used to be stored and then dropped by every client, so the resource could
// never be reached.
func TestNormalizeHost(t *testing.T) {
	ok := map[string]string{
		"10.0.0.5":         "10.0.0.5",
		" 10.0.0.5 ":       "10.0.0.5",
		"\t192.168.1.33\n": "192.168.1.33",
		"127.0.0.1":        "127.0.0.1", // a shield may protect a loopback-bound service
		"172.16.0.1":       "172.16.0.1",
		"8.8.8.8":          "8.8.8.8",
	}
	for in, want := range ok {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}

	bad := []string{
		"", "   ",
		"db.internal", "db.internal.acme.com", "localhost", "*.acme.com",
		"10.0.0.5:22", "10.0.0.0/24", "10.0.0", "10.0.0.256", "010.0.0.5",
		"::1", "fd00::5", "::ffff:10.0.0.5", "fe80::1%eth0",
		"0.0.0.0", "255.255.255.255", "224.0.0.1", "239.255.255.250",
	}
	for _, in := range bad {
		got, err := NormalizeHost(in)
		if !errors.Is(err, ErrInvalidHost) {
			t.Errorf("NormalizeHost(%q) = %q, %v; want ErrInvalidHost", in, got, err)
		}
	}

	var ue *apperr.UserError
	if !errors.As(ErrInvalidHost, &ue) {
		t.Fatal("ErrInvalidHost must be a user-safe error so the console shows its message")
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

	t.Run("create refuses a non-IPv4 host and writes nothing", func(t *testing.T) {
		for _, h := range []string{"db.internal.acme.com", "fd00::5", "0.0.0.0", ""} {
			if _, err := Create(ctx, db, tenantID, in(h)); !errors.Is(err, ErrInvalidHost) {
				t.Fatalf("host %q: err = %v, want ErrInvalidHost", h, err)
			}
		}
		if n := count(); n != 0 {
			t.Fatalf("%d resources written, want 0", n)
		}
	})

	t.Run("create stores the trimmed address", func(t *testing.T) {
		row, err := Create(ctx, db, tenantID, in(" 10.0.0.7 "))
		if err != nil {
			t.Fatal(err)
		}
		if row.Host != "10.0.0.7" {
			t.Fatalf("host = %q, want 10.0.0.7", row.Host)
		}
	})

	t.Run("update refuses a non-IPv4 host and changes nothing", func(t *testing.T) {
		row, err := Create(ctx, db, tenantID, in("10.0.0.8"))
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"db.internal", "::1", "255.255.255.255", "  "} {
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
