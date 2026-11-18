// Package tenantcreate serves provider tenant creation (Sprint 23 S2, F7,
// D-44):
//
//	POST /provider/tenants {slug, name, plan_id?, admin_email, reason} → 201
//	POST /provider/tenants/{id}/first-admin-invitation {admin_email?, reason} → 201
//
// Super-admin only (tenant.create, D-06, checked in decide()). The workspace
// is created through the same bootstrap as self-signup (bootstrap.CreateTenant)
// with a single-use, time-limited first-admin invitation; the provider audit
// row (provider actor, reason, invitation operation; never the token) commits
// in the same transaction. The token is only ever put in the invitation email;
// it is never logged, audited or returned.
package tenantcreate

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
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/quota"
)

const maxReasonRunes = 500

// Creator is bootstrap.Service's tenant creation.
type Creator interface {
	CreateTenant(ctx context.Context, in bootstrap.CreateTenantInput, auditFn bootstrap.CreateTenantAudit) (*bootstrap.CreatedTenant, error)
	ReissueFirstAdminInvitation(ctx context.Context, workspaceID, adminEmail string, auditFn bootstrap.ReissueAudit) (*bootstrap.ReissuedInvitation, error)
}

// InvitationSender emails the invitation link (invitation.Emailer).
type InvitationSender interface {
	SendInvitation(inv *invitation.Invitation, workspaceName string) error
}

// Handler serves POST /provider/tenants.
type Handler struct {
	Creator       Creator
	Authz         *provider.Authz
	ProviderStore *provider.Store
	Sender        InvitationSender
}

type createRequest struct {
	Slug       string  `json:"slug"`
	Name       string  `json:"name"`
	PlanID     *string `json:"plan_id"`
	AdminEmail string  `json:"admin_email"`
	Reason     string  `json:"reason"`
}

