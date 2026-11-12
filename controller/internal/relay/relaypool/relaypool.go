// Package relaypool holds the shared definitions of relay pools (Decision
// Record D-37 … D-41): the built-in default pool, region code validation and
// the SQL that says which pools a workspace is authorized on. It imports
// nothing but the standard library, so the relay store, the connector control
// stream and providerquery (which must not depend on net/http, D-04, not even
// through the gRPC-generated packages) share one definition. The per-pool
// relay list set lives in package relaylist.
package relaypool

import "regexp"

// DefaultPoolID is the built-in default pool (migration 057): no region, never
// residency, cannot be deleted. Relays and workspaces start in it.
const DefaultPoolID = "00000000-0000-0000-0000-000000000001"

// MaxRegionLen and the pattern match the relay_pools.region CHECK.
const MaxRegionLen = 32

var regionPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// ValidRegion reports whether code is a canonical region code: lowercase
// letters and digits in hyphen-separated parts, starting with a letter, at
// most MaxRegionLen characters (for example "in-south-1"). No normalisation is
// applied: "IN-South-1" or " in-south-1" are rejected, not rewritten.
func ValidRegion(code string) bool {
	return len(code) <= MaxRegionLen && regionPattern.MatchString(code)
}

// MaxNameRunes bounds a pool name (relay_pools.name CHECK).
const MaxNameRunes = 64

// AuthorizedPools returns a SQL boolean expression over a workspaces row
// aliased w: whether the pool poolExpr names is one of the workspace's
// authorized pools, that is its primary pool, or its previous pool while a
// graceful migration window is open (D-40). PR 2 adds the failover pool. The
// same definition serves placement validation (PR 1) and the relay policy
// (PR 3).
func AuthorizedPools(poolExpr string) string {
	return `(w.relay_pool_id = ` + poolExpr + `
	         OR (w.relay_pool_previous_id = ` + poolExpr + ` AND w.relay_pool_previous_until > NOW()))`
}
