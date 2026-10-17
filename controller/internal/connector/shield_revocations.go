package connector

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/shield"
)

// shieldRevocationRetryInterval is how often the dispatcher re-scans the
// outbox for work left by a failed delivery or a previous process.
const shieldRevocationRetryInterval = 5 * time.Second

// ShieldRevocationDispatcher drains the shield revocation outbox
// (certificate audit bug 3). shield.Revoke records the work in the same
// transaction as the revocation; for each workspace with pending work the
// dispatcher invalidates the workspace's policy (resources behind the revoked
// shield become unreachable) and queues the workspace's complete revocation
// list on every connected connector. Only then are the outbox rows deleted.
// A failure leaves them, with the error recorded, for the next pass; a
// connector that is not connected receives the complete list when its
// Control stream opens.
type ShieldRevocationDispatcher struct {
	Pool           *pgxpool.Pool
	Registry       *ConnectorRegistry
	PolicyNotifier PolicyChangeNotifier

	once sync.Once
	wake chan struct{}
}

func (d *ShieldRevocationDispatcher) init() {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
}

// Wake asks for a pass now (after a revocation committed). Nil-safe and
// non-blocking; correctness does not depend on it.
func (d *ShieldRevocationDispatcher) Wake() {
	if d == nil {
		return
	}
	d.init()
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run processes the outbox at start (work left by a previous process), on
// every Wake, and every shieldRevocationRetryInterval, until ctx ends.
func (d *ShieldRevocationDispatcher) Run(ctx context.Context) {
	d.init()
	ticker := time.NewTicker(shieldRevocationRetryInterval)
	defer ticker.Stop()
	for {
		if err := d.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("shield revocation dispatcher: %v (will retry)", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}

// ProcessOnce delivers every pending outbox entry once.
func (d *ShieldRevocationDispatcher) ProcessOnce(ctx context.Context) error {
	rows, err := d.Pool.Query(ctx,
		`SELECT id, tenant_id::text FROM shield_revocation_outbox ORDER BY id LIMIT 1000`)
	if err != nil {
		return fmt.Errorf("read shield revocation outbox: %w", err)
	}
	pending := map[string][]int64{}
	var tenants []string
	for rows.Next() {
		var id int64
		var tenantID string
		if err := rows.Scan(&id, &tenantID); err != nil {
			rows.Close()
			return fmt.Errorf("scan shield revocation outbox: %w", err)
		}
		if _, seen := pending[tenantID]; !seen {
			tenants = append(tenants, tenantID)
		}
		pending[tenantID] = append(pending[tenantID], id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read shield revocation outbox: %w", err)
	}

	var errs []error
	for _, tenantID := range tenants {
		ids := pending[tenantID]
		if err := d.deliver(ctx, tenantID); err != nil {
			errs = append(errs, fmt.Errorf("workspace %s: %w", tenantID, err))
			if _, uerr := d.Pool.Exec(ctx,
				`UPDATE shield_revocation_outbox
				    SET attempts = attempts + 1, last_error = $2
				  WHERE id = ANY($1)`,
				ids, err.Error(),
			); uerr != nil {
				errs = append(errs, fmt.Errorf("record delivery failure: %w", uerr))
			}
			continue
		}
		// Only the rows read above: a revocation committed meanwhile keeps
		// its own row and is delivered by the next pass.
		if _, err := d.Pool.Exec(ctx,
			`DELETE FROM shield_revocation_outbox WHERE id = ANY($1)`, ids,
		); err != nil {
			errs = append(errs, fmt.Errorf("clear delivered work: %w", err))
		}
	}
	return errors.Join(errs...)
}

// deliver invalidates tenantID's policy and pushes its complete revocation
// list to every connected connector of the workspace.
func (d *ShieldRevocationDispatcher) deliver(ctx context.Context, tenantID string) error {
	if d.PolicyNotifier != nil {
		if err := d.PolicyNotifier.NotifyPolicyChange(ctx, tenantID); err != nil {
			return fmt.Errorf("invalidate policy: %w", err)
		}
	}
	revoked, err := shield.RevokedShieldIDs(ctx, d.Pool, tenantID)
	if err != nil {
		return err
	}
	if d.Registry != nil {
		if err := d.Registry.PushShieldRevocations(tenantID, revoked); err != nil {
			return fmt.Errorf("push revocation list: %w", err)
		}
	}
	return nil
}

func shieldRevocationMessage(revoked []string) *pb.ConnectorControlMessage {
	return &pb.ConnectorControlMessage{
		Body: &pb.ConnectorControlMessage_ShieldRevocations{
			ShieldRevocations: &pb.ShieldRevocationList{RevokedShieldIds: revoked},
		},
	}
}

// PushShieldRevocations queues workspaceID's complete revoked-shield list on
// every connected connector of that workspace. It fails if any queue was
// full, so the caller retries; re-sending is harmless (the list is
// authoritative and applying it is idempotent).
func (r *ConnectorRegistry) PushShieldRevocations(workspaceID string, revoked []string) error {
	var errs []error
	for _, c := range r.ClientsForWorkspace(workspaceID) {
		if err := c.send(shieldRevocationMessage(revoked)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// sendShieldRevocations queues the workspace's complete revocation list on a
// newly opened Control stream. The stream is already registered, so a
// revocation committed after this read reaches it through the dispatcher.
func (h *EnrollmentHandler) sendShieldRevocations(ctx context.Context, client *connectorStreamClient) error {
	revoked, err := shield.RevokedShieldIDs(ctx, h.Pool, client.tenantID)
	if err != nil {
		return err
	}
	return client.send(shieldRevocationMessage(revoked))
}
