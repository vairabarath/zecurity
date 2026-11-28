package provider

// Provider API keys (Sprint 27 K1, F35; Decision Record D-76).
//
// A key is zpk_<id without dashes>_<base64url of 32 random bytes>. Only the
// SHA-256 of the whole key is stored and compared in constant time; the key
// itself is returned once, by the create call. A key has one role (never
// super-admin), is read-only (decide() refuses every write for an api_key
// actor), expires within 365 days, may be limited to source CIDRs, and is
// rate limited per key (process-local, D-02; fails closed).
//
// A key is never a session: nothing here calls IssueSession or mints a JWT,
// and RequireProvider refuses keys on the auth, TOTP, operator and key routes.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	apiKeyScheme = "zpk_"
	// APIKeyPrefixLen is how much of the key lists show ("zpk_" + 8 hex).
	APIKeyPrefixLen = 12
	// APIKeyMaxLifetime: every key expires within 365 days (D-76, Q3).
	APIKeyMaxLifetime = 365 * 24 * time.Hour
	// APIKeyMaxCIDRs bounds the optional source allowlist.
	APIKeyMaxCIDRs = 20
	// apiKeyTouchEvery: last_used_at / last_used_ip are written at most this
	// often per key.
	apiKeyTouchEvery = time.Minute

	// Audit actions of the key routes (the permissions are ActionAPIKey*).
	AuditAPIKeyCreate = ActionAPIKeyCreate
	AuditAPIKeyRevoke = ActionAPIKeyRevoke
)

// APIKeyRoles are the roles a key may have: every role except super-admin.
var APIKeyRoles = []string{RoleRelayOps, RoleAuditor, RoleBilling, RoleSupport}

// ValidAPIKeyRole reports whether role may be given to a key.
func ValidAPIKeyRole(role string) bool {
	for _, r := range APIKeyRoles {
		if r == role {
			return true
		}
	}
	return false
}

var (
	// ErrInvalidAPIKey covers every authentication failure (malformed,
	// unknown, wrong secret, revoked, expired, outside the CIDR allowlist):
	// the caller never learns which check failed.
	ErrInvalidAPIKey = errors.New("invalid api key")
	// ErrAPIKeyRateLimited: over the per-key rate, or the limiter failed
	// (fail closed).
	ErrAPIKeyRateLimited = errors.New("api key rate limited")

	ErrAPIKeyNotFound      = errors.New("api key not found")
	ErrAPIKeyRevoked       = errors.New("api key already revoked")
	ErrAPIKeyNameTaken     = errors.New("api key name taken")
	ErrAPIKeyInvalidRole   = errors.New("api key role not allowed")
	ErrAPIKeyInvalidExpiry = errors.New("api key expiry out of range")
	ErrAPIKeyInvalidName   = errors.New("invalid api key name")
	ErrAPIKeyInvalidCIDR   = errors.New("invalid api key cidr")
	ErrAPIKeyNameMismatch  = errors.New("confirmation does not match the key name")
)

// IsAPIKeyToken reports whether a bearer token is an API key (by its
// scheme), so RequireProvider sends it to key authentication and never to
// the JWT verifier.
func IsAPIKeyToken(token string) bool { return strings.HasPrefix(token, apiKeyScheme) }

// generateAPIKey returns a new key for id, its list prefix and its SHA-256.
func generateAPIKey(id string) (key, prefix string, sum [32]byte, err error) {
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", sum, fmt.Errorf("api key secret: %w", err)
	}
	key = apiKeyScheme + strings.ReplaceAll(id, "-", "") + "_" + base64.RawURLEncoding.EncodeToString(secret[:])
	return key, key[:APIKeyPrefixLen], sha256.Sum256([]byte(key)), nil
}

// parseAPIKey splits a key into its id (with dashes). It checks the shape
// only; the secret is checked by hash.
func parseAPIKey(key string) (id string, ok bool) {
	rest, found := strings.CutPrefix(key, apiKeyScheme)
	if !found {
		return "", false
	}
	hexID, secret, found := strings.Cut(rest, "_")
	if !found || len(hexID) != 32 || len(secret) != 43 {
		return "", false
	}
	if _, err := hex.DecodeString(hexID); err != nil || strings.ToLower(hexID) != hexID {
		return "", false
	}
	if b, err := base64.RawURLEncoding.DecodeString(secret); err != nil || len(b) != 32 {
		return "", false
	}
	return hexID[0:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:32], true
}

