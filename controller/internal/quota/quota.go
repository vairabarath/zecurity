// Package quota is the one definition of a tenant's quotas (Sprint 23 S3,
// F12; Decision Record D-47 on top of the plans of D-43).
//
// A tenant's limit for each of the six keys is its plan's value, overlaid by
// a per-tenant override (workspace_quota_overrides, migration 065). A nil
// limit is unlimited; a missing key is an error, never unlimited (fail
// closed). Usage is counted with the D-47 rules (Count).
//
// Every creation path calls Reserve (or ReserveUser) inside the transaction
// that creates the row: it locks the workspace row (SELECT … FOR NO KEY UPDATE), so
// creations in one workspace are serialised and two of them can't both take
// the last slot; it then counts and refuses at the limit with *Exceeded. Any
// lookup error refuses too. Lowering a limit never deletes anything: such a
// tenant is "over" and can't create more.
//
// The package imports only the standard library and pgx (no net/http), so
// internal/providerquery (D-04) and internal/plans share it.
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The six quota keys of D-47, in display order. internal/plans aliases them.
const (
	KeyUsers          = "users"
	KeyResources      = "resources"
	KeyRemoteNetworks = "remote_networks"
	KeyConnectors     = "connectors"
	KeyShields        = "shields"
	KeyClientDevices  = "client_devices"
)

// Keys lists every quota key.
var Keys = []string{KeyUsers, KeyResources, KeyRemoteNetworks, KeyConnectors, KeyShields, KeyClientDevices}

// ValidKey reports whether k is a quota key.
func ValidKey(k string) bool {
	for _, key := range Keys {
		if k == key {
			return true
		}
	}
	return false
}

// Code is the machine-readable error code of a refusal at the limit.
const Code = "QUOTA_EXCEEDED"

// Sources of a tenant's limit for a key.
const (
	SourcePlan      = "plan"      // the plan's value (a number)
	SourceOverride  = "override"  // a per-tenant override (a number or unlimited)
	SourceUnlimited = "unlimited" // the plan says unlimited and there is no override
)

var (
	// ErrWorkspaceNotFound: no such workspace (callers refuse).
	ErrWorkspaceNotFound = errors.New("quota: workspace not found")
	// ErrIncompleteLimits: the plan lacks a key (migration 062 prevents it;
	// callers refuse if it ever happens).
	ErrIncompleteLimits = errors.New("quota: plan limits incomplete")
	// ErrUnknownKey: not one of Keys.
	ErrUnknownKey = errors.New("quota: unknown key")
)

// Exceeded is a creation refused at the limit (D-47: quota_exceeded
// {key, limit, used}). Its message is safe to show to tenant users.
type Exceeded struct {
	Key   string
	Limit int64
	Used  int64
}

func (e *Exceeded) Error() string {
	return fmt.Sprintf("quota exceeded: %s limit is %d (in use: %d); remove some or ask your provider to raise the limit",
		strings.ReplaceAll(e.Key, "_", " "), e.Limit, e.Used)
}

// HTTPStatus is the HTTP status of a REST refusal at the limit (409: the
// request is valid but conflicts with the tenant's current usage).
const HTTPStatus = 409

// Body is the REST JSON body of a refusal (D-47: quota_exceeded {key,
// limit, used}); "message" is the user-safe text.
func (e *Exceeded) Body() map[string]any {
	return map[string]any{"error": "quota_exceeded", "code": Code, "key": e.Key, "limit": e.Limit, "used": e.Used, "message": e.Error()}
}

