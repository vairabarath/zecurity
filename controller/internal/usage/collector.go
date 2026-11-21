package usage

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/relay"
)

// Sprint 25 U1b (F32; Decision Record D-57, D-58, D-59): the usage
// collector. Usage is counted, never estimated:
//
//   - Relayed bytes come from R1's cumulative per-workspace relay counters
//     (D-54). Each heartbeat adds the difference from the counters already
//     counted, which are stored (usage_relay_baselines) in the same
//     transaction as the usage they produced: a controller restart loses
//     nothing and a failed write counts nothing, so the next heartbeat
//     counts the whole span once. Bytes are attributed to the UTC hour the
//     heartbeat arrived in.
//   - Session, connector and shield minutes and monthly active users are
//     counted once per collected minute; the minute's row in
//     usage_collector_minutes, written in the same transaction, makes a
//     re-run count nothing. A minute the controller missed is unmetered and
//     leaves its hour incomplete; it is never back-filled.
//
// Single controller (D-02): process-local, no cross-replica locking.

// Meters (D-57); the names match 071_usage's CHECK.
const (
	MeterRelayBytesToClients    = "relay_bytes_to_clients"
	MeterRelayBytesToConnectors = "relay_bytes_to_connectors"
	MeterSessionMinutes         = "session_minutes"
	MeterConnectorMinutes       = "connector_minutes"
	MeterShieldMinutes          = "shield_minutes"
	// MeterActiveUsers is monthly, from usage_active_users (not usage_hourly).
	MeterActiveUsers = "active_users"
)

// DefaultRetentionMonths is D-58's retention.
const DefaultRetentionMonths = 25

// relayRunMarker is the tenant id of a relay's run marker row in
// usage_relay_baselines: it records that this relay run is being watched,
// so a tenant that first appears during the run is counted from 0 and a new
// run (a relay restart) counts its counters from the restart.
const relayRunMarker = "00000000-0000-0000-0000-000000000000"

// restartSlackSeconds matches R1 and B2: heartbeat delivery jitter.
const restartSlackSeconds = 5

// lockKey serialises a minute's collection with a tenant purge (D-58), so a
// purge can never leave a purged tenant's user ids behind: the collector's
// minute transaction and the purge's usage freeze take the same
// transaction-scoped advisory lock. ASCII "usage" as a number.
const lockKey int64 = 0x7573616765

// Collector writes the usage ledger.
type Collector struct {
	DB *pgxpool.Pool
	// Now overrides the clock in tests.
	Now func() time.Time
	// Retention overrides D-58's 25 months (tests, USAGE_RETENTION); 0 =
	// DefaultRetentionMonths.
	Retention time.Duration

	lastPrunedHour time.Time
}

var _ relay.WorkspaceUsageSink = (*Collector)(nil)

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// Run collects every minute until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	log.Printf("usage collector: every minute (kept %s)", c.retentionText())
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		c.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Collector) retentionText() string {
	if c.Retention > 0 {
		return c.Retention.String()
	}
	return fmt.Sprintf("%d months", DefaultRetentionMonths)
}

// Tick counts the current minute and, once per hour, prunes past retention.
func (c *Collector) Tick(ctx context.Context) {
	now := c.now()
	if _, err := c.CollectMinute(ctx, now); err != nil {
		log.Printf("usage collector: minute %s: %v (unmetered)", now.Truncate(time.Minute).Format(time.RFC3339), err)
	}
	if hour := now.Truncate(time.Hour); !c.lastPrunedHour.Equal(hour) {
		if err := c.Prune(ctx, now); err != nil {
			log.Printf("usage collector: prune: %v", err)
		} else {
			c.lastPrunedHour = hour
		}
	}
}

