package scim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// DirectoryService is the SCIM-facing identity orchestration layer (ADR-025):
// it binds (workspace_id, connection_id) from the validated token into every
// query and mutation so no operation can ever touch another workspace or
// connection (§10). It reuses the existing identity pipeline (Resolver/Linker)
// rather than rebuilding it.
//
// Provisioning/deprovisioning of canonical users is performed through the
// identity.Resolver (lookup) and identity.Linker (JIT-create via a SCIM
// provisioner). Deprovision/Reactivate (Phase 6) run inside a single
// transaction and enqueue a device-trust event into the durable outbox.
type DirectoryService struct {
	pool      *pgxpool.Pool
	idpStore  *idp.Store
	resolver  *identity.Resolver
	publisher identity.EventPublisher
	notifier  *policy.Notifier
	sink      identity.SideEffectSink // device-trust outbox seam (Phase 6)
	revoker   *identity.Revoker       // generation bump / session kill (Phase 6)
	audit     identity.EventPublisher // conflict approval/transition audit (Phase 8)
	perm      *permission.Store       // explicit fine-grained permissions (Phase 8)
}

// NewDirectoryService builds a DirectoryService. A nil publisher defaults to
// NopPublisher; a nil notifier disables ACL invalidation (acceptable for the
// attribute-only updates in Phase 5, where membership is untouched). A nil sink
// disables device-trust outbox emission (deprovision still cuts access locally);
// a nil revoker disables the generation-bump session kill. A nil audit publisher
// disables conflict auditing; a nil perm store disables Accept-Link authorization
// (Accept-Link will error until a permission store is wired).
func NewDirectoryService(
	pool *pgxpool.Pool,
	idpStore *idp.Store,
	pub identity.EventPublisher,
	notifier *policy.Notifier,
	sink identity.SideEffectSink,
	revoker *identity.Revoker,
) *DirectoryService {
	if pub == nil {
		pub = identity.NopPublisher{}
	}
	return &DirectoryService{
		pool:      pool,
		idpStore:  idpStore,
		resolver:  identity.NewResolver(pool),
		publisher: pub,
		notifier:  notifier,
		sink:      sink,
		revoker:   revoker,
		audit:     pub, // reuse the same publisher for conflict audit unless overridden
	}
}

// WithPermissionStore wires the explicit fine-grained permission store used by
// Phase 8 Accept-Link authorization (identity.mapping.break_glass). It is a
// fluent setter so existing constructor call sites need not change.
func (s *DirectoryService) WithPermissionStore(p *permission.Store) *DirectoryService {
	s.perm = p
	return s
}

// scope is the resolved (workspace, connection) pair bound from the token.
// Every query below carries both columns in its WHERE/INSERT.
type scope struct {
	workspaceID  string
	connectionID string
	provider     string
	issuer       string
	subjectClaim string
	scimIdent    string
	scimEnabled  bool
}

// resolveScope loads the connection for the token's scope and enforces that SCIM
// is permitted (fail-closed: a connection whose mapping was never proven and was
// not break-glass-overridden must not accept directory writes). The connection's
// tenant_id IS the workspace_id (identity_connections.tenant_id), which is
// exactly the scope the token carries.
func (s *DirectoryService) resolveScope(ctx context.Context, workspaceID, connectionID string) (*scope, *SCIMError) {
	conn, err := s.idpStore.GetByID(ctx, connectionID)
	if err != nil {
		if err == idp.ErrConnectionNotFound {
			return nil, newSCIMError(404, "", "connection not found")
		}
		return nil, newSCIMError(500, "", "load connection: "+err.Error())
	}
	// Tenant guard: the connection must belong to the token's workspace.
	if conn.TenantID == nil || *conn.TenantID != workspaceID {
		return nil, newSCIMError(404, "", "connection not found")
	}
	// Fail-closed: SCIM disabled until the mapping is proven (Phase 4/5 round-trip)
	// or break-glass enabled. A non-enabled connection must not accept writes.
	if !conn.ScimEnabled {
		return nil, newSCIMError(403, "", "SCIM is disabled for this connection: mapping not verified")
	}
	// DISABLE is the reversible off-switch (ADR-025 §12): SCIM provisioning and
	// all directory sync stop while the connection is disabled. A disabled (or
	// deleted) connection must not accept SCIM writes until re-enabled.
	if conn.Status != "active" {
		return nil, newSCIMError(403, "", "SCIM is disabled for this connection: connection status is "+conn.Status)
	}
	return &scope{
		workspaceID:  workspaceID,
		connectionID: connectionID,
		provider:     conn.Provider,
		issuer:       conn.Issuer,
		subjectClaim: conn.SubjectClaim,
		scimIdent:    conn.ScimIdentifier,
		scimEnabled:  true,
	}, nil
}

