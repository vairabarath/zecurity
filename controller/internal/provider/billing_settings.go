package provider

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/billing"
)

// Billing settings API (Sprint 26 P1b, F34; D-64, D-67, D-68, D-69, D-70).
// Billing is tenant data: super-admin and billing write, auditor reads too
// (D-73); relay-ops and support 403, by decide().
//
//	GET   /provider/billing/settings  (plan.read)  → settings + countries
//	PATCH /provider/billing/settings  (billing.settings.write)
//	      {country?, currency?, tax_rate_bp?, tax_label?, invoice_retention_years?,
//	       email_tenants?, reason} → 200 settings + switched_cards
//
// A currency change switches every priced plan card in the same transaction
// (each gets a revision); amounts are not converted (D-64). The write
// commits with one billing.settings.update audit row (reason, before/after,
// and the cards it switched).

// BillingSettingsHandler serves the billing settings API.
type BillingSettingsHandler struct {
	DB    *pgxpool.Pool
	Authz *Authz
	Store *Store
}

type billingSettingsResponse struct {
	billing.Settings
	Countries     []billing.Country    `json:"countries"`
	SwitchedCards []billing.CardSwitch `json:"switched_cards,omitempty"`
}

// Get: GET /provider/billing/settings.
func (h *BillingSettingsHandler) Get(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := h.Authz.CanReadBillingSettings(actor); err != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	s, err := billing.GetSettings(r.Context(), h.DB)
	if err != nil {
		log.Printf("billing settings: read: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	WriteJSON(w, http.StatusOK, billingSettingsResponse{Settings: s, Countries: billing.Countries})
}

type billingSettingsPatch struct {
	Country               *string `json:"country"`
	Currency              *string `json:"currency"`
	TaxRateBP             *int    `json:"tax_rate_bp"`
	TaxLabel              *string `json:"tax_label"`
	InvoiceRetentionYears *int    `json:"invoice_retention_years"`
	EmailTenants          *bool   `json:"email_tenants"`
	Reason                string  `json:"reason"`
}

// Update: PATCH /provider/billing/settings.
func (h *BillingSettingsHandler) Update(w http.ResponseWriter, r *http.Request) {
	actor, ok := ActorFromContext(r.Context())
	if !ok {
		WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if err := h.Authz.CanWriteBillingSettings(actor); err != nil {
		WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req billingSettingsPatch
	if !DecodeJSON(w, r, &req) {
		WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	switch {
	case reason == "":
		WriteError(w, http.StatusBadRequest, "reason_required")
		return
	case utf8.RuneCountInString(reason) > 500:
		WriteError(w, http.StatusBadRequest, "reason_too_long")
		return
	}
	patch := billing.SettingsPatch{
		Country: req.Country, Currency: req.Currency, TaxRateBP: req.TaxRateBP, TaxLabel: req.TaxLabel,
		InvoiceRetentionYears: req.InvoiceRetentionYears, EmailTenants: req.EmailTenants,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		after    billing.Settings
		switched []billing.CardSwitch
	)
	err := pgx.BeginFunc(ctx, h.DB, func(tx pgx.Tx) error {
		before, a, sw, changed, err := billing.UpdateSettings(ctx, tx, patch, actor.Email, reason)
		if err != nil {
			return err
		}
		after, switched = a, sw
		if !changed {
			return nil
		}
		details := map[string]any{"reason": reason}
		if before.Currency != after.Currency {
			details["switched_cards"] = switched // ids, names, new revisions; amounts not converted
		}
		userID := actor.UserID
		return h.Store.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderUserID: &userID,
			ProviderEmail:  actor.Email,
			Action:         ActionBillingSettingsUpdate,
			TargetType:     "billing_settings",
			TargetID:       "billing_settings",
			Details:        WithChange(details, before.AuditView(), after.AuditView()),
			IPAddress:      clientIP(r),
		})
	})
	switch {
	case errors.Is(err, billing.ErrInvalidCountry):
		WriteError(w, http.StatusBadRequest, "invalid_country")
	case errors.Is(err, billing.ErrInvalidCurrency):
		WriteError(w, http.StatusBadRequest, "invalid_currency")
	case errors.Is(err, billing.ErrInvalidSettings):
		WriteError(w, http.StatusBadRequest, "invalid_settings")
	case err != nil:
		log.Printf("billing settings: update: %v", err)
		WriteError(w, http.StatusInternalServerError, "server_error")
	default:
		WriteJSON(w, http.StatusOK, billingSettingsResponse{Settings: after, Countries: billing.Countries, SwitchedCards: switched})
	}
}
