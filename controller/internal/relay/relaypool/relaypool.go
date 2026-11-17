// Package relaypool holds the shared definitions of relay pools (Decision
// Record D-37 … D-41): the built-in default pool, region code validation and
// the SQL that says which pools a workspace is authorized on. It imports
// nothing but the standard library, so the relay store, the connector control
// stream and providerquery (which must not depend on net/http, D-04, not even
// through the gRPC-generated packages) share one definition. The per-pool
// relay list set lives in package relaylist. Besides the standard library it
// imports only package eligible (one SQL constant, no dependencies).
package relaypool

import (
	"regexp"
	"time"

	"github.com/yourorg/ztna/controller/internal/relay/eligible"
)

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
// authorized pools, that is its primary pool, its failover pool (D-39: same
// region by rule, so failover and failback never leave the authorization),
// or its previous pool while a graceful migration window is open (D-40). The
// same definition serves placement validation and the relay policy (PR 3).
func AuthorizedPools(poolExpr string) string {
	return `(w.relay_pool_id = ` + poolExpr + `
	         OR w.relay_failover_pool_id = ` + poolExpr + `
	         OR (w.relay_pool_previous_id = ` + poolExpr + ` AND w.relay_pool_previous_until > NOW()))`
}

// EffectivePool returns a SQL expression over a workspaces row aliased w and
// its primary pool's row aliased primaryAlias: the pool whose relays the
// workspace's connectors receive (D-39). That is the failover pool while the
// primary is exhausted and a failover pool is set, otherwise the primary. An
// exhausted primary without a failover pool stays the effective pool, so its
// connectors get its empty list (fail closed, never an implicit fallback).
//
// F11-2 (D-39 refinement, 2026-11-17): a tenant is never kept on a failover
// pool with no eligible relay while its primary has one. The rule is per
// tenant (each has its own failover pool) and derived on every read: the
// primary's stored failover state and hold-down are untouched, and tenants
// of the same primary with a healthy failover pool still wait out the
// hold-down. With both pools empty the failover pool stays effective (an
// empty list either way, never another pool).
//
// This is the only definition of a tenant's effective pool: both relay-list
// delivery paths (the broadcast set and the connect-time list) and the
// tenant read use it, so the console can't report a path the connectors
// don't get.
func EffectivePool(primaryAlias string) string {
	return `(CASE WHEN w.relay_failover_pool_id IS NOT NULL AND ` + primaryAlias + `.exhausted_at IS NOT NULL
	               AND (EXISTS (SELECT 1 FROM relays WHERE pool_id = w.relay_failover_pool_id AND ` + eligible.Where + `)
	                    OR NOT EXISTS (SELECT 1 FROM relays WHERE pool_id = w.relay_pool_id AND ` + eligible.Where + `))
	              THEN w.relay_failover_pool_id ELSE w.relay_pool_id END)`
}

// Failover states of a pool as shown by the API (D-39).
const (
	StateHealthy    = "healthy"     // not exhausted
	StateFailedOver = "failed_over" // exhausted: tenants with a failover pool use it
	StateRecovering = "recovering"  // exhausted, eligible again, inside the hold-down
	// StateNoEligibleRelays is a read-only state (F11-1): the pool isn't
	// failed over or recovering, but has no eligible relay, so a tenant whose
	// connectors get its list has no relay path. Failover state is tracked
	// only for pools some tenant uses as primary with a failover pool, so an
	// empty failover pool (or an empty primary without one) would otherwise
	// read "healthy".
	StateNoEligibleRelays = "no_eligible_relays"
)

// ReadState is the state reads show: State, except that a pool that is
// neither failed over nor recovering and has no eligible relay reads
// StateNoEligibleRelays, never healthy (F11-1).
func ReadState(exhaustedAt, eligibleSince *time.Time, eligibleRelays int64) string {
	s := State(exhaustedAt, eligibleSince)
	if s == StateHealthy && eligibleRelays == 0 {
		return StateNoEligibleRelays
	}
	return s
}

// A tenant's relay path (F11-1): the pool whose list its connectors get, or
// none when that pool has no eligible relay.
const (
	RelayPathPrimary  = "primary"
	RelayPathFailover = "failover"
	RelayPathNone     = "none"
)

// State returns a pool's failover state from its stored fields.
func State(exhaustedAt, eligibleSince *time.Time) string {
	switch {
	case exhaustedAt == nil:
		return StateHealthy
	case eligibleSince != nil:
		return StateRecovering
	default:
		return StateFailedOver
	}
}
