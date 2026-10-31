package connector

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"google.golang.org/protobuf/proto"
)

// shieldGRPCPort is the Connector's Shield-facing gRPC port; a Connector's
// lan_addr host plus this port is where a Shield dials it.
const shieldGRPCPort = "9091"

// ShieldSigner signs a shield's PeerListPayload (D21-R a2) and
// ResourceSetPayload (D21-R S) with the tenant's Workspace CA key
// (pki.Service).
type ShieldSigner interface {
	SignShieldPeerList(ctx context.Context, tenantID string, payload []byte) ([]byte, error)
	SignShieldResourceSet(ctx context.Context, tenantID string, payload []byte) ([]byte, error)
}

// peerListIssuer builds the controller-signed peer list each shield accepts
// as its only source of peers (D21-R a2). The shield takes a list only when
// its issued_at is later than the last one it accepted, so mu serialises
// "pick issued_at, read the Connectors": a later issued_at is always built
// from a read that started later, so it never carries an older membership
// than an earlier one (single controller instance, D-02). Signing (a CA key
// load and decrypt) happens after mu is released.
type peerListIssuer struct {
	signer ShieldSigner
	mu     sync.Mutex
	lastMS int64
}

// SetShieldSigner enables signed peer lists and resource sets in every shield
// snapshot. Call once at startup, before any Control stream is served.
// Without a signer, snapshots carry neither, and shields change nothing.
func (r *ConnectorRegistry) SetShieldSigner(s ShieldSigner) {
	r.peerLists = &peerListIssuer{signer: s}
}

func (r *ConnectorRegistry) peerListIssuer() *peerListIssuer {
	if r == nil {
		return nil
	}
	return r.peerLists
}

// build returns shieldID's signed peer list: every Connector of its Remote
// Network that has an identity, isn't revoked and has reported an address.
// Nil issuer → nil list, no error.
func (p *peerListIssuer) build(ctx context.Context, db *pgxpool.Pool, shieldID string) (*shieldpb.SignedPeerList, error) {
	if p == nil || p.signer == nil {
		return nil, nil
	}
	tenantID, raw, err := p.readPayload(ctx, db, shieldID)
	if err != nil {
		return nil, err
	}
	sig, err := p.signer.SignShieldPeerList(ctx, tenantID, raw)
	if err != nil {
		return nil, fmt.Errorf("peer list: %w", err)
	}
	return &shieldpb.SignedPeerList{Payload: raw, Signature: sig}, nil
}

// readPayload picks issued_at and reads the membership under mu, and returns
// the serialized payload and the tenant whose key signs it.
func (p *peerListIssuer) readPayload(ctx context.Context, db *pgxpool.Pool, shieldID string) (string, []byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	issuedAt := time.Now().UnixMilli()
	if issuedAt <= p.lastMS {
		issuedAt = p.lastMS + 1
	}

	var tenantID, trustDomain, rnID string
	if err := db.QueryRow(ctx,
		`SELECT s.tenant_id::text, COALESCE(w.trust_domain, ''), s.remote_network_id::text
		   FROM shields s
		   JOIN workspaces w ON w.id = s.tenant_id
		  WHERE s.id = $1`,
		shieldID,
	).Scan(&tenantID, &trustDomain, &rnID); err != nil {
		return "", nil, fmt.Errorf("peer list: load shield %s: %w", shieldID, err)
	}
	if trustDomain == "" {
		return "", nil, fmt.Errorf("peer list: shield %s: workspace has no trust domain", shieldID)
	}

	rows, err := db.Query(ctx,
		`SELECT id::text, COALESCE(lan_addr, '')
		   FROM connectors
		  WHERE remote_network_id = $1
		    AND tenant_id = $2
		    AND status IN ('active', 'disconnected')
		    AND revoked_at IS NULL
		  ORDER BY id`,
		rnID, tenantID,
	)
	if err != nil {
		return "", nil, fmt.Errorf("peer list: connectors for shield %s: %w", shieldID, err)
	}
	defer rows.Close()
	payload := &shieldpb.PeerListPayload{
		TrustDomain:    trustDomain,
		ShieldId:       shieldID,
		IssuedAtUnixMs: uint64(issuedAt),
		Peers:          []*shieldpb.PeerConnector{},
	}
	for rows.Next() {
		var id, lanAddr string
		if err := rows.Scan(&id, &lanAddr); err != nil {
			return "", nil, fmt.Errorf("peer list: scan connector: %w", err)
		}
		if addr := shieldFacingAddr(lanAddr); addr != "" {
			payload.Peers = append(payload.Peers, &shieldpb.PeerConnector{ConnectorId: id, ConnectorAddr: addr})
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("peer list: connectors for shield %s: %w", shieldID, err)
	}

	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return "", nil, fmt.Errorf("peer list: marshal: %w", err)
	}
	p.lastMS = issuedAt
	return tenantID, raw, nil
}

