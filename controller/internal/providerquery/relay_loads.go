package providerquery

import (
	"context"
	"fmt"
)

// RelayLoadsCap bounds RelayLoads. Relays are operator-created and few; a
// fleet beyond this is read in part and flagged, never silently.
const RelayLoadsCap = 1000

// RelayLoads returns every relay that is not revoked or deleted, with its
// status, connections, bandwidth and headroom exactly as GET /provider/relays
// shows them (same columns, same derivation), unpaged and ordered by id.
//
// It is the ONE reader of current relay load (Sprint 24 path.md, conflict
// zones): the F26 alert evaluator (A1: relay offline, near / over capacity)
// and the F23 metric sampler (M1) both use it, so an alert and a chart never
// disagree about the same relay. truncated reports that RelayLoadsCap cut
// the list. The persisted heartbeat is used (no Valkey read per relay).
func RelayLoads(ctx context.Context, q Querier) (relays []RelaySummary, truncated bool, err error) {
	rows, err := q.Query(ctx, `
SELECT`+relaySummaryColumns+relayFrom+`
 WHERE r.status NOT IN ('revoked', 'deleted')
 ORDER BY r.id
 LIMIT $1`, RelayLoadsCap+1)
	if err != nil {
		return nil, false, fmt.Errorf("relay loads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		s, err := scanRelaySummary(rows)
		if err != nil {
			return nil, false, fmt.Errorf("relay loads: scan: %w", err)
		}
		relays = append(relays, s)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("relay loads: %w", err)
	}
	if len(relays) > RelayLoadsCap {
		return relays[:RelayLoadsCap], true, nil
	}
	return relays, false, nil
}
