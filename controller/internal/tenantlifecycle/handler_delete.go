package tenantlifecycle

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Provider tenant deletion API (Sprint 23 S4, D-46):
//
//	POST /provider/tenants/{id}/delete         super-admin {reason, confirm_slug} → 200 {delete_after}
//	POST /provider/tenants/{id}/delete/cancel  super-admin {reason}               → 204
//	PUT  /provider/tenants/{id}/legal-hold     super-admin {on, reason}           → 204
//
// Delete only for a suspended tenant (D-10), with the typed slug (D-17).
// Each write commits with its provider audit row (before/after). Scheduling
// emails each tenant admin a link to download the tenant's own logs until
// the purge; the link is never returned, logged or audited. Extra error
// codes: delete_scheduled, delete_not_scheduled, purge_started,
// legal_hold_unchanged.

type deleteRequest struct {
	Reason      string `json:"reason"`
	ConfirmSlug string `json:"confirm_slug"`
}

type legalHoldRequest struct {
	On     *bool  `json:"on"`
	Reason string `json:"reason"`
}

func (h *Handler) grace() time.Duration {
	if h.DeleteGrace > 0 {
		return h.DeleteGrace
	}
	return DefaultDeleteGrace
}

func (h *Handler) deleteAudit(r *http.Request, actor provider.Actor, action, workspaceID, reason string) DeleteInTx {
	return func(ctx context.Context, tx pgx.Tx, before, after Deletion) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         action,
			TargetType:     "tenant",
			TargetID:       workspaceID,
			Details:        provider.WithChange(map[string]any{"reason": reason}, before.Audit(), after.Audit()),
			IPAddress:      provider.ClientIP(r),
		})
	}
}

// Delete: POST /provider/tenants/{id}/delete {reason, confirm_slug}.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	actor, workspaceID, ok := h.begin(w, r, h.Authz.CanDeleteTenant)
	if !ok {
		return
	}
	var req deleteRequest
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
	res, err := h.Store.ScheduleDelete(ctx, workspaceID, req.ConfirmSlug, actor.Email, reason, h.grace(),
		h.deleteAudit(r, actor, provider.ActionTenantDelete, workspaceID, reason))
	if err != nil {
		writeError(w, "schedule delete of tenant "+workspaceID, err)
		return
	}
	if h.Mailer != nil {
		for _, l := range res.Links {
			l := l
			go func() {
				link := strings.TrimSuffix(h.AppBaseURL, "/") + "/tenant-export#token=" + l.Token
				body := fmt.Sprintf("Your Zecurity workspace is suspended and scheduled for deletion on %s.\r\n\r\n"+
					"Until then you can download your workspace's logs (admin audit log, access log and certificate history):\r\n%s\r\n\r\n"+
					"The link works until the deletion. After that, the workspace and its data can't be recovered.",
					l.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), link)
				if err := h.Mailer.SendPlain(l.Email, "Your Zecurity workspace is scheduled for deletion", body); err != nil {
					log.Printf("tenant delete %s: email the download link to an admin: %v", workspaceID, err)
				}
			}()
		}
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"delete_after": res.After.DeleteAfter.UTC()})
}

// CancelDelete: POST /provider/tenants/{id}/delete/cancel {reason}.
func (h *Handler) CancelDelete(w http.ResponseWriter, r *http.Request) {
	actor, workspaceID, ok := h.begin(w, r, h.Authz.CanCancelTenantDelete)
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
	if _, _, err := h.Store.CancelDelete(ctx, workspaceID,
		h.deleteAudit(r, actor, provider.ActionTenantDeleteCancel, workspaceID, reason)); err != nil {
		writeError(w, "cancel delete of tenant "+workspaceID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// LegalHold: PUT /provider/tenants/{id}/legal-hold {on, reason}.
func (h *Handler) LegalHold(w http.ResponseWriter, r *http.Request) {
	actor, workspaceID, ok := h.begin(w, r, h.Authz.CanSetTenantLegalHold)
	if !ok {
		return
	}
	var req legalHoldRequest
	if !provider.DecodeJSON(w, r, &req) || req.On == nil {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, _, err := h.Store.SetLegalHold(ctx, workspaceID, *req.On, actor.Email, reason,
		h.deleteAudit(r, actor, provider.ActionTenantLegalHoldSet, workspaceID, reason)); err != nil {
		writeError(w, "legal hold of tenant "+workspaceID, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
