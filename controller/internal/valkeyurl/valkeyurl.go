// Package valkeyurl parses the controller's VALKEY_URL setting.
//
// Accepted form: redis://[userinfo@]host[:port][/db]
//
//   - db is a non-negative database index selected on connect (SELECT);
//     omitted or "/" means 0.
//   - userinfo is accepted and ignored, as it always has been: the
//     controller does not authenticate to Valkey with URL credentials.
//
// Before Sprint 21 K6 the "/db" part was kept as part of the dial address
// ("host:6379/15"), so any URL with a database index failed with "unknown
// port" and the controller could not start.
package valkeyurl

import (
	"fmt"
	"strconv"
	"strings"
)

// Options is a parsed VALKEY_URL.
type Options struct {
	// Addr is host[:port] for valkey.ClientOption.InitAddress.
	Addr string
	// DB is the database index for valkey.ClientOption.SelectDB.
	DB int
}

// Parse parses a redis:// URL into a dial address and database index.
func Parse(raw string) (Options, error) {
	rest, ok := strings.CutPrefix(raw, "redis://")
	if !ok {
		return Options{}, fmt.Errorf("expected redis:// URL, got %q", raw)
	}
	if strings.ContainsAny(rest, "?#") {
		return Options{}, fmt.Errorf("query and fragment are not supported in %q", raw)
	}
	hostport, dbPart, hasPath := strings.Cut(rest, "/")
	if i := strings.LastIndex(hostport, "@"); i != -1 {
		hostport = hostport[i+1:]
	}
	if hostport == "" {
		return Options{}, fmt.Errorf("missing host in %q", raw)
	}
	opts := Options{Addr: hostport}
	if hasPath && dbPart != "" {
		db, err := strconv.Atoi(dbPart)
		if err != nil || db < 0 || strings.Contains(dbPart, "/") {
			return Options{}, fmt.Errorf("invalid database index %q in %q", dbPart, raw)
		}
		opts.DB = db
	}
	return opts, nil
}