// AsExceeded returns the *Exceeded in err's chain, if any.
func AsExceeded(err error) (*Exceeded, bool) {
	var e *Exceeded
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Limit is one key's effective limit and where it comes from.
type Limit struct {
	Value  *int64 // nil = unlimited
	Source string
}

// limitsColumns selects, for a workspaces row aliased w joined to its plan
// aliased p, the plan's limits and the workspace's overrides as a JSON array
// of {key, limit}. Both Effective and AllUsage read limits through it, so a
// creation check and the usage the provider sees resolve limits identically.
const limitsColumns = `p.limits,
       COALESCE((SELECT jsonb_agg(jsonb_build_object('key', o.key, 'limit', o.limit_value))
                   FROM workspace_quota_overrides o WHERE o.workspace_id = w.id), '[]'::jsonb)`

const limitsFrom = ` FROM workspaces w JOIN plans p ON p.id = w.plan_id WHERE w.id = $1::uuid`

// resolve turns the plan's limits and the overrides into effective limits.
// A missing or negative plan key fails closed.
func resolve(planRaw, overridesRaw []byte) (map[string]Limit, error) {
	var plan map[string]*int64
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		return nil, fmt.Errorf("quota: decode plan limits: %w", err)
	}
	out := make(map[string]Limit, len(Keys))
	for _, k := range Keys {
		v, ok := plan[k]
		if !ok || (v != nil && *v < 0) {
			return nil, ErrIncompleteLimits
		}
		if v == nil {
			out[k] = Limit{Source: SourceUnlimited}
		} else {
			out[k] = Limit{Value: v, Source: SourcePlan}
		}
	}
	var overrides []struct {
		Key   string `json:"key"`
		Limit *int64 `json:"limit"`
	}
	if err := json.Unmarshal(overridesRaw, &overrides); err != nil {
		return nil, fmt.Errorf("quota: decode overrides: %w", err)
	}
	for _, o := range overrides {
		if !ValidKey(o.Key) {
			return nil, fmt.Errorf("quota: override for %q: %w", o.Key, ErrUnknownKey)
		}
		out[o.Key] = Limit{Value: o.Limit, Source: SourceOverride}
	}
	return out, nil
}

// Effective returns the workspace's effective limit for every key: the plan's
// values overlaid with its overrides (one statement). Run it inside the
// creating transaction so the check is atomic with the creation.
func Effective(ctx context.Context, q Querier, workspaceID string) (map[string]Limit, error) {
	var planRaw, overridesRaw []byte
	err := q.QueryRow(ctx, `SELECT `+limitsColumns+limitsFrom, workspaceID).Scan(&planRaw, &overridesRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkspaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("quota: read limits: %w", err)
	}
	return resolve(planRaw, overridesRaw)
}

// Values returns only the limit values of Effective (nil = unlimited).
func Values(l map[string]Limit) map[string]*int64 {
	out := make(map[string]*int64, len(l))
	for k, v := range l {
		out[k] = v.Value
	}
	return out
}

// liveUserSQL / pendingInviteSQL are the two parts of the D-47 user count.
// Every user row that isn't removed counts (active, locked and SCIM-
// suspended users; status 'deleted' doesn't), each on its own: two accounts
// with the same email are two users. A pending, unexpired invitation counts
// once per email, and only while no such user has that email (the person is
// then counted by the user row, D-47: an invited member counts once).
// Expired or accepted invitations don't count. $1 is the workspace id.
const liveUserSQL = `SELECT 1 FROM users u WHERE u.tenant_id = $1::uuid AND u.status <> 'deleted'`

const pendingInviteSQL = `SELECT DISTINCT lower(i.email) AS email FROM invitations i
 WHERE i.workspace_id = $1::uuid AND i.status = 'pending' AND i.expires_at > NOW()
   AND NOT EXISTS (SELECT 1 FROM users u WHERE u.tenant_id = $1::uuid AND u.status <> 'deleted'
                                         AND lower(u.email) = lower(i.email))`

// countSQL counts a key's usage with the D-47 rules. $1 is the workspace id.
var countSQL = map[string]string{
	KeyUsers: `SELECT (SELECT count(*) FROM (` + liveUserSQL + `) u) + (SELECT count(*) FROM (` + pendingInviteSQL + `) i)`,
	// Every defined resource in any state except deleting (un-protecting
	// doesn't free a slot). Discovered services are other tables and never
	// count.
	KeyResources: `SELECT count(*) FROM resources WHERE tenant_id = $1::uuid AND status <> 'deleting'`,
	// Every provisioned network, available or not; deleted ones don't count.
	KeyRemoteNetworks: `SELECT count(*) FROM remote_networks WHERE tenant_id = $1::uuid AND status <> 'deleted'`,
	// Pending (token issued), active and inactive agents; revoked ones don't.
	KeyConnectors: `SELECT count(*) FROM connectors WHERE tenant_id = $1::uuid AND status <> 'revoked' AND revoked_at IS NULL`,
	KeyShields:    `SELECT count(*) FROM shields WHERE tenant_id = $1::uuid AND status <> 'revoked'`,
	// Enrolled, not revoked.
	KeyClientDevices: `SELECT count(*) FROM client_devices WHERE workspace_id = $1::uuid AND revoked_at IS NULL`,
}

