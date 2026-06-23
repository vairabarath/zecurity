package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestNewValkeyClient_Success(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer mr.Close()

	rc, err := newValkeyClient("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if rc == nil {
		t.Fatal("expected non-nil valkeyClient")
	}
}

func TestNewValkeyClient_BadURL(t *testing.T) {
	_, err := newValkeyClient("not-a-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNewValkeyClient_Unreachable(t *testing.T) {
	_, err := newValkeyClient("redis://127.0.0.1:1") // nothing listening
	if err == nil {
		t.Fatal("expected error for unreachable Valkey")
	}
}

func TestSetAndGetPKCEState(t *testing.T) {
	rc, _ := newTestValkey(t)
	ctx := context.Background()

	if err := rc.SetPKCEState(ctx, "state-abc", "verifier-xyz", nil); err != nil {
		t.Fatalf("SetPKCEState: %v", err)
	}

	val, workspaceName, found, err := rc.GetAndDeletePKCEState(ctx, "state-abc")
	if err != nil {
		t.Fatalf("GetAndDeletePKCEState: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if val != "verifier-xyz" {
		t.Fatalf("expected verifier-xyz, got %s", val)
	}
	if workspaceName != "" {
		t.Fatalf("expected empty workspace name, got %q", workspaceName)
	}

	// Second retrieval — should be gone (single-use).
	_, _, found, err = rc.GetAndDeletePKCEState(ctx, "state-abc")
	if err != nil {
		t.Fatalf("second GetAndDeletePKCEState: %v", err)
	}
	if found {
		t.Fatal("expected found=false on second retrieval (single-use)")
	}
}

func TestGetAndDeletePKCEState_Expired(t *testing.T) {
	rc, mr := newTestValkey(t)
	ctx := context.Background()

	if err := rc.SetPKCEState(ctx, "state-exp", "verifier-exp", nil); err != nil {
		t.Fatalf("SetPKCEState: %v", err)
	}

	// Fast-forward miniredis past the 5-minute TTL.
	mr.FastForward(6 * time.Minute)

	_, _, found, err := rc.GetAndDeletePKCEState(ctx, "state-exp")
	if err != nil {
		t.Fatalf("GetAndDeletePKCEState: %v", err)
	}
	if found {
		t.Fatal("expected found=false after TTL expiry")
	}
}

func TestSetAndGetPKCEState_WithWorkspaceName(t *testing.T) {
	rc, _ := newTestValkey(t)
	ctx := context.Background()
	workspaceName := "Acme Workspace"

	if err := rc.SetPKCEState(ctx, "state-json", "verifier-json", &workspaceName); err != nil {
		t.Fatalf("SetPKCEState with workspaceName: %v", err)
	}

	val, gotWorkspaceName, found, err := rc.GetAndDeletePKCEState(ctx, "state-json")
	if err != nil {
		t.Fatalf("GetAndDeletePKCEState with workspaceName: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if val != "verifier-json" {
		t.Fatalf("expected verifier-json, got %s", val)
	}
	if gotWorkspaceName != workspaceName {
		t.Fatalf("expected workspaceName %q, got %q", workspaceName, gotWorkspaceName)
	}
}

func TestSetAndGetRefreshSession(t *testing.T) {
	rc, _ := newTestValkey(t)
	ctx := context.Background()

	want := RefreshSession{Token: "token-abc", OriginalIAT: 1700000000, MaxLifetimeAt: 1700000000 + 30*24*3600}
	if err := rc.SetRefreshSession(ctx, "user-1", want, 7*24*time.Hour); err != nil {
		t.Fatalf("SetRefreshSession: %v", err)
	}

	got, found, err := rc.GetRefreshSession(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetRefreshSession: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestGetRefreshSession_NotFound(t *testing.T) {
	rc, _ := newTestValkey(t)
	ctx := context.Background()

	_, found, err := rc.GetRefreshSession(ctx, "nonexistent-user")
	if err != nil {
		t.Fatalf("GetRefreshSession: %v", err)
	}
	if found {
		t.Fatal("expected found=false for nonexistent key")
	}
}

func TestDeleteRefreshToken(t *testing.T) {
	rc, _ := newTestValkey(t)
	ctx := context.Background()

	sess := RefreshSession{Token: "token-del"}
	if err := rc.SetRefreshSession(ctx, "user-del", sess, time.Hour); err != nil {
		t.Fatalf("SetRefreshSession: %v", err)
	}

	if err := rc.DeleteRefreshToken(ctx, "user-del"); err != nil {
		t.Fatalf("DeleteRefreshToken: %v", err)
	}

	_, found, err := rc.GetRefreshSession(ctx, "user-del")
	if err != nil {
		t.Fatalf("GetRefreshSession after delete: %v", err)
	}
	if found {
		t.Fatal("expected found=false after delete")
	}
}

func TestRefreshToken_Expired(t *testing.T) {
	rc, mr := newTestValkey(t)
	ctx := context.Background()

	sess := RefreshSession{Token: "token-ttl"}
	if err := rc.SetRefreshSession(ctx, "user-ttl", sess, time.Hour); err != nil {
		t.Fatalf("SetRefreshSession: %v", err)
	}

	// Fast-forward past the TTL.
	mr.FastForward(2 * time.Hour)

	_, found, err := rc.GetRefreshSession(ctx, "user-ttl")
	if err != nil {
		t.Fatalf("GetRefreshSession: %v", err)
	}
	if found {
		t.Fatal("expected found=false after TTL expiry")
	}
}
