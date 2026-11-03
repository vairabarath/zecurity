package connector

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *pushCoalescer) idle(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, running := c.jobs[key]
	return !running
}

// A burst for one key collapses into the run in progress plus one more run,
// with the latest work; nothing triggered during a run is lost.
func TestPushCoalescerCollapsesABurstLatestWins(t *testing.T) {
	c := newPushCoalescer(4, time.Second)
	var mu sync.Mutex
	var ran []string
	record := func(name string) { mu.Lock(); ran = append(ran, name); mu.Unlock() }
	started := make(chan struct{})
	release := make(chan struct{})
	c.trigger("k", func(context.Context) { close(started); <-release; record("A") })
	<-started
	c.trigger("k", func(context.Context) { record("B") })
	c.trigger("k", func(context.Context) { record("C") })
	close(release)
	waitFor(t, "worker exit", func() bool { return c.idle("k") })
	mu.Lock()
	defer mu.Unlock()
	if len(ran) != 2 || ran[0] != "A" || ran[1] != "C" {
		t.Fatalf("ran %v, want [A C]", ran)
	}
}

// After the worker exits, a new trigger starts a new one.
func TestPushCoalescerRestartsAfterDrain(t *testing.T) {
	c := newPushCoalescer(4, time.Second)
	var n atomic.Int32
	c.trigger("k", func(context.Context) { n.Add(1) })
	waitFor(t, "first run", func() bool { return c.idle("k") && n.Load() == 1 })
	c.trigger("k", func(context.Context) { n.Add(1) })
	waitFor(t, "second run", func() bool { return c.idle("k") && n.Load() == 2 })
}

// A panicking run doesn't wedge its key: the queued run still happens and
// later triggers still start a worker.
func TestPushCoalescerSurvivesAPanic(t *testing.T) {
	c := newPushCoalescer(4, time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	var n atomic.Int32
	c.trigger("k", func(context.Context) { close(started); <-release; panic("boom") })
	<-started
	c.trigger("k", func(context.Context) { n.Add(1) })
	close(release)
	waitFor(t, "queued run after a panic", func() bool { return c.idle("k") && n.Load() == 1 })
	c.trigger("k", func(context.Context) { n.Add(1) })
	waitFor(t, "run after a panic", func() bool { return c.idle("k") && n.Load() == 2 })
}

// Different keys run concurrently, at most maxConcurrent at once; each run
// gets a deadline.
func TestPushCoalescerBoundsConcurrencyAcrossKeys(t *testing.T) {
	c := newPushCoalescer(2, time.Second)
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	var done sync.WaitGroup
	for _, key := range []string{"a", "b", "c", "d"} {
		done.Add(1)
		c.trigger(key, func(ctx context.Context) {
			defer done.Done()
			if _, ok := ctx.Deadline(); !ok {
				t.Error("run without a deadline")
			}
			now := inFlight.Add(1)
			for {
				p := peak.Load()
				if now <= p || peak.CompareAndSwap(p, now) {
					break
				}
			}
			<-release
			inFlight.Add(-1)
		})
	}
	waitFor(t, "two runs in flight", func() bool { return inFlight.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak in flight %d, want 2", p)
	}
	close(release)
	done.Wait()
}
