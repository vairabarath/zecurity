package connector

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ShieldRevocationDispatcher drains the shield revocation outbox
// (certificate audit bug 3).
//
// Certificate audit bug 3: not implemented yet — revocations are not
// propagated to the policy or to connectors.
type ShieldRevocationDispatcher struct {
	Pool           *pgxpool.Pool
	Registry       *ConnectorRegistry
	PolicyNotifier PolicyChangeNotifier
}

// ProcessOnce delivers every pending outbox entry once.
func (d *ShieldRevocationDispatcher) ProcessOnce(ctx context.Context) error {
	return nil
}
