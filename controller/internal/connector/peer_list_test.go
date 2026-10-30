package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// D21-R (a2): each shield snapshot carries a controller-signed peer list. A
// Connector can withhold or replay one, never forge it, so the shield no
// longer takes its peers from the Connector it is attached to.

// keySigner signs like pki.SignShieldPeerList, with a test key.
type keySigner struct {
	key     *ecdsa.PrivateKey
	tenants []string
}

func (s *keySigner) SignShieldPeerList(_ context.Context, tenantID string, payload []byte) ([]byte, error) {
	s.tenants = append(s.tenants, tenantID)
	return ecdsa.SignASN1(rand.Reader, s.key, pki.ShieldPeerListDigest(payload))
}

func newKeySigner(t *testing.T) *keySigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &keySigner{key: key}
}

func TestShieldFacingAddr(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.5":           "10.0.0.5:9091",
		"10.0.0.5:9092":      "10.0.0.5:9091",
		"[2001:db8::1]:9092": "[2001:db8::1]:9091",
		"":                   "",
		":9092":              "",
	} {
		if got := shieldFacingAddr(in); got != want {
			t.Errorf("shieldFacingAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// The list holds exactly the network's Connectors that have an identity, are
// not revoked and have an address; it is bound to the shield and its
// workspace, signed over the domain-separated digest, and every list is
// issued later than the one before.
func TestBuildSignedPeerList(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("peers-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)

	conn := func(status, lanAddr string, revoked bool) string {
		t.Helper()
		var revokedAt *time.Time
		if revoked {
			now := time.Now()
			revokedAt = &now
		}
		id := seedConnector(t, pool, ctx, wsID, rnID, td, status, revokedAt)
		if _, err := pool.Exec(ctx, `UPDATE connectors SET lan_addr = NULLIF($2, '') WHERE id = $1`, id, lanAddr); err != nil {
			t.Fatal(err)
		}
		return id
	}
	active := conn("active", "10.0.0.1", false)
	disconnected := conn("disconnected", "10.0.0.2:9092", false)
	conn("revoked", "10.0.0.3", true)
	conn("pending", "10.0.0.4", false)
	conn("active", "", false) // no address yet: nothing to dial

	// A Connector of another network is never a peer.
	var otherRN string
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'other', 'office') RETURNING id`,
		wsID).Scan(&otherRN); err != nil {
		t.Fatal(err)
	}
	other := seedConnector(t, pool, ctx, wsID, otherRN, td, "active", nil)
	if _, err := pool.Exec(ctx, `UPDATE connectors SET lan_addr = '10.9.9.9' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}

	var shieldID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
		 VALUES ($1, $2, $3, 'shield', 'active', $4) RETURNING id::text`,
		wsID, rnID, active, td).Scan(&shieldID); err != nil {
		t.Fatal(err)
	}

	signer := newKeySigner(t)
	r := NewConnectorRegistry()
	r.SetPeerListSigner(signer)

	msg, err := buildSnapshotMsg(ctx, pool, r, shieldID)
	if err != nil {
		t.Fatal(err)
	}
	signed := msg.GetResourceSnapshots().GetShieldSnapshots()[shieldID].GetSignedPeerList()
	if signed == nil {
		t.Fatal("snapshot carries no signed peer list")
	}
	if !ecdsa.VerifyASN1(&signer.key.PublicKey, pki.ShieldPeerListDigest(signed.Payload), signed.Signature) {
		t.Fatal("signature does not verify over the domain-separated digest")
	}
	if ecdsa.VerifyASN1(&signer.key.PublicKey, digestWithoutContext(signed.Payload), signed.Signature) {
		t.Fatal("signature verifies without the context prefix")
	}
	if len(signer.tenants) != 1 || signer.tenants[0] != wsID {
		t.Fatalf("signed with tenants %v, want [%s]", signer.tenants, wsID)
	}

	var payload shieldpb.PeerListPayload
	if err := proto.Unmarshal(signed.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TrustDomain != td || payload.ShieldId != shieldID {
		t.Fatalf("bound to (%q, %q), want (%q, %q)", payload.TrustDomain, payload.ShieldId, td, shieldID)
	}
	want := map[string]string{active: "10.0.0.1:9091", disconnected: "10.0.0.2:9091"}
	if len(payload.Peers) != len(want) {
		t.Fatalf("peers = %v, want %v", payload.Peers, want)
	}
	for _, p := range payload.Peers {
		if want[p.ConnectorId] != p.ConnectorAddr {
			t.Fatalf("peer %s at %q, want %v", p.ConnectorId, p.ConnectorAddr, want)
		}
	}

	// Lists keep ordering when the clock falls behind the last issued_at
	// (same millisecond, or a clock step back).
	r.peerLists.lastMS = int64(payload.IssuedAtUnixMs) + 60_000
	next, err := buildSnapshotMsg(ctx, pool, r, shieldID)
	if err != nil {
		t.Fatal(err)
	}
	var nextPayload shieldpb.PeerListPayload
	if err := proto.Unmarshal(next.GetResourceSnapshots().GetShieldSnapshots()[shieldID].GetSignedPeerList().GetPayload(), &nextPayload); err != nil {
		t.Fatal(err)
	}
	if nextPayload.IssuedAtUnixMs <= payload.IssuedAtUnixMs+60_000 {
		t.Fatalf("issued_at %d not after the last issued %d", nextPayload.IssuedAtUnixMs, payload.IssuedAtUnixMs+60_000)
	}

	// Without a signer the snapshot still goes, with no peer list.
	plain, err := buildSnapshotMsg(ctx, pool, NewConnectorRegistry(), shieldID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.GetResourceSnapshots().GetShieldSnapshots()[shieldID].GetSignedPeerList() != nil {
		t.Fatal("peer list without a signer")
	}
}

// PushPeerListsForNetworkOf re-sends a signed list to every enrolled shield
// of the network through its current Connector, e.g. after a revoke.
func TestPushPeerListsForNetworkOf(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("peers-push-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	connA := seedConnector(t, pool, ctx, wsID, rnID, td, "active", nil)
	connB := seedConnector(t, pool, ctx, wsID, rnID, td, "active", nil)
	shield := func(conn, status string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
			 VALUES ($1, $2, $3, 'shield', $4, $5) RETURNING id::text`,
			wsID, rnID, conn, status, td).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	onA := shield(connA, "active")
	onB := shield(connB, "disconnected")
	shield(connB, "revoked")

	r := NewConnectorRegistry()
	r.SetPeerListSigner(newKeySigner(t))
	clientA := &connectorStreamClient{connectorID: connA, tenantID: wsID, outbound: make(chan *pb.ConnectorControlMessage, 8)}
	clientB := &connectorStreamClient{connectorID: connB, tenantID: wsID, outbound: make(chan *pb.ConnectorControlMessage, 8)}
	r.clients[connA] = clientA
	r.clients[connB] = clientB

	PushPeerListsForNetworkOf(ctx, pool, r, connA)

	got := func(c *connectorStreamClient) []string {
		var ids []string
		for len(c.outbound) > 0 {
			for id, snap := range (<-c.outbound).GetResourceSnapshots().GetShieldSnapshots() {
				if snap.GetSignedPeerList() == nil {
					t.Fatalf("snapshot for %s without a peer list", id)
				}
				ids = append(ids, id)
			}
		}
		return ids
	}
	if a := got(clientA); len(a) != 1 || a[0] != onA {
		t.Fatalf("connector A got %v, want [%s]", a, onA)
	}
	if b := got(clientB); len(b) != 1 || b[0] != onB {
		t.Fatalf("connector B got %v, want [%s] (not the revoked shield)", b, onB)
	}
}

// digestWithoutContext is SHA-384 over the payload alone, without the
// domain-separation prefix.
func digestWithoutContext(payload []byte) []byte {
	sum := sha512.Sum384(payload)
	return sum[:]
}
