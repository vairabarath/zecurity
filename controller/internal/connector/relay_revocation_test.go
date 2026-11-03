package connector

import (
	"context"
	"errors"
	"testing"
)

func TestRelayRevocationChecker_NotReadyUntilFirstLoad(t *testing.T) {
	c := NewRelayRevocationChecker(func(ctx context.Context) ([]string, error) {
		return []string{"aa"}, nil
	})

	if c.Ready() {
		t.Fatal("expected checker to be not-ready before the first Refresh")
	}
	if c.IsRevoked("aa") {
		t.Fatal("expected IsRevoked to be false before any load, even for a serial the load would return")
	}
}

func TestRelayRevocationChecker_ReadyAndRevokedAfterLoad(t *testing.T) {
	c := NewRelayRevocationChecker(func(ctx context.Context) ([]string, error) {
		return []string{"aa", "bb"}, nil
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !c.Ready() {
		t.Fatal("expected checker to be ready after a successful Refresh")
	}
	if !c.IsRevoked("aa") || !c.IsRevoked("bb") {
		t.Fatal("expected loaded serials to be reported revoked")
	}
	if c.IsRevoked("cc") {
		t.Fatal("expected an unloaded serial to not be reported revoked")
	}
}

func TestRelayRevocationChecker_NeverUnrevokeOnTransientFailure(t *testing.T) {
	loadErr := errors.New("db unavailable")
	failNext := false
	c := NewRelayRevocationChecker(func(ctx context.Context) ([]string, error) {
		if failNext {
			return nil, loadErr
		}
		return []string{"aa"}, nil
	})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if !c.IsRevoked("aa") {
		t.Fatal("expected aa to be revoked after the first successful load")
	}

	failNext = true
	if err := c.Refresh(context.Background()); !errors.Is(err, loadErr) {
		t.Fatalf("second Refresh error = %v, want %v", err, loadErr)
	}

	// A transient failure must not clear the previously-loaded set or Ready state.
	if !c.Ready() {
		t.Fatal("expected checker to remain ready after a transient refresh failure")
	}
	if !c.IsRevoked("aa") {
		t.Fatal("expected previously-loaded revocation to survive a transient refresh failure")
	}
}
