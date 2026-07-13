// Package transport is the Track B (ADR-015) transport/connectivity control
// plane. It compiles and serves a per-workspace TransportSnapshot — which
// connector + relay serves each remote network — independently of the ACL
// (authorization) plane, so a relay change propagates without recompiling or
// bumping the ACL snapshot.
package transport

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store reads the raw transport topology: active connectors and their relay
// coordinates, workspace-scoped. It is the transport-plane counterpart to
// policy.Store's connector lookup.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates a transport Store over the given pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// WorkspaceConnectorRow is one active connector in a workspace together with
// its (optional) relay placement. Mirrors policy.RemoteNetworkConnectorsRow but
// workspace-scoped rather than remote-network-scoped.
type WorkspaceConnectorRow struct {
	RemoteNetworkID   string
	ConnectorID       string
	LanAddr           string
	TrustDomain       string
	RelayPublicAddr   string
	RelayObservedHost string
	RelayID           string
}

// GetWorkspaceConnectors returns every active connector in the workspace with
// its relay placement (LEFT JOIN so a connector without a placement still
// appears as direct-only). Same JOIN shape as
// policy.GetConnectorsForRemoteNetworks, but scoped to the whole workspace so
// the transport snapshot covers every remote network's connectivity in one
// compile. Rows are ordered by remote_network_id then freshest heartbeat first.
func (s *Store) GetWorkspaceConnectors(ctx context.Context, workspaceID string) ([]*WorkspaceConnectorRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT c.remote_network_id::text,
		        c.id::text,
		        COALESCE(c.lan_addr, ''),
		        COALESCE(c.trust_domain, ''),
		        COALESCE(NULLIF(r.public_addr, ''), ''),
		        CASE
		          WHEN r.address_scope = 'public' AND r.observed_ip IS NOT NULL
		            THEN host(r.observed_ip)
		          ELSE ''
		        END,
		        COALESCE(r.id::text, '')
		   FROM connectors c
		   LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
		   LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
		  WHERE c.tenant_id = $1
		    AND c.status = 'active'
		  ORDER BY c.remote_network_id, c.last_heartbeat_at DESC NULLS LAST`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("get workspace connectors: %w", err)
	}
	defer rows.Close()

	var out []*WorkspaceConnectorRow
	for rows.Next() {
		r := &WorkspaceConnectorRow{}
		if err := rows.Scan(&r.RemoteNetworkID, &r.ConnectorID, &r.LanAddr, &r.TrustDomain, &r.RelayPublicAddr, &r.RelayObservedHost, &r.RelayID); err != nil {
			return nil, fmt.Errorf("scan workspace connector row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
