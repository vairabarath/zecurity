package connector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"google.golang.org/protobuf/proto"
)

// signResourceSet signs shieldID's desired set (`resources`, at the snapshot's
// `generation`) together with its pending removes: the resources the shield
// must stop enforcing and ack as "unprotected" (an unprotect in progress, or a
// deletion tombstone). The shield applies resources only from this signed set
// (D21-R S), so a Connector can withhold or replay one, never forge, alter or
// roll back one. Nil issuer → nil set, no error.
func (p *peerListIssuer) signResourceSet(
	ctx context.Context,
	db *pgxpool.Pool,
	shieldID string,
	generation uint64,
	resources []*shieldpb.ResourceInstruction,
) (*shieldpb.SignedResourceSet, error) {
	if p == nil || p.signer == nil {
		return nil, nil
	}
	var tenantID, trustDomain string
	if err := db.QueryRow(ctx,
		`SELECT s.tenant_id::text, COALESCE(w.trust_domain, '')
		   FROM shields s
		   JOIN workspaces w ON w.id = s.tenant_id
		  WHERE s.id = $1`,
		shieldID,
	).Scan(&tenantID, &trustDomain); err != nil {
		return nil, fmt.Errorf("resource set: load shield %s: %w", shieldID, err)
	}
	if trustDomain == "" {
		return nil, fmt.Errorf("resource set: shield %s: workspace has no trust domain", shieldID)
	}

	rows, err := db.Query(ctx,
		`SELECT id::text, host, protocol, port_from, port_to
		   FROM resources
		  WHERE shield_id = $1
		    AND ((status = 'protecting' AND pending_action = 'remove')
		         OR status = 'deleting')
		  ORDER BY id`,
		shieldID,
	)
	if err != nil {
		return nil, fmt.Errorf("resource set: removes for shield %s: %w", shieldID, err)
	}
	defer rows.Close()
	payload := &shieldpb.ResourceSetPayload{
		TrustDomain: trustDomain,
		ShieldId:    shieldID,
		Generation:  generation,
		Resources:   resources,
		Removes:     []*shieldpb.ResourceInstruction{},
	}
	for rows.Next() {
		var id, host, protocol string
		var portFrom, portTo int32
		if err := rows.Scan(&id, &host, &protocol, &portFrom, &portTo); err != nil {
			return nil, fmt.Errorf("resource set: scan remove: %w", err)
		}
		payload.Removes = append(payload.Removes, &shieldpb.ResourceInstruction{
			ResourceId: id,
			Host:       host,
			Protocol:   protocol,
			PortFrom:   portFrom,
			PortTo:     portTo,
			Action:     "remove",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resource set: removes for shield %s: %w", shieldID, err)
	}

	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("resource set: marshal: %w", err)
	}
	sig, err := p.signer.SignShieldResourceSet(ctx, tenantID, raw)
	if err != nil {
		return nil, fmt.Errorf("resource set: %w", err)
	}
	return &shieldpb.SignedResourceSet{Payload: raw, Signature: sig}, nil
}
