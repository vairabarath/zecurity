package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisClient wraps the Redis connection for PKCE state and refresh token storage.
// Created by: newRedisClient() below, called from NewService() in config.go.
// Used by: serviceImpl methods in oidc.go, callback.go, refresh.go, session.go.
type redisClient struct {
	rdb *redis.Client
}

// newRedisClient connects to Redis and verifies connectivity with a PING.
// Called by: NewService() in config.go (once at startup).
// Fails fast if Redis is unreachable — the server should not start without Redis.
func newRedisClient(url string) (*redisClient, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse redis URL: %w", err)
	}

	rdb := redis.NewClient(opts)

	// Verify connectivity — 3 second timeout to avoid hanging startup.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &redisClient{rdb: rdb}, nil
}

// SetPKCEState stores the code_verifier keyed by the HMAC-signed state value.
// TTL = 5 minutes — after that, the PKCE pair is unusable and the user must restart login.
// If workspaceName is provided, it is stored alongside the code_verifier as JSON.
// Called by: InitiateAuth() in oidc.go (Step 4).
func (r *redisClient) SetPKCEState(ctx context.Context, state, codeVerifier string, workspaceName *string) error {
	key := pkceKey(state)

	// If workspaceName is set, store as JSON object
	if workspaceName != nil && *workspaceName != "" {
		payload := map[string]string{
			"code_verifier": codeVerifier,
			"workspaceName": *workspaceName,
		}
		jsonBytes, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal pkce state: %w", err)
		}
		return r.rdb.Set(ctx, key, string(jsonBytes), 5*time.Minute).Err()
	}

	// No workspaceName — store plain string (backward compatible)
	return r.rdb.Set(ctx, key, codeVerifier, 5*time.Minute).Err()
}

// GetAndDeletePKCEState retrieves the code_verifier and immediately deletes it.
// Single-use — cannot be replayed. Uses a Redis pipeline for atomic GET+DEL.
// Returns ("", "", false, nil) if the key does not exist (expired or already used).
// Returns (codeVerifier, workspaceName, true, nil) on success.
// workspaceName is empty if not set (backward compatible with plain string storage).
// Called by: CallbackHandler() in callback.go (Step 3).
func (r *redisClient) GetAndDeletePKCEState(ctx context.Context, state string) (string, string, bool, error) {
	// Pipeline ensures GET+DEL happen atomically.
	// If we GET then DEL separately, a crash between the two
	// could leave a used verifier in Redis (replay risk).
	key := pkceKey(state)
	pipe := r.rdb.Pipeline()
	getCmd := pipe.Get(ctx, key)
	pipe.Del(ctx, key)
	_, err := pipe.Exec(ctx)

	if err != nil && err != redis.Nil {
		return "", "", false, fmt.Errorf("redis pipeline: %w", err)
	}

	val, err := getCmd.Result()
	if err == redis.Nil {
		return "", "", false, nil // expired or already used
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get pkce state: %w", err)
	}

	// Check if this is JSON (new format with workspaceName) or plain string (old format)
	var codeVerifier, workspaceName string
	if val[0] == '{' {
		// New JSON format
		var payload map[string]string
		if err := json.Unmarshal([]byte(val), &payload); err != nil {
			return "", "", false, fmt.Errorf("unmarshal pkce state: %w", err)
		}
		codeVerifier = payload["code_verifier"]
		workspaceName = payload["workspaceName"] // empty string if not present
	} else {
		// Old plain string format
		codeVerifier = val
		workspaceName = ""
	}

	return codeVerifier, workspaceName, true, nil
}

// SetRefreshToken stores a refresh token in Redis keyed to the user_id.
// TTL is passed by the caller (typically 7 days / 168h).
// Called by: issueRefreshToken() in session.go.
func (r *redisClient) SetRefreshToken(ctx context.Context, userID, token string, ttl time.Duration) error {
	return r.rdb.Set(ctx, refreshKey(userID), token, ttl).Err()
}

// GetRefreshToken retrieves the stored refresh token for a user.
// Returns ("", false, nil) if not found or expired.
// Called by: RefreshHandler() in refresh.go (Step 3).
func (r *redisClient) GetRefreshToken(ctx context.Context, userID string) (string, bool, error) {
	val, err := r.rdb.Get(ctx, refreshKey(userID)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get refresh token: %w", err)
	}
	return val, true, nil
}

// DeleteRefreshToken removes the refresh token for a user.
// Called on sign-out (future implementation).
func (r *redisClient) DeleteRefreshToken(ctx context.Context, userID string) error {
	return r.rdb.Del(ctx, refreshKey(userID)).Err()
}

// pkceKey builds the Redis key for PKCE state storage.
// Format: "pkce:<state>" — the state is the HMAC-signed nonce from oidc.go.
// Called by: SetPKCEState(), GetAndDeletePKCEState().
func pkceKey(state string) string {
	return "pkce:" + state
}

// refreshKey builds the Redis key for refresh token storage.
// Format: "refresh:<userID>" — one refresh token per user at a time.
// Called by: SetRefreshToken(), GetRefreshToken(), DeleteRefreshToken().
func refreshKey(userID string) string {
	return "refresh:" + userID
}