// CollectMinute counts the minute containing now: session, connector and
// shield minutes, monthly active users, and the tenant snapshots. It
// reports false when the minute was already counted (nothing is written).
func (c *Collector) CollectMinute(ctx context.Context, now time.Time) (bool, error) {
	now = now.UTC()
	minute := now.Truncate(time.Minute)
	hour := now.Truncate(time.Hour)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	counted := false
	err := pgx.BeginTxFunc(ctx, c.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
			return fmt.Errorf("lock: %w", err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO usage_collector_minutes (minute) VALUES ($1) ON CONFLICT DO NOTHING`, minute)
		if err != nil {
			return fmt.Errorf("record minute: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already counted
		}
		// $1 = the instant measured (as M1), $2 = the hour. Zero counts are
		// not stored: a complete hour without a row is 0.
		if _, err := tx.Exec(ctx, `
INSERT INTO usage_hourly (tenant_id, meter, hour, quantity)
SELECT w.id, v.meter, $2, v.q
  FROM workspaces w
 CROSS JOIN LATERAL (VALUES
       ('`+MeterSessionMinutes+`', (SELECT count(*) FROM client_devices d
                                     WHERE d.workspace_id = w.id AND `+providerquery.LiveClientSessionSQL+`)),
       ('`+MeterConnectorMinutes+`', (SELECT count(*) FROM connectors c
                                       WHERE c.tenant_id = w.id AND `+providerquery.ActiveConnectorSQL+`)),
       ('`+MeterShieldMinutes+`', (SELECT count(*) FROM shields sh
                                    WHERE sh.tenant_id = w.id AND `+providerquery.ActiveShieldSQL+`))
       ) AS v(meter, q)
 WHERE `+providerquery.CountedTenantSQL+` AND v.q > 0
ON CONFLICT (tenant_id, meter, hour) DO UPDATE SET quantity = usage_hourly.quantity + EXCLUDED.quantity`,
			now, hour); err != nil {
			return fmt.Errorf("time meters: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO usage_active_users (tenant_id, month, user_id, first_seen)
SELECT DISTINCT d.workspace_id, $2::date, d.user_id, $1::timestamptz
  FROM client_devices d
  JOIN workspaces w ON w.id = d.workspace_id
 WHERE `+providerquery.CountedTenantSQL+` AND `+providerquery.LiveClientSessionSQL+`
ON CONFLICT DO NOTHING`, now, month); err != nil {
			return fmt.Errorf("active users: %w", err)
		}
		if err := upsertTenantSnapshots(ctx, tx, now); err != nil {
			return err
		}
		counted = true
		return nil
	})
	return counted, err
}

// upsertTenantSnapshots keeps usage_tenants current for every counted tenant
// (rewritten only when the name, slug or plan changed; never after purge).
func upsertTenantSnapshots(ctx context.Context, tx pgx.Tx, now time.Time) error {
	if _, err := tx.Exec(ctx, `
INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, updated_at)
SELECT w.id, w.name, w.slug, p.name, $1
  FROM workspaces w
  LEFT JOIN plans p ON p.id = w.plan_id
 WHERE `+providerquery.CountedTenantSQL+`
ON CONFLICT (tenant_id) DO UPDATE
   SET name = EXCLUDED.name, slug = EXCLUDED.slug, plan_name = EXCLUDED.plan_name, updated_at = EXCLUDED.updated_at
 WHERE usage_tenants.purged_at IS NULL
   AND (usage_tenants.name, usage_tenants.slug, usage_tenants.plan_name)
       IS DISTINCT FROM (EXCLUDED.name, EXCLUDED.slug, EXCLUDED.plan_name)`, now); err != nil {
		return fmt.Errorf("tenant snapshots: %w", err)
	}
	return nil
}

// RecordRelayCounters counts the bytes one relay heartbeat adds (D-59). It
// implements relay.WorkspaceUsageSink and logs its own errors: the
// heartbeat must not fail on usage, and a failed write counts nothing, so
// the next heartbeat counts the whole span once.
func (c *Collector) RecordRelayCounters(ctx context.Context, relayID string, at time.Time, uptimeSeconds uint64, counters map[string]relay.WorkspaceTrafficSample) {
	if err := c.recordRelayCounters(ctx, relayID, at, uptimeSeconds, counters); err != nil {
		log.Printf("usage collector: relay %s heartbeat: %v (counted with the next heartbeat)", relayID, err)
	}
}

type baseline struct {
	startedAt               time.Time
	toClients, toConnectors uint64
	at                      time.Time
}

