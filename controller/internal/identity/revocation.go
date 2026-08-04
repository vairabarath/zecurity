package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSessionRevoked is returned by CheckGeneration when a token's stamped
// generation is behind the user's current identity_generation — i.e. an admin
// action (suspend/lock, connection delete/disable, provider migration) revoked
// the session after the token was issued.
var ErrSessionRevoked = errors.New("session revoked")

// CheckGeneration enforces mass session revocation at token-refresh time.
//
// Every access token carries the users.identity_generation it was minted under
// (the "gen" claim). A refresh re-reads the current generation: if the token is
// behind, the whole session (access + refresh chain) is dead. tokenGen == 0
// means the token predates generations (legacy) and is accepted for
// generation purposes — lifecycle/status is still enforced separately.
//
// Access tokens are short-lived (≤15m), so enforcing here (rather than on every
// request) gives immediate practical revocation of the refresh chain plus a
// bounded ride-out of any live access token — with no per-request DB read.
func CheckGeneration(tokenGen, currentGen int) error {
	if tokenGen != 0 && tokenGen < currentGen {
		return fmt.Errorf("%w (token gen %d < current %d)", ErrSessionRevoked, tokenGen, currentGen)
	}
	return nil
}

// SessionInvalidator drops a user's server-side session state (the Valkey
// refresh session) so a generation bump takes effect at once, rather than only
// when the current access token expires. Implemented by internal/auth.
type SessionInvalidator interface {
	InvalidateUserSessions(ctx context.Context, userID string) error
}

// Revoker performs mass session revocation for a user by bumping
// users.identity_generation — invalidating every previously issued JWT/refresh
// whose stamped generation is now stale (enforced by CheckGeneration at
// refresh) — and best-effort deleting the live refresh session so re-refresh
// fails immediately. Used by the admin plane (PENDING-04 Phase 6/7): connection
// delete/disable, user suspend/lock, provider migration.
type Revoker struct {
	pool        *pgxpool.Pool
	invalidator SessionInvalidator // optional; DB bump is the source of truth
	publisher   EventPublisher
}

// NewRevoker wires a Revoker. invalidator may be nil (the generation bump alone
// still revokes at the next refresh); a nil publisher defaults to NopPublisher.
func NewRevoker(pool *pgxpool.Pool, inv SessionInvalidator, pub EventPublisher) *Revoker {
	if pub == nil {
		pub = NopPublisher{}
	}
	return &Revoker{pool: pool, invalidator: inv, publisher: pub}
}

// BumpGeneration increments identity_generation for a user and returns the new
// value. The DB bump is authoritative; the refresh-session delete and the audit
// event are best-effort and never fail the caller's primary action.
func (r *Revoker) BumpGeneration(ctx context.Context, tenantID, userID, actorEmail string) (int, error) {
	var gen int
	if err := r.pool.QueryRow(ctx,
		`UPDATE users SET identity_generation = identity_generation + 1, updated_at = NOW()
		 WHERE id = $1
		 RETURNING identity_generation`,
		userID,
	).Scan(&gen); err != nil {
		return 0, fmt.Errorf("bump identity_generation: %w", err)
	}

	if r.invalidator != nil {
		_ = r.invalidator.InvalidateUserSessions(ctx, userID) // best-effort; refresh re-checks anyway
	}
	_ = r.publisher.Publish(ctx, Event{
		TenantID:    tenantID,
		ActorUserID: userID,
		ActorEmail:  actorEmail,
		Action:      ActionGenerationBump,
		TargetType:  "user",
		TargetID:    userID,
		Details:     map[string]any{"generation": gen},
	})
	return gen, nil
}
