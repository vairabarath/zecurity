package connector

import (
	"context"
	"log"
	"sync"
	"time"
)

// pushCoalescer runs fire-and-forget background pushes (D21-R a2) without
// letting a burst of triggers (a flapping Connector, a reconnect storm after
// a controller restart) start one goroutine per trigger:
//
//   - at most one worker per key; a trigger for a key whose worker is
//     running only replaces the queued work (latest wins), so a burst
//     collapses into the run in progress plus one more run;
//   - the queued work is read and cleared under mu, and the worker exits
//     only when it finds nothing queued under that same lock, so a trigger
//     is never lost between "nothing queued" and the worker leaving;
//   - each run is bounded by timeout and recovers a panic, so a key can't
//     stay wedged with a dead worker;
//   - sem bounds how many runs execute at once across all keys.
type pushCoalescer struct {
	mu      sync.Mutex
	jobs    map[string]*pushJob // a key is present while its worker runs
	sem     chan struct{}
	timeout time.Duration
}

type pushJob struct {
	next func(context.Context) // nil: nothing queued
}

// maxConcurrentPushes bounds background pushes in flight at once; each one
// builds snapshots and signs peer lists (a Workspace CA key decrypt each).
const maxConcurrentPushes = 4

func newPushCoalescer(maxConcurrent int, timeout time.Duration) *pushCoalescer {
	return &pushCoalescer{
		jobs:    make(map[string]*pushJob),
		sem:     make(chan struct{}, maxConcurrent),
		timeout: timeout,
	}
}

// trigger queues work for key and starts the key's worker if none runs.
func (c *pushCoalescer) trigger(key string, work func(context.Context)) {
	c.mu.Lock()
	if job, running := c.jobs[key]; running {
		job.next = work
		c.mu.Unlock()
		return
	}
	job := &pushJob{next: work}
	c.jobs[key] = job
	c.mu.Unlock()
	go c.worker(key, job)
}

func (c *pushCoalescer) worker(key string, job *pushJob) {
	for {
		c.mu.Lock()
		work := job.next
		job.next = nil
		if work == nil {
			delete(c.jobs, key)
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		c.runOne(key, work)
	}
}

func (c *pushCoalescer) runOne(key string, work func(context.Context)) {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("background push %s panicked: %v", key, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	work(ctx)
}