// APIKey is a key as lists show it: never the secret or its hash.
type APIKey struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Role         string     `json:"role"`
	Prefix       string     `json:"prefix"`
	Status       string     `json:"status"` // active | expired | revoked
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	LastUsedIP   *string    `json:"last_used_ip"`
	AllowedCIDRs []string   `json:"allowed_cidrs"`
	RevokedAt    *time.Time `json:"revoked_at"`
	RevokedBy    *string    `json:"revoked_by"`
	RevokeReason *string    `json:"revoke_reason"`
}

const apiKeySelect = `
SELECT k.id::text, k.name, k.role, k.prefix,
       CASE WHEN k.revoked_at IS NOT NULL THEN 'revoked'
            WHEN k.expires_at <= NOW() THEN 'expired' ELSE 'active' END,
       cu.email, k.created_at, k.expires_at, k.last_used_at, k.last_used_ip,
       ARRAY(SELECT c::text FROM unnest(k.allowed_cidrs) AS c),
       k.revoked_at, ru.email, k.revoke_reason
  FROM provider_api_keys k
  JOIN provider_users cu ON cu.id = k.created_by
  LEFT JOIN provider_users ru ON ru.id = k.revoked_by`

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	if err := row.Scan(&k.ID, &k.Name, &k.Role, &k.Prefix, &k.Status, &k.CreatedBy, &k.CreatedAt, &k.ExpiresAt,
		&k.LastUsedAt, &k.LastUsedIP, &k.AllowedCIDRs, &k.RevokedAt, &k.RevokedBy, &k.RevokeReason); err != nil {
		return nil, err
	}
	if k.AllowedCIDRs == nil {
		k.AllowedCIDRs = []string{}
	}
	return &k, nil
}

// ListAPIKeys returns every key, newest first (revoked ones included).
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, apiKeySelect+` ORDER BY k.created_at DESC, k.id`)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// NewAPIKey is a validated create request.
type NewAPIKey struct {
	Name         string
	Role         string
	ExpiresAt    time.Time
	AllowedCIDRs []string // canonical prefixes
	Reason       string
}

// ValidateNewAPIKey normalizes and checks a create request at now: name 1–64
// printable characters, a key role (never super-admin), expiry after now and
// at most 365 days ahead, at most APIKeyMaxCIDRs valid CIDRs.
func ValidateNewAPIKey(name, role string, expiresAt, now time.Time, cidrs []string) (NewAPIKey, error) {
	name = strings.TrimSpace(name)
	if n := len([]rune(name)); n < 1 || n > 64 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return NewAPIKey{}, ErrAPIKeyInvalidName
	}
	if !ValidAPIKeyRole(role) {
		return NewAPIKey{}, ErrAPIKeyInvalidRole
	}
	// One minute of slack below the DB's created_at + 365 days.
	if !expiresAt.After(now) || expiresAt.After(now.Add(APIKeyMaxLifetime-time.Minute)) {
		return NewAPIKey{}, ErrAPIKeyInvalidExpiry
	}
	if len(cidrs) > APIKeyMaxCIDRs {
		return NewAPIKey{}, ErrAPIKeyInvalidCIDR
	}
	out := NewAPIKey{Name: name, Role: role, ExpiresAt: expiresAt.UTC(), AllowedCIDRs: []string{}}
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			return NewAPIKey{}, ErrAPIKeyInvalidCIDR
		}
		out.AllowedCIDRs = append(out.AllowedCIDRs, p.Masked().String())
	}
	return out, nil
}

