package mfa

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConsolePolicy answers "does this workspace require MFA to use the admin
// console?" (PENDING-06 M-6, workspaces.console_require_mfa).
//
// The answer is read on every console request, so it is cached in process
// for a short time. Set drops the cache entry, and the controller is a
// single instance (D-02), so a change made here applies immediately; the TTL
// only bounds a change made by something else (a DB restore, a test).
type ConsolePolicy struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]consoleEntry
	// versions counts Set calls per workspace, so a read that started
	// before a Set never caches its older answer.
	versions map[string]uint64
}

type consoleEntry struct {
	required bool
	until    time.Time
}

func NewConsolePolicy(pool *pgxpool.Pool) *ConsolePolicy {
	return &ConsolePolicy{pool: pool, ttl: 15 * time.Second, now: time.Now, cache: map[string]consoleEntry{}, versions: map[string]uint64{}}
}

// Required reports whether the workspace requires MFA on the console. An
// unknown workspace is an error (callers fail closed).
func (p *ConsolePolicy) Required(ctx context.Context, workspaceID string) (bool, error) {
	p.mu.Lock()
	if e, ok := p.cache[workspaceID]; ok && e.until.After(p.now()) {
		p.mu.Unlock()
		return e.required, nil
	}
	// A Set that commits while this read is in flight bumps the version,
	// and then this (possibly older) value is not cached.
	version := p.versions[workspaceID]
	p.mu.Unlock()

	var required bool
	err := p.pool.QueryRow(ctx,
		`SELECT console_require_mfa FROM workspaces WHERE id = $1`, workspaceID).Scan(&required)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("mfa: workspace %s not found", workspaceID)
	}
	if err != nil {
		return false, fmt.Errorf("mfa: read console policy: %w", err)
	}
	p.mu.Lock()
	if p.versions[workspaceID] == version {
		p.cache[workspaceID] = consoleEntry{required: required, until: p.now().Add(p.ttl)}
	}
	p.mu.Unlock()
	return required, nil
}

// Set turns the requirement on or off and reports the previous value.
func (p *ConsolePolicy) Set(ctx context.Context, workspaceID string, required bool) (previous bool, err error) {
	err = p.pool.QueryRow(ctx,
		`WITH old AS (SELECT console_require_mfa FROM workspaces WHERE id = $1 FOR UPDATE)
		 UPDATE workspaces w SET console_require_mfa = $2, updated_at = NOW()
		   FROM old WHERE w.id = $1
		 RETURNING old.console_require_mfa`, workspaceID, required).Scan(&previous)
	if err != nil {
		return false, fmt.Errorf("mfa: set console policy: %w", err)
	}
	p.mu.Lock()
	// Drop rather than write: two Sets can reach here in the opposite order
	// to their commits. The next read loads the committed value, and the
	// version bump stops a read already in flight from caching its answer.
	p.versions[workspaceID]++
	delete(p.cache, workspaceID)
	p.mu.Unlock()
	return previous, nil
}
