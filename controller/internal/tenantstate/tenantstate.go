// Package tenantstate is the one definition of "may this tenant act now"
// (Sprint 23 S1, Decision Record D-45).
//
// A tenant (workspace) is suspended by the provider when
// workspaces.suspended_at is set. workspaces.status stays 'active' (D-34), so
// the existing status='active' predicates are not involved: suspension is
// checked only through this package.
//
// Every path D-45 refuses while suspended calls Check (Go) or embeds
// NotSuspended (SQL, for checks that must be atomic with a write): admin and
// client login, token refresh, new client sessions, configuration mutations
// and enrollment of new connectors, shields and client devices. A lookup
// error is treated as ineligible by every caller (fail closed).
//
// Agents are deliberately NOT gated here (D-08): an existing connector's or
// shield's authentication, renewals and control streams never consult this
// package. A suspended tenant's agents get a deny-all ACL snapshot instead.
//
// The package imports only the standard library and pgx so every layer (auth,
// client, connector, shield, policy, relay, middleware) can use it.
package tenantstate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrSuspended means the tenant is suspended by the provider.
var ErrSuspended = errors.New("tenant suspended")

// ErrNotFound means no such workspace. Callers treat it as ineligible.
var ErrNotFound = errors.New("tenant not found")

// Code is the machine-readable error code shown to tenant users (admin UI,
// client, APIs) when a request is refused because the tenant is suspended.
const Code = "TENANT_SUSPENDED"

// Message is the user-facing text for a refusal (D-45: a clear message, not
// a generic error). It never includes the provider's reason.
const Message = "this workspace is suspended by the provider"

// Querier is what Check needs: a pool, a connection or a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NotSuspended returns a SQL boolean expression over a workspaces row aliased
// alias: true while the tenant is not suspended. It is the SQL form of Check,
// for conditions that must hold atomically with a write.
func NotSuspended(alias string) string {
	return alias + ".suspended_at IS NULL"
}

// Suspended returns the negation of NotSuspended.
func Suspended(alias string) string {
	return alias + ".suspended_at IS NOT NULL"
}

// Check reports whether the workspace may act now: nil when it may,
// ErrSuspended when it is suspended, ErrNotFound when it doesn't exist, and
// any other error when the state could not be read. Callers deny on every
// non-nil error (fail closed).
func Check(ctx context.Context, q Querier, workspaceID string) error {
	if workspaceID == "" {
		return ErrNotFound
	}
	var suspended bool
	err := q.QueryRow(ctx,
		`SELECT `+Suspended("w")+` FROM workspaces w WHERE w.id::text = $1`,
		workspaceID).Scan(&suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("tenantstate: read suspension: %w", err)
	}
	if suspended {
		return ErrSuspended
	}
	return nil
}

// CheckForShare is Check inside a transaction, holding a share lock on the
// workspace row until the transaction ends. A suspension (which updates the
// row) then waits for the caller's transaction, or the caller waits for the
// suspension and sees it: an enrollment can't commit after a suspension it
// didn't see. q must be a transaction.
func CheckForShare(ctx context.Context, q Querier, workspaceID string) error {
	if workspaceID == "" {
		return ErrNotFound
	}
	var suspended bool
	err := q.QueryRow(ctx,
		`SELECT `+Suspended("w")+` FROM workspaces w WHERE w.id::text = $1 FOR SHARE`,
		workspaceID).Scan(&suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("tenantstate: read suspension: %w", err)
	}
	if suspended {
		return ErrSuspended
	}
	return nil
}

// IsSuspended reports whether err is (or wraps) ErrSuspended. Callers use it
// to choose the suspended message; any other error is still a refusal.
func IsSuspended(err error) bool { return errors.Is(err, ErrSuspended) }
