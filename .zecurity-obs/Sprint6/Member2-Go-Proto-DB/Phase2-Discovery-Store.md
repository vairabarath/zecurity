---
type: phase
status: pending
sprint: 6
member: M2
phase: Phase2-Discovery-Store
depends_on:
  - M2-D1-A
  - M2-D1-B
  - M2-D1-C
  - M2-D1-D
  - buf generate
tags:
  - go
  - db
  - discovery
---

# M2 Phase 2 — Discovery Store Package

---

## What You're Building

`controller/internal/discovery/` — DB helper package used by M3's resolvers and control handler. Two files: `config.go` and `store.go`.

---

## Files to Create

### 1. `controller/internal/discovery/config.go`

```go
package discovery

import "time"

type Config struct {
    ScanResultTTL time.Duration // default 24h — purge old scan results
}

func NewConfig() Config {
    return Config{
        ScanResultTTL: 24 * time.Hour,
    }
}
```

---

### 2. `controller/internal/discovery/store.go`

```go
package discovery

import (
    "context"
    "database/sql"
    "time"
)

// DiscoveredService mirrors the DB row.
type DiscoveredService struct {
    ShieldID    string
    Protocol    string
    Port        int
    BoundIP     string
    ServiceName string
    FirstSeen   time.Time
    LastSeen    time.Time
}

// ScanResult mirrors the DB row.
type ScanResult struct {
    RequestID     string
    ConnectorID   string
    IP            string
    Port          int
    Protocol      string
    ServiceName   string
    ReachableFrom string    // connector_id that ran the scan (same as ConnectorID, exposed to GQL)
    FirstSeen     time.Time
}

// UpsertDiscoveredServices inserts or updates discovered services for a shield.
// For added services: upsert on (shield_id, protocol, port), update last_seen.
// For removed services: delete rows matching (shield_id, protocol, port).
func UpsertDiscoveredServices(ctx context.Context, db *sql.DB, shieldID string, added, removed []DiscoveredService) error {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    now := time.Now().UTC()
    for _, svc := range added {
        _, err := tx.ExecContext(ctx, `
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
        _, err := tx.ExecContext(ctx, `
            DELETE FROM shield_discovered_services
            WHERE shield_id=$1 AND protocol=$2 AND port=$3
        `, shieldID, svc.Protocol, svc.Port)
        if err != nil {
            return err
        }
    }

    return tx.Commit()
}

// ReplaceDiscoveredServices replaces ALL discovered services for a shield (full_sync=true).
func ReplaceDiscoveredServices(ctx context.Context, db *sql.DB, shieldID string, services []DiscoveredService) error {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    if _, err := tx.ExecContext(ctx, `DELETE FROM shield_discovered_services WHERE shield_id=$1`, shieldID); err != nil {
        return err
    }

    now := time.Now().UTC()
    for _, svc := range services {
        _, err := tx.ExecContext(ctx, `
            INSERT INTO shield_discovered_services
                (shield_id, protocol, port, bound_ip, service_name, first_seen, last_seen)
            VALUES ($1, $2, $3, $4, $5, $6, $7)
        `, shieldID, svc.Protocol, svc.Port, svc.BoundIP, svc.ServiceName, now, now)
        if err != nil {
            return err
        }
    }

    return tx.Commit()
}

// GetDiscoveredServices returns all discovered services for a shield, ordered by port.
func GetDiscoveredServices(ctx context.Context, db *sql.DB, shieldID string) ([]DiscoveredService, error) {
    rows, err := db.QueryContext(ctx, `
        SELECT shield_id, protocol, port, bound_ip, service_name, first_seen, last_seen
        FROM shield_discovered_services
        WHERE shield_id=$1
        ORDER BY port ASC
    `, shieldID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var result []DiscoveredService
    for rows.Next() {
        var s DiscoveredService
        if err := rows.Scan(&s.ShieldID, &s.Protocol, &s.Port, &s.BoundIP, &s.ServiceName, &s.FirstSeen, &s.LastSeen); err != nil {
            return nil, err
        }
        result = append(result, s)
    }
    return result, rows.Err()
}

// DeleteDiscoveredService removes a single service entry.
func DeleteDiscoveredService(ctx context.Context, db *sql.DB, shieldID, protocol string, port int) error {
    _, err := db.ExecContext(ctx, `
        DELETE FROM shield_discovered_services
        WHERE shield_id=$1 AND protocol=$2 AND port=$3
    `, shieldID, protocol, port)
    return err
}

// UpsertScanResults bulk-inserts scan results for a given request.
func UpsertScanResults(ctx context.Context, db *sql.DB, connectorID string, results []ScanResult) error {
    tx, err := db.BeginTx(ctx, nil)
    if err != nil {
        return err
    }
    defer tx.Rollback()

    now := time.Now().UTC()
    for _, r := range results {
        _, err := tx.ExecContext(ctx, `
            INSERT INTO connector_scan_results
                (request_id, connector_id, ip, port, protocol, service_name, first_seen)
            VALUES ($1, $2, $3, $4, $5, $6, $7)
            ON CONFLICT (request_id, ip, port, protocol) DO NOTHING
        `, r.RequestID, connectorID, r.IP, r.Port, r.Protocol, r.ServiceName, now)
        if err != nil {
            return err
        }
    }
    return tx.Commit()
}

// GetScanResults returns all scan results for a given request_id.
func GetScanResults(ctx context.Context, db *sql.DB, requestID string) ([]ScanResult, error) {
    rows, err := db.QueryContext(ctx, `
        SELECT request_id, connector_id, ip, port, protocol, service_name, connector_id, first_seen
        FROM connector_scan_results
        WHERE request_id=$1
        ORDER BY ip, port
    `, requestID)
    if err != nil {
        return nil, err
    }
    defer rows.Close()

    var result []ScanResult
    for rows.Next() {
        var r ScanResult
        if err := rows.Scan(&r.RequestID, &r.ConnectorID, &r.IP, &r.Port, &r.Protocol, &r.ServiceName, &r.ReachableFrom, &r.FirstSeen); err != nil {
            return nil, err
        }
        result = append(result, r)
    }
    return result, rows.Err()
}

// PurgeScanResults deletes scan results older than the given cutoff.
func PurgeScanResults(ctx context.Context, db *sql.DB, olderThan time.Time) error {
    _, err := db.ExecContext(ctx, `DELETE FROM connector_scan_results WHERE first_seen < $1`, olderThan)
    return err
}
```

---

## Build Check

```bash
cd controller && go build ./...
```
