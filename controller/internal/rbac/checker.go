package rbac

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTTL is the cache backstop for role changes made outside the
// controller's own write paths (direct SQL, bootstrap). Writes that go through
// the controller invalidate the entry at once.
const DefaultTTL = 30 * time.Second

// ErrNoAccess means the caller has no live, active user row in the workspace.
// Callers deny the request (fail closed).
var ErrNoAccess = errors.New("rbac: no active user in workspace")

// Invalidator drops cached roles after a write that changes a user's role or
// status. Implemented by *Checker; accepted as an interface so stores can take
// a nil-safe dependency.
type Invalidator interface {
	Invalidate(tenantID, userID string)
	InvalidateTenant(tenantID string)
}

type cacheKey struct{ tenant, user string }

type cacheEntry struct {
	role    string
	expires time.Time
}

// Checker resolves a caller's live workspace role from users.role, with a
// process-local cache (single controller instance, D-02).
//
// Only active users are cached. A missing row, a non-active status or a lookup
// error is never cached and always denies.
type Checker struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time

	mu    sync.Mutex
	cache map[cacheKey]cacheEntry
	// gen counts invalidations per key and tenantGen per workspace. A lookup
	// records both before its DB read and stores its result only if neither
	// moved, so a read that raced an invalidation can't put a stale role back.
	gen       map[cacheKey]uint64
	tenantGen map[string]uint64

	afterRead func() // test hook: runs between the DB read and the cache store
}

// NewChecker builds a Checker with the default TTL.
func NewChecker(pool *pgxpool.Pool) *Checker {
	return &Checker{
		pool: pool, ttl: DefaultTTL, now: time.Now,
		cache: map[cacheKey]cacheEntry{}, gen: map[cacheKey]uint64{}, tenantGen: map[string]uint64{},
	}
}

// Role returns the caller's current role. It returns ErrNoAccess when the user
// row is missing or not active, and a wrapped error when the lookup fails;
// either way the caller must deny.
func (c *Checker) Role(ctx context.Context, tenantID, userID string) (string, error) {
	if tenantID == "" || userID == "" {
		return "", ErrNoAccess
	}
	k := cacheKey{tenantID, userID}
	c.mu.Lock()
	if e, ok := c.cache[k]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.role, nil
	}
	gen, tgen := c.gen[k], c.tenantGen[tenantID]
	c.mu.Unlock()

	var role, status string
	err := c.pool.QueryRow(ctx,
		`SELECT role, status FROM users WHERE id = $1 AND tenant_id = $2`,
		userID, tenantID,
	).Scan(&role, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		c.drop(k)
		return "", ErrNoAccess
	}
	if err != nil {
		return "", fmt.Errorf("rbac: role lookup: %w", err)
	}
	if status != "active" || !ValidRole(role) {
		c.drop(k)
		return "", ErrNoAccess
	}
	if c.afterRead != nil {
		c.afterRead()
	}
	c.mu.Lock()
	if c.gen[k] == gen && c.tenantGen[tenantID] == tgen {
		c.cache[k] = cacheEntry{role: role, expires: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	// The role read here is still returned: it was current when read, and the
	// next request re-reads after an invalidation.
	return role, nil
}

func (c *Checker) drop(k cacheKey) {
	c.mu.Lock()
	delete(c.cache, k)
	c.mu.Unlock()
}

// Invalidate drops one user's cached role.
func (c *Checker) Invalidate(tenantID, userID string) {
	if c == nil {
		return
	}
	k := cacheKey{tenantID, userID}
	c.mu.Lock()
	delete(c.cache, k)
	c.gen[k]++
	c.mu.Unlock()
}

// InvalidateTenant drops every cached role in a workspace (bulk changes such
// as disabling an identity connection).
func (c *Checker) InvalidateTenant(tenantID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for k := range c.cache {
		if k.tenant == tenantID {
			delete(c.cache, k)
		}
	}
	c.tenantGen[tenantID]++
	c.mu.Unlock()
}

type roleCtxKey struct{}

// WithRole records the live role resolved for this request, so a resolver
// behind @requires can read it without a second lookup.
func WithRole(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, roleCtxKey{}, role)
}

// RoleFrom returns the live role recorded by WithRole.
func RoleFrom(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(roleCtxKey{}).(string)
	return r, ok && r != ""
}