// canonicalKey extracts the Canonical Identity Key from a SCIM resource using the
// connection's configured scimIdentifier attribute (default externalId). It never
// keys on email.
func (sc *scope) canonicalKey(resource map[string]any) string {
	return ExtractScimIdentifier(resource, sc.scimIdent)
}

// provisionResult is what Provision returns: the SCIM user + whether it was newly
// created (201) or an idempotent re-provision (200).
type provisionResult struct {
	user     User
	created  bool
	conflict bool // true when a JIT/manual identity already occupies the key
}

// Provision creates or idempotently re-provisions a canonical user from a SCIM
// User resource.
//
//   - Resolve on (connection, canonicalKey). Hit on a SCIM-provisioned user →
//     idempotent 200 (no new link; the UNIQUE(tenant,conn,subject) guard holds).
//   - Hit on a JIT/manual identity → 409 identity_conflict (the Phase 8 record is
//     written by Phase 8; Phase 5 only refuses to take over).
//   - Miss → Linker JIT-creates with provisioned_by=scim, provisioning_owner=scim,
//     sync_instance_id, in one atomic transaction.
func (s *DirectoryService) Provision(ctx context.Context, sc *scope, resource map[string]any, syncInst string) (*provisionResult, *SCIMError) {
	key := sc.canonicalKey(resource)
	if key == "" {
		return nil, newSCIMError(400, "invalidValue", "missing canonical identity attribute: "+sc.scimIdent)
	}

	core, found, err := s.resolver.Resolve(ctx, sc.connectionID, key, sc.workspaceID)
	if err != nil {
		return nil, newSCIMError(500, "", "resolve identity: "+err.Error())
	}
	if found {
		owner, linked, perr := s.scopedProvisioningOwner(ctx, sc, core.UserID)
		if perr != nil {
			return nil, newSCIMError(500, "", perr.Error())
		}
		if owner == "scim" && linked {
			u, uerr := s.loadUser(ctx, sc, core.UserID)
			if uerr != nil {
				return nil, newSCIMError(500, "", uerr.Error())
			}
			return &provisionResult{user: u, created: false}, nil
		}
		// Existing JIT/manual identity with this key → conflict. Phase 8 writes
		// the persistent record; we refuse takeover here.
		s.ensurePendingConflict(ctx, sc.workspaceID, sc.connectionID, key, core.UserID,
			conflictSnapshot{externalID: key, userName: userNameOf(resource), email: primaryEmailOf(resource)})
		return &provisionResult{conflict: true}, newSCIMError(409, "identity_conflict",
			"an existing identity already owns this canonical key; admin approval required")
	}

	// First-seen → JIT-create via the SCIM provisioner (atomic user + link).
	prov := newSCIMProvisioner(s.pool, sc.workspaceID, sc.connectionID, syncInst, s.publisher)
	linker := identity.NewLinker(prov)
	email := userNameOf(resource)
	name := displayNameOf(resource)
	created, err := linker.Link(ctx, identity.ProvisionInput{
		Email:        email,
		Provider:     sc.provider,
		Subject:      key,
		Name:         name,
		ConnectionID: sc.connectionID,
		Issuer:       sc.issuer,
	})
	if err != nil {
		return nil, newSCIMError(500, "", "provision identity: "+err.Error())
	}
	if err := s.touchSyncInstance(ctx, sc, syncInst); err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	u, uerr := s.loadUser(ctx, sc, created.UserID)
	if uerr != nil {
		return nil, newSCIMError(500, "", uerr.Error())
	}
	return &provisionResult{user: u, created: true}, nil
}

