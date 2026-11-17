package relay

import (
	"sort"
	"sync"
)

// F11-2 (D-39 refinement): a tenant whose primary pool is exhausted but
// eligible again, and whose failover pool has no eligible relay, is sent its
// primary's list at once instead of waiting out the hold-down
// (relaypool.EffectivePool). The override is derived on every list build: it
// is neither stored nor audited (the pool-level failover and failback rows
// are unchanged). overrideLog remembers which workspaces were overridden at
// the last build, so the controller logs each override when it starts and
// when it ends, not on every build. Process-local state is acceptable with a
// single controller (D-02); after a restart the current overrides are logged
// once more.
type overrideLog struct {
	mu     sync.Mutex
	active map[string]bool
}

func newOverrideLog() *overrideLog { return &overrideLog{active: map[string]bool{}} }

// observe records the workspaces overridden now and returns those that
// entered and left the override since the last call, sorted.
func (l *overrideLog) observe(now map[string]bool) (entered, left []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ws := range now {
		if !l.active[ws] {
			entered = append(entered, ws)
		}
	}
	for ws := range l.active {
		if !now[ws] {
			left = append(left, ws)
		}
	}
	sort.Strings(entered)
	sort.Strings(left)
	l.active = make(map[string]bool, len(now))
	for ws := range now {
		l.active[ws] = true
	}
	return entered, left
}