// Count returns the workspace's usage of key (D-47 counting rules).
func Count(ctx context.Context, q Querier, workspaceID, key string) (int64, error) {
	sql, ok := countSQL[key]
	if !ok {
		return 0, ErrUnknownKey
	}
	var n int64
	if err := q.QueryRow(ctx, sql, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("quota: count %s: %w", key, err)
	}
	return n, nil
}

// Usage is one key's limit, source, usage and whether the tenant is over it
// (a lowered limit: more in use than allowed).
type Usage struct {
	Key    string
	Limit  *int64
	Source string
	Used   int64
	Over   bool
}

// AllUsage returns every key's limit and usage, in Keys order, in one
// statement (one snapshot): the limits as Effective resolves them and the
// counts as Count makes them.
func AllUsage(ctx context.Context, q Querier, workspaceID string) ([]Usage, error) {
	sql := `SELECT ` + limitsColumns
	for _, k := range Keys {
		sql += `,
       (` + countSQL[k] + `)`
	}
	sql += limitsFrom
	var planRaw, overridesRaw []byte
	used := make([]int64, len(Keys))
	dest := []any{&planRaw, &overridesRaw}
	for i := range used {
		dest = append(dest, &used[i])
	}
	err := q.QueryRow(ctx, sql, workspaceID).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrWorkspaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("quota: read usage: %w", err)
	}
	limits, err := resolve(planRaw, overridesRaw)
	if err != nil {
		return nil, err
	}
	out := make([]Usage, 0, len(Keys))
	for i, k := range Keys {
		l := limits[k]
		out = append(out, Usage{Key: k, Limit: l.Value, Source: l.Source, Used: used[i], Over: l.Value != nil && used[i] > *l.Value})
	}
	return out, nil
}

// lock serialises creations in the workspace until the transaction ends.
// FOR NO KEY UPDATE, not FOR UPDATE (PR #224 review): it still serialises
// quota transactions with each other and with the FOR UPDATE / UPDATE of
// plan assignment, overrides and suspension, but not with the FOR KEY SHARE
// lock every foreign-key insert referencing the workspace takes (audit,
// outbox, discovery rows). Under FOR UPDATE those inserts all waited, and
// SCIM Deprovision vs Reactivate on one user could deadlock.
func lock(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM workspaces WHERE id = $1::uuid FOR NO KEY UPDATE`, workspaceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkspaceNotFound
	}
	if err != nil {
		return fmt.Errorf("quota: lock workspace: %w", err)
	}
	return nil
}

// check refuses with *Exceeded when one more of key would pass the limit.
func check(ctx context.Context, tx pgx.Tx, workspaceID, key string) error {
	limits, err := Effective(ctx, tx, workspaceID)
	if err != nil {
		return err
	}
	l := limits[key]
	if l.Value == nil {
		return nil
	}
	used, err := Count(ctx, tx, workspaceID, key)
	if err != nil {
		return err
	}
	if used >= *l.Value {
		return &Exceeded{Key: key, Limit: *l.Value, Used: used}
	}
	return nil
}

// Reserve is called inside the transaction that creates one more of key
// (not users: see ReserveUser), before the insert. It locks the workspace
// row, so a concurrent creation waits for this transaction and then counts
// its row. nil: the creation may proceed. *Exceeded: at the limit. Any other
// error: refuse (fail closed).
func Reserve(ctx context.Context, tx pgx.Tx, workspaceID, key string) error {
	if key == KeyUsers {
		return fmt.Errorf("quota: users go through ReserveUser or ReserveInvitation")
	}
	if !ValidKey(key) {
		return ErrUnknownKey
	}
	if err := lock(ctx, tx, workspaceID); err != nil {
		return err
	}
	return check(ctx, tx, workspaceID, key)
}

// ReserveUser is Reserve for the users key when a user row is created (sign-
// in, SCIM provisioning, first signup) or a removed user is reactivated.
// It takes no new slot only when it uses the person's own pending
// invitation: an unexpired pending invitation for email exists and no live
// user has that email yet (the invitation's slot becomes the user's). So an
// invited join works even over a lowered limit, but a second account with an
// email already in use takes a slot like any other (the slot is the user
// row, never the email alone).
func ReserveUser(ctx context.Context, tx pgx.Tx, workspaceID, email string) error {
	if err := lock(ctx, tx, workspaceID); err != nil {
		return err
	}
	if identity := strings.ToLower(strings.TrimSpace(email)); identity != "" {
		var invited bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM (`+pendingInviteSQL+`) p WHERE p.email = $2)`,
			workspaceID, identity).Scan(&invited); err != nil {
			return fmt.Errorf("quota: look up invitation: %w", err)
		}
		if invited {
			return nil
		}
	}
	return check(ctx, tx, workspaceID, KeyUsers)
}

