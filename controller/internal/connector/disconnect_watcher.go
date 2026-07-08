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
func RunDisconnectWatcher(ctx context.Context, pool *pgxpool.Pool, cfg Config, notifier PolicyChangeNotifier) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			workspaceIDs, err := markDisconnected(ctx, pool, cfg.DisconnectThreshold)
			if err != nil {
				log.Printf("disconnect watcher: %v", err)
				continue
			}
			if len(workspaceIDs) > 0 {
				log.Printf("disconnect watcher: marked connector(s) disconnected workspaces=%d", len(workspaceIDs))
			}
			for _, workspaceID := range workspaceIDs {
				if notifier != nil {
					if err := notifier.NotifyPolicyChange(ctx, workspaceID); err != nil {
						log.Printf("disconnect watcher: notify policy change workspace=%s: %v", workspaceID, err)
					}
				}
			}
		}
	}
}

func markDisconnected(ctx context.Context, pool *pgxpool.Pool, threshold time.Duration) ([]string, error) {
	rows, err := pool.Query(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < NOW() - $1::interval
		    AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')
			RETURNING tenant_id::text`,
		fmt.Sprintf("%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("mark disconnected: %w", err)
	}
	defer rows.Close()

	seen := map[string]struct{}{}
	var workspaceIDs []string
	for rows.Next() {
		var workspaceID string
		if err := rows.Scan(&workspaceID); err != nil {
			return nil, fmt.Errorf("scan disconnected workspace: %w", err)
		}
		if _, ok := seen[workspaceID]; ok {
			continue
		}
		seen[workspaceID] = struct{}{}
		workspaceIDs = append(workspaceIDs, workspaceID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate disconnected workspaces: %w", err)
	}
	return workspaceIDs, nil
}
