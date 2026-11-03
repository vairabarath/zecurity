package connector

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/resource"
)

// PENDING-14 F-5: a shield never sees a DNS name. Every instruction for a
// DNS-name resource (pushed, in the snapshot, in the signed set's desired
// resources and removes) carries the bound shield's LAN IP.
func TestShieldInstructionsCarryLANIPForNames(t *testing.T) {
	row := &resource.Row{ID: "r1", Host: "db.acme.example", ShieldHost: "10.0.0.5", Protocol: "tcp", PortFrom: 5432, PortTo: 5432, PendingAction: "apply"}
	if got := instructionForRow(row); got.Host != "10.0.0.5" {
		t.Fatalf("pushed instruction host = %q, want the shield's LAN IP", got.Host)
	}

	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("resname-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	conn := seedConnector(t, pool, ctx, wsID, rnID, td, "active", nil)
	var shieldID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain, lan_ip)
		 VALUES ($1, $2, $3, 'shield', 'active', $4, '10.0.0.5') RETURNING id::text`,
		wsID, rnID, conn, td).Scan(&shieldID); err != nil {
		t.Fatal(err)
	}
	res := func(name, status, action string, port int) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
			 VALUES ($1, $2, $3, $4, $5, 'tcp', $6, $6, $7, $8) RETURNING id::text`,
			wsID, rnID, shieldID, name, name+".acme.example", port, status, action).Scan(&id); err != nil {
			t.Fatalf("seed resource %s: %v", name, err)
		}
		return id
	}
	res("protected", "protected", "apply", 22)
	res("unprotecting", "protecting", "remove", 443)
	res("deleting", "deleting", "remove", 5432)

	signer := newKeySigner(t)
	r := NewConnectorRegistry()
	r.SetShieldSigner(signer)
	msg, err := buildSnapshotMsg(ctx, pool, r, shieldID)
	if err != nil {
		t.Fatal(err)
	}
	snap := msg.GetResourceSnapshots().GetShieldSnapshots()[shieldID]
	for _, in := range snap.GetResources() {
		if in.Host != "10.0.0.5" {
			t.Fatalf("snapshot resource %s host = %q, want 10.0.0.5", in.ResourceId, in.Host)
		}
	}
	var payload shieldpb.ResourceSetPayload
	if err := proto.Unmarshal(snap.GetSignedResourceSet().GetPayload(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Resources) != 1 || len(payload.Removes) != 2 {
		t.Fatalf("signed set: %d resources, %d removes; want 1, 2", len(payload.Resources), len(payload.Removes))
	}
	for _, in := range append(payload.Resources, payload.Removes...) {
		if in.Host != "10.0.0.5" {
			t.Fatalf("signed %s host = %q, want 10.0.0.5", in.ResourceId, in.Host)
		}
	}
}
