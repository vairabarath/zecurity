// Package geoip resolves a client device's country from the address the
// controller sees it connect from (PENDING-20 G1).
//
// The data is a local .mmdb country database (DB-IP Lite Country by default,
// G-3; any GeoIP2-Country-compatible file works). No address leaves the
// controller. A missing, unreadable or empty database makes every lookup
// unknown, and an unknown country never satisfies a country rule (G-4), so a
// broken database denies rather than allows.
package geoip

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oschwald/maxminddb-golang/v2"
)

// Lookup is the result of resolving one address.
type Lookup struct {
	// Country is an ISO 3166-1 alpha-2 code, or "" when unknown.
	Country string
	// Prefix is the /24 (IPv4) or /48 (IPv6) the address belongs to; the full
	// address is never kept (G-6). Invalid when the address was invalid.
	Prefix netip.Prefix
	// Build is the database build time behind Country (zero when no database
	// answered, e.g. a dev override or an unknown result without a database).
	Build time.Time
}

// Resolver resolves addresses to countries. Implementations must be safe for
// concurrent use.
type Resolver interface {
	Resolve(addr netip.Addr) Lookup
}

type dbState struct {
	reader *maxminddb.Reader
	build  time.Time
	mtime  time.Time
	size   int64
}

// DB resolves countries from a local .mmdb file, with optional
// development-only overrides. It reloads the file when it changes.
type DB struct {
	path      string
	overrides []override
	state     atomic.Pointer[dbState]
	mu        sync.Mutex // serializes reloads
}

type override struct {
	prefix  netip.Prefix
	country string
}

// Open loads the database at path (it may be empty: then every lookup that no
// override answers is unknown). devOverrides is GEOIP_DEV_OVERRIDES
// ("10.0.0.0/8=IN,127.0.0.0/8=US"); it must only be passed in development
// (G-7) — the caller enforces that.
func Open(path, devOverrides string) (*DB, error) {
	ov, err := ParseOverrides(devOverrides)
	if err != nil {
		return nil, err
	}
	db := &DB{path: strings.TrimSpace(path), overrides: ov}
	if db.path == "" {
		log.Printf("geoip: GEOIP_DB_PATH is not set; every country lookup is unknown (country rules deny)")
		return db, nil
	}
	if _, err := db.Reload(); err != nil {
		// Not fatal: the controller still runs; geofenced resources deny.
		log.Printf("geoip: load %s failed: %v; every country lookup is unknown until it loads", db.path, err)
	}
	return db, nil
}

// ParseOverrides parses "cidr=CC,cidr=CC". Codes must be ISO alpha-2.
func ParseOverrides(s string) ([]override, error) {
	var out []override
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cidr, cc, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("geoip: override %q is not cidr=CC", part)
		}
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("geoip: override %q: %w", part, err)
		}
		cc = strings.ToUpper(strings.TrimSpace(cc))
		if !ValidCountryCode(cc) {
			return nil, fmt.Errorf("geoip: override %q: %q is not an ISO 3166-1 alpha-2 code", part, cc)
		}
		out = append(out, override{prefix: prefix.Masked(), country: cc})
	}
	return out, nil
}

// Reload re-reads the file if its size or mtime changed. It reports whether a
// new database was loaded. The whole file is read into memory, so a reader in
// use is never closed under a concurrent lookup.
func (db *DB) Reload() (bool, error) {
	if db.path == "" {
		return false, nil
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	info, err := os.Stat(db.path)
	if err != nil {
		return false, err
	}
	if cur := db.state.Load(); cur != nil && cur.mtime.Equal(info.ModTime()) && cur.size == info.Size() {
		return false, nil
	}
	buf, err := os.ReadFile(db.path)
	if err != nil {
		return false, err
	}
	reader, err := maxminddb.OpenBytes(buf)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", db.path, err)
	}
	build := time.Unix(int64(reader.Metadata.BuildEpoch), 0).UTC()
	db.state.Store(&dbState{reader: reader, build: build, mtime: info.ModTime(), size: info.Size()})
	log.Printf("geoip: loaded %s (%s, built %s)", db.path, reader.Metadata.DatabaseType, build.Format("2006-01-02"))
	return true, nil
}

// Build returns the loaded database's build time (zero when none is loaded).
func (db *DB) Build() time.Time {
	if s := db.state.Load(); s != nil {
		return s.build
	}
	return time.Time{}
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// Resolve returns the address's country, or "" when it can't be known.
func (db *DB) Resolve(addr netip.Addr) Lookup {
	addr = addr.Unmap()
	out := Lookup{Prefix: NetworkPrefix(addr)}
	if !addr.IsValid() {
		return out
	}
	for _, o := range db.overrides {
		if o.prefix.Contains(addr) {
			out.Country = o.country
			return out
		}
	}
	if !IsPublic(addr) {
		return out
	}
	s := db.state.Load()
	if s == nil {
		return out
	}
	var rec countryRecord
	if err := s.reader.Lookup(addr).Decode(&rec); err != nil {
		log.Printf("geoip: lookup failed: %v", err)
		return out
	}
	if cc := strings.ToUpper(rec.Country.ISOCode); ValidCountryCode(cc) {
		out.Country = cc
		out.Build = s.build
	}
	return out
}

// IsPublic reports whether addr is a globally routed unicast address; private,
// loopback, link-local, CGNAT, multicast and unspecified addresses have no
// country.
func IsPublic(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	if cgnat.Contains(addr) {
		return false
	}
	return addr.IsGlobalUnicast()
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// NetworkPrefix returns the /24 (IPv4) or /48 (IPv6) containing addr (G-6).
func NetworkPrefix(addr netip.Addr) netip.Prefix {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return netip.Prefix{}
	}
	bits := 48
	if addr.Is4() {
		bits = 24
	}
	p, err := addr.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// ValidCountryCode reports whether s is two upper-case ASCII letters. (Whether
// the code is an assigned country is the database's business; the console
// offers only assigned codes.)
func ValidCountryCode(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// ErrNoDatabase is returned by helpers that need a loaded database.
var ErrNoDatabase = errors.New("geoip: no database loaded")
