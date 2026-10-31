package connector

import (
	"crypto/ecdsa"
	"fmt"
	"sort"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// D21-R (S): every shield snapshot carries the desired set and the pending
// removes, signed for that shield at the snapshot's generation.
func TestBuildSignedResourceSet(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("resset-%d", time.Now().UnixNano())
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
			 VALUES ($1, $2, $3, $4, '10.0.0.5', 'tcp', $5, $5, $6, $7) RETURNING id::text`,
			wsID, rnID, shieldID, name, port, status, action).Scan(&id); err != nil {
			t.Fatalf("seed resource %s: %v", name, err)
		}
		return id
	}
	protected := res("protected", "protected", "apply", 22)
	applying := res("applying", "protecting", "apply", 80)
	unprotecting := res("unprotecting", "protecting", "remove", 443)
	deleting := res("deleting", "deleting", "remove", 5432)
	res("unprotected", "unprotected", "remove", 8080)

	signer := newKeySigner(t)
	r := NewConnectorRegistry()
	r.SetShieldSigner(signer)
	msg, err := buildSnapshotMsg(ctx, pool, r, shieldID)
	if err != nil {
		t.Fatal(err)
	}
	snap := msg.GetResourceSnapshots().GetShieldSnapshots()[shieldID]
	signed := snap.GetSignedResourceSet()
	if signed == nil {
		t.Fatal("snapshot carries no signed resource set")
	}
	if !ecdsa.VerifyASN1(&signer.key.PublicKey, pki.ShieldResourceSetDigest(signed.Payload), signed.Signature) {
		t.Fatal("signature does not verify over the resource-set digest")
	}
	if ecdsa.VerifyASN1(&signer.key.PublicKey, pki.ShieldPeerListDigest(signed.Payload), signed.Signature) {
		t.Fatal("a resource-set signature verifies as a peer list")
	}

	var payload shieldpb.ResourceSetPayload
	if err := proto.Unmarshal(signed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TrustDomain != td || payload.ShieldId != shieldID || payload.Generation != snap.Generation {
		t.Fatalf("bound to (%q, %q, %d), want (%q, %q, %d)",
			payload.TrustDomain, payload.ShieldId, payload.Generation, td, shieldID, snap.Generation)
	}
	ids := func(list []*shieldpb.ResourceInstruction) []string {
		var out []string
		for _, r := range list {
			out = append(out, r.ResourceId)
		}
		sort.Strings(out)
		return out
	}
	sorted := func(in ...string) []string { sort.Strings(in); return in }
	if got, want := ids(payload.Resources), sorted(protected, applying); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("desired %v, want %v", got, want)
	}
	if got, want := ids(payload.Removes), sorted(unprotecting, deleting); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("removes %v, want %v", got, want)
	}
	for _, rm := range payload.Removes {
		if rm.Host != "10.0.0.5" || rm.Protocol != "tcp" || rm.PortFrom == 0 {
			t.Fatalf("remove %s does not carry its rule: %+v", rm.ResourceId, rm)
		}
	}

	// Without a signer the snapshot still goes, unsigned.
	plain, err := buildSnapshotMsg(ctx, pool, NewConnectorRegistry(), shieldID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.GetResourceSnapshots().GetShieldSnapshots()[shieldID].GetSignedResourceSet() != nil {
		t.Fatal("resource set signed without a signer")
	}
}
