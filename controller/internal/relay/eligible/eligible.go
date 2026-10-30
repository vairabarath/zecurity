// Package eligible holds the one definition of a relay that connectors may be
// placed on (ADR-016; Sprint 22 Phase D, D-32). The published relay list
// (relay.Store.BuildLabelledRelayList), the last-eligible-relay drain guard and
// the provider read API (providerquery) all use it, so the guard can never
// count a relay that connectors are not offered. It is its own package because
// providerquery must not import the relay package (and its net/http, D-04).
package eligible

// Where is a SQL boolean expression over the unaliased columns of one relays
// row: active, capacity tier high or medium, not draining, and with a routable
// address (a non-empty public_addr, or public scope with an observed IP, from
// which the list builder derives the address). It is never NULL (FU-7):
// callers select it as a Go bool, and a NULL column (address_scope,
// capacity_label) must read as "not eligible".
const Where = `COALESCE((status = 'active'
	AND capacity_label IN ('high', 'medium')
	AND drain_started_at IS NULL
	AND (NULLIF(public_addr, '') IS NOT NULL
	     OR (address_scope = 'public' AND observed_ip IS NOT NULL))), false)`
