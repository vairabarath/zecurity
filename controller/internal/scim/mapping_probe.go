package scim

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/idp"
)

// MappingProbeResult is the outcome of an engine-level SCIM mapping round-trip
// probe (ADR-025 §3.1 — the POST→GET→verify→DELETE check Phase 4 deferred to
// Phase 5, and Phase 5 never wired).
//
// It proves ONLY that the connection's configured scimIdentifier attribute
// round-trips correctly through the real SCIM Provision→Get path. It does
// NOT, and cannot, prove subjectClaim correctness: scope.subjectClaim is not
// read anywhere by Provision, Get, or Deprovision, and ExtractSubjectClaim
// (mapping.go) has no production caller anywhere in this codebase today —
// subjectClaim resolution is an OIDC login-side contract (internal/auth) this
// probe does not exercise. Callers MUST NOT treat Verified==true as proof
// that subjectClaim is correct; Reason says so explicitly.
type MappingProbeResult struct {
	Verified bool
	Reason   string
}

// ProbeMapping exercises the real SCIM engine — Provision, Get, and
// (guaranteed) Deprovision — against a synthetic, disposable identity, to
// prove that the connection's configured scimIdentifier attribute genuinely
// survives canonical-key extraction and storage, using the exact same code
// path a real IdP's SCIM push would use.
//
// What this deliberately does NOT do, and why:
//
//   - It does not call Store.Mint or otherwise create/touch scim_tokens.
//     Store.Mint enforces the ≤2-active-token dual-rotation limit
//     (maxActiveTokens, token_store.go) and will silently revoke the
//     connection's oldest LIVE token via applyThirdTokenRule if two are
//     already active. Minting a throwaway probe token risks revoking a real
//     customer credential as a side effect of clicking "test connection" —
//     unacceptable. The probe never mints, rotates, or revokes any token.
//   - It does not go through AuthMiddleware or the public /scim/v2 HTTP
//     router. That router's resolveScope is fail-closed on !conn.ScimEnabled
//     (directory_service.go), which is exactly the state a pre-enable probe
//     runs in — the public endpoint cannot be honestly driven here without
//     first flipping scim_enabled, which would mutate live connection state.
//     This probe therefore proves mapping/CRUD correctness only; it does not
//     exercise bearer-token authentication or the HTTP layer at all.
//
// The scope it builds mirrors exactly what resolveScope produces on success
// (same fields, same values, read from the same *idp.Connection), skipping
// only the two gate checks (!conn.ScimEnabled, conn.Status != "active") that
// exist specifically to block writes before the mapping is proven — the
// state this probe is invoked to resolve.
func (s *DirectoryService) ProbeMapping(ctx context.Context, conn *idp.Connection) MappingProbeResult {
	workspaceID := conn.TenantIDOrEmpty()
	if workspaceID == "" {
		return MappingProbeResult{
			Verified: false,
			Reason:   "mapping probe skipped: connection has no workspace (platform-global connections are not SCIM-eligible)",
		}
	}

	sc := &scope{
		workspaceID:  workspaceID,
		connectionID: conn.ID,
		provider:     conn.Provider,
		issuer:       conn.Issuer,
		subjectClaim: conn.SubjectClaim,
		scimIdent:    conn.ScimIdentifier,
		scimEnabled:  true, // probe-local value only; never persisted, never used to satisfy resolveScope's real gate
	}

	scimIdentAttr := conn.ScimIdentifier
	if scimIdentAttr == "" {
		scimIdentAttr = DefaultScimIdentifier
	}

	probeID := uuid.New().String()
	probeExternalID := "mapping-probe-" + probeID
	resource := map[string]any{
		"userName":    fmt.Sprintf("mapping-probe-%s@probe.internal", probeID),
		scimIdentAttr: probeExternalID,
	}

	syncInst, err := s.EnsureSyncInstance(ctx, sc)
	if err != nil {
		return MappingProbeResult{Verified: false, Reason: "mapping probe setup failed: " + err.Error()}
	}

	res, serr := s.Provision(ctx, sc, resource, syncInst)
	if serr != nil {
		return MappingProbeResult{Verified: false, Reason: "mapping probe provision failed: " + serr.Detail}
	}
	userID := res.user.ID

	// Cleanup is attempted on every exit path (Get failure, mapping
	// mismatch, success, or a panic) — Deprovision runs even when the
	// verdict below is computed first.
	//
	// Known limitation, documented rather than silently accepted: cleanup
	// itself depends on this Deprovision call succeeding. It is intentionally
	// best-effort/log-only here (matching every other best-effort cleanup
	// path in this package, e.g. Revoker.AfterBump) — a cleanup failure is
	// logged and never overrides the mapping verdict already computed below,
	// which reflects only Provision+Get+comparison success, not cleanup
	// success. If Deprovision fails here for a transient/database reason
	// (not "user already gone"), the probe's Verified result can still be
	// true while a residual probe user is left behind until the next
	// successful probe or a manual cleanup. This is an accepted limitation
	// of the current design, not a silent bug — it was not "fixed" by
	// retrying or by flipping Verified to false on cleanup failure, since
	// that would conflate "is the mapping proven" (Provision/Get, already
	// answered) with "did cleanup succeed" (an orthogonal, best-effort
	// concern), which is not this phase's scope to redesign.
	defer func() {
		if derr := s.Deprovision(ctx, sc, userID, true, syncInst); derr != nil {
			fmt.Printf("mapping probe: cleanup deprovision failed for probe user %s (workspace=%s connection=%s): %s — a residual probe user may remain\n",
				userID, sc.workspaceID, sc.connectionID, derr.Detail)
		}
	}()

	got, gerr := s.Get(ctx, sc, userID)
	if gerr != nil {
		return MappingProbeResult{Verified: false, Reason: "mapping probe get failed: " + gerr.Detail}
	}
	if got.ExternalID != probeExternalID {
		return MappingProbeResult{
			Verified: false,
			Reason: fmt.Sprintf(
				"scimIdentifier round-trip mismatch: sent %q via attribute %q, got %q back",
				probeExternalID, scimIdentAttr, got.ExternalID,
			),
		}
	}

	return MappingProbeResult{
		Verified: true,
		Reason: "scimIdentifier round-trip verified via SCIM Provision→Get " +
			"(subjectClaim is an OIDC login-side contract not exercised by this probe)",
	}
}
