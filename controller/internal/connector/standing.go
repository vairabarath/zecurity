package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ConnectorStanding is a connector's authoritative revocation state, read
// from the database (certificate audit bug 7). A valid certificate alone does
// not prove the connector is still trusted: revocation does not shorten the
// certificate's lifetime.
type ConnectorStanding struct {
	Status    string
	RevokedAt *time.Time
}

// Revoked reports whether the connector is revoked. Revocation is terminal,
// and either marker counts on its own: status 'revoked' or revoked_at set
// (older rows can carry one without the other).
func (s ConnectorStanding) Revoked() bool {
	return s.Status == "revoked" || s.RevokedAt != nil
}

// RowQuerier runs a single-row query. *pgxpool.Pool satisfies it.
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadConnectorStanding reads the standing of connectorID in trustDomain.
// It returns (nil, nil) when no such connector exists in that trust domain,
// including an entity ID that is not a UUID, and an error only when the
// state cannot be read.
func LoadConnectorStanding(ctx context.Context, db RowQuerier, trustDomain, connectorID string) (*ConnectorStanding, error) {
	if _, err := uuid.Parse(connectorID); err != nil {
		return nil, nil
	}
	var s ConnectorStanding
	err := db.QueryRow(ctx,
		`SELECT status, revoked_at FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&s.Status, &s.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load connector standing: %w", err)
	}
	return &s, nil
}
