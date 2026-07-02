package shield

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *service) UpdateShieldHealth(ctx context.Context, shieldID, connectorID, status, version, lanIP string, lastHeartbeatAt int64) (bool, error) {
	var connectorChanged bool
	err := s.db.QueryRow(ctx,
		`WITH current AS (
		     SELECT connector_id
		       FROM shields
		      WHERE id = $4
		   ),
		   updated AS (
		     UPDATE shields sh
		        SET connector_id       = $5,
		            last_heartbeat_at = to_timestamp($1),
		            status            = $2,
		            version           = $3,
		            lan_ip            = NULLIF($6, ''),
		            updated_at        = NOW()
		       FROM connectors c
		      WHERE sh.id = $4
		        AND c.id = $5
		        AND c.tenant_id = sh.tenant_id
		        AND c.remote_network_id = sh.remote_network_id
		        AND c.status = 'active'
		      RETURNING (SELECT current.connector_id FROM current) IS DISTINCT FROM $5 AS connector_changed
		   )
		   SELECT connector_changed FROM updated`,
		lastHeartbeatAt, status, version, shieldID, connectorID, lanIP,
	).Scan(&connectorChanged)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return connectorChanged, nil
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