// CreateAPIKey stores a new key and its audit row (reason, after; never the
// secret or hash) in one transaction, and returns the key once.
func (s *Store) CreateAPIKey(ctx context.Context, actor AuditActor, in NewAPIKey) (*APIKey, string, error) {
	var created *APIKey
	var full string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
			return fmt.Errorf("api key id: %w", err)
		}
		key, prefix, sum, err := generateAPIKey(id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO provider_api_keys (id, name, role, prefix, secret_sha256, created_by, expires_at, allowed_cidrs)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8::cidr[])`,
			id, in.Name, in.Role, prefix, sum[:], actor.UserID, in.ExpiresAt, in.AllowedCIDRs); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrAPIKeyNameTaken
			}
			return fmt.Errorf("insert api key: %w", err)
		}
		k, err := scanAPIKey(tx.QueryRow(ctx, apiKeySelect+` WHERE k.id = $1`, id))
		if err != nil {
			return fmt.Errorf("read api key: %w", err)
		}
		uid := actor.UserID
		if err := s.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderUserID: &uid, ProviderEmail: actor.Email, IPAddress: actor.IP,
			Action: AuditAPIKeyCreate, TargetType: "api_key", TargetID: id,
			Details: WithChange(map[string]any{"reason": in.Reason}, nil, map[string]any{
				"name": k.Name, "role": k.Role, "prefix": k.Prefix,
				"expires_at": k.ExpiresAt.UTC().Format(time.RFC3339), "allowed_cidrs": k.AllowedCIDRs,
			}),
		}); err != nil {
			return err
		}
		created, full = k, key
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return created, full, nil
}

// RevokeAPIKey revokes a key (terminal) with its audit row; confirmName must
// equal the key's name (D-17 style typed confirmation).
func (s *Store) RevokeAPIKey(ctx context.Context, actor AuditActor, id, reason, confirmName string) (*APIKey, error) {
	var out *APIKey
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := scanAPIKey(tx.QueryRow(ctx, apiKeySelect+` WHERE k.id = $1 FOR UPDATE OF k`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAPIKeyNotFound
		}
		if err != nil {
			return fmt.Errorf("lock api key: %w", err)
		}
		if before.Status == "revoked" {
			return ErrAPIKeyRevoked
		}
		if strings.TrimSpace(confirmName) != before.Name {
			return ErrAPIKeyNameMismatch
		}
		if _, err := tx.Exec(ctx, `UPDATE provider_api_keys SET revoked_at = NOW(), revoked_by = $2, revoke_reason = $3 WHERE id = $1`,
			id, actor.UserID, reason); err != nil {
			return fmt.Errorf("revoke api key: %w", err)
		}
		after, err := scanAPIKey(tx.QueryRow(ctx, apiKeySelect+` WHERE k.id = $1`, id))
		if err != nil {
			return fmt.Errorf("read api key: %w", err)
		}
		uid := actor.UserID
		if err := s.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderUserID: &uid, ProviderEmail: actor.Email, IPAddress: actor.IP,
			Action: AuditAPIKeyRevoke, TargetType: "api_key", TargetID: id,
			Details: WithChange(map[string]any{"reason": reason, "name": before.Name, "prefix": before.Prefix},
				map[string]any{"status": before.Status}, map[string]any{"status": "revoked"}),
		}); err != nil {
			return err
		}
		out = after
		return nil
	})
	return out, err
}

// Reasons recorded on keys revoked because their creator lost super-admin
// (D-78).
const (
	RevokeReasonCreatorDisabled = "creator disabled"
	RevokeReasonCreatorDemoted  = "creator demoted"
)

// revokeCreatorKeysTx (D-78, Sprint 27 X6) permanently revokes every key
// creatorID created that isn't revoked yet, inside the caller's transaction
// (the operator disable or role change), so the operator change and the
// revocations commit or fail together. Each key gets the acting super-admin
// as revoked_by, the system reason and its own api_key.revoke audit row (name
// and prefix, never the secret). Revocation is terminal: re-enabling or
// re-promoting the operator restores nothing. Returns how many keys were
// revoked.
func (s *Store) revokeCreatorKeysTx(ctx context.Context, tx pgx.Tx, actor AuditActor, creatorID, reason, cause string) (int, error) {
	rows, err := tx.Query(ctx,
		`UPDATE provider_api_keys SET revoked_at = NOW(), revoked_by = $2, revoke_reason = $3
		  WHERE created_by = $1 AND revoked_at IS NULL
		  RETURNING id::text, name, prefix`, creatorID, actor.UserID, reason)
	if err != nil {
		return 0, fmt.Errorf("revoke creator api keys: %w", err)
	}
	type revoked struct{ id, name, prefix string }
	var keys []revoked
	for rows.Next() {
		var k revoked
		if err := rows.Scan(&k.id, &k.name, &k.prefix); err != nil {
			rows.Close()
			return 0, fmt.Errorf("revoked api key: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("revoke creator api keys: %w", err)
	}
	for _, k := range keys {
		uid := actor.UserID
		if err := s.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderUserID: &uid, ProviderEmail: actor.Email, IPAddress: actor.IP,
			Action: AuditAPIKeyRevoke, TargetType: "api_key", TargetID: k.id,
			Details: WithChange(map[string]any{"reason": reason, "name": k.name, "prefix": k.prefix, "cause": cause, "creator_id": creatorID},
				map[string]any{"status": "active"}, map[string]any{"status": "revoked"}),
		}); err != nil {
			return 0, err
		}
	}
	return len(keys), nil
}

// apiKeyAuthRow is what authentication needs from a key row.
type apiKeyAuthRow struct {
	id, name, role, prefix string
	sum                    []byte
	expiresAt              time.Time
	revoked                bool
	cidrs                  []string
}

func (s *Store) apiKeyForAuth(ctx context.Context, id string) (*apiKeyAuthRow, error) {
	var r apiKeyAuthRow
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, role, prefix, secret_sha256, expires_at, revoked_at IS NOT NULL,
		       ARRAY(SELECT c::text FROM unnest(allowed_cidrs) AS c)
		  FROM provider_api_keys WHERE id = $1`, id).
		Scan(&r.id, &r.name, &r.role, &r.prefix, &r.sum, &r.expiresAt, &r.revoked, &r.cidrs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) touchAPIKey(ctx context.Context, id, ip string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE provider_api_keys SET last_used_at = NOW(), last_used_ip = $2 WHERE id = $1 AND revoked_at IS NULL`, id, nullIfEmpty(ip))
	return err
}

// APIKeyLimiter rate-limits requests per key. An error refuses the request
// (fail closed).
type APIKeyLimiter interface {
	Allow(keyID string, now time.Time) (bool, error)
}

// TokenBucketLimiter is the process-local per-key limiter (D-02: one
// controller instance).
type TokenBucketLimiter struct {
	Rate  float64 // tokens per second
	Burst float64

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewTokenBucketLimiter returns a limiter of rate requests per second with
// the given burst.
func NewTokenBucketLimiter(rate, burst float64) *TokenBucketLimiter {
	return &TokenBucketLimiter{Rate: rate, Burst: burst, buckets: map[string]*bucket{}}
}

// Allow takes one token for keyID.
func (l *TokenBucketLimiter) Allow(keyID string, now time.Time) (bool, error) {
	if l == nil || l.Rate <= 0 || l.Burst < 1 {
		return false, errors.New("api key limiter not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[keyID]
	if b == nil {
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[keyID] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * l.Rate
		if b.tokens > l.Burst {
			b.tokens = l.Burst
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// APIKeyAuthenticator turns a bearer key into an api_key Actor.
type APIKeyAuthenticator struct {
	Store   *Store
	Limiter APIKeyLimiter
	Now     func() time.Time

	mu        sync.Mutex
	lastTouch map[string]time.Time
}

// NewAPIKeyAuthenticator returns an authenticator with the default limits:
// 10 requests per second per key, bursts of 30.
func NewAPIKeyAuthenticator(store *Store) *APIKeyAuthenticator {
	return &APIKeyAuthenticator{Store: store, Limiter: NewTokenBucketLimiter(10, 30), Now: time.Now}
}

// Authenticate checks key from ip. Every failure of the key itself is
// ErrInvalidAPIKey; over the rate (or a limiter error) is
// ErrAPIKeyRateLimited; anything else is an internal error.
func (a *APIKeyAuthenticator) Authenticate(ctx context.Context, key, ip string) (Actor, error) {
	now := a.Now()
	id, ok := parseAPIKey(key)
	if !ok {
		return Actor{}, ErrInvalidAPIKey
	}
	row, err := a.Store.apiKeyForAuth(ctx, id)
	if errors.Is(err, ErrAPIKeyNotFound) {
		return Actor{}, ErrInvalidAPIKey
	}
	if err != nil {
		return Actor{}, fmt.Errorf("api key lookup: %w", err)
	}
	sum := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare(sum[:], row.sum) != 1 {
		return Actor{}, ErrInvalidAPIKey
	}
	if row.revoked || !now.Before(row.expiresAt) || !ValidAPIKeyRole(row.role) {
		return Actor{}, ErrInvalidAPIKey
	}
	if !ipAllowed(ip, row.cidrs) {
		return Actor{}, ErrInvalidAPIKey
	}
	allowed, err := a.Limiter.Allow(row.id, now)
	if err != nil || !allowed {
		if err != nil {
			log.Printf("provider api key: limiter: %v", err)
		}
		return Actor{}, ErrAPIKeyRateLimited
	}
	a.touch(ctx, row.id, ip, now)
	return Actor{
		Kind:      ActorKindAPIKey,
		Email:     "api_key:" + row.name + " (" + row.prefix + ")",
		Role:      row.role,
		KeyID:     row.id,
		KeyName:   row.name,
		KeyPrefix: row.prefix,
	}, nil
}

// ipAllowed: an empty allowlist allows every source; otherwise the source
// must parse and fall inside one prefix (fail closed).
func ipAllowed(ip string, cidrs []string) bool {
	if len(cidrs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

// touch records last use at most once per apiKeyTouchEvery per key; a
// failed write is logged, never fatal to the request.
func (a *APIKeyAuthenticator) touch(ctx context.Context, id, ip string, now time.Time) {
	a.mu.Lock()
	if a.lastTouch == nil {
		a.lastTouch = map[string]time.Time{}
	}
	if last, ok := a.lastTouch[id]; ok && now.Sub(last) < apiKeyTouchEvery {
		a.mu.Unlock()
		return
	}
	a.lastTouch[id] = now
	a.mu.Unlock()
	if err := a.Store.touchAPIKey(ctx, id, ip); err != nil {
		log.Printf("provider api key: last used: %v", err)
	}
}
