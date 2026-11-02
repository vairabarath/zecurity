// Package geoiptest writes small GeoIP2-Country-shaped .mmdb files for tests.
package geoiptest

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// WriteCountryDB writes a database mapping each CIDR to an ISO country code
// and returns its path (inside t.TempDir()).
func WriteCountryDB(t testing.TB, build time.Time, networks map[string]string) string {
	t.Helper()
	tree, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType: "DBIP-Country-Lite",
		RecordSize:   24,
		BuildEpoch:   build.Unix(),
	})
	if err != nil {
		t.Fatalf("mmdbwriter.New: %v", err)
	}
	for cidr, cc := range networks {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("parse %s: %v", cidr, err)
		}
		rec := mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(cc)}}
		if err := tree.Insert(network, rec); err != nil {
			t.Fatalf("insert %s: %v", cidr, err)
		}
	}
	path := filepath.Join(t.TempDir(), "country.mmdb")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := tree.WriteTo(f); err != nil {
		t.Fatalf("write mmdb: %v", err)
	}
	return path
}
