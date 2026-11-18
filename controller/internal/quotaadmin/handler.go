// Package quotaadmin serves the provider's quota override API (Sprint 23 S3,
// F12; Decision Record D-47). It is separate from internal/quota, which must
// stay free of net/http (providerquery reads usage through it, D-04).
package quotaadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/quota"
)

//	PUT /provider/tenants/{id}/quotas/{key}  super-admin
//	    {"limit": <n> | null, "reason": "…"}   set the override (null = unlimited)
//	    {"inherit": true,     "reason": "…"}   remove it (the plan's value applies)
//	    → 204
//
// The write commits with its provider audit row (before/after: limit and
// source, F17). Lowering a limit never deletes anything (D-47). Errors are
// {"error":"<code>"}: forbidden, not_found, unknown_key, invalid_limit,
// limit_required, reason_required, reason_too_long, invalid_request.

// MaxReasonRunes bounds a reason (same as every provider write).
const MaxReasonRunes = 500

// Handler serves the quota override API.
type Handler struct {
	Pool          *pgxpool.Pool
	Authz         *provider.Authz
	ProviderStore *provider.Store
}

type setRequest struct {
	// Limit is raw so "absent" and null can be told apart.
	Limit   json.RawMessage `json:"limit"`
	Inherit bool            `json:"inherit"`
	Reason  string          `json:"reason"`
}

// Set: PUT /provider/tenants/{id}/quotas/{key}.
func (h *Handler) Set(w http.ResponseWriter, r *http.Request) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("quota admin: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	workspaceID, key := r.PathValue("id"), r.PathValue("key")
	if err := h.Authz.CanSetTenantQuota(actor, provider.Target{Type: "tenant", ID: workspaceID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	if _, err := uuid.Parse(workspaceID); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	}
	if !quota.ValidKey(key) {
		provider.WriteError(w, http.StatusNotFound, "unknown_key")
		return
	}
	var req setRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	o, code := parseOverride(req)
	if code != "" {
		provider.WriteError(w, http.StatusBadRequest, code)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		provider.WriteError(w, http.StatusBadRequest, "reason_required")
		return
	}
	if utf8.RuneCountInString(reason) > MaxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := quota.SetOverride(ctx, h.Pool, workspaceID, key, o, reason, actor.Email,
		func(ctx context.Context, tx pgx.Tx, before, after quota.Limit) error {
			return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderUserID: &actor.UserID,
				ProviderEmail:  actor.Email,
				Action:         provider.ActionTenantQuotaSet,
				TargetType:     "tenant",
				TargetID:       workspaceID,
				Details:        provider.WithChange(map[string]any{"key": key, "reason": reason}, before.Audit(), after.Audit()),
				IPAddress:      provider.ClientIP(r),
			})
		})
	switch {
	case errors.Is(err, quota.ErrWorkspaceNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, quota.ErrInvalidLimit):
		provider.WriteError(w, http.StatusBadRequest, "invalid_limit")
		return
	case err != nil:
		log.Printf("quota admin: set %s for tenant %s: %v", key, workspaceID, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseOverride turns the body into an Override, or an error code.
func parseOverride(req setRequest) (quota.Override, string) {
	if req.Inherit {
		if len(req.Limit) != 0 {
			return quota.Override{}, "invalid_request" // inherit and a limit together
		}
		return quota.Override{Inherit: true}, ""
	}
	raw := bytes.TrimSpace(req.Limit)
	if len(raw) == 0 {
		return quota.Override{}, "limit_required"
	}
	if bytes.Equal(raw, []byte("null")) {
		return quota.Override{}, "" // explicitly unlimited
	}
	// Only a bare JSON integer: not a string ("3" would decode into a
	// json.Number), no fraction or exponent, not negative.
	for _, c := range raw {
		if c < '0' || c > '9' {
			return quota.Override{}, "invalid_limit"
		}
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return quota.Override{}, "invalid_limit"
	}
	return quota.Override{Limit: &n}, ""
}
