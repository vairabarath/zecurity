package shield

import (
	"context"
	"fmt"
	"log"
	"time"
)

func (s *service) UpdateShieldHealth(ctx context.Context, shieldID, connectorID, status, version string, lastHeartbeatAt int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE shields
		    SET last_heartbeat_at = to_timestamp($1),
		        status            = $2,
		        version           = $3,
		        updated_at        = NOW()
		  WHERE id           = $4
		    AND connector_id = $5`,
		lastHeartbeatAt, status, version, shieldID, connectorID,
	)
	return err
}

func (s *service) RunDisconnectWatcher(ctx context.Context) {
	interval := s.cfg.DisconnectThreshold / 2
	if interval <= 0 {
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.markDisconnected(ctx)
			if err != nil {
				log.Printf("shield disconnect watcher: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("shield disconnect watcher: marked %d shield(s) disconnected", n)
			}
		}
	}
}

func (s *service) markDisconnected(ctx context.Context) (int64, error) {
	tag, err := s.db.Exec(ctx,
		`UPDATE shields
		    SET status = 'disconnected',
		        updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < NOW() - $1::interval
		    AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')`,
		fmt.Sprintf("%d seconds", int(s.cfg.DisconnectThreshold.Seconds())),
	)
	if err != nil {
		return 0, fmt.Errorf("mark disconnected shields: %w", err)
	}

	return tag.RowsAffected(), nil
}
