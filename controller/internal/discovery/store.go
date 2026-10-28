package discovery

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

type DiscoveredService struct {
	ShieldID    string
	Protocol    string
	Port        int
	BoundIP     string
	ServiceName string
	FirstSeen   time.Time
	LastSeen    time.Time
}

type ScanResult struct {
	RequestID     string
	ConnectorID   string
	IP            string
	Port          int
	Protocol      string
	ServiceName   string
	ReachableFrom string
	FirstSeen     time.Time
}

func UpsertDiscoveredServices(ctx context.Context, db *pgxpool.Pool, shieldID string, added, removed []DiscoveredService) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	for _, svc := range added {
		_, err := tx.Exec(ctx, `
			INSERT INTO shield_discovered_services
				(shield_id, protocol, port, bound_ip, service_name, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (shield_id, protocol, port)
			DO UPDATE SET bound_ip=EXCLUDED.bound_ip, service_name=EXCLUDED.service_name, last_seen=EXCLUDED.last_seen
		`, shieldID, svc.Protocol, svc.Port, svc.BoundIP, svc.ServiceName, now, now)
		if err != nil {
			return err
		}
	}

	for _, svc := range removed {
		_, err := tx.Exec(ctx, `
			DELETE FROM shield_discovered_services
			WHERE shield_id=$1 AND protocol=$2 AND port=$3
		`, shieldID, svc.Protocol, svc.Port)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func ReplaceDiscoveredServices(ctx context.Context, db *pgxpool.Pool, shieldID string, services []DiscoveredService) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `DELETE FROM shield_discovered_services WHERE shield_id=$1`, shieldID); err != nil {
		return err
	}

	for _, svc := range services {
		_, err := tx.Exec(ctx, `
			INSERT INTO shield_discovered_services
				(shield_id, protocol, port, bound_ip, service_name, first_seen, last_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, shieldID, svc.Protocol, svc.Port, svc.BoundIP, svc.ServiceName, now, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// ErrShieldNotFound answers a shield that doesn't exist or belongs to another
// workspace, with one user-safe message for both, so the response reveals
// nothing about other workspaces' shields or what they discovered.
var ErrShieldNotFound = apperr.UserErrorf("shield not found")

// GetDiscoveredServices returns the services discovered on one of tenantID's
// shields (an empty list if it has none). The shield's ownership is checked in
// the same query: a foreign, missing or malformed shield ID is
// ErrShieldNotFound, never another workspace's services.
// ShieldRemoteNetwork returns the remote network of one of the workspace's
// shields; a foreign, missing or malformed shield is ErrShieldNotFound.
func ShieldRemoteNetwork(ctx context.Context, db *pgxpool.Pool, tenantID, shieldID string) (string, error) {
	if _, err := uuid.Parse(shieldID); err != nil {
		return "", ErrShieldNotFound
	}
	var rn string
	err := db.QueryRow(ctx,
		`SELECT remote_network_id::text FROM shields WHERE id=$1 AND tenant_id=$2`,
		shieldID, tenantID).Scan(&rn)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrShieldNotFound
	}
	if err != nil {
		return "", err
	}
	return rn, nil
}

func GetDiscoveredServices(ctx context.Context, db *pgxpool.Pool, tenantID, shieldID string) ([]DiscoveredService, error) {
	if _, err := uuid.Parse(shieldID); err != nil {
		return nil, ErrShieldNotFound
	}
	rows, err := db.Query(ctx, `
		SELECT s.id::text, d.protocol, d.port, d.bound_ip, d.service_name, d.first_seen, d.last_seen
		FROM shields s
		LEFT JOIN shield_discovered_services d ON d.shield_id = s.id
		WHERE s.id=$1 AND s.tenant_id=$2
		ORDER BY d.port ASC
	`, shieldID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	owned := false
	result := []DiscoveredService{}
	for rows.Next() {
		owned = true
		// The service columns are NULL on the shield's row when it has none.
		var (
			s                              DiscoveredService
			protocol, boundIP, serviceName *string
			port                           *int
			firstSeen, lastSeen            *time.Time
		)
		if err := rows.Scan(&s.ShieldID, &protocol, &port, &boundIP, &serviceName, &firstSeen, &lastSeen); err != nil {
			return nil, err
		}
		if protocol == nil {
			continue
		}
		s.Protocol, s.Port, s.BoundIP, s.ServiceName = *protocol, *port, *boundIP, *serviceName
		s.FirstSeen, s.LastSeen = *firstSeen, *lastSeen
		result = append(result, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !owned {
		return nil, ErrShieldNotFound
	}
	return result, nil
}

func DeleteDiscoveredService(ctx context.Context, db *pgxpool.Pool, shieldID, protocol string, port int) error {
	_, err := db.Exec(ctx, `
		DELETE FROM shield_discovered_services
		WHERE shield_id=$1 AND protocol=$2 AND port=$3
	`, shieldID, protocol, port)
	return err
}

func UpsertScanResults(ctx context.Context, db *pgxpool.Pool, connectorID string, results []ScanResult) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	for _, r := range results {
		_, err := tx.Exec(ctx, `
			INSERT INTO connector_scan_results
				(request_id, connector_id, ip, port, protocol, service_name, first_seen)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (request_id, ip, port, protocol) DO NOTHING
		`, r.RequestID, connectorID, r.IP, r.Port, r.Protocol, r.ServiceName, now)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// GetScanResults returns tenantID's results for a scan request: only rows
// reported by tenantID's own connectors (FU-3). A result row's connector is the
// authenticated stream that reported it, and a connector's workspace never
// changes, so the join is the ownership check; the request ID is not.
// Foreign and unknown request IDs both return an empty list.
func GetScanResults(ctx context.Context, db *pgxpool.Pool, tenantID, requestID string) ([]ScanResult, error) {
	rows, err := db.Query(ctx, `
		SELECT r.request_id, r.connector_id, r.ip, r.port, r.protocol, r.service_name, r.first_seen
		FROM connector_scan_results r
		JOIN connectors c ON c.id = r.connector_id AND c.tenant_id = $2
		WHERE r.request_id=$1
		ORDER BY r.ip, r.port
	`, requestID, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ScanResult
	for rows.Next() {
		var r ScanResult
		if err := rows.Scan(&r.RequestID, &r.ConnectorID, &r.IP, &r.Port, &r.Protocol, &r.ServiceName, &r.FirstSeen); err != nil {
			return nil, err
		}
		r.ReachableFrom = r.ConnectorID
		result = append(result, r)
	}
	return result, rows.Err()
}

func PurgeScanResults(ctx context.Context, db *pgxpool.Pool, olderThan time.Time) error {
	_, err := db.Exec(ctx, `DELETE FROM connector_scan_results WHERE first_seen < $1`, olderThan)
	return err
}