// ReserveMappingProbe is called instead of ReserveUser when the SCIM mapping
// probe creates its synthetic probe user (Sprint 24 X1, D-47 refinement
// 2026-11-20): the probe takes no users slot, so SCIM can be enabled on a
// workspace at its user limit. It still takes the workspace lock, so the
// probe is serialised with every other user creation. The probe user is
// created suspended (no login, device enrollment or network access) and is
// removed in the same flow; the caller guarantees one probe per workspace
// at a time. Never call this for a real user.
func ReserveMappingProbe(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	return lock(ctx, tx, workspaceID)
}

// ReserveInvitation is Reserve for the users key when an invitation is
// created or accepted (no user row is created). A person the tenant already
// counts (a live user or a pending invitation with that email) takes no new
// slot: re-inviting a member or accepting an invitation never fails, even
// over a lowered limit. Anyone else takes a slot, so pending invitations
// count and mass-inviting can't get past the limit.
func ReserveInvitation(ctx context.Context, tx pgx.Tx, workspaceID, email string) error {
	if err := lock(ctx, tx, workspaceID); err != nil {
		return err
	}
	if identity := strings.ToLower(strings.TrimSpace(email)); identity != "" {
		var counted bool
		if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM users u WHERE u.tenant_id = $1::uuid AND u.status <> 'deleted' AND lower(u.email) = $2)
			OR EXISTS (SELECT 1 FROM (`+pendingInviteSQL+`) p WHERE p.email = $2)`,
			workspaceID, identity).Scan(&counted); err != nil {
			return fmt.Errorf("quota: look up identity: %w", err)
		}
		if counted {
			return nil
		}
	}
	return check(ctx, tx, workspaceID, KeyUsers)
}

// Override is a change to one key's per-tenant override (provider API,
// tenant.quota.set). Inherit removes the override (the plan's value applies
// again); otherwise Limit is set (nil = explicitly unlimited).
type Override struct {
	Inherit bool
	Limit   *int64
}

// Beginner starts a transaction (*pgxpool.Pool).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrInvalidLimit: a negative limit.
var ErrInvalidLimit = errors.New("quota: invalid limit")

// SetOverride applies o to (workspace, key) in one transaction with the
// workspace row locked (so it orders with creations) and returns the key's
// effective limit before and after. inTx runs in the same transaction (the
// audit row); its error rolls everything back. changed=false: nothing
// differed (no write, inTx not called). Lowering a limit never deletes
// anything (D-47).
func SetOverride(ctx context.Context, db Beginner, workspaceID, key string, o Override, reason, setBy string,
	inTx func(ctx context.Context, tx pgx.Tx, before, after Limit) error) (changed bool, err error) {
	if !ValidKey(key) {
		return false, ErrUnknownKey
	}
	if !o.Inherit && o.Limit != nil && *o.Limit < 0 {
		return false, ErrInvalidLimit
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("quota: begin set override: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lock(ctx, tx, workspaceID); err != nil {
		return false, err
	}
	beforeAll, err := Effective(ctx, tx, workspaceID)
	if err != nil {
		return false, err
	}
	before := beforeAll[key]
	if o.Inherit {
		tag, err := tx.Exec(ctx,
			`DELETE FROM workspace_quota_overrides WHERE workspace_id = $1::uuid AND key = $2`, workspaceID, key)
		if err != nil {
			return false, fmt.Errorf("quota: clear override: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return false, nil
		}
	} else {
		if before.Source == SourceOverride && sameLimit(before.Value, o.Limit) {
			return false, nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by, set_at)
			VALUES ($1::uuid, $2, $3, $4, $5, NOW())
			ON CONFLICT (workspace_id, key) DO UPDATE
			   SET limit_value = EXCLUDED.limit_value, reason = EXCLUDED.reason,
			       set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at`,
			workspaceID, key, o.Limit, reason, setBy); err != nil {
			return false, fmt.Errorf("quota: set override: %w", err)
		}
	}
	afterAll, err := Effective(ctx, tx, workspaceID)
	if err != nil {
		return false, err
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, afterAll[key]); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("quota: commit set override: %w", err)
	}
	return true, nil
}

func sameLimit(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Audit is the before/after shape of a limit in the provider audit (F17).
func (l Limit) Audit() map[string]any {
	return map[string]any{"limit": l.Value, "source": l.Source}
}
