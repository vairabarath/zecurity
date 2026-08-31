package shield

import (
	"context"
	"errors"
	"net"

	pgx "github.com/jackc/pgx/v5"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetPeerConnectors handles the ShieldService.GetPeerConnectors RPC — the
// recovery path for a shield that can no longer reach ANY connector it knows.
//
// WHY THIS EXISTS. A shield learns peer coordinates only from PeerConnectorList
// pushes over its Control stream, and by design it never heartbeats to the
// controller. So when its connector's address changes, the shield's only channel
// for the new address is the peer whose address just changed — it retries the
// dead one forever:
//
//	peer Connector unreachable, trying next connector_addr=192.168.1.87:9091
//
// during which connectors.lan_addr in this database was already correct.
// Recovery previously required re-adding the old IP to an interface so the shield
// could reconnect once. This RPC is the out-of-band path that removes that.
//
// TRUST MODEL. Unlike RenewCert — which a connector proxies, so the verified
// identity there is the connector's — this is called by the shield DIRECTLY over
// its own mTLS channel. The shield is therefore identified by its certificate and
// by nothing else: GetPeerConnectorsRequest is empty on purpose, so there is no
// caller-supplied field to forge. Never add one.
//
// A shield receives only the connectors in its OWN remote network. That isolation
// is the property to protect: a wrong join here would leak another remote
// network's topology to any enrolled shield.
func (s *service) GetPeerConnectors(
	ctx context.Context,
	_ *shieldpb.GetPeerConnectorsRequest,
) (*shieldpb.PeerConnectorList, error) {
	if spiffe.Role(ctx) != appmeta.SPIFFERoleShield {
		return nil, status.Error(codes.PermissionDenied, "caller is not a shield")
	}
	shieldID := spiffe.EntityID(ctx)
	if shieldID == "" {
		return nil, status.Error(codes.Unauthenticated, "no shield identity in certificate")
	}

	// remote_network_id is carried on the shield row itself, so this lookup does
	// NOT go through the shield's connector. That matters: a shield whose
	// connector row was deleted still resolves its remote network, and the
	// recovery path keeps working in exactly the case that needs it most.
	var tenantID, remoteNetworkID, shieldStatus string
	err := s.db.QueryRow(ctx,
		`SELECT tenant_id::text, remote_network_id::text, status
		   FROM shields
		  WHERE id = $1`,
		shieldID,
	).Scan(&tenantID, &remoteNetworkID, &shieldStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "shield not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load shield: %v", err)
	}
	if shieldStatus == "revoked" || shieldStatus == "deleted" {
		return nil, status.Errorf(codes.PermissionDenied, "shield status is %q", shieldStatus)
	}

	rows, err := s.db.Query(ctx,
		`SELECT id::text, COALESCE(lan_addr, ''), COALESCE(public_ip, '')
		   FROM connectors
		  WHERE remote_network_id = $1
		    AND tenant_id = $2
		    AND status = 'active'
		    AND (lan_addr IS NOT NULL OR public_ip IS NOT NULL)
		  ORDER BY id`,
		remoteNetworkID, tenantID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load peer connectors: %v", err)
	}
	defer rows.Close()

	var peers []*shieldpb.PeerConnector
	for rows.Next() {
		var id, lanAddr, publicIP string
		if err := rows.Scan(&id, &lanAddr, &publicIP); err != nil {
			return nil, status.Errorf(codes.Internal, "scan peer connector: %v", err)
		}
		// Same address preference as selectConnector (token.go), deliberately:
		// lan_addr is stored already carrying the shield-facing :9091 port, while
		// public_ip is a bare host that needs it appended. A shield dials exactly
		// what enrollment handed it, so these two must not drift apart.
		addr := lanAddr
		if addr == "" {
			addr = net.JoinHostPort(publicIP, "9091")
		}
		peers = append(peers, &shieldpb.PeerConnector{
			ConnectorId:   id,
			ConnectorAddr: addr,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, status.Errorf(codes.Internal, "iterate peer connectors: %v", err)
	}

	// An empty list must be an ERROR, never an empty success. The shield's
	// apply_peer_connector_list IGNORES an empty list and keeps its existing one,
	// so returning success here would leave it retrying the same dead addresses
	// while believing it had recovered — a silent no-op.
	if len(peers) == 0 {
		return nil, status.Error(codes.FailedPrecondition,
			"no active connector with a reachable address in this remote network")
	}

	return &shieldpb.PeerConnectorList{Peers: peers}, nil
}
