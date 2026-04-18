package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
)

type valkeyClient struct {
	rdb valkeycompat.Cmdable
}

func newValkeyClient(url string) (*valkeyClient, error) {
	addr, err := parseValkeyAddr(url)
	if err != nil {
		return nil, fmt.Errorf("parse valkey URL: %w", err)
	}

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		return nil, fmt.Errorf("create valkey client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
		return nil, fmt.Errorf("ping valkey: %w", err)
	}

	rdb := valkeycompat.NewAdapter(client)
	return &valkeyClient{rdb: rdb}, nil
}

func parseValkeyAddr(rawURL string) (string, error) {
	after, found := strings.CutPrefix(rawURL, "redis://")
	if !found {
		return "", fmt.Errorf("expected redis:// URL, got: %s", rawURL)
	}
	if idx := strings.LastIndex(after, "@"); idx != -1 {
		after = after[idx+1:]
	}
	return after, nil
}

func (r *valkeyClient) SetPKCEState(ctx context.Context, state, codeVerifier string, workspaceName *string) error {
	key := pkceKey(state)

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

	return r.rdb.Set(ctx, key, codeVerifier, 5*time.Minute).Err()
}

func (r *valkeyClient) GetAndDeletePKCEState(ctx context.Context, state string) (string, string, bool, error) {
	val, err := r.rdb.GetDel(ctx, pkceKey(state)).Result()
	if err == valkeycompat.Nil {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get pkce state: %w", err)
	}

	var codeVerifier, workspaceName string
	if val[0] == '{' {
		var payload map[string]string
		if err := json.Unmarshal([]byte(val), &payload); err != nil {
			return "", "", false, fmt.Errorf("unmarshal pkce state: %w", err)
		}
		codeVerifier = payload["code_verifier"]
		workspaceName = payload["workspaceName"]
	} else {
		codeVerifier = val
		workspaceName = ""
	}

	return codeVerifier, workspaceName, true, nil
}

func (r *valkeyClient) SetRefreshToken(ctx context.Context, userID, token string, ttl time.Duration) error {
	return r.rdb.Set(ctx, refreshKey(userID), token, ttl).Err()
}

func (r *valkeyClient) GetRefreshToken(ctx context.Context, userID string) (string, bool, error) {
	val, err := r.rdb.Get(ctx, refreshKey(userID)).Result()
	if err == valkeycompat.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get refresh token: %w", err)
	}
	return val, true, nil
}

func (r *valkeyClient) DeleteRefreshToken(ctx context.Context, userID string) error {
	return r.rdb.Del(ctx, refreshKey(userID)).Err()
}

func pkceKey(state string) string {
	return "pkce:" + state
}

func refreshKey(userID string) string {
	return "refresh:" + userID
}
