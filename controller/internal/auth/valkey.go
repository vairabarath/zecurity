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
		InitAddress:  []string{addr},
		DisableCache: true,
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

// PKCEState is the login scratchpad stored in Redis between the IdP redirect and
// the callback, keyed by `state` and single-use (5-min TTL). CodeVerifier is the
// PKCE verifier; WorkspaceName is set on the signup flow; Nonce and ConnectionID
// support OIDC login — which connection/adapter to verify against, plus id_token
// replay protection (PENDING-04 Phase 4).
type PKCEState struct {
	CodeVerifier  string `json:"code_verifier"`
	WorkspaceName string `json:"workspace_name,omitempty"`
	Nonce         string `json:"nonce,omitempty"`
	ConnectionID  string `json:"connection_id,omitempty"`
}

func (r *valkeyClient) SetPKCEState(ctx context.Context, state string, st PKCEState) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshal pkce state: %w", err)
	}
	return r.rdb.Set(ctx, pkceKey(state), string(payload), 5*time.Minute).Err()
}

func (r *valkeyClient) GetAndDeletePKCEState(ctx context.Context, state string) (PKCEState, bool, error) {
	val, err := r.rdb.GetDel(ctx, pkceKey(state)).Result()
	if err == valkeycompat.Nil {
		return PKCEState{}, false, nil
	}
	if err != nil {
		return PKCEState{}, false, fmt.Errorf("get pkce state: %w", err)
	}

	var st PKCEState
	if err := json.Unmarshal([]byte(val), &st); err != nil {
		return PKCEState{}, false, fmt.Errorf("unmarshal pkce state: %w", err)
	}
	return st, true, nil
}

// RefreshSession is the Redis payload for a user's active refresh token.
//
// Token rotates on every refresh; OriginalIAT and MaxLifetimeAt are preserved
// across rotations to enforce an absolute lifetime cap. See ADR-006.
type RefreshSession struct {
	Token         string `json:"token"`
	OriginalIAT   int64  `json:"original_iat"`    // Unix seconds — initial login time
	MaxLifetimeAt int64  `json:"max_lifetime_at"` // Unix seconds — hard expiry, ignored if 0
}

func (r *valkeyClient) SetRefreshSession(ctx context.Context, userID string, sess RefreshSession, ttl time.Duration) error {
	payload, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("marshal refresh session: %w", err)
	}
	return r.rdb.Set(ctx, refreshKey(userID), string(payload), ttl).Err()
}

func (r *valkeyClient) GetRefreshSession(ctx context.Context, userID string) (RefreshSession, bool, error) {
	val, err := r.rdb.Get(ctx, refreshKey(userID)).Result()
	if err == valkeycompat.Nil {
		return RefreshSession{}, false, nil
	}
	if err != nil {
		return RefreshSession{}, false, fmt.Errorf("get refresh session: %w", err)
	}
	var sess RefreshSession
	if err := json.Unmarshal([]byte(val), &sess); err != nil {
		return RefreshSession{}, false, fmt.Errorf("unmarshal refresh session: %w", err)
	}
	return sess, true, nil
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
