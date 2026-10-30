package main

import (
	"sort"
	"sync"
	"time"
)

// workerSet runs the controller's background goroutines and waits for them
// at shutdown, by name, so a worker that doesn't stop is reported instead of
// holding the process until it is killed (live E2E run 2026-10-30, O-8).
type workerSet struct {
	wg      sync.WaitGroup
	mu      sync.Mutex
	running map[string]int
}

func newWorkerSet() *workerSet {
	return &workerSet{running: map[string]int{}}
}

// Go runs fn in a goroutine tracked under name.
func (w *workerSet) Go(name string, fn func()) {
	w.wg.Add(1)
	w.mu.Lock()
	w.running[name]++
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			if w.running[name]--; w.running[name] <= 0 {
				delete(w.running, name)
			}
			w.mu.Unlock()
		}()
		fn()
	}()
}

// Wait waits up to timeout for every worker. It returns the names of those
// still running (sorted), or nil once all have finished.
func (w *workerSet) Wait(timeout time.Duration) []string {
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	stuck := make([]string, 0, len(w.running))
	for name := range w.running {
		stuck = append(stuck, name)
	}
	sort.Strings(stuck)
	return stuck
}
