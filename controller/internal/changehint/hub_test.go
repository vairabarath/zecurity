package changehint

import (
	"fmt"
	"sync"
	"testing"
)

func woken(s *Subscription) bool {
	select {
	case <-s.C:
		return true
	default:
		return false
	}
}

func TestNotifyWakesOnlyThatWorkspaceAndCoalesces(t *testing.T) {
	h := New()
	a := h.Subscribe("ws-a", "dev-1")
	b := h.Subscribe("ws-b", "dev-2")
	defer a.Close()
	defer b.Close()

	for i := 0; i < 5; i++ {
		h.Notify("ws-a")
	}
	if !woken(a) {
		t.Fatal("ws-a stream not woken")
	}
	if woken(a) {
		t.Fatal("five changes left more than one pending wake")
	}
	if woken(b) {
		t.Fatal("a ws-a change woke a ws-b stream")
	}
}

func TestSecondStreamOfADeviceReplacesTheFirst(t *testing.T) {
	h := New()
	first := h.Subscribe("ws", "dev")
	second := h.Subscribe("ws", "dev")
	select {
	case <-first.Replaced:
	default:
		t.Fatal("the older stream was not told it was replaced")
	}
	if h.Len() != 1 {
		t.Fatalf("subscriptions = %d, want 1 per device", h.Len())
	}
	first.Close() // the replaced stream ending must not remove the new one
	h.Notify("ws")
	if !woken(second) {
		t.Fatal("closing the replaced stream removed the current one")
	}
	second.Close()
	second.Close() // idempotent
	if h.Len() != 0 {
		t.Fatalf("subscriptions after close = %d, want 0", h.Len())
	}
}

// Notify, Subscribe and Close race freely (run with -race); nothing leaks.
func TestConcurrentUseLeavesNothingBehind(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s := h.Subscribe("ws", fmt.Sprintf("dev-%d", i%10))
				woken(s)
				s.Close()
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.Notify("ws")
			}
		}()
	}
	wg.Wait()
	if h.Len() != 0 {
		t.Fatalf("subscriptions left = %d, want 0", h.Len())
	}
}
