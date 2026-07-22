package relay

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"sort"
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mark provisioned: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE relays
		    SET enrollment_token_jti = NULL,
		        status               = 'active',
		        cert_serial          = $2,
		        cert_not_after       = $3,
		        version              = NULLIF($4, ''),
		        hostname             = NULLIF($5, ''),
		        updated_at           = NOW()
		  WHERE id = $1
		    AND status NOT IN ('revoked', 'deleted')`,
		id, certSerial, certNotAfter, version, hostname,
	)
	if err != nil {
		return fmt.Errorf("mark relay provisioned: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO relay_certificates (relay_id, serial, not_after)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (serial) DO NOTHING`,
		id, certSerial, certNotAfter,
	); err != nil {
		return fmt.Errorf("record provisioned relay cert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark provisioned: %w", err)
	}
	return nil
}

// RevokedSerial is one revoked, still-unexpired relay certificate serial.
// Consumed by the relay CRL generator (Phase 3) and the controller checker (Phase 4).
type RevokedSerial struct {
	Serial    string
	RevokedAt time.Time
}

// RecordIssuedCert appends one issued relay certificate to the history table.
// Called inside MarkProvisioned (and future renewal) so every serial a relay has
// held is tracked. serial is the canonical SerialNumber.Text(16) form.
func (s *Store) RecordIssuedCert(ctx context.Context, relayID, serial string, notAfter time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO relay_certificates (relay_id, serial, not_after)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (serial) DO NOTHING`,
		relayID, serial, notAfter,
	)
	if err != nil {
		return fmt.Errorf("record issued relay cert: %w", err)
	}
	return nil
}

// RevokeAllForRelay marks every not-yet-revoked certificate of a relay revoked.
// Returns how many serials were revoked. Idempotent: a second call revokes 0.
func (s *Store) RevokeAllForRelay(ctx context.Context, relayID, reason string) (int, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relay_certificates
		    SET revoked_at = NOW(), revocation_reason = NULLIF($2, '')
		  WHERE relay_id = $1 AND revoked_at IS NULL`,
		relayID, reason,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke relay certs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListRevokedRelaySerials returns every revoked, still-unexpired relay serial.
// Expired serials are omitted — a CRL need not carry an already-invalid cert.
func (s *Store) ListRevokedRelaySerials(ctx context.Context) ([]RevokedSerial, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT serial, revoked_at
		   FROM relay_certificates
		  WHERE revoked_at IS NOT NULL AND not_after > NOW()
		  ORDER BY revoked_at`)
	if err != nil {
		return nil, fmt.Errorf("list revoked relay serials: %w", err)
	}
	defer rows.Close()

	var out []RevokedSerial
	for rows.Next() {
		var r RevokedSerial
		if err := rows.Scan(&r.Serial, &r.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan revoked relay serial: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkDeleted soft-deletes a relay (status='deleted') so it stops being served
// to connectors — BuildLabelledRelayList and RecordHeartbeat both reject
// 'deleted'. It does NOT revoke the relay certificate: CRL revocation is the
// PENDING-02 seam. Returns ErrRelayNotFound if no matching non-deleted row.
func (s *Store) MarkDeleted(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relays SET status = 'deleted', updated_at = NOW()
                WHERE id = $1 AND status <> 'deleted'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("mark relay deleted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}

// RevokeRelay atomically revokes every unexpired certificate of a relay and marks
// the relay 'revoked' (a terminal, heartbeat-safe status). Returns the number of
// certificate serials revoked. FOR UPDATE serializes against MarkProvisioned so a
// concurrent provision cannot undo the revoke. Rows are never deleted — the CRL
// needs the serial until not_after. Returns ErrRelayNotFound if the relay is gone.
func (s *Store) RevokeRelay(ctx context.Context, relayID, reason string,
	inTx func(ctx context.Context, tx pgx.Tx, revoked int) error) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin revoke relay: %w", err)
	}
	defer tx.Rollback(ctx)

	var status string
	if err := tx.QueryRow(ctx,
		`SELECT status FROM relays WHERE id = $1 FOR UPDATE`, relayID,
	).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrRelayNotFound
		}
		return 0, fmt.Errorf("lock relay for revoke: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE relay_certificates
		    SET revoked_at = NOW(), revocation_reason = NULLIF($2, '')
		  WHERE relay_id = $1 AND revoked_at IS NULL`,
		relayID, reason,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke relay certs: %w", err)
	}
	revoked := int(tag.RowsAffected())

	// Don't downgrade an already-deleted relay; 'deleted' is the more terminal state.
	if status != "deleted" {
		if _, err := tx.Exec(ctx,
			`UPDATE relays SET status = 'revoked', updated_at = NOW() WHERE id = $1`,
			relayID,
		); err != nil {
			return 0, fmt.Errorf("mark relay revoked: %w", err)
		}
	}
	// In-tx hook (audit row, and for Delete the 'deleted' status flip). A hook error
	// returns here → the deferred tx.Rollback reverts the whole revoke, so revocation
	// rows + status + audit commit atomically or not at all.
	if inTx != nil {
		if err := inTx(ctx, tx, revoked); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit revoke relay: %w", err)
	}
	return int(tag.RowsAffected()), nil
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

// ListConnectorsForRelay returns the connectors currently placed on a relay,
// grouped by workspace (tenant) ID. Used by the transport-plane triggers
// (ADR-017): a relay online/metadata/eviction event affects only the connectors
// on that relay, so the caller notifies exactly those via NotifyTopologyChange
// rather than recompiling the whole workspace ACL.
func (s *Store) ListConnectorsForRelay(ctx context.Context, relayID string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.tenant_id::text, c.id::text
		   FROM connector_relay_placement crp
		   JOIN connectors c ON c.id = crp.connector_id
		  WHERE crp.relay_id = $1`,
		relayID,
	)
	if err != nil {
		return nil, fmt.Errorf("list connectors for relay: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]string)
	for rows.Next() {
		var wsID, connID string
		if err := rows.Scan(&wsID, &connID); err != nil {
			return nil, fmt.Errorf("scan relay connector row: %w", err)
		}
		out[wsID] = append(out[wsID], connID)
	}
	return out, rows.Err()
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
		       capacity_label
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
		var id, publicAddr, observedIP, addrScope, label string
		if err := rows.Scan(&id, &publicAddr, &observedIP, &addrScope, &label); err != nil {
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
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// F11: the connector compares this version by equality to decide whether to
	// re-probe (relay_ranking.rs version_matches). It must be identical for
	// identical content and change whenever the eligible set, an address, or a
	// label changes — regardless of DB row order. A content fingerprint gives
	// that; the previous wall-clock stamp did not (it changed on every broadcast
	// and disagreed with the connect-time push, F11).
	list.Version = relayListVersion(list.Relays)
	return list, nil
}

// relayListVersion derives a deterministic, content-addressed version for a
// LabelledRelayList. Order-independent (relays are sorted by id first) so the
// same eligible set always yields the same version, and sensitive to any
// relay id / address / label / membership change.
//
// SpiffeId is intentionally omitted from the fingerprint: it is a pure function
// of RelayId (appmeta.RelaySPIFFEID above), so hashing the id already covers it.
// If SpiffeId ever becomes independent of RelayId, add it here — otherwise two
// relays differing only in spiffe_id would collide on the same version.
func relayListVersion(relays []*connectorpb.LabelledRelayInfo) uint64 {
	ordered := make([]*connectorpb.LabelledRelayInfo, len(relays))
	copy(ordered, relays)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].RelayId < ordered[j].RelayId })
	h := fnv.New64a()
	for _, r := range ordered {
		// NUL separators keep field boundaries unambiguous across concatenation.
		fmt.Fprintf(h, "%s\x00%s\x00%d\x00", r.RelayId, r.RelayAddr, int32(r.Label))
	}
	return h.Sum64()
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
		current         string
		pending         *string
		pendingSince    *time.Time
		connectionCount uint32
		maxConnections  uint32
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
	if maxConnections == 0 {
		log.Printf("relay %s reports max_connections=0 (RELAY_MAX_CONNECTIONS unset); marking ineligible", relayID)
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
		    AND status NOT IN ('deleted', 'revoked')`,
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