// Get returns a single SCIM user by its canonical user id, scoped to the
// workspace+connection. Tombstoned (status='deleted') users are hidden.
func (s *DirectoryService) Get(ctx context.Context, sc *scope, userID string) (*User, *SCIMError) {
	var status string
	var tenant string
	err := s.pool.QueryRow(ctx,
		`SELECT u.status, u.tenant_id
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.id = $1 AND ei.connection_id = $2
		  LIMIT 1`,
		userID, sc.connectionID,
	).Scan(&status, &tenant)
	if err != nil {
		return nil, newSCIMError(404, "", "user not found")
	}
	if tenant != sc.workspaceID {
		return nil, newSCIMError(404, "", "user not found")
	}
	if status == "deleted" {
		// Tombstone hidden from normal SCIM reads (ADR-025 §9).
		return nil, newSCIMError(404, "", "user not found")
	}
	u, uerr := s.loadUser(ctx, sc, userID)
	if uerr != nil {
		return nil, newSCIMError(500, "", uerr.Error())
	}
	return &u, nil
}

// Filter returns users matching a minimal SCIM filter: eq on userName or
// externalId only (ADR-025 §8). Anything else is rejected with 400.
// Tombstones (status='deleted') are excluded from collections.
func (s *DirectoryService) Filter(ctx context.Context, sc *scope, filter string) ([]User, *SCIMError) {
	attr, val, ok := parseEqFilter(filter)
	if !ok {
		return nil, newSCIMError(400, "invalidFilter",
			"only 'eq' filters on userName or externalId are supported")
	}
	var rows []User
	var err error
	switch attr {
	case "username":
		rows, err = s.queryByUserName(ctx, sc, val)
	case "externalid":
		// externalId maps to the canonical key (external_identities.subject),
		// never email.
		rows, err = s.queryByExternalID(ctx, sc, val)
	default:
		return nil, newSCIMError(400, "invalidFilter",
			"unsupported filter attribute: "+attr+" (only userName, externalId)")
	}
	if err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	return rows, nil
}