type invitationJSON struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TenantCreatedJSON is the 201 body. It carries no token.
type TenantCreatedJSON struct {
	ID          string         `json:"id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	TrustDomain string         `json:"trust_domain"`
	PlanID      string         `json:"plan_id"`
	PlanName    string         `json:"plan_name"`
	Invitation  invitationJSON `json:"invitation"`
}

func reasonFrom(w http.ResponseWriter, raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	if reason == "" {
		provider.WriteError(w, http.StatusBadRequest, "reason_required")
		return "", false
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return "", false
	}
	return reason, true
}

func (h *Handler) send(inv *invitation.Invitation, workspaceID, name string) {
	if h.Sender == nil {
		return
	}
	c := *inv
	go func() {
		if err := h.Sender.SendInvitation(&c, name); err != nil {
			log.Printf("tenantcreate: first-admin invitation email for workspace %s: %v", workspaceID, err)
		}
	}()
}

type reinviteRequest struct {
	AdminEmail string `json:"admin_email"`
	Reason     string `json:"reason"`
}

// ReinviteJSON is the 201 body of a re-issue. It carries no token.
type ReinviteJSON struct {
	TenantID   string         `json:"tenant_id"`
	Invitation invitationJSON `json:"invitation"`
}

// Reinvite: POST /provider/tenants/{id}/first-admin-invitation
// {admin_email?, reason}. A new single-use, 7-day first-admin invitation for
// a tenant whose first admin never signed in; earlier links stop working.
// admin_email omitted re-sends to the previous invitee.
func (h *Handler) Reinvite(w http.ResponseWriter, r *http.Request) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("tenantcreate: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	if err := h.Authz.CanReinviteFirstAdmin(actor, provider.Target{Type: "tenant", ID: id}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req reinviteRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := reasonFrom(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := h.Creator.ReissueFirstAdminInvitation(ctx, id, req.AdminEmail, func(ctx context.Context, tx pgx.Tx, ri *bootstrap.ReissuedInvitation) error {
		before := map[string]any{"invitee_email": ri.PreviousEmail, "revoked_invitation_ids": ri.RevokedIDs}
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionTenantFirstAdminReinvite,
			TargetType:     "tenant",
			TargetID:       id,
			Details: provider.WithChange(
				map[string]any{
					"reason": reason,
					// The invitation operation, never its token (D-44).
					"invitation": map[string]any{
						"operation":  "first_admin.reinvite",
						"id":         ri.Invitation.ID,
						"email":      ri.Invitation.Email,
						"role":       "admin",
						"expires_at": ri.Invitation.ExpiresAt.UTC().Format(time.RFC3339),
					},
				},
				before,
				map[string]any{"invitee_email": ri.Invitation.Email, "invitation_id": ri.Invitation.ID}),
			IPAddress: provider.ClientIP(r),
		})
	})
	if err != nil {
		writeError(w, err)
		return
	}
	h.send(res.Invitation, id, res.WorkspaceName)
	provider.WriteJSON(w, http.StatusCreated, ReinviteJSON{
		TenantID:   id,
		Invitation: invitationJSON{ID: res.Invitation.ID, Email: res.Invitation.Email, ExpiresAt: res.Invitation.ExpiresAt.UTC()},
	})
}

// Create: POST /provider/tenants.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("tenantcreate: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := h.Authz.CanCreateTenant(actor); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req createRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := reasonFrom(w, req.Reason)
	if !ok {
		return
	}
	if req.PlanID != nil {
		if _, err := uuid.Parse(*req.PlanID); err != nil {
			provider.WriteError(w, http.StatusBadRequest, "plan_not_found")
			return
		}
	}

	in := bootstrap.CreateTenantInput{Slug: req.Slug, Name: req.Name, PlanID: req.PlanID, AdminEmail: req.AdminEmail}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	t, err := h.Creator.CreateTenant(ctx, in, func(ctx context.Context, tx pgx.Tx, t *bootstrap.CreatedTenant) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionTenantCreate,
			TargetType:     "tenant",
			TargetID:       t.WorkspaceID,
			Details: provider.WithChange(
				map[string]any{
					"reason": reason,
					// The invitation operation, never its token (D-44).
					"invitation": map[string]any{
						"operation":  "first_admin.invite",
						"id":         t.Invitation.ID,
						"email":      t.AdminEmail,
						"role":       "admin",
						"expires_at": t.Invitation.ExpiresAt.UTC().Format(time.RFC3339),
					},
				},
				nil,
				map[string]any{
					"slug": t.Slug, "name": t.Name, "trust_domain": t.TrustDomain,
					"plan_id": t.PlanID, "plan_name": t.PlanName,
				}),
			IPAddress: provider.ClientIP(r),
		})
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// Best-effort, after commit (as for a tenant admin's invitation): the
	// invitee can also simply sign in with the invited email.
	h.send(t.Invitation, t.WorkspaceID, t.Name)

	provider.WriteJSON(w, http.StatusCreated, TenantCreatedJSON{
		ID: t.WorkspaceID, Slug: t.Slug, Name: t.Name, TrustDomain: t.TrustDomain,
		PlanID: t.PlanID, PlanName: t.PlanName,
		Invitation: invitationJSON{ID: t.Invitation.ID, Email: t.AdminEmail, ExpiresAt: t.Invitation.ExpiresAt.UTC()},
	})
}

func writeError(w http.ResponseWriter, err error) {
	// Sprint 23 S3 (D-47): the first-admin invitation is over the users
	// quota (409 quota_exceeded {key, limit, used}).
	if e, ok := quota.AsExceeded(err); ok {
		provider.WriteJSON(w, quota.HTTPStatus, e.Body())
		return
	}
	switch {
	case errors.Is(err, bootstrap.ErrInvalidSlug):
		provider.WriteError(w, http.StatusBadRequest, "invalid_slug")
	case errors.Is(err, bootstrap.ErrInvalidName):
		provider.WriteError(w, http.StatusBadRequest, "invalid_name")
	case errors.Is(err, bootstrap.ErrInvalidEmail):
		provider.WriteError(w, http.StatusBadRequest, "invalid_email")
	case errors.Is(err, bootstrap.ErrPlanNotFound):
		provider.WriteError(w, http.StatusBadRequest, "plan_not_found")
	case errors.Is(err, bootstrap.ErrSlugTaken):
		provider.WriteError(w, http.StatusConflict, "slug_taken")
	case errors.Is(err, bootstrap.ErrTenantNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, bootstrap.ErrTenantNotActive):
		provider.WriteError(w, http.StatusConflict, "tenant_not_active")
	case errors.Is(err, bootstrap.ErrFirstAdminExists):
		provider.WriteError(w, http.StatusConflict, "first_admin_exists")
	case errors.Is(err, bootstrap.ErrEmailHasAccount):
		provider.WriteError(w, http.StatusConflict, "email_has_account")
	default:
		log.Printf("tenantcreate: create: %v", err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}
