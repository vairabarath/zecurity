package geoip_test

import (
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/geoip"
	"github.com/yourorg/ztna/controller/internal/geoip/geoiptest"
)

// Documentation ranges are "reserved" for the writer, so tests use ordinary
// public ranges with made-up country assignments.
var testNets = map[string]string{
	"81.2.69.0/24":  "GB",
	"49.36.0.0/16":  "IN",
	"2a02:ff0::/32": "DE",
}

func TestResolveCountries(t *testing.T) {
	build := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	db, err := geoip.Open(geoiptest.WriteCountryDB(t, build, testNets), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		addr, country, prefix string
	}{
		{"81.2.69.142", "GB", "81.2.69.0/24"},
		{"::ffff:81.2.69.142", "GB", "81.2.69.0/24"}, // IPv4-mapped is unmapped
		{"49.36.200.7", "IN", "49.36.200.0/24"},
		{"2a02:ff0:1:2::5", "DE", "2a02:ff0:1::/48"},
		{"8.8.8.8", "", "8.8.8.0/24"},       // not in this database
		{"10.1.2.3", "", "10.1.2.0/24"},     // private: never a country
		{"127.0.0.1", "", "127.0.0.0/24"},   // loopback
		{"100.64.1.1", "", "100.64.1.0/24"}, // CGNAT
		{"fe80::1", "", "fe80::/48"},        // link-local
	} {
		got := db.Resolve(netip.MustParseAddr(tc.addr))
		if got.Country != tc.country || got.Prefix.String() != tc.prefix {
			t.Errorf("%s: got %q %s, want %q %s", tc.addr, got.Country, got.Prefix, tc.country, tc.prefix)
		}
		if tc.country != "" && !got.Build.Equal(build) {
			t.Errorf("%s: build %v, want %v", tc.addr, got.Build, build)
		}
	}
}

func TestNoDatabaseIsUnknown(t *testing.T) {
	db, err := geoip.Open("", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Resolve(netip.MustParseAddr("81.2.69.142")); got.Country != "" {
		t.Fatalf("no database: country %q, want unknown", got.Country)
	}
	bad, err := geoip.Open("/nonexistent/country.mmdb", "")
	if err != nil {
		t.Fatalf("a missing file must not stop the controller: %v", err)
	}
	if got := bad.Resolve(netip.MustParseAddr("81.2.69.142")); got.Country != "" {
		t.Fatalf("missing file: country %q, want unknown", got.Country)
	}
}

func TestDevOverrides(t *testing.T) {
	db, err := geoip.Open("", "10.0.0.0/8=in, 127.0.0.0/8=US")
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Resolve(netip.MustParseAddr("10.200.0.7")); got.Country != "IN" {
		t.Fatalf("override: %q, want IN", got.Country)
	}
	if got := db.Resolve(netip.MustParseAddr("127.0.0.1")); got.Country != "US" {
		t.Fatalf("override: %q, want US", got.Country)
	}
	for _, bad := range []string{"10.0.0.0/8", "10.0.0.0/8=IND", "nope=IN", "10.0.0.0/8=1N"} {
		if _, err := geoip.Open("", bad); err == nil {
			t.Errorf("override %q accepted", bad)
		}
	}
}

func TestReloadOnChange(t *testing.T) {
	path := geoiptest.WriteCountryDB(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), map[string]string{"81.2.69.0/24": "GB"})
	db, err := geoip.Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := db.Reload(); changed {
		t.Fatal("unchanged file reported as reloaded")
	}
	next := geoiptest.WriteCountryDB(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), map[string]string{"81.2.69.0/24": "FR"})
	b, _ := os.ReadFile(next)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(path, future, future)
	changed, err := db.Reload()
	if err != nil || !changed {
		t.Fatalf("reload: changed=%v err=%v", changed, err)
	}
	if got := db.Resolve(netip.MustParseAddr("81.2.69.1")); got.Country != "FR" {
		t.Fatalf("after reload: %q, want FR", got.Country)
	}
	if db.Build().Month() != time.October {
		t.Fatalf("build after reload: %v", db.Build())
	}
}
