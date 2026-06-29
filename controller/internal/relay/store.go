package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	connectorpb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

var ErrRelayNotFound = errors.New("relay not found")

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// RelayRow mirrors the relays table.
type RelayRow struct {
	ID                 string
	Name               string
	Status             string
	DNSAllowlist       []string
	IPAllowlist        []string
	EnrollmentTokenJTI *string // nullable once burned
	CertSerial         *string
	CertNotAfter       *time.Time
	Version            *string
	Hostname           *string
	PublicAddr         *string
	ObservedIP         *string
	ObservedPort       *int
	AddressScope       *string
	LastHeartbeatAt    *time.Time
}

// CreateRelay inserts a new relay row with status='pending'.
// The enrollment_token_jti is attached separately via AttachJTI once the
// caller has issued the JWT (the JWT's sub claim is the just-minted relay id).
func (s *Store) CreateRelay(ctx context.Context, name string, dnsAllowlist, ipAllowlist []string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO relays (name, dns_allowlist, ip_allowlist)
		 VALUES ($1, $2, $3)
		 RETURNING id`,
		name, dnsAllowlist, ipAllowlist,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert relay: %w", err)
	}
	return id, nil
}

// AttachJTI records the issued provisioning-token jti on the relay row.
func (s *Store) AttachJTI(ctx context.Context, id, jti string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relays SET enrollment_token_jti = $2, updated_at = NOW() WHERE id = $1`,
		id, jti,
	)
	if err != nil {
		return fmt.Errorf("attach jti: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}

// LoadRelayByID returns the relay row or ErrRelayNotFound.
func (s *Store) LoadRelayByID(ctx context.Context, id string) (*RelayRow, error) {
	r := &RelayRow{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, status, dns_allowlist, ip_allowlist,
		        enrollment_token_jti, cert_serial, cert_not_after,
		        version, hostname, public_addr, observed_ip::text,
		        observed_port, address_scope, last_heartbeat_at
		   FROM relays WHERE id = $1`,
		id,
	).Scan(&r.ID, &r.Name, &r.Status, &r.DNSAllowlist, &r.IPAllowlist,
		&r.EnrollmentTokenJTI, &r.CertSerial, &r.CertNotAfter,
		&r.Version, &r.Hostname, &r.PublicAddr, &r.ObservedIP,
		&r.ObservedPort, &r.AddressScope, &r.LastHeartbeatAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRelayNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load relay: %w", err)
	}
	return r, nil
}

// MarkProvisioned burns the jti, flips status to active, and records cert
// metadata. The Provision RPC calls this after pki.SignRelayCert succeeds.
func (s *Store) MarkProvisioned(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relays
		    SET enrollment_token_jti = NULL,
		        status               = 'active',
		        cert_serial          = $2,
		        cert_not_after       = $3,
		        version              = NULLIF($4, ''),
		        hostname             = NULLIF($5, ''),
		        updated_at           = NOW()
		  WHERE id = $1`,
		id, certSerial, certNotAfter, version, hostname,
	)
	if err != nil {
		return fmt.Errorf("mark relay provisioned: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}

// InsertProvisionedRelay creates a relays row for a self-provisioning relay
// (one that arrived at Provision without a pre-existing POST /api/relays).
// Status lands directly at 'active' since the cert has already been signed.
// ON CONFLICT keeps it race-safe if two Provision calls land in parallel.
func (s *Store) InsertProvisionedRelay(ctx context.Context, id, name string, dnsAllowlist, ipAllowlist []string, certSerial string, certNotAfter time.Time, version, hostname string) error {
	if name == "" {
		name = "relay-" + id
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO relays
		     (id, name, status, dns_allowlist, ip_allowlist,
		      cert_serial, cert_not_after, version, hostname, updated_at)
		 VALUES ($1, $2, 'active', $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NOW())
		 ON CONFLICT (id) DO UPDATE
		    SET status         = 'active',
		        cert_serial    = EXCLUDED.cert_serial,
		        cert_not_after = EXCLUDED.cert_not_after,
		        version        = EXCLUDED.version,
		        hostname       = EXCLUDED.hostname,
		        updated_at     = NOW()`,
		id, name, dnsAllowlist, ipAllowlist, certSerial, certNotAfter, version, hostname,
	)
	if err != nil {
		return fmt.Errorf("insert provisioned relay: %w", err)
	}
	return nil
}

// RecordHeartbeat marks an authenticated Relay healthy and refreshes its
// runtime and certificate metadata.
// UpsertPlacement inserts or updates a connector_relay_placement row.
// Returns true when the relay_id actually changed (new attachment or different relay)
// so the caller can decide whether to invalidate the policy cache.
func (s *Store) UpsertPlacement(ctx context.Context, connectorID, relayID string, attachedAt time.Time, source string) (bool, error) {
	var changed bool
	err := s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT relay_id FROM connector_relay_placement WHERE connector_id = $1
		), upsert AS (
			INSERT INTO connector_relay_placement
			     (connector_id, relay_id, attached_at, last_confirmed, source)
			VALUES ($1, $2, $3, NOW(), $4)
			ON CONFLICT (connector_id) DO UPDATE
			SET relay_id       = EXCLUDED.relay_id,
			    attached_at    = EXCLUDED.attached_at,
			    last_confirmed = NOW(),
			    source         = EXCLUDED.source
			RETURNING connector_id
		)
		SELECT
			CASE
				WHEN NOT EXISTS (SELECT 1 FROM old) THEN true
				WHEN EXISTS (SELECT 1 FROM old WHERE old.relay_id IS DISTINCT FROM $2) THEN true
				ELSE false
			END AS changed
	`, connectorID, relayID, attachedAt, source).Scan(&changed)
	if err != nil {
		return false, fmt.Errorf("upsert placement: %w", err)
	}
	return changed, nil
}

// DeletePlacement removes a connector_relay_placement row.
// Returns true when a row was actually deleted (the connector had a placement).
func (s *Store) DeletePlacement(ctx context.Context, connectorID string) (bool, error) {
	var changed bool
	err := s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT connector_id FROM connector_relay_placement WHERE connector_id = $1
		), del AS (
			DELETE FROM connector_relay_placement WHERE connector_id = $1
			RETURNING connector_id
		)
		SELECT EXISTS (SELECT 1 FROM old) AS changed
	`, connectorID).Scan(&changed)
	if err != nil {
		return false, fmt.Errorf("delete placement: %w", err)
	}
	return changed, nil
}

// BumpLastConfirmed updates the last_confirmed timestamp for a connector's
// placement row without changing the relay. It does NOT return a changed
// signal — the caller must NOT trigger a policy notification from this.
func (s *Store) BumpLastConfirmed(ctx context.Context, connectorID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE connector_relay_placement SET last_confirmed = NOW() WHERE connector_id = $1`,
		connectorID,
	)
	if err != nil {
		return fmt.Errorf("bump last confirmed: %w", err)
	}
	return nil
}

// ListWorkspacesForRelay returns the distinct workspace (tenant) IDs for all
// connectors currently assigned to a relay via connector_relay_placement.
// Used by the heartbeat handler to invalidate ACL snapshots when a relay's
// address or metadata changes.
func (s *Store) ListWorkspacesForRelay(ctx context.Context, relayID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT c.tenant_id::text
		   FROM connector_relay_placement crp
		   JOIN connectors c ON c.id = crp.connector_id
		  WHERE crp.relay_id = $1`,
		relayID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workspaces for relay: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan workspace id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// EvictExpiredRelays marks active relays whose last heartbeat is older than
// before as inactive. Returns the IDs of relays that were evicted so the
// caller can notify affected workspaces.
func (s *Store) EvictExpiredRelays(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`UPDATE relays
		    SET status     = 'inactive',
		        updated_at = NOW()
		  WHERE status = 'active'
		    AND last_heartbeat_at < $1
		 RETURNING id::text`,
		before,
	)
	if err != nil {
		return nil, fmt.Errorf("evict expired relays: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan evicted relay id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CapacityLabelTransition reports the outcome of running the hysteresis
// state machine on a single heartbeat. Promoted is true when the published
// capacity_label changed (and connectors must therefore be told about it).
type CapacityLabelTransition struct {
	Promoted      bool
	PreviousLabel string
	NewLabel      string
}

// labelledRelayDefaultPort is the QUIC port a relay listens on when only an
// observed IP is known (no public_addr override). Matches the connector / client
// relay default; kept private to the package because the public surface is the
// already-resolved relay_addr returned in LabelledRelayInfo.
const labelledRelayDefaultPort = "9093"

// BuildLabelledRelayList assembles the current ADR-016 eligibility list for
// connector control-stream push. Includes only active relays whose published
// capacity_label is high or medium and that have a routable address (either
// an explicit public_addr or an observed public IP). The version field is
// the latest last_label_changed_at observed across those rows, expressed as
// epoch seconds — monotonic across promotions so the connector can skip a
// re-probe when nothing has changed.
func (s *Store) BuildLabelledRelayList(ctx context.Context) (*connectorpb.LabelledRelayList, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text,
		       COALESCE(public_addr, ''),
		       COALESCE(host(observed_ip), ''),
		       COALESCE(address_scope, ''),
		       capacity_label,
		       EXTRACT(EPOCH FROM last_label_changed_at)::bigint
		  FROM relays
		 WHERE status = 'active'
		   AND capacity_label IN ('high', 'medium')
		   AND (public_addr IS NOT NULL OR address_scope = 'public')`)
	if err != nil {
		return nil, fmt.Errorf("list labelled relays: %w", err)
	}
	defer rows.Close()

	list := &connectorpb.LabelledRelayList{}
	for rows.Next() {
		var (
			id, publicAddr, observedIP, addrScope, label string
			labelChangedAt                               int64
		)
		if err := rows.Scan(&id, &publicAddr, &observedIP, &addrScope, &label, &labelChangedAt); err != nil {
			return nil, fmt.Errorf("scan labelled relay row: %w", err)
		}
		addr := publicAddr
		if addr == "" && addrScope == "public" && observedIP != "" {
			addr = net.JoinHostPort(observedIP, labelledRelayDefaultPort)
		}
		if addr == "" {
			continue
		}
		var lbl connectorpb.RelayCapacityLabel
		switch label {
		case CapacityLabelHigh:
			lbl = connectorpb.RelayCapacityLabel_RELAY_CAPACITY_HIGH
		case CapacityLabelMedium:
			lbl = connectorpb.RelayCapacityLabel_RELAY_CAPACITY_MEDIUM
		default:
			// Low / unrecognised — filtered out by the SQL guard, but skip
			// defensively in case the enum drifts.
			continue
		}
		list.Relays = append(list.Relays, &connectorpb.LabelledRelayInfo{
			RelayId:   id,
			RelayAddr: addr,
			SpiffeId:  appmeta.RelaySPIFFEID(id),
			Label:     lbl,
		})
		if uint64(labelChangedAt) > list.Version {
			list.Version = uint64(labelChangedAt)
		}
	}
	return list, rows.Err()
}

// EvaluateCapacityLabel runs the tier-label hysteresis state machine against
// the current heartbeat counters persisted on the relay row. Behaviour:
//   - If the computed candidate matches the published capacity_label, clear
//     any in-flight pending fields (a transient candidate that didn't survive).
//   - If the candidate differs from both the published label and the current
//     pending label, start a new hold-down window.
//   - If the candidate matches an in-flight pending label and the hold-down
//     window has elapsed, promote it to capacity_label and stamp
//     last_label_changed_at. Promoted = true so the caller can push the
//     updated LabelledRelayList to connectors.
//
// Wraps the read-decide-write cycle in a transaction with SELECT ... FOR UPDATE
// so two concurrent heartbeats can't race the state machine. Heartbeats are
// already serialised per relay by the Redis db-write cache in practice, but
// the lock is the correctness contract — not the cache.
func (s *Store) EvaluateCapacityLabel(ctx context.Context, relayID string, holdDown time.Duration) (CapacityLabelTransition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CapacityLabelTransition{}, fmt.Errorf("begin capacity-label tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		current          string
		pending          *string
		pendingSince     *time.Time
		connectionCount  uint32
		maxConnections   uint32
	)
	err = tx.QueryRow(ctx, `
		SELECT capacity_label,
		       pending_capacity_label,
		       pending_label_since,
		       connection_count,
		       max_connections
		  FROM relays
		 WHERE id = $1
		   AND status <> 'deleted'
		 FOR UPDATE`, relayID).Scan(&current, &pending, &pendingSince, &connectionCount, &maxConnections)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CapacityLabelTransition{}, ErrRelayNotFound
		}
		return CapacityLabelTransition{}, fmt.Errorf("read capacity-label state: %w", err)
	}

	candidate := computeCandidateLabel(current, connectionCount, maxConnections)
	decision := decideHysteresis(current, pending, pendingSince, candidate, time.Now().UTC(), holdDown)

	_, err = tx.Exec(ctx, `
		UPDATE relays
		   SET capacity_label         = $2,
		       pending_capacity_label = $3,
		       pending_label_since    = $4,
		       last_label_changed_at  = CASE WHEN $5 THEN NOW() ELSE last_label_changed_at END,
		       updated_at             = NOW()
		 WHERE id = $1`, relayID, decision.NewLabel, decision.NewPending, decision.NewPendingSince, decision.Promoted)
	if err != nil {
		return CapacityLabelTransition{}, fmt.Errorf("write capacity-label state: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CapacityLabelTransition{}, fmt.Errorf("commit capacity-label tx: %w", err)
	}

	return CapacityLabelTransition{
		Promoted:      decision.Promoted,
		PreviousLabel: current,
		NewLabel:      decision.NewLabel,
	}, nil
}

func (s *Store) RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname, observedIP string, observedPort int, addressScope, publicAddr string, connectionCount, maxConnections uint32) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relays
		    SET status            = 'active',
		        cert_serial       = $2,
		        cert_not_after    = $3,
		        version           = NULLIF($4, ''),
		        hostname          = NULLIF($5, ''),
		        observed_ip       = NULLIF($6, '')::inet,
		        observed_port     = NULLIF($7, 0),
		        address_scope     = NULLIF($8, ''),
		        public_addr       = NULLIF($9, ''),
		        connection_count  = $10,
		        max_connections   = $11,
		        last_heartbeat_at = NOW(),
		        updated_at        = NOW()
		  WHERE id = $1
		    AND status <> 'deleted'`,
		id, certSerial, certNotAfter, version, hostname, observedIP, observedPort, addressScope, publicAddr, connectionCount, maxConnections,
	)
	if err != nil {
		return fmt.Errorf("record relay heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}
