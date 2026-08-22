package scim

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Types ────────────────────────────────────────────────────────────────────

// groupRow is the minimal fields needed from the groups table for SCIM.
type groupRow struct {
	ID            string
	WorkspaceID   string
	Origin        string
	ConnectionID  string
	Name          string
	ExternalID    string
	SyncInstanceID string
	CreatedAt     string
	UpdatedAt     string
}

const (
	OriginSCIM   = "scim"
	OriginManual = "manual"
	OriginSystem = "system"
)

// memberChange is a single SCIM Group membership mutation carrying its own op.
// It is used for the PUT full-replacement path (ReplaceGroup), where every
// value is a "replace" and the set is deduped into one final membership.
type memberChange struct {
	Op    string // "add" | "remove" | "replace"
	Value string // user externalId/subject or user id
}

// patchOp is ONE RFC 7644 PATCH operation carrying ALL of its member values.
// The operation boundary is preserved (a single "replace" with N values is one
// op) so PatchGroup applies it atomically: a replace op resets the working set
// EXACTLY ONCE, then adds every value from that op — rather than treating each
// value as an independent replace that would clobber the previous ones.
type patchOp struct {
	Op     string   // "add" | "remove" | "replace"
	Values []string // user externalId/subject or user id
}

// groupPatch is the normalized, order-preserving list of membership operations
// for a SCIM Group PATCH. Operations execute sequentially in request order.
type groupPatch struct {
	Ops []patchOp
}

// memberFilterRe matches a targeted-removal path of the form
// members[value eq "user-id"] (case-insensitive on the path).
var memberFilterRe = regexp.MustCompile(`^members\[value eq "([^"]*)"\]$`)

// ── DirectoryService group methods ───────────────────────────────────────────

// CreateGroup creates a scim-origin group for the resolved scope. It opens a
// sync instance for the connection (the first SCIM write opens one), stamps the
// group's sync_instance_id for provenance, and stamps the connection's
// last_sync_at so Identity Health stays current.
func (s *DirectoryService) CreateGroup(ctx context.Context, sc *scope, externalID, displayName string) (*groupRow, *SCIMError) {
	if externalID == "" {
		return nil, newSCIMError(400, "invalidValue", "externalId is required for scim groups")
	}
	inst, err := s.EnsureSyncInstance(ctx, sc)
	if err != nil {
		return nil, newSCIMError(500, "", "ensure sync instance: "+err.Error())
	}
	var g groupRow
	err = s.pool.QueryRow(ctx,
		`INSERT INTO groups (workspace_id, origin, connection_id, external_id, name, sync_instance_id)
		 VALUES ($1, 'scim', $2, $3, $4, $5)
		 RETURNING id, workspace_id, name, TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at, TO_CHAR(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at`,
		sc.workspaceID, sc.connectionID, externalID, displayName, nullIfUUID(inst),
	).Scan(&g.ID, &g.WorkspaceID, &g.Name, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, newSCIMError(409, "uniqueness", "group externalId already exists for this connection")
		}
		return nil, newSCIMError(500, "", "create group: "+err.Error())
	}
	g.Origin = OriginSCIM
	g.ConnectionID = sc.connectionID
	g.ExternalID = externalID
	g.SyncInstanceID = inst
	if err := s.touchSyncInstance(ctx, sc, inst); err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	return &g, nil
}

