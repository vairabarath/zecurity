package audit

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Access-stream retention (PENDING-11 P-4, S-6). The admin stream is kept
// forever in v1.
const (
	// MinAccessRetentionDays is the floor the database enforces
	// (audit_access_retention_floor); ACCESS_LOG_RETENTION_DAYS can only
	// lengthen retention.
	MinAccessRetentionDays = 90
	retentionInterval      = 6 * time.Hour
	retentionBatch         = 10000
	// maxCallsPerWorkspace bounds one run's work per workspace; the rest
	// waits for the next run.
	maxCallsPerWorkspace = 100
)

// RetentionDays clamps a configured value to the floor.
func RetentionDays(configured int) int {
	if configured < MinAccessRetentionDays {
		return MinAccessRetentionDays
	}
	return configured
}

// AccessRetention purges access events older than Days, one workspace at a
// time, through the owner-defined audit_purge_access (the app role can't
// delete audit rows itself). Each call that deletes rows appends an
// audit.access_purged row to that workspace's admin stream.
type AccessRetention struct {
	Pool *pgxpool.Pool
	Days int
}

// Run purges once shortly after start, then every 6 hours.
func (r *AccessRetention) Run(ctx context.Context) {
	first := time.NewTimer(time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	t := time.NewTicker(retentionInterval)
	defer t.Stop()
	for {
		if n, err := r.PurgeOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("audit retention: %v", err)
		} else if n > 0 {
			log.Printf("audit retention: purged %d access events older than %d days", n, r.Days)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PurgeOnce runs one pass over every workspace and returns the rows deleted.
// A failure in one workspace doesn't stop the others.
func (r *AccessRetention) PurgeOnce(ctx context.Context) (int64, error) {
	days := RetentionDays(r.Days)
	rows, err := r.Pool.Query(ctx,
		`SELECT h.workspace_id::text FROM audit_stream_heads h
		  WHERE h.stream = 'access' AND h.last_seq > h.purged_through_seq`)
	if err != nil {
		return 0, err
	}
	var workspaces []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			rows.Close()
			return 0, err
		}
		workspaces = append(workspaces, ws)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	var firstErr error
	for _, ws := range workspaces {
		for i := 0; i < maxCallsPerWorkspace; i++ {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			var n int64
			// The cutoff is computed by the database clock, the same one the
			// floor check uses.
			err := r.Pool.QueryRow(ctx,
				`SELECT audit_purge_access($1, now() - make_interval(days => $2), $3)`,
				ws, days, retentionBatch).Scan(&n)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			total += n
			if n < retentionBatch {
				break
			}
		}
	}
	return total, firstErr
}