// Update applies a directory-owned attribute change (PUT full replace / PATCH
// ops) to an existing SCIM-owned user. Zecurity-owned attributes (role, manual
// group membership, etc.) are rejected at the mutation layer — never persisted.
//
// Phase 5 scope (per plan decision): the schema has no name/title/department
// column on users, so directory-owned updates are limited to email, active
// (status), and the external_identities sync_instance_id. Any patch targeting
// an unsupported/unowned attribute is rejected with 400 so the directory never
// silently wins a tug-of-war it shouldn't (ADR-025 §4).
func (s *DirectoryService) Update(ctx context.Context, sc *scope, userID string, patch *userPatch, syncInst string) (*User, *SCIMError) {
	owner, linked, err := s.scopedProvisioningOwner(ctx, sc, userID)
	if err != nil {
		return nil, newSCIMError(404, "", "user not found")
	}
	if owner != "scim" || !linked {
		s.ensurePendingConflict(ctx, sc.workspaceID, sc.connectionID, s.canonicalKeyOfUser(ctx, sc, userID), userID, conflictSnapshot{})
		return nil, newSCIMError(409, "identity_conflict",
			"cannot update a non-SCIM owned identity via SCIM")
	}

	// Reject unowned/unsupported attribute writes at the mutation layer.
	for _, a := range patch.Attributes {
		if !supportedDirectoryAttr(a.Name) {
			return nil, newSCIMError(400, "invalidValue",
				"attribute not directory-owned in this release: "+a.Name)
		}
	}

	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET
		   email = COALESCE($3::text, email),
		   status = CASE WHEN $4::bool IS NOT NULL THEN
		       CASE WHEN $4::bool THEN 'active' ELSE 'suspended' END
		       ELSE status END,
		   updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2`,
		userID, sc.workspaceID, nullIfText(patch.Email), patch.Active,
	); err != nil {
		return nil, newSCIMError(500, "", "update user: "+err.Error())
	}
	if syncInst != "" {
		if _, err := s.pool.Exec(ctx,
			`UPDATE external_identities SET sync_instance_id = $3
			 WHERE user_id = $1 AND connection_id = $2`,
			userID, sc.connectionID, syncInst,
		); err != nil {
			return nil, newSCIMError(500, "", "update sync_instance: "+err.Error())
		}
	}
	if err := s.touchSyncInstance(ctx, sc, syncInst); err != nil {
		return nil, newSCIMError(500, "", err.Error())
	}
	if s.notifier != nil {
		// Attribute changes do not alter membership, but the ACL snapshot is
		// invalidated for parity with any identity mutation (ADR-025 §8).
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	u, uerr := s.loadUser(ctx, sc, userID)
	if uerr != nil {
		return nil, newSCIMError(500, "", uerr.Error())
	}
	return &u, nil
}

// Deprovision cuts Zecurity access for a SCIM-owned user and durably emits a
// device-trust revoke event. All steps run inside a single transaction so the
// status change, generation bump, and outbox enqueue commit atomically — and a
// sink/enqueue failure rolls back the identity mutation (build-gate invariant).
//
// hard=false  → status='suspended' (reversible)   + reason "suspended"
// hard=true   → status='deleted'    (soft delete) + reason "deleted"
//
// The device-trust event is enqueued (not executed): downstream device cert
// revocation is asynchronous via the durable outbox and PENDING-13. A downstream
// failure must never roll back this committed identity mutation.
func (s *DirectoryService) Deprovision(ctx context.Context, sc *scope, userID string, hard bool, syncInst string) *SCIMError {
	owner, linked, err := s.scopedProvisioningOwner(ctx, sc, userID)
	if err != nil {
		return newSCIMError(404, "", "user not found")
	}
	if owner != "scim" || !linked {
		s.ensurePendingConflict(ctx, sc.workspaceID, sc.connectionID, s.canonicalKeyOfUser(ctx, sc, userID), userID, conflictSnapshot{})
		return newSCIMError(409, "identity_conflict",
			"cannot deprovision a non-SCIM owned identity via SCIM")
	}
	newStatus := "suspended"
	reason := identity.DeviceTrustReasonSuspended
	if hard {
		newStatus = "deleted"
		reason = identity.DeviceTrustReasonDeleted
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE users SET status = $3, updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2`,
		userID, sc.workspaceID, newStatus,
	)
	if err != nil {
		return newSCIMError(500, "", "deprovision user: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return newSCIMError(404, "", "user not found")
	}
	var bumpGen int
	if s.revoker != nil {
		var berr error
		bumpGen, berr = s.revoker.BumpGenerationTx(ctx, tx, sc.workspaceID, userID, "scim")
		if berr != nil {
			if errors.Is(berr, pgx.ErrNoRows) {
				return newSCIMError(404, "", "user not found")
			}
			return newSCIMError(500, "", "bump generation: "+berr.Error())
		}
	}
	if hard {
		if _, err := tx.Exec(ctx,
			`DELETE FROM group_members
			  WHERE user_id = $1
			    AND group_id IN (SELECT id FROM groups WHERE workspace_id = $2)`,
			userID, sc.workspaceID,
		); err != nil {
			return newSCIMError(500, "", "remove group memberships: "+err.Error())
		}
	}
	if s.sink != nil {
		corr := uuid.New()
		evt := identity.DeviceTrustEvent{
			WorkspaceID:   sc.workspaceID,
			UserID:        userID,
			Reason:        reason,
			CorrelationID: corr.String(),
		}
		if err := s.sink.Enqueue(ctx, tx, evt); err != nil {
			return newSCIMError(500, "", "enqueue device-trust event: "+err.Error())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return newSCIMError(500, "", "commit deprovision: "+err.Error())
	}
	if s.revoker != nil {
		s.revoker.AfterBump(ctx, sc.workspaceID, userID, "scim", bumpGen)
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	if syncInst != "" {
		_ = s.touchSyncInstance(ctx, sc, syncInst)
	}
	return nil
}

// Reactivate restores a suspended SCIM-owned user to active and durably emits a
// device-trust re-enrollment event. Per ADR-028 the user's devices were already
// cert-revoked on the prior suspend, so reactivation only flips status and
// signals re-enrollment (no generation bump — the bump already happened on
// suspend). Idempotent against a non-active user.
func (s *DirectoryService) Reactivate(ctx context.Context, sc *scope, userID string, syncInst string) *SCIMError {
	owner, linked, err := s.scopedProvisioningOwner(ctx, sc, userID)
	if err != nil {
		return newSCIMError(404, "", "user not found")
	}
	if owner != "scim" || !linked {
		s.ensurePendingConflict(ctx, sc.workspaceID, sc.connectionID, s.canonicalKeyOfUser(ctx, sc, userID), userID, conflictSnapshot{})
		return newSCIMError(409, "identity_conflict",
			"cannot reactivate a non-SCIM owned identity via SCIM")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE users SET status = 'active', updated_at = NOW()
		 WHERE id = $1 AND tenant_id = $2`,
		userID, sc.workspaceID,
	)
	if err != nil {
		return newSCIMError(500, "", "reactivate user: "+err.Error())
	}
	if tag.RowsAffected() == 0 {
		return newSCIMError(404, "", "user not found")
	}
	if s.sink != nil {
		corr := uuid.New()
		evt := identity.DeviceTrustEvent{
			WorkspaceID:   sc.workspaceID,
			UserID:        userID,
			CorrelationID: corr.String(),
		}
		if err := s.sink.Enqueue(ctx, tx, evt); err != nil {
			return newSCIMError(500, "", "enqueue device-trust event: "+err.Error())
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return newSCIMError(500, "", "commit reactivate: "+err.Error())
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyPolicyChange(ctx, sc.workspaceID)
	}
	if syncInst != "" {
		_ = s.touchSyncInstance(ctx, sc, syncInst)
	}
	return nil
}

// ── internal helpers ────────────────────────────────────────────────────────

// canonicalKeyOfUser returns the canonical identity key (external_identities.subject)
// for a given user within the connection scope. It is used to key conflict
// records when a collision is detected on a verb other than provision (where the
// key is already known). Never resolves by email; scoped to the connection.
func (s *DirectoryService) canonicalKeyOfUser(ctx context.Context, sc *scope, userID string) string {
	var sub string
	if err := s.pool.QueryRow(ctx,
		`SELECT ei.subject FROM external_identities ei
		  WHERE ei.user_id = $1 AND ei.connection_id = $2
		  LIMIT 1`,
		userID, sc.connectionID,
	).Scan(&sub); err != nil {
		// Best-effort: an empty key simply yields no conflict row.
		return ""
	}
	return sub
}

// scopedProvisioningOwner returns users.provisioning_owner for a user id,
// scoped to the token's (workspace, connection). The JOIN on
// external_identities enforces connection isolation; tenant_id enforces
// workspace isolation.
//
// Every SCIM mutation path MUST resolve ownership through this helper. An
// unscoped lookup here is a tenant-isolation hole: it lets a token for one
// workspace pass the ownership guard for a user in another, after which the
// scoped UPDATE affects 0 rows (not an error) and execution continues into the
// generation bump and the device-trust enqueue.
// It reports two facts separately, because they carry different HTTP outcomes:
//
//	err != nil  → the user does not exist IN THIS WORKSPACE  → 404
//	!linked     → the user exists here but is not bound to THIS connection
//	owner       → who currently owns the user's directory attributes
//
// The existence test must NOT be an inner join on external_identities: a
// JIT/manual user has no link row for this connection, and collapsing that into
// a 404 would swallow the 409 identity_conflict + pending-conflict record that
// ADR-025 §4.1 requires. Callers treat (!linked || owner != "scim") as a
// conflict, which also keeps a SCIM user owned by a *different* connection out
// of this token's reach.
func (s *DirectoryService) scopedProvisioningOwner(ctx context.Context, sc *scope, userID string) (owner string, linked bool, err error) {
	if err := s.pool.QueryRow(ctx,
		`SELECT u.provisioning_owner,
		        EXISTS (
		          SELECT 1 FROM external_identities ei
		           WHERE ei.user_id = u.id AND ei.connection_id = $3
		        )
		   FROM users u
		  WHERE u.id = $1 AND u.tenant_id = $2`,
		userID, sc.workspaceID, sc.connectionID,
	).Scan(&owner, &linked); err != nil {
		return "", false, fmt.Errorf("load provisioning_owner: %w", err)
	}
	return owner, linked, nil
}

// loadUser reads a canonical user and projects it as a SCIM User resource,
// scoped to the connection. meta.version = users.identity_generation (ADR-025 §8).
func (s *DirectoryService) loadUser(ctx context.Context, sc *scope, userID string) (User, error) {
	var (
		email       string
		providerSub string
		status      string
		gen         int
		updatedAt   string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT u.email, u.provider_sub, u.status, u.identity_generation,
		        TO_CHAR(u.updated_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		   FROM users u
		  WHERE u.id = $1 AND u.tenant_id = $2`,
		userID, sc.workspaceID,
	).Scan(&email, &providerSub, &status, &gen, &updatedAt)
	if err != nil {
		return User{}, fmt.Errorf("load user: %w", err)
	}
	return User{
		Schemas:    []string{userSchema},
		ID:         userID,
		UserName:   email,
		ExternalID: providerSub, // subject = externalId equivalent in v1
		Active:     status == "active",
		Emails:     []Email{{Primary: true, Value: email, Type: "work"}},
		Meta: Meta{
			ResourceType: "User",
			Version:      fmt.Sprintf("%d", gen),
			LastModified: updatedAt,
			Location:     "/scim/v2/Users/" + userID,
		},
	}, nil
}

// queryByUserName filters SCIM users by email (the SCIM userName hint), scoped to
// the connection, excluding tombstones.
func (s *DirectoryService) queryByUserName(ctx context.Context, sc *scope, val string) ([]User, error) {
	ids, err := s.userIDsByEmail(ctx, sc, val)
	if err != nil {
		return nil, err
	}
	return s.loadUsers(ctx, sc, ids), nil
}

// queryByExternalID filters SCIM users by the canonical key (subject), scoped to
// the connection, excluding tombstones. This is the identity-key path — never
// email.
func (s *DirectoryService) queryByExternalID(ctx context.Context, sc *scope, val string) ([]User, error) {
	ids, err := s.userIDsBySubject(ctx, sc, val)
	if err != nil {
		return nil, err
	}
	return s.loadUsers(ctx, sc, ids), nil
}

func (s *DirectoryService) userIDsByEmail(ctx context.Context, sc *scope, email string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT u.id::text
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.tenant_id = $1 AND ei.connection_id = $2
		    AND u.status <> 'deleted' AND LOWER(u.email) = LOWER($3)`,
		sc.workspaceID, sc.connectionID, email,
	)
	if err != nil {
		return nil, fmt.Errorf("query users by email: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *DirectoryService) userIDsBySubject(ctx context.Context, sc *scope, subject string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT u.id::text
		   FROM users u
		   JOIN external_identities ei ON ei.user_id = u.id
		  WHERE u.tenant_id = $1 AND ei.connection_id = $2
		    AND u.status <> 'deleted' AND ei.subject = $3`,
		sc.workspaceID, sc.connectionID, subject,
	)
	if err != nil {
		return nil, fmt.Errorf("query users by subject: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *DirectoryService) loadUsers(ctx context.Context, sc *scope, ids []string) []User {
	out := make([]User, 0, len(ids))
	for _, id := range ids {
		u, err := s.loadUser(ctx, sc, id)
		if err != nil {
			continue
		}
		out = append(out, u)
	}
	return out
}

// touchSyncInstance stamps last_sync_at on the sync instance AND on the
// owning connection. The connection timestamp is what Identity Health derives
// from (ADR-025 §12: "SCIM availability equals deprovision timeliness"), so it
// must move on every successful SCIM write — not only when a sync instance row
// is explicitly open.
func (s *DirectoryService) touchSyncInstance(ctx context.Context, sc *scope, syncInst string) error {
	if sc != nil {
		if err := s.idpStore.TouchConnectionSync(ctx, sc.workspaceID, sc.connectionID); err != nil {
			// Best-effort: a missing connection row should not fail the write.
			if err != idp.ErrConnectionNotFound {
				return fmt.Errorf("touch connection sync: %w", err)
			}
		}
	}
	if syncInst == "" {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE scim_sync_instances SET last_sync_at = NOW() WHERE id = $1`, syncInst,
	); err != nil {
		return fmt.Errorf("touch sync instance: %w", err)
	}
	return nil
}

// IdentityHealthState enumerates the connection sync-health states (ADR-025 §12).
type IdentityHealthState string

const (
	HealthHealthy      IdentityHealthState = "Healthy"
	HealthDelayed      IdentityHealthState = "Delayed"
	HealthDisconnected IdentityHealthState = "Disconnected"
	HealthDisabled     IdentityHealthState = "Disabled"
)

// Health thresholds (ADR-025 leaves the cutoffs to implementation): staleness is
// measured from identity_connections.last_sync_at.
const (
	healthHealthyMaxAge = 24 * time.Hour     // ≤24h  → Healthy
	healthDelayedMaxAge = 3 * 24 * time.Hour // ≤72h → Delayed; beyond → Disconnected
)

// IdentityHealth derives the connection's sync-health state from last_sync_at
// and current status. A non-active connection is reported as "Disabled" (its
// health is not meaningful once SCIM is off). Null last_sync_at → Disconnected.
func (s *DirectoryService) IdentityHealth(ctx context.Context, workspaceID, connectionID string) (IdentityHealthState, error) {
	var (
		status   string
		lastSync *time.Time
	)
	if err := s.pool.QueryRow(ctx,
		`SELECT status, last_sync_at FROM identity_connections
		  WHERE id = $1 AND tenant_id = $2`, connectionID, workspaceID,
	).Scan(&status, &lastSync); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", idp.ErrConnectionNotFound
		}
		return "", fmt.Errorf("read connection health: %w", err)
	}
	if status != "active" {
		return HealthDisabled, nil
	}
	if lastSync == nil {
		return HealthDisconnected, nil
	}
	age := time.Since(*lastSync)
	switch {
	case age <= healthHealthyMaxAge:
		return HealthHealthy, nil
	case age <= healthDelayedMaxAge:
		return HealthDelayed, nil
	default:
		return HealthDisconnected, nil
	}
}

func nullIfText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// scanIDs collects a single text column from a row set.
func scanIDs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]string, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}

// ── SCIM resource attribute helpers ──────────────────────────────────────────

// userNameOf returns the SCIM userName (typically the email-formatted login).
func userNameOf(resource map[string]any) string {
	if v, ok := resource["userName"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// primaryEmailOf returns the SCIM resource's primary email, per RFC 7643 §4.1.2:
// the "emails" multi-valued attribute, preferring the entry marked primary, and
// falling back to userName (which providers conventionally format as an email).
// Snapshot/context use only — identity is NEVER resolved by email (ADR-025 §4.1).
func primaryEmailOf(resource map[string]any) string {
	list, ok := resource["emails"].([]any)
	if !ok {
		return userNameOf(resource)
	}
	var first string
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		val, _ := m["value"].(string)
		val = strings.TrimSpace(val)
		if val == "" {
			continue
		}
		if p, _ := m["primary"].(bool); p {
			return val
		}
		if first == "" {
			first = val
		}
	}
	if first != "" {
		return first
	}
	return userNameOf(resource)
}

// displayNameOf returns a SCIM displayName if present (notification hint; not
// persisted in Phase 5 — see the name-column gap noted in the phase file).
func displayNameOf(resource map[string]any) string {
	if v, ok := resource["displayName"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// supportedDirectoryAttr reports whether a SCIM attribute name is a directory-
// owned attribute Phase 5 may persist. Unsupported directory attributes (name,
// title, department, …) are rejected at the mutation layer — they have no column
// yet (see Phase 5 plan decision).
func supportedDirectoryAttr(name string) bool {
	switch strings.ToLower(name) {
	case "emails", "email", "active", "username":
		return true
	default:
		return false
	}
}
