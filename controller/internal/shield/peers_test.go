package shield

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetPeerConnectors is the recovery path for a shield stranded by a connector IP
// change. Gated on SHIELD_TEST_DATABASE_URL; skipped otherwise.
//
// The headline case is the one that stranded a real shield: the connector's
// address moves, connectors.lan_addr is already correct here, and the shield has
// no channel to learn it. The isolation case is the security property — a shield
// must never receive another remote network's topology.
func TestGetPeerConnectors(t *testing.T) {
	dsn := os.Getenv("SHIELD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SHIELD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	_, _ = db.Exec(ctx, `DELETE FROM workspaces WHERE slug = 'peerrecovery-test'`)

	must := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := db.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
		return id
	}

	wsID := must(`INSERT INTO workspaces (slug, name, trust_domain, status)
		VALUES ('peerrecovery-test','peerrecovery','peerrecovery.example.com','active') RETURNING id`)
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID) })

	// Two remote networks. The shield lives in A; B exists only to prove it never leaks.
	rnA := must(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'rn-a','office') RETURNING id`, wsID)
	rnB := must(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'rn-b','other') RETURNING id`, wsID)

	connA1 := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, lan_addr)
		VALUES ($1,$2,'conn-a1','active','192.168.1.87:9091') RETURNING id`, wsID, rnA)
	connA2 := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, public_ip)
		VALUES ($1,$2,'conn-a2','active','203.0.113.9') RETURNING id`, wsID, rnA)
	connB := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, lan_addr)
		VALUES ($1,$2,'conn-b','active','10.9.9.9:9091') RETURNING id`, wsID, rnB)

	shieldID := must(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		VALUES ($1,$2,$3,'sh','active') RETURNING id`, wsID, rnA, connA1)

	svc := &service{db: db}

	asShield := func(id string) context.Context {
		return spiffe.WithIdentity(ctx,
			"spiffe://peerrecovery.example.com/shield/"+id,
			appmeta.SPIFFERoleShield, id, "peerrecovery.example.com")
	}

	addrOf := func(list *shieldpb.PeerConnectorList, connID string) string {
		t.Helper()
		for _, p := range list.Peers {
			if p.ConnectorId == connID {
				return p.ConnectorAddr
			}
		}
		return ""
	}

	// 1. The stranding scenario. The shield's connector moves; the DB already
	//    knows. This call is the only way the shield can find out.
	if _, err := db.Exec(ctx,
		`UPDATE connectors SET lan_addr = '192.168.1.33:9091' WHERE id = $1`, connA1,
	); err != nil {
		t.Fatalf("move connector: %v", err)
	}

	list, err := svc.GetPeerConnectors(asShield(shieldID), &shieldpb.GetPeerConnectorsRequest{})
	if err != nil {
		t.Fatalf("GetPeerConnectors: %v", err)
	}
	if got := addrOf(list, connA1); got != "192.168.1.33:9091" {
		t.Fatalf("connector address = %q, want the NEW %q — a shield calling this "+
			"while stranded would be handed the same dead address", got, "192.168.1.33:9091")
	}

	// lan_addr is stored already carrying :9091; a bare public_ip must have it
	// appended. These two forms must match what enrollment hands out, or the
	// shield dials something it was never given.
	if got := addrOf(list, connA2); got != "203.0.113.9:9091" {
		t.Fatalf("public_ip peer address = %q, want %q", got, "203.0.113.9:9091")
	}

	// 2. THE security property: remote-network isolation.
	if got := addrOf(list, connB); got != "" {
		t.Fatalf("a shield in rn-a received rn-b's connector at %q — this leaks "+
			"another remote network's topology to any enrolled shield", got)
	}
	if len(list.Peers) != 2 {
		t.Fatalf("got %d peers, want exactly the 2 in this shield's remote network", len(list.Peers))
	}

	// 3. Identity comes from the certificate. A caller with no shield identity,
	//    or the wrong role, gets nothing — there is no request field to fall back
	//    to, and there must never be one.
	if _, err := svc.GetPeerConnectors(ctx, &shieldpb.GetPeerConnectorsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthenticated caller: got %v, want PermissionDenied", err)
	}
	connCtx := spiffe.WithIdentity(ctx, "spiffe://peerrecovery.example.com/connector/x",
		appmeta.SPIFFERoleConnector, connA1, "peerrecovery.example.com")
	if _, err := svc.GetPeerConnectors(connCtx, &shieldpb.GetPeerConnectorsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("connector caller: got %v, want PermissionDenied", err)
	}

	// 4. A revoked shield must not be handed live topology.
	if _, err := db.Exec(ctx, `UPDATE shields SET status = 'revoked' WHERE id = $1`, shieldID); err != nil {
		t.Fatalf("revoke shield: %v", err)
	}
	if _, err := svc.GetPeerConnectors(asShield(shieldID), &shieldpb.GetPeerConnectorsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("revoked shield: got %v, want PermissionDenied", err)
	}
	if _, err := db.Exec(ctx, `UPDATE shields SET status = 'active' WHERE id = $1`, shieldID); err != nil {
		t.Fatalf("un-revoke shield: %v", err)
	}

	// 5. No reachable peer must be an ERROR, not an empty success. The shield's
	//    apply_peer_connector_list ignores an empty list and keeps the stale one,
	//    so an empty success would leave it retrying dead addresses believing it
	//    had recovered.
	if _, err := db.Exec(ctx, `UPDATE connectors SET status = 'revoked' WHERE remote_network_id = $1`, rnA); err != nil {
		t.Fatalf("revoke connectors: %v", err)
	}
	_, err = svc.GetPeerConnectors(asShield(shieldID), &shieldpb.GetPeerConnectorsRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no reachable peer: got %v, want FailedPrecondition — an empty "+
			"success is silently ignored by the shield", err)
	}
}
