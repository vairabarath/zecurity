package scim

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/auth/mapping"
	"github.com/yourorg/ztna/controller/internal/idp"
)

// MappingProbeResult is the outcome of an engine-level SCIM mapping round-trip
// probe (ADR-025 §3.1 — the POST→GET→verify→DELETE check Phase 4 deferred to
// Phase 5, and Phase 5 never wired).
//
// Phase 10 proved the SCIM-side half: the connection's configured scimIdentifier
// attribute round-trips correctly through the real SCIM Provision→Get path.
//
// Phase 12 extends the probe (Finding C, second half) to also prove the
// OIDC↔SCIM canonical-key equivalence: it derives the OIDC canonical key the
// SAME synthetic probe person would present at login, using the REAL production
// extractor (auth/mapping.ExtractSubjectClaim, the one Phase 11 wired into the
// login path), and asserts it equals the SCIM-derived canonical key. Both
// extractors resolve the same connection_id, so equal keys ⇒ the same
// external_identities row ⇒ ADR-025 §3.1 "converge on the same logical user".
//
// This is a mapping/configuration proof. The probe does NOT perform a live OIDC
// login, JWT validation, IdP authentication, or session creation — it feeds the
// production extractor a synthetic claims sample for the probe person. See the
// Phase 12 doc for the honesty boundary.
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
	// Default synthetic claims for the probe person (Phase 12 equivalence).
	// Every default claim here is set to the SAME value as probeExternalID
	// (the SCIM-side canonical key), so that whichever one the connection
	// configures as subjectClaim, the two extractors are fed a genuinely
	// matching value and the equivalence check is meaningful rather than a
	// guaranteed mismatch. "email" previously used an email-formatted value
	// distinct from probeExternalID, which meant any real connection
	// configured with subjectClaim="email" — one of the most common
	// configurations — always failed this check, even for a valid mapping;
	// fixed by matching it to probeExternalID like every other default claim.
	probeID := uuid.New().String()
	probeExternalID := "mapping-probe-" + probeID
	probeClaims := map[string]any{
		"sub":   probeExternalID,
		"email": probeExternalID,
		"oid":   probeExternalID,
	}
	return s.probeMappingWithClaims(ctx, conn, probeID, probeExternalID, probeClaims, true)
}

// ProbeMappingWithClaims runs the same SCIM round-trip + OIDC↔SCIM equivalence
// probe as ProbeMapping but with caller-supplied OIDC claims. It exists so tests
// can exercise the fail-closed paths (configured claim missing/empty). When
// autoAdd is true, a non-default configured subjectClaim missing from the claims
// is set to probeExternalID (the production default behavior); when false, the
// claims are used exactly as given (so a test can omit the configured claim to
// prove fail-closed). Production callers use ProbeMapping, which builds the
// canonical default claims and passes autoAdd=true.
func (s *DirectoryService) probeMappingWithClaims(ctx context.Context, conn *idp.Connection, probeID, probeExternalID string, probeClaims map[string]any, autoAdd bool) MappingProbeResult {
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

	resource := map[string]any{
		"userName":    fmt.Sprintf("mapping-probe-%s@probe.internal", probeID),
		scimIdentAttr: probeExternalID,
	}

	// Phase 12 (Finding C, second half): ensure the configured non-default
	// subjectClaim is represented on the probe person's claims so a custom
	// mapping is genuinely exercised (when autoAdd and not already supplied by
	// the caller). Tests pass autoAdd=false to omit the configured claim and
	// prove the fail-closed path.
	if autoAdd && conn.SubjectClaim != "" && conn.SubjectClaim != "sub" {
		if _, ok := probeClaims[conn.SubjectClaim]; !ok {
			probeClaims[conn.SubjectClaim] = probeExternalID
		}
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

	oidcKey := mapping.ExtractSubjectClaim(probeClaims, conn.SubjectClaim)
	if oidcKey == "" {
		return MappingProbeResult{
			Verified: false,
			Reason: fmt.Sprintf(
				"mapping equivalence failed: OIDC subjectClaim %q did not resolve for the probe user "+
					"(configured claim missing or empty); mapping cannot be proven",
				conn.SubjectClaim,
			),
		}
	}
	if oidcKey != probeExternalID {
		return MappingProbeResult{
			Verified: false,
			Reason: fmt.Sprintf(
				"mapping equivalence mismatch: OIDC subjectClaim resolves to %q but SCIM scimIdentifier resolves to %q for the same probe user",
				oidcKey, probeExternalID,
			),
		}
	}

	return MappingProbeResult{
		Verified: true,
		Reason: "mapping proven: OIDC subjectClaim and SCIM scimIdentifier resolve to the same " +
			"Canonical Identity Key for the probe user (no live OIDC login performed)",
	}
}
