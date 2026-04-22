package connector

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDisconnectWatcher is a safety net for abruptly broken streams.
// Normal Control stream shutdown marks connectors disconnected immediately.
func RunDisconnectWatcher(ctx context.Context, pool *pgxpool.Pool, cfg Config) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := markDisconnected(ctx, pool, cfg.DisconnectThreshold)
			if err != nil {
				log.Printf("disconnect watcher: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("disconnect watcher: marked %d connector(s) disconnected", n)
			}
		}
	}
}

func markDisconnected(ctx context.Context, pool *pgxpool.Pool, threshold time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < NOW() - $1::interval
		    AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')`,
		fmt.Sprintf("%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return 0, fmt.Errorf("mark disconnected: %w", err)
	}
	return tag.RowsAffected(), nil
}
