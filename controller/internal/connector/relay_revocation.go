package connector

import (
	"context"
	"sync"
	"time"
)

// RelayRevocationChecker holds an in-memory set of revoked (unexpired) relay
// certificate serials, refreshed from the DB periodically and on demand. The
// SPIFFE interceptors consult it to reject a revoked relay at Heartbeat and other
// authenticated relay RPCs. (Provision is unauthenticated — skipped by the
// interceptor — so a revoked relay is blocked from re-provisioning by the
// MarkProvisioned status guard instead, not here.)
//
// Fail-closed: until the first successful load, Ready() is false and callers MUST
// deny — a relay whose revocation state is unknown is not trusted.
type RelayRevocationChecker struct {
	load    func(context.Context) ([]string, error)
	mu      sync.RWMutex
	revoked map[string]struct{}
	ready   bool
}

// NewRelayRevocationChecker builds a checker backed by `load`, which returns the
// current revoked, unexpired relay serials (canonical SerialNumber.Text(16)).
func NewRelayRevocationChecker(load func(context.Context) ([]string, error)) *RelayRevocationChecker {
	return &RelayRevocationChecker{load: load, revoked: map[string]struct{}{}}
}

// Refresh reloads the revoked set. On error the previous set + ready state are
// kept (never un-revoke on a transient failure).
func (c *RelayRevocationChecker) Refresh(ctx context.Context) error {
	serials, err := c.load(ctx)
	if err != nil {
		return err
	}
	set := make(map[string]struct{}, len(serials))
	for _, s := range serials {
		set[s] = struct{}{}
	}
	c.mu.Lock()
	c.revoked = set
	c.ready = true
	c.mu.Unlock()
	return nil
}

// Ready reports whether at least one successful load has happened.
func (c *RelayRevocationChecker) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// IsRevoked reports whether serialHex (canonical SerialNumber.Text(16)) is revoked.
func (c *RelayRevocationChecker) IsRevoked(serialHex string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.revoked[serialHex]
	return ok
}

// SpawnRefresh runs Refresh every interval until ctx is done. onErr (may be nil)
// is invoked with any refresh error so the caller can log it.
func (c *RelayRevocationChecker) SpawnRefresh(ctx context.Context, interval time.Duration, onErr func(error)) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := c.Refresh(ctx); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
}