// GetGroup returns a scim group by id or external_id, scoped to the connection.
func (s *DirectoryService) GetGroup(ctx context.Context, sc *scope, idOrExternal string) (*groupRow, *SCIMError) {
	row := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, origin, connection_id, name, external_id, TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at, TO_CHAR(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at
		   FROM groups
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		    AND (id::text = $3 OR external_id = $3)`,
		sc.workspaceID, sc.connectionID, idOrExternal,
	)
	var g groupRow
	err := row.Scan(&g.ID, &g.WorkspaceID, &g.Origin, &g.ConnectionID, &g.Name, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, newSCIMError(404, "", "group not found")
		}
		return nil, newSCIMError(500, "", "get group: "+err.Error())
	}
	if g.Origin == "" {
		g.Origin = OriginSCIM
	}
	return &g, nil
}

// ListGroups returns scim-origin groups for the scope.
func (s *DirectoryService) ListGroups(ctx context.Context, sc *scope) ([]groupRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, origin, connection_id, name, external_id, TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at, TO_CHAR(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at
		   FROM groups
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		  ORDER BY created_at`,
		sc.workspaceID, sc.connectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()
	var out []groupRow
	for rows.Next() {
		var g groupRow
		if err := rows.Scan(&g.ID, &g.WorkspaceID, &g.Origin, &g.ConnectionID, &g.Name, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		if g.Origin == "" {
			g.Origin = OriginSCIM
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// UpdateGroup replaces mutable display-name metadata for a scim group.
func (s *DirectoryService) UpdateGroup(ctx context.Context, sc *scope, idOrExternal, displayName string) (*groupRow, *SCIMError) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	var g groupRow
	err = tx.QueryRow(ctx,
		`UPDATE groups
		    SET name = COALESCE(NULLIF($3, ''), name),
		        updated_at = NOW()
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		    AND (id::text = $4 OR external_id = $4)
		  RETURNING id, workspace_id, origin, connection_id, name, external_id, TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at, TO_CHAR(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at`,
		sc.workspaceID, sc.connectionID, displayName, idOrExternal,
	).Scan(&g.ID, &g.WorkspaceID, &g.Origin, &g.ConnectionID, &g.Name, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, newSCIMError(404, "", "group not found")
		}
		return nil, newSCIMError(500, "", "update group: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, newSCIMError(500, "", "commit update group: "+err.Error())
	}
	if g.Origin == "" {
		g.Origin = OriginSCIM
	}
	return &g, nil
}

// ReplaceGroup performs full-group replacement for PUT semantics:
// displayName + exact membership set, after resolving all member refs.
// members == nil means "do not touch membership" (name-only PUT). An empty
// (non-nil) members slice clears all membership. Unknown members return 404
// with zero mutation.
func (s *DirectoryService) ReplaceGroup(ctx context.Context, sc *scope, idOrExternal, displayName string, members []memberChange) (*groupRow, *SCIMError) {
	if members == nil {
		return s.UpdateGroup(ctx, sc, idOrExternal, displayName)
	}

	target := make(map[string]struct{})
	var unknowns []string
	for _, mc := range members {
		ids, err := s.userIDsByExternalOrUUID(ctx, sc, mc.Value)
		if err != nil {
			return nil, newSCIMError(500, "", "resolve members: "+err.Error())
		}
		if len(ids) == 0 {
			unknowns = append(unknowns, mc.Value)
			continue
		}
		target[ids[0]] = struct{}{}
	}
	if len(unknowns) > 0 {
		return nil, newSCIMError(404, "", fmt.Sprintf("unknown members: %s", strings.Join(unknowns, ", ")))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	var g groupRow
	err = tx.QueryRow(ctx,
		`UPDATE groups
		    SET name = COALESCE(NULLIF($3, ''), name),
		        updated_at = NOW()
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		    AND (id::text = $4 OR external_id = $4)
		  RETURNING id, workspace_id, origin, connection_id, name, external_id, TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at, TO_CHAR(updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS updated_at`,
		sc.workspaceID, sc.connectionID, displayName, idOrExternal,
	).Scan(&g.ID, &g.WorkspaceID, &g.Origin, &g.ConnectionID, &g.Name, &g.ExternalID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, newSCIMError(404, "", "group not found")
		}
		return nil, newSCIMError(500, "", "replace group: "+err.Error())
	}

	if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1`, g.ID); err != nil {
		return nil, newSCIMError(500, "", "replace group members: "+err.Error())
	}
	for uid := range target {
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`,
			g.ID, uid,
		); err != nil {
			return nil, newSCIMError(500, "", "replace group member: "+err.Error())
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, newSCIMError(500, "", "commit replace group: "+err.Error())
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	if err := s.touchSyncInstance(ctx, sc, s.CurrentSyncInstance(ctx, sc)); err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	return &g, nil
}

// PatchGroup applies membership mutations honoring RFC 7644 add/remove/replace
// semantics, in order, atomically.
//
// Invariants:
//   - Every referenced member across EVERY operation is resolved and validated
//     BEFORE any mutation. A single unknown member → 404 and NO membership
//     change (zero-partial).
//   - Operations execute sequentially in request order.
//   - add     = union with existing membership (all values of the op).
//     remove  = subtraction of all values of the op from existing membership.
//     replace = reset the working set EXACTLY ONCE, then add every value carried
//     by that single operation (full replacement by that op). A replace
//     op with N values yields exactly those N members — NOT N
//     independent clobbering replaces.
//   - All membership changes happen inside one transaction.
func (s *DirectoryService) PatchGroup(ctx context.Context, sc *scope, idOrExternal string, patch *groupPatch) (*groupRow, *SCIMError) {
	if len(patch.Ops) == 0 {
		return s.GetGroup(ctx, sc, idOrExternal)
	}

	// 1) Resolve & validate ALL referenced members across ALL ops before mutating.
	current, err := s.ListGroupMembers(ctx, sc, idOrExternal)
	if err != nil {
		return nil, newSCIMError(500, "", "list group members: "+err.Error())
	}

	currentSet := make(map[string]struct{}, len(current))
	for _, id := range current {
		currentSet[id] = struct{}{}
	}

	type resolvedOp struct {
		op      string
		userIDs []string
	}
	var ops []resolvedOp
	var unknowns []string
	for _, p := range patch.Ops {
		uop := strings.ToUpper(strings.TrimSpace(p.Op))
		switch uop {
		case "ADD", "REMOVE", "REPLACE":
		default:
			return nil, newSCIMError(400, "invalidValue", "unsupported group patch op: "+uop)
		}
		var ids []string
		for _, v := range p.Values {
			rids, rerr := s.userIDsByExternalOrUUID(ctx, sc, v)
			if rerr != nil {
				return nil, newSCIMError(500, "", "resolve members: "+rerr.Error())
			}
			if len(rids) == 0 {
				unknowns = append(unknowns, v)
				continue
			}
			ids = append(ids, rids[0])
		}
		ops = append(ops, resolvedOp{op: uop, userIDs: ids})
	}
	if len(unknowns) > 0 {
		return nil, newSCIMError(404, "", fmt.Sprintf("unknown members: %s", strings.Join(unknowns, ", ")))
	}

	// 2) Simulate op application IN ORDER to compute the final set.
	working := make(map[string]struct{}, len(currentSet))
	for id := range currentSet {
		working[id] = struct{}{}
	}
	for _, o := range ops {
		switch o.op {
		case "ADD":
			for _, uid := range o.userIDs {
				working[uid] = struct{}{}
			}
		case "REMOVE":
			for _, uid := range o.userIDs {
				delete(working, uid)
			}
		case "REPLACE":
			// Reset exactly once for this operation, then add all its members.
			working = make(map[string]struct{})
			for _, uid := range o.userIDs {
				working[uid] = struct{}{}
			}
		}
	}
	var toAdd, toRemove []string
	for id := range working {
		if _, ok := currentSet[id]; !ok {
			toAdd = append(toAdd, id)
		}
	}
	for id := range currentSet {
		if _, ok := working[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}

	// 3) Apply the delta transactionally (group resolved with connection scope).
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	if err := s.applyGroupMembershipDelta(ctx, tx, sc, idOrExternal, toAdd, toRemove); err != nil {
		return nil, newSCIMError(500, "", "apply membership: "+err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, newSCIMError(500, "", "commit patch group: "+err.Error())
	}

	g, serr := s.GetGroup(ctx, sc, idOrExternal)
	if serr != nil {
		return nil, serr
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	if err := s.touchSyncInstance(ctx, sc, s.CurrentSyncInstance(ctx, sc)); err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	return g, nil
}

// DeleteGroup removes a scim group and its memberships.
func (s *DirectoryService) DeleteGroup(ctx context.Context, sc *scope, idOrExternal string) *SCIMError {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM groups
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		    AND (id::text = $3 OR external_id = $3)`,
		sc.workspaceID, sc.connectionID, idOrExternal,
	)
	if err != nil {
		return newSCIMError(500, "", "delete group: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return newSCIMError(404, "", "group not found")
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	return nil
}

// ── membership helpers ───────────────────────────────────────────────────────

// ListGroupMembers returns user ids for a scim group, scoped to the connection.
func (s *DirectoryService) ListGroupMembers(ctx context.Context, sc *scope, idOrExternal string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT gm.user_id::text
		   FROM group_members gm
		   JOIN groups g ON g.id = gm.group_id
		  WHERE g.workspace_id = $1
		    AND g.origin = 'scim'
		    AND g.connection_id = $2
		    AND (g.id::text = $3 OR g.external_id = $3)`,
		sc.workspaceID, sc.connectionID, idOrExternal,
	)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// ListGroupMembersAsMembers returns membership references for a SCIM group
// response, scoped to the connection.
func (s *DirectoryService) ListGroupMembersAsMembers(ctx context.Context, sc *scope, groupID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT gm.user_id::text
		   FROM group_members gm
		   JOIN groups g ON g.id = gm.group_id
		  WHERE g.id::text = $1 AND g.origin = 'scim' AND g.connection_id = $2`,
		groupID, sc.connectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list group member refs: %w", err)
	}
	defer rows.Close()
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(ids))
	for _, uid := range ids {
		out = append(out, Member{
			Value:   uid,
			Type:    "User",
			Ref:     "/scim/v2/Users/" + uid,
			Display: uid,
		})
	}
	return out, nil
}

// userIDsByExternalOrUUID resolves a member reference to a canonical user id
// strictly within the authenticated (workspace, connection) scope.
//
// Supported forms:
//   - externalId / canonical subject for the same connection (primary)
//   - user uuid, but ONLY when the user is linked to the SAME connection via
//     external_identities. This blocks a SCIM connection from reaching into
//     another connection's members by UUID (cross-connection isolation).
func (s *DirectoryService) userIDsByExternalOrUUID(ctx context.Context, sc *scope, value string) ([]string, error) {
	// Primary: canonical subject (externalId) within the connection.
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT u.id::text
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.tenant_id = $1
		    AND ei.connection_id = $2
		    AND u.status <> 'deleted'
		    AND ei.subject = $3`,
		sc.workspaceID, sc.connectionID, value,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve member by subject: %w", err)
	}
	defer rows.Close()
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		return ids, nil
	}

	// Fallback: user UUID, but only if the value is a well-formed UUID and the
	// user is linked to THIS connection. The JOIN on external_identities
	// enforces the connection boundary; a user belonging to a different
	// connection (same workspace) will not resolve. Non-UUID values (e.g. an
	// unknown externalId) are skipped so they surface as "unknown member".
	if _, perr := uuid.Parse(value); perr != nil {
		return nil, nil
	}
	rows, err = s.pool.Query(ctx,
		`SELECT DISTINCT u.id::text
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.id = $1
		    AND u.tenant_id = $2
		    AND ei.connection_id = $3
		    AND u.status <> 'deleted'`,
		value, sc.workspaceID, sc.connectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve member by id: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// applyGroupMembershipDelta applies add/remove sets to a scim group, resolving
// the group id WITH connection scoping (never escaping the authenticated
// connection). The entire operation runs inside the supplied transaction.
func (s *DirectoryService) applyGroupMembershipDelta(ctx context.Context, tx pgx.Tx, sc *scope, idOrExternal string, toAdd, toRemove []string) error {
	var groupID string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM groups
		  WHERE workspace_id = $1
		    AND origin = 'scim'
		    AND connection_id = $2
		    AND (id::text = $3 OR external_id = $3)`,
		sc.workspaceID, sc.connectionID, idOrExternal,
	).Scan(&groupID); err != nil {
		return fmt.Errorf("resolve group id: %w", err)
	}

	for _, uid := range toAdd {
		if _, err := tx.Exec(ctx,
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			groupID, uid,
		); err != nil {
			return fmt.Errorf("add group member: %w", err)
		}
	}
	for _, uid := range toRemove {
		if _, err := tx.Exec(ctx,
			`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`,
			groupID, uid,
		); err != nil {
			return fmt.Errorf("remove group member: %w", err)
		}
	}
	return nil
}

// OriginAwareID builds a stable origin-aware identifier for logging/auditing.
func OriginAwareID(id, origin, externalID string) string {
	if origin != "" && externalID != "" {
		return fmt.Sprintf("%s:%s:%s", origin, id, externalID)
	}
	if origin != "" {
		return fmt.Sprintf("%s:%s", origin, id)
	}
	return id
}

// ── request parsing ──────────────────────────────────────────────────────────

// patchGroupFromOps converts RFC 7644 PATCH Operations into a normalized
// groupPatch. Membership values are parsed as arrays of member objects
// (RFC 7644 §3.5.2), and targeted removal paths members[value eq "x"] are
// supported. Each operation keeps its own op so mixed add/remove/replace PATCHes
// are preserved.
func patchGroupFromOps(ops []map[string]any) (*groupPatch, error) {
	p := &groupPatch{}
	for _, op := range ops {
		opType, _ := op["op"].(string)
		opType = strings.ToUpper(strings.TrimSpace(opType))
		path, _ := op["path"].(string)
		switch opType {
		case "ADD", "REMOVE", "REPLACE":
			values, err := groupMemberValues(opType, path, op["value"])
			if err != nil {
				return nil, err
			}
			// Preserve the operation boundary: all values of one PATCH op stay
			// together so PatchGroup applies them as a single add/remove/replace.
			p.Ops = append(p.Ops, patchOp{Op: opType, Values: values})
		default:
			return nil, fmt.Errorf("unsupported PATCH op %q", opType)
		}
	}
	if len(p.Ops) == 0 {
		return nil, fmt.Errorf("no membership operations in PATCH")
	}
	return p, nil
}

// groupMemberValues extracts member reference values from a single SCIM PATCH op.
func groupMemberValues(opType, path string, value any) ([]string, error) {
	normed := strings.ToLower(strings.TrimSpace(path))

	// No path: value may be a full resource object carrying a "members" key.
	if normed == "" {
		if obj, ok := value.(map[string]any); ok {
			if mem, ok := obj["members"]; ok {
				return groupMemberValues(opType, "members", mem)
			}
		}
		return nil, fmt.Errorf("group PATCH requires a members path or a members value object")
	}

	// Targeted removal: members[value eq "user-id"].
	if m := memberFilterRe.FindStringSubmatch(normed); m != nil {
		if opType != "REMOVE" {
			return nil, fmt.Errorf("filtered path %q is only valid for remove", path)
		}
		return []string{m[1]}, nil
	}

	if normed != "members" {
		return nil, fmt.Errorf("unsupported group patch path: %s", path)
	}

	// RFC 7644: membership value is an array of member objects.
	arr, ok := value.([]any)
	if !ok {
		// Lenient fallback: a single member object.
		if obj, ok := value.(map[string]any); ok {
			arr = []any{obj}
		} else {
			return nil, fmt.Errorf("membership op requires an array of member values")
		}
	}

	out := make([]string, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("member value must be an object")
		}
		v, _ := obj["value"].(string)
		if v == "" {
			if ref, _ := obj["$ref"].(string); ref != "" {
				v = ref
			}
		}
		if v == "" {
			return nil, fmt.Errorf("member value missing \"value\"")
		}
		out = append(out, v)
	}
	return out, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "unique constraint") ||
		strings.Contains(err.Error(), "duplicate key value")
}
