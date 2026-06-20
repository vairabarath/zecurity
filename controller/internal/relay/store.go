package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
		        version, hostname, last_heartbeat_at
		   FROM relays WHERE id = $1`,
		id,
	).Scan(&r.ID, &r.Name, &r.Status, &r.DNSAllowlist, &r.IPAllowlist,
		&r.EnrollmentTokenJTI, &r.CertSerial, &r.CertNotAfter,
		&r.Version, &r.Hostname, &r.LastHeartbeatAt)
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
func (s *Store) RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE relays
		    SET status            = 'active',
		        cert_serial       = $2,
		        cert_not_after    = $3,
		        version           = NULLIF($4, ''),
		        hostname          = NULLIF($5, ''),
		        last_heartbeat_at = NOW(),
		        updated_at        = NOW()
		  WHERE id = $1
		    AND status <> 'deleted'`,
		id, certSerial, certNotAfter, version, hostname,
	)
	if err != nil {
		return fmt.Errorf("record relay heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRelayNotFound
	}
	return nil
}
