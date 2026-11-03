package main

import (
	"reflect"
	"testing"
	"time"
)

func TestWorkerSetWaitAllDone(t *testing.T) {
	w := newWorkerSet()
	for _, n := range []string{"a", "b", "b"} {
		w.Go(n, func() {})
	}
	if stuck := w.Wait(time.Second); stuck != nil {
		t.Fatalf("stuck = %v, want nil", stuck)
	}
}

// O-8: a worker that never returns no longer blocks shutdown; it is named.
func TestWorkerSetWaitNamesStragglers(t *testing.T) {
	w := newWorkerSet()
	block := make(chan struct{})
	defer close(block)
	w.Go("finishes", func() {})
	w.Go("outbox-processor-2", func() { <-block })
	w.Go("relay-expiry-loop", func() { <-block })
	start := time.Now()
	stuck := w.Wait(50 * time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Wait took %v, want about the timeout", d)
	}
	if want := []string{"outbox-processor-2", "relay-expiry-loop"}; !reflect.DeepEqual(stuck, want) {
		t.Fatalf("stuck = %v, want %v", stuck, want)
	}
}
