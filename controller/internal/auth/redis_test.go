package auth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestNewRedisClient_Success(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	defer mr.Close()

	rc, err := newRedisClient("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if rc == nil {
		t.Fatal("expected non-nil redisClient")
	}
}

func TestNewRedisClient_BadURL(t *testing.T) {
	_, err := newRedisClient("not-a-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestNewRedisClient_Unreachable(t *testing.T) {
	_, err := newRedisClient("redis://127.0.0.1:1") // nothing listening
	if err == nil {
		t.Fatal("expected error for unreachable Redis")
	}
}

func TestSetAndGetPKCEState(t *testing.T) {
	rc, _ := newTestRedis(t)
	ctx := context.Background()

	// Store a PKCE state.
	if err := rc.SetPKCEState(ctx, "state-abc", "verifier-xyz", nil); err != nil {
		t.Fatalf("SetPKCEState: %v", err)
	}

	// Retrieve it — should return the verifier and delete the key.
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
	rc, mr := newTestRedis(t)
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
	rc, _ := newTestRedis(t)
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

func TestSetAndGetRefreshToken(t *testing.T) {
	rc, _ := newTestRedis(t)
	ctx := context.Background()

	// Store a refresh token.
	if err := rc.SetRefreshToken(ctx, "user-1", "token-abc", 7*24*time.Hour); err != nil {
		t.Fatalf("SetRefreshToken: %v", err)
	}

	// Retrieve it.
	val, found, err := rc.GetRefreshToken(ctx, "user-1")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if val != "token-abc" {
		t.Fatalf("expected token-abc, got %s", val)
	}
}

func TestGetRefreshToken_NotFound(t *testing.T) {
	rc, _ := newTestRedis(t)
	ctx := context.Background()

	_, found, err := rc.GetRefreshToken(ctx, "nonexistent-user")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if found {
		t.Fatal("expected found=false for nonexistent key")
	}
}

func TestDeleteRefreshToken(t *testing.T) {
	rc, _ := newTestRedis(t)
	ctx := context.Background()

	if err := rc.SetRefreshToken(ctx, "user-del", "token-del", time.Hour); err != nil {
		t.Fatalf("SetRefreshToken: %v", err)
	}

	// Delete it.
	if err := rc.DeleteRefreshToken(ctx, "user-del"); err != nil {
		t.Fatalf("DeleteRefreshToken: %v", err)
	}

	// Should be gone.
	_, found, err := rc.GetRefreshToken(ctx, "user-del")
	if err != nil {
		t.Fatalf("GetRefreshToken after delete: %v", err)
	}
	if found {
		t.Fatal("expected found=false after delete")
	}
}

func TestRefreshToken_Expired(t *testing.T) {
	rc, mr := newTestRedis(t)
	ctx := context.Background()

	if err := rc.SetRefreshToken(ctx, "user-ttl", "token-ttl", time.Hour); err != nil {
		t.Fatalf("SetRefreshToken: %v", err)
	}

	// Fast-forward past the TTL.
	mr.FastForward(2 * time.Hour)

	_, found, err := rc.GetRefreshToken(ctx, "user-ttl")
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if found {
		t.Fatal("expected found=false after TTL expiry")
	}
}
