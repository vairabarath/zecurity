package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// D-78 (Sprint 27 acceptance, fix X6): disabling or demoting a super-admin
// permanently revokes every API key they created, in the same transaction;
// re-enabling or re-promoting restores nothing; other super-admins' keys are
// untouched; each revocation is audited without the key.
func TestAPIKeys_CreatorDisabledOrDemoted(t *testing.T) {
	store, pool := provider.NewTestStore(t)
	ctx := context.Background()
	_, boot, err := provider.Bootstrap(ctx, store, e2eEmail, e2eTempPass)
	if err != nil {
		t.Fatal(err)
	}
	bootActor := provider.AuditActor{UserID: boot.ID, Email: boot.Email}
	op := func(email, role string) *provider.ProviderUser {
		u, err := store.CreateOperator(ctx, bootActor, email, role, "x-hash")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	alice := op("alice@inkyank.com", provider.RoleSuperAdmin)
	bob := op("bob@inkyank.com", provider.RoleSuperAdmin)
	carol := op("carol@inkyank.com", provider.RoleSuperAdmin)
	ops := op("ops@inkyank.com", provider.RoleRelayOps)

	keyOf := func(creator *provider.ProviderUser, name string) (id, key string) {
		k, full, err := store.CreateAPIKey(ctx, provider.AuditActor{UserID: creator.ID, Email: creator.Email},
			provider.NewAPIKey{Name: name, Role: provider.RoleAuditor, ExpiresAt: time.Now().Add(30 * 24 * time.Hour), AllowedCIDRs: []string{}, Reason: "x6 test"})
		if err != nil {
			t.Fatal(err)
		}
		return k.ID, full
	}
	a1, a1Key := keyOf(alice, "alice-1")
	_, a2Key := keyOf(alice, "alice-2")
	b1, b1Key := keyOf(bob, "bob-1")
	_, c1Key := keyOf(carol, "carol-1")
	_, bootKey := keyOf(boot, "boot-1")

	auth := provider.NewAPIKeyAuthenticator(store)
	works := func(key string) bool {
		_, err := auth.Authenticate(ctx, key, "203.0.113.7")
		if err != nil && !errors.Is(err, provider.ErrInvalidAPIKey) {
			t.Fatalf("authenticate: %v", err)
		}
		return err == nil
	}
	for _, k := range []string{a1Key, a2Key, b1Key, c1Key, bootKey} {
		if !works(k) {
			t.Fatal("a fresh key doesn't work")
		}
	}
	var revokedBy, reason string
	revokeOf := func(id string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT revoked_by::text, revoke_reason FROM provider_api_keys WHERE id = $1`, id).Scan(&revokedBy, &reason); err != nil {
			t.Fatalf("key %s not revoked: %v", id, err)
		}
	}

	// Disable alice (by boot): both her keys are revoked, nobody else's.
	if changed, err := store.SetDisabled(ctx, bootActor, alice.ID, true); err != nil || !changed {
		t.Fatalf("disable: %v %v", changed, err)
	}
	if works(a1Key) || works(a2Key) {
		t.Fatal("a disabled creator's key still works")
	}
	if !works(b1Key) || !works(c1Key) || !works(bootKey) {
		t.Fatal("other super-admins' keys were affected")
	}
	revokeOf(a1)
	if revokedBy != boot.ID || reason != provider.RevokeReasonCreatorDisabled {
		t.Fatalf("revoked_by %s reason %q", revokedBy, reason)
	}
	var disableDetails string
	if err := pool.QueryRow(ctx, `SELECT details::text FROM provider_audit_logs WHERE action = $1 AND target_id = $2`,
		provider.AuditOperatorDisable, alice.ID).Scan(&disableDetails); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(disableDetails, `"api_keys_revoked": 2`) {
		t.Fatalf("disable audit lacks the count: %s", disableDetails)
	}
	var keyAudits int
	var auditText string
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(string_agg(details::text, ' '), '') FROM provider_audit_logs
	                               WHERE action = $1 AND details->>'cause' = $2`, provider.AuditAPIKeyRevoke, provider.AuditOperatorDisable).Scan(&keyAudits, &auditText); err != nil {
		t.Fatal(err)
	}
	if keyAudits != 2 || strings.Contains(auditText, a1Key) || strings.Contains(auditText, a2Key) || !strings.Contains(auditText, provider.RevokeReasonCreatorDisabled) {
		t.Fatalf("per-key revoke audit: %d %s", keyAudits, auditText)
	}

	// Re-enabling alice restores nothing.
	if _, err := store.SetDisabled(ctx, bootActor, alice.ID, false); err != nil {
		t.Fatal(err)
	}
	if works(a1Key) || works(a2Key) {
		t.Fatal("re-enabling the creator restored a revoked key")
	}

	// Demote bob: his key is revoked; promoting him again restores nothing.
	if _, changed, err := store.ChangeRole(ctx, bootActor, bob.ID, provider.RoleAuditor); err != nil || !changed {
		t.Fatalf("demote: %v %v", changed, err)
	}
	if works(b1Key) {
		t.Fatal("a demoted creator's key still works")
	}
	revokeOf(b1)
	if revokedBy != boot.ID || reason != provider.RevokeReasonCreatorDemoted {
		t.Fatalf("revoked_by %s reason %q", revokedBy, reason)
	}
	var roleDetails string
	if err := pool.QueryRow(ctx, `SELECT details::text FROM provider_audit_logs WHERE action = $1 AND target_id = $2`,
		provider.AuditOperatorRoleChange, bob.ID).Scan(&roleDetails); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(roleDetails, `"api_keys_revoked": 1`) {
		t.Fatalf("role change audit lacks the count: %s", roleDetails)
	}
	if _, _, err := store.ChangeRole(ctx, bootActor, bob.ID, provider.RoleSuperAdmin); err != nil {
		t.Fatal(err)
	}
	if works(b1Key) {
		t.Fatal("re-promoting the creator restored a revoked key")
	}

	// A role change that isn't a demotion from super-admin revokes nothing
	// and records no count.
	if _, _, err := store.ChangeRole(ctx, bootActor, ops.ID, provider.RoleAuditor); err != nil {
		t.Fatal(err)
	}
	var opsDetails string
	if err := pool.QueryRow(ctx, `SELECT details::text FROM provider_audit_logs WHERE action = $1 AND target_id = $2`,
		provider.AuditOperatorRoleChange, ops.ID).Scan(&opsDetails); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(opsDetails, "api_keys_revoked") {
		t.Fatalf("non-demotion recorded revocations: %s", opsDetails)
	}

	// The actor-less Store.Disable path revokes too.
	if err := store.Disable(ctx, carol.ID); err != nil {
		t.Fatal(err)
	}
	if works(c1Key) {
		t.Fatal("Store.Disable left the creator's key working")
	}
	if !works(bootKey) {
		t.Fatal("boot's key was affected")
	}
}

// D-78 race (security review of X6): a key created while its creator is
// being disabled or demoted must never survive. CreateAPIKey locks the
// creator row FOR SHARE and re-checks it; the disable/demotion holds the row
// FOR UPDATE, so a create waiting behind it sees the new state and is
// refused. A creator who is already disabled or not a super-admin is refused
// outright.
func TestAPIKeys_CreateRacesCreatorDisable(t *testing.T) {
	store, pool := provider.NewTestStore(t)
	ctx := context.Background()
	_, boot, err := provider.Bootstrap(ctx, store, e2eEmail, e2eTempPass)
	if err != nil {
		t.Fatal(err)
	}
	bootActor := provider.AuditActor{UserID: boot.ID, Email: boot.Email}
	dave, err := store.CreateOperator(ctx, bootActor, "dave@inkyank.com", provider.RoleSuperAdmin, "x-hash")
	if err != nil {
		t.Fatal(err)
	}
	daveActor := provider.AuditActor{UserID: dave.ID, Email: dave.Email}
	newKey := func(name string) error {
		_, _, err := store.CreateAPIKey(ctx, daveActor, provider.NewAPIKey{Name: name, Role: provider.RoleAuditor,
			ExpiresAt: time.Now().Add(30 * 24 * time.Hour), AllowedCIDRs: []string{}, Reason: "race test"})
		return err
	}

	// A disable transaction holds dave's row (as SetDisabled's lockTarget does).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM provider_users WHERE id = $1 FOR UPDATE`, dave.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- newKey("racing") }()
	select {
	case err := <-done:
		t.Fatalf("create did not wait for the creator lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(ctx, `UPDATE provider_users SET disabled_at = NOW() WHERE id = $1`, dave.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, provider.ErrAPIKeyCreatorNotActive) {
			t.Fatalf("create after the creator was disabled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("create never finished")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_api_keys WHERE created_by = $1`, dave.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("keys stored for a disabled creator: %d %v", n, err)
	}

	// Already disabled, or no longer a super-admin: refused outright.
	if err := newKey("after-disable"); !errors.Is(err, provider.ErrAPIKeyCreatorNotActive) {
		t.Fatalf("disabled creator: %v", err)
	}
	erin, err := store.CreateOperator(ctx, bootActor, "erin@inkyank.com", provider.RoleAuditor, "x-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateAPIKey(ctx, provider.AuditActor{UserID: erin.ID, Email: erin.Email}, provider.NewAPIKey{Name: "erin",
		Role: provider.RoleAuditor, ExpiresAt: time.Now().Add(24 * time.Hour), AllowedCIDRs: []string{}, Reason: "x"}); !errors.Is(err, provider.ErrAPIKeyCreatorNotActive) {
		t.Fatalf("non-super-admin creator: %v", err)
	}
}
