// Package providerquery holds the read-only queries behind the provider read
// APIs (Sprint 21 Phase R, D-04/D-15/D-23). It returns DTOs and knows nothing
// about HTTP: it must never import net/http (enforced by a test), so a
// provider GraphQL read API could reuse it later.
//
// Response surface rule: the DTO structs are the allowed response surface.
// Every query selects exactly the columns a DTO carries, never SELECT *, so
// secret, identity and network fields can't leak through JSON omission.
package providerquery

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Querier is the read side of *pgxpool.Pool and pgx.Tx, so a caller can run
// these queries on the pool or inside its own transaction. It deliberately
// has no Exec: this package never writes.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	// ErrNotFound: the requested entity doesn't exist.
	ErrNotFound = errors.New("providerquery: not found")
	// ErrInvalidCursor: the pagination cursor isn't one this package issued.
	ErrInvalidCursor = errors.New("providerquery: invalid cursor")
	// ErrInvalidFilter: a filter value is outside its allowed set.
	ErrInvalidFilter = errors.New("providerquery: invalid filter")
	// ErrInvalidID: an id isn't a UUID.
	ErrInvalidID = errors.New("providerquery: invalid id")
)

// DetailCap bounds every nested collection in a detail response; a list that
// hits it is cut and flagged *_truncated (Sprint 21: no nested pagination).
const DetailCap = 200