func (c *Collector) recordRelayCounters(ctx context.Context, relayID string, at time.Time, uptimeSeconds uint64, counters map[string]relay.WorkspaceTrafficSample) error {
	at = at.UTC()
	startedAt := at.Add(-time.Duration(uptimeSeconds) * time.Second).Truncate(time.Second)
	hour := at.Truncate(time.Hour)
	tenants := make([]string, 0, len(counters))
	for ws := range counters {
		tenants = append(tenants, ws)
	}
	sort.Strings(tenants) // stable lock order on the baseline rows
	return pgx.BeginTxFunc(ctx, c.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// The relay's run marker, locked: heartbeats of one relay are
		// counted one at a time.
		var marker *baseline
		var m baseline
		err := tx.QueryRow(ctx, `SELECT relay_started_at, at FROM usage_relay_baselines
			 WHERE relay_id = $1 AND tenant_id = $2 FOR UPDATE`, relayID, relayRunMarker).Scan(&m.startedAt, &m.at)
		switch {
		case err == nil:
			marker = &m
		case err != pgx.ErrNoRows:
			return fmt.Errorf("read run marker: %w", err)
		}
		if marker != nil && !at.After(marker.at) {
			return nil // a late or repeated heartbeat: already counted
		}
		// Same run as the marker (R1's rule): the relay has been up at least
		// since the last counted heartbeat.
		sameRun := marker != nil && float64(uptimeSeconds)+restartSlackSeconds >= at.Sub(marker.at).Seconds()

		prev := map[string]baseline{}
		if len(tenants) > 0 {
			rows, err := tx.Query(ctx, `SELECT tenant_id::text, relay_started_at, to_clients, to_connectors, at
				  FROM usage_relay_baselines WHERE relay_id = $1 AND tenant_id = ANY($2::uuid[])
				 ORDER BY tenant_id FOR UPDATE`, relayID, tenants)
			if err != nil {
				return fmt.Errorf("read baselines: %w", err)
			}
			for rows.Next() {
				var id string
				var b baseline
				var tc, tn int64
				if err := rows.Scan(&id, &b.startedAt, &tc, &tn, &b.at); err != nil {
					rows.Close()
					return fmt.Errorf("read baselines: %w", err)
				}
				b.toClients, b.toConnectors = uint64(tc), uint64(tn)
				prev[id] = b
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read baselines: %w", err)
			}
		}

		for _, ws := range tenants {
			cur := counters[ws]
			dClients, dConnectors := delta(marker != nil, sameRun, prev, ws, cur)
			for _, add := range []struct {
				meter string
				n     uint64
			}{{MeterRelayBytesToClients, dClients}, {MeterRelayBytesToConnectors, dConnectors}} {
				if add.n == 0 {
					continue
				}
				if _, err := tx.Exec(ctx, `INSERT INTO usage_hourly (tenant_id, meter, hour, quantity) VALUES ($1, $2, $3, $4)
					ON CONFLICT (tenant_id, meter, hour) DO UPDATE SET quantity = usage_hourly.quantity + EXCLUDED.quantity`,
					ws, add.meter, hour, int64(add.n)); err != nil {
					return fmt.Errorf("add %s: %w", add.meter, err)
				}
			}
			if err := putBaseline(ctx, tx, relayID, ws, startedAt, cur.ToClient, cur.ToConnector, at); err != nil {
				return err
			}
		}
		return putBaseline(ctx, tx, relayID, relayRunMarker, startedAt, 0, 0, at)
	})
}

// delta is what one tenant's counters add since the last counted heartbeat:
//   - first sight of this relay (no run marker): nothing — the counters may
//     hold bytes from before metering started; they become the baseline;
//   - a new relay run (restart): the counters themselves — everything since
//     the restart is new (only the bytes between the last counted heartbeat
//     and the restart are lost);
//   - a tenant new in a watched run, or its counters lower than its baseline
//     (the relay dropped and re-created its entry): the counters themselves;
//   - otherwise the difference.
func delta(watched, sameRun bool, prev map[string]baseline, ws string, cur relay.WorkspaceTrafficSample) (toClients, toConnectors uint64) {
	if !watched {
		return 0, 0
	}
	b, ok := prev[ws]
	if !sameRun || !ok || cur.ToClient < b.toClients || cur.ToConnector < b.toConnectors {
		return cur.ToClient, cur.ToConnector
	}
	return cur.ToClient - b.toClients, cur.ToConnector - b.toConnectors
}

