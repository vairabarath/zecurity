package tenantlifecycle

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Provider tenant lifecycle API (Sprint 23 S1, D-45):
//
//	POST /provider/tenants/{id}/suspend  super-admin {reason, confirm_slug} → 204
//	POST /provider/tenants/{id}/resume   super-admin {reason}               → 204
//
// The reason is mandatory for both (D-17 extended: tenant-affecting writes),
// and suspend also needs the tenant's slug typed back (checked by the server).
// Each write commits with its provider audit row (before/after, F17); the
// reason is in the body and the audit, never in the URL. Errors are
// {"error":"<code>"}: reason_required, reason_too_long, confirmation_mismatch,
// already_suspended, not_suspended, not_found, forbidden.

// MaxReasonRunes bounds a reason (same bound as the relay writes).
const MaxReasonRunes = 500

// fanoutTimeout bounds the post-commit work (session revocation, ACL push,
// relay lists). It runs detached from the request: the state is committed.
const fanoutTimeout = 30 * time.Second

// SessionInvalidator ends every server-side session of a user and revokes
// the client devices enrolled under them (auth.Service implements it).
type SessionInvalidator interface {
	InvalidateUserSessions(ctx context.Context, userID string) error
}

// PolicyNotifier recompiles and pushes a workspace's ACL (policy.Notifier).
type PolicyNotifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
}

// Handler serves the suspend and resume endpoints.
type Handler struct {
	Store         *Store
	Authz         *provider.Authz
	ProviderStore *provider.Store
	Sessions      SessionInvalidator
	Policy        PolicyNotifier
	// PublishRelayLists rebuilds and broadcasts the relay lists (a
	// suspended tenant's is empty). Optional.
	PublishRelayLists func(ctx context.Context) error

	// S4 (D-46): the grace between scheduling and the purge (default
	// DefaultDeleteGrace), and the emailer and admin-UI base URL for the
	// download link sent to the tenant's admins. Mailer is optional.
	DeleteGrace time.Duration
	Mailer      Mailer
	AppBaseURL  string
}

// Mailer sends one plain-text email (invitation.Emailer.SendPlain).
type Mailer interface {
	SendPlain(to, subject, body string) error
}

type suspendRequest struct {
	Reason      string `json:"reason"`
	ConfirmSlug string `json:"confirm_slug"`
}

type resumeRequest struct {
	Reason string `json:"reason"`
}

// Suspend: POST /provider/tenants/{id}/suspend {reason, confirm_slug}.
func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	actor, workspaceID, ok := h.begin(w, r, h.Authz.CanSuspendTenant)
	if !ok {
		return
	}
	var req suspendRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := h.Store.Suspend(ctx, workspaceID, req.ConfirmSlug, actor.Email, reason,
		func(ctx context.Context, tx pgx.Tx, before, after Suspension) error {
			return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderUserID: &actor.UserID,
				ProviderEmail:  actor.Email,
				Action:         provider.ActionTenantSuspend,
				TargetType:     "tenant",
				TargetID:       workspaceID,
				Details:        provider.WithChange(map[string]any{"reason": reason}, before.Audit(), after.Audit()),
				IPAddress:      provider.ClientIP(r),
			})
		})
	if err != nil {
		writeError(w, "suspend tenant "+workspaceID, err)
		return
	}
	// Right after commit (D-45): end every session, push deny-all, publish
	// the (empty) relay list. Detached from the request: it's committed.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(r.Context()), fanoutTimeout)
	defer fcancel()
	if h.Sessions != nil {
		for _, userID := range res.UserIDs {
			if err := h.Sessions.InvalidateUserSessions(fctx, userID); err != nil {
				// The generation bump already revokes at the next refresh,
				// and refresh is refused while suspended.
				log.Printf("tenant suspend %s: end sessions of user %s: %v", workspaceID, userID, err)
			}
		}
	}
	h.afterChange(fctx, workspaceID, "suspend")
	w.WriteHeader(http.StatusNoContent)
}

// Resume: POST /provider/tenants/{id}/resume {reason}.
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	actor, workspaceID, ok := h.begin(w, r, h.Authz.CanResumeTenant)
	if !ok {
		return
	}
	var req resumeRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	_, _, err := h.Store.Resume(ctx, workspaceID,
		func(ctx context.Context, tx pgx.Tx, before, after Suspension) error {
			return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderUserID: &actor.UserID,
				ProviderEmail:  actor.Email,
				Action:         provider.ActionTenantResume,
				TargetType:     "tenant",
				TargetID:       workspaceID,
				Details:        provider.WithChange(map[string]any{"reason": reason}, before.Audit(), after.Audit()),
				IPAddress:      provider.ClientIP(r),
			})
		})
	if err != nil {
		writeError(w, "resume tenant "+workspaceID, err)
		return
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(r.Context()), fanoutTimeout)
	defer fcancel()
	h.afterChange(fctx, workspaceID, "resume")
	w.WriteHeader(http.StatusNoContent)
}

// begin authorizes the actor (server-side, D-06) and validates the path id.
func (h *Handler) begin(w http.ResponseWriter, r *http.Request,
	can func(provider.Actor, provider.Target) error) (provider.Actor, string, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("tenant lifecycle: %s without provider actor (wiring bug)", r.URL.Path)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return provider.Actor{}, "", false
	}
	workspaceID := r.PathValue("id")
	if err := can(actor, provider.Target{Type: "tenant", ID: workspaceID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return provider.Actor{}, "", false
	}
	if _, err := uuid.Parse(workspaceID); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return provider.Actor{}, "", false
	}
	return actor, workspaceID, true
}

// afterChange pushes the new ACL (deny-all or normal) to the tenant's
// connectors and clients and publishes the relay lists. Best effort: the
// state is committed, and connectors converge at their next sync anyway.
func (h *Handler) afterChange(ctx context.Context, workspaceID, what string) {
	if h.Policy != nil {
		if err := h.Policy.NotifyPolicyChange(ctx, workspaceID); err != nil {
			log.Printf("tenant %s %s: push ACL: %v", what, workspaceID, err)
		}
	}
	if h.PublishRelayLists != nil {
		if err := h.PublishRelayLists(ctx); err != nil {
			log.Printf("tenant %s %s: publish relay lists: %v (connectors get the current list at the next sync or reconnect)", what, workspaceID, err)
		}
	}
}

// requireReason validates a mandatory reason (D-17 extended).
func requireReason(w http.ResponseWriter, raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		provider.WriteError(w, http.StatusBadRequest, "reason_required")
		return "", false
	}
	if utf8.RuneCountInString(reason) > MaxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return "", false
	}
	return reason, true
}

func writeError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrConfirmationMismatch):
		provider.WriteError(w, http.StatusBadRequest, "confirmation_mismatch")
	case errors.Is(err, ErrAlreadySuspended):
		provider.WriteError(w, http.StatusConflict, "already_suspended")
	case errors.Is(err, ErrNotSuspended):
		provider.WriteError(w, http.StatusConflict, "not_suspended")
	case errors.Is(err, ErrDeleteScheduled):
		provider.WriteError(w, http.StatusConflict, "delete_scheduled")
	case errors.Is(err, ErrDeleteNotScheduled):
		provider.WriteError(w, http.StatusConflict, "delete_not_scheduled")
	case errors.Is(err, ErrPurgeStarted):
		provider.WriteError(w, http.StatusConflict, "purge_started")
	case errors.Is(err, ErrLegalHoldUnchanged):
		provider.WriteError(w, http.StatusConflict, "legal_hold_unchanged")
	default:
		log.Printf("tenant lifecycle: %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}