// shieldFacingAddr turns a Connector's lan_addr (host or host:port) into the
// host:9091 address a Shield dials; "" when there is no host.
func shieldFacingAddr(lanAddr string) string {
	host := lanAddr
	if h, _, err := net.SplitHostPort(lanAddr); err == nil {
		host = h
	}
	if host == "" {
		return ""
	}
	return net.JoinHostPort(host, shieldGRPCPort)
}

// peerListPushTimeout bounds a background peer-list push.
const peerListPushTimeout = 30 * time.Second

// pushPeerListsInBackground runs PushPeerListsForNetworkOf off the caller's
// path: called from a Connector's Control recv loop, whose AuthorizationAck
// must not wait behind snapshot builds and CA-key signatures (Phase M, D18).
// Coalesced per Connector (pushCoalescer).
func pushPeerListsInBackground(db *pgxpool.Pool, r *ConnectorRegistry, connectorID string) {
	if r.peerListIssuer() == nil {
		return
	}
	r.pushes.trigger("network-of:"+connectorID, func(ctx context.Context) {
		PushPeerListsForNetworkOf(ctx, db, r, connectorID)
	})
}

// pushSnapshotInBackground is PushSnapshotForShield off the caller's path,
// for the same reason. Coalesced per shield; the latest target Connector wins.
func pushSnapshotInBackground(db *pgxpool.Pool, r *ConnectorRegistry, connectorID, shieldID string) {
	r.pushes.trigger("shield:"+shieldID, func(ctx context.Context) {
		PushSnapshotForShield(ctx, db, r, connectorID, shieldID)
	})
}

// PushPeerListsForNetworkOf re-sends the snapshot, and with it the signed peer
// list, of every enrolled shield in connectorID's Remote Network (D21-R a2).
// Called when that network's Connector membership changes: a Connector
// enrolled, came back, changed address or was revoked. Each push goes to the
// shield's current Connector; an offline one gets it on reconnect.
func PushPeerListsForNetworkOf(ctx context.Context, db *pgxpool.Pool, r *ConnectorRegistry, connectorID string) {
	if r.peerListIssuer() == nil {
		return
	}
	rows, err := db.Query(ctx,
		`SELECT s.id::text, s.connector_id::text
		   FROM shields s
		   JOIN connectors c
		     ON c.remote_network_id = s.remote_network_id
		    AND c.tenant_id = s.tenant_id
		  WHERE c.id = $1
		    AND s.status NOT IN ('revoked', 'deleted')`,
		connectorID,
	)
	if err != nil {
		log.Printf("peer list: shields in the network of connector %s: %v", connectorID, err)
		return
	}
	type target struct{ shieldID, connectorID string }
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.shieldID, &t.connectorID); err != nil {
			log.Printf("peer list: scan shield: %v", err)
			continue
		}
		targets = append(targets, t)
	}
	rows.Close()
	for _, t := range targets {
		PushSnapshotForShield(ctx, db, r, t.connectorID, t.shieldID)
	}
}
