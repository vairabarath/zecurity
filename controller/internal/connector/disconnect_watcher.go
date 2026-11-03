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
func RunDisconnectWatcher(ctx context.Context, pool *pgxpool.Pool, cfg Config, policy PolicyChangeNotifier, topology TransportChangeNotifier) {
	ticker := time.NewTicker(cfg.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			transitions, err := markDisconnected(ctx, pool, cfg.DisconnectThreshold)
			if err != nil {
				log.Printf("disconnect watcher: %v", err)
				continue
			}
			if len(transitions) > 0 {
				log.Printf("disconnect watcher: marked connector(s) disconnected workspaces=%d", len(transitions))
			}
			for workspaceID, connectorIDs := range transitions {
				if policy != nil {
					if err := policy.NotifyPolicyChange(ctx, workspaceID); err != nil {
						log.Printf("disconnect watcher: notify policy change workspace=%s: %v", workspaceID, err)
					}
				}
				if topology != nil {
					if err := topology.NotifyTopologyChange(ctx, workspaceID, connectorIDs); err != nil {
						log.Printf("disconnect watcher: notify topology change workspace=%s: %v", workspaceID, err)
					}
				}
			}
		}
	}
}

func markDisconnected(ctx context.Context, pool *pgxpool.Pool, threshold time.Duration) (map[string][]string, error) {
	rows, err := pool.Query(ctx,
		`UPDATE connectors
		    SET status = 'disconnected', updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < NOW() - $1::interval
		    AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')
			RETURNING tenant_id::text, id::text`,
		fmt.Sprintf("%d seconds", int(threshold.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("mark disconnected: %w", err)
	}
	defer rows.Close()

	seen := map[string]map[string]struct{}{}
	transitions := map[string][]string{}
	for rows.Next() {
		var workspaceID, connectorID string
		if err := rows.Scan(&workspaceID, &connectorID); err != nil {
			return nil, fmt.Errorf("scan disconnected workspace: %w", err)
		}
		if seen[workspaceID] == nil {
			seen[workspaceID] = map[string]struct{}{}
		}
		if _, ok := seen[workspaceID][connectorID]; ok {
			continue
		}
		seen[workspaceID][connectorID] = struct{}{}
		transitions[workspaceID] = append(transitions[workspaceID], connectorID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate disconnected workspaces: %w", err)
	}
	return transitions, nil
}