func putBaseline(ctx context.Context, tx pgx.Tx, relayID, tenantID string, startedAt time.Time, toClients, toConnectors uint64, at time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO usage_relay_baselines (relay_id, tenant_id, relay_started_at, to_clients, to_connectors, at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (relay_id, tenant_id) DO UPDATE
		   SET relay_started_at = EXCLUDED.relay_started_at, to_clients = EXCLUDED.to_clients,
		       to_connectors = EXCLUDED.to_connectors, at = EXCLUDED.at`,
		relayID, tenantID, startedAt, int64(toClients), int64(toConnectors), at); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	return nil
}

// Prune deletes usage older than the retention (D-58), and the snapshots of
// purged tenants with no usage left.
func (c *Collector) Prune(ctx context.Context, now time.Time) error {
	now = now.UTC()
	cutoff := now.AddDate(0, -DefaultRetentionMonths, 0)
	if c.Retention > 0 {
		cutoff = now.Add(-c.Retention)
	}
	cutoffMonth := time.Date(cutoff.Year(), cutoff.Month(), 1, 0, 0, 0, 0, time.UTC)
	return pgx.BeginTxFunc(ctx, c.DB, pgx.TxOptions{}, func(tx pgx.Tx) error {
		for _, q := range []struct {
			sql string
			arg time.Time
		}{
			{`DELETE FROM usage_hourly WHERE hour < $1`, cutoff},
			{`DELETE FROM usage_collector_minutes WHERE minute < $1`, cutoff},
			// A month is kept while any of it is inside the retention.
			{`DELETE FROM usage_active_users WHERE month < $1`, cutoffMonth},
			{`DELETE FROM usage_active_user_counts WHERE month < $1`, cutoffMonth},
			// Baselines of relays not heard from within the retention.
			{`DELETE FROM usage_relay_baselines WHERE at < $1`, cutoff},
		} {
			if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
				return fmt.Errorf("prune: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM usage_tenants t
			 WHERE t.purged_at IS NOT NULL
			   AND NOT EXISTS (SELECT 1 FROM usage_hourly h WHERE h.tenant_id = t.tenant_id)
			   AND NOT EXISTS (SELECT 1 FROM usage_active_user_counts a WHERE a.tenant_id = t.tenant_id)`); err != nil {
			return fmt.Errorf("prune tenants: %w", err)
		}
		return nil
	})
}

// FreezeTenant runs inside the tenant purge's transaction (D-58, amends
// D-46), before the workspace is deleted: the tenant's usage_tenants
// snapshot is written (if missing) and frozen with purged_at, and its
// monthly active users become counts, so its usage keeps its id and name
// but no user ids. It takes the collector's lock, so no minute can add user
// rows for the tenant once the freeze has run.
func FreezeTenant(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("usage freeze: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, updated_at, purged_at)
SELECT w.id, w.name, w.slug, p.name, now(), now()
  FROM workspaces w LEFT JOIN plans p ON p.id = w.plan_id
 WHERE w.id = $1
ON CONFLICT (tenant_id) DO UPDATE
   SET name = EXCLUDED.name, slug = EXCLUDED.slug, plan_name = EXCLUDED.plan_name,
       updated_at = EXCLUDED.updated_at, purged_at = EXCLUDED.purged_at
 WHERE usage_tenants.purged_at IS NULL`, tenantID); err != nil {
		return fmt.Errorf("usage freeze: snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO usage_active_user_counts (tenant_id, month, users)
SELECT tenant_id, month, count(*) FROM usage_active_users WHERE tenant_id = $1 GROUP BY tenant_id, month
ON CONFLICT (tenant_id, month) DO UPDATE SET users = usage_active_user_counts.users + EXCLUDED.users`, tenantID); err != nil {
		return fmt.Errorf("usage freeze: active-user counts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM usage_active_users WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("usage freeze: drop user ids: %w", err)
	}
	return nil
}
