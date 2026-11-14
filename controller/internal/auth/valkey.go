package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/valkeyurl"
)

type valkeyClient struct {
	rdb valkeycompat.Cmdable
}

func newValkeyClient(url string) (*valkeyClient, error) {
	opts, err := valkeyurl.Parse(url)
	if err != nil {
		return nil, fmt.Errorf("parse valkey URL: %w", err)
	}

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{opts.Addr},
		SelectDB:     opts.DB,
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
	// PENDING-06: when this session authenticated (Unix seconds; 0 on
	// sessions stored before this field). Survives refresh rotation. The MFA
	// mark is NOT here: it lives in its own key (sessionMFAKey) so stamping
	// or clearing it never races the rotation's read-modify-write.
	AuthAt int64 `json:"auth_at,omitempty"`
}

// SetRefreshSession stores one login's refresh session under
// refresh:{<userID>}:<sid> and records sid in the user's index set, so a user
// can hold several independent sessions (admin UI tabs, the client daemon)
// without one login or rotation overwriting another. sid "" addresses the
// legacy single-session key refresh:<userID>, which is only ever read and
// deleted now, never written. The index TTL is pushed out on every write, so
// it always outlives the sessions it lists.
func (r *valkeyClient) SetRefreshSession(ctx context.Context, userID, sid string, sess RefreshSession, ttl time.Duration) error {
	payload, err := json.Marshal(sess)
	if err != nil {
		return fmt.Errorf("marshal refresh session: %w", err)
	}
	if sid == "" {
		err = r.rdb.Set(ctx, refreshKey(userID, ""), string(payload), ttl).Err()
	} else {
		_, err = r.rdb.TxPipelined(ctx, func(p valkeycompat.Pipeliner) error {
			p.Set(ctx, refreshKey(userID, sid), string(payload), ttl)
			p.SAdd(ctx, refreshIndexKey(userID), sid)
			p.Expire(ctx, refreshIndexKey(userID), ttl)
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("store refresh session: %w", err)
	}
	return nil
}

func (r *valkeyClient) GetRefreshSession(ctx context.Context, userID, sid string) (RefreshSession, bool, error) {
	val, err := r.rdb.Get(ctx, refreshKey(userID, sid)).Result()
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

// DeleteRefreshSession ends one login's session (logout, max-lifetime cap,
// legacy-key migration). Other sessions of the same user are untouched.
func (r *valkeyClient) DeleteRefreshSession(ctx context.Context, userID, sid string) error {
	if sid == "" {
		return r.rdb.Del(ctx, refreshKey(userID, "")).Err()
	}
	_, err := r.rdb.TxPipelined(ctx, func(p valkeycompat.Pipeliner) error {
		p.Del(ctx, refreshKey(userID, sid))
		p.Del(ctx, sessionMFAKey(userID, sid))
		p.SRem(ctx, refreshIndexKey(userID), sid)
		return nil
	})
	return err
}

// EndRefreshSession is DeleteRefreshSession that also reports whether the
// session existed (logout audits only a session it actually ended).
func (r *valkeyClient) EndRefreshSession(ctx context.Context, userID, sid string) (bool, error) {
	if sid == "" {
		n, err := r.rdb.Del(ctx, refreshKey(userID, "")).Result()
		return n > 0, err
	}
	var del *valkeycompat.IntCmd
	_, err := r.rdb.TxPipelined(ctx, func(p valkeycompat.Pipeliner) error {
		del = p.Del(ctx, refreshKey(userID, sid))
		p.Del(ctx, sessionMFAKey(userID, sid))
		p.SRem(ctx, refreshIndexKey(userID), sid)
		return nil
	})
	if err != nil {
		return false, err
	}
	return del.Val() > 0, nil
}

// DeleteAllRefreshSessions ends every session of the user, including a legacy
// single-session key. Used when the user itself is revoked (status or
// identity_generation), where no session may survive.
func (r *valkeyClient) DeleteAllRefreshSessions(ctx context.Context, userID string) error {
	sids, err := r.rdb.SMembers(ctx, refreshIndexKey(userID)).Result()
	if err != nil && err != valkeycompat.Nil {
		return fmt.Errorf("list refresh sessions: %w", err)
	}
	keys := []string{refreshIndexKey(userID)}
	for _, sid := range sids {
		keys = append(keys, refreshKey(userID, sid), sessionMFAKey(userID, sid))
	}
	if err := r.rdb.Del(ctx, keys...).Err(); err != nil {
		return err
	}
	return r.rdb.Del(ctx, refreshKey(userID, "")).Err()
}

// ── PENDING-06: a session's MFA mark ─────────────────────────────────────────
//
// refresh_mfa:{<userID>}:<sid> holds the Unix second the session last passed
// MFA. It is a separate key from the refresh session on purpose: a single SET
// or DEL cannot lose to (or clobber) a concurrent refresh rotation, which
// reads and rewrites the session record. The key is deleted with its session
// and expires on its own at ttl (the session's remaining maximum lifetime).

// SetSessionMFA records that (userID, sid) passed MFA at `at`.
func (r *valkeyClient) SetSessionMFA(ctx context.Context, userID, sid string, at time.Time, ttl time.Duration) error {
	if err := r.rdb.Set(ctx, sessionMFAKey(userID, sid), at.Unix(), ttl).Err(); err != nil {
		return fmt.Errorf("store session mfa: %w", err)
	}
	return nil
}

// GetSessionMFA returns when (userID, sid) last passed MFA, or 0.
func (r *valkeyClient) GetSessionMFA(ctx context.Context, userID, sid string) (int64, error) {
	v, err := r.rdb.Get(ctx, sessionMFAKey(userID, sid)).Int64()
	if err == valkeycompat.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get session mfa: %w", err)
	}
	return v, nil
}

// ClearAllSessionMFA removes the MFA mark from every session of the user.
func (r *valkeyClient) ClearAllSessionMFA(ctx context.Context, userID string) error {
	sids, err := r.rdb.SMembers(ctx, refreshIndexKey(userID)).Result()
	if err != nil && err != valkeycompat.Nil {
		return fmt.Errorf("list refresh sessions: %w", err)
	}
	if len(sids) == 0 {
		return nil
	}
	keys := make([]string, 0, len(sids))
	for _, sid := range sids {
		keys = append(keys, sessionMFAKey(userID, sid))
	}
	return r.rdb.Del(ctx, keys...).Err()
}

func sessionMFAKey(userID, sid string) string {
	return "refresh_mfa:{" + userID + "}:" + sid
}

func pkceKey(state string) string {
	return "pkce:" + state
}

// refreshKey is refresh:{<userID>}:<sid>, or the legacy refresh:<userID> for
// sid "". sid is validated by the caller (validSessionID) before it gets here.
// The {userID} hash tag keeps a user's session keys and index in one slot, so
// they can be written in one MULTI.
func refreshKey(userID, sid string) string {
	if sid == "" {
		return "refresh:" + userID
	}
	return "refresh:{" + userID + "}:" + sid
}

func refreshIndexKey(userID string) string {
	return "refresh_sids:{" + userID + "}"
}
