package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Channel API (Sprint 24 A2, D-52, D-53). Super-admin only; every write
// needs a reason and is audited with before/after (never the secret or the
// full URL). The signing secret is returned only by create and
// rotate-secret, once, with Cache-Control: no-store.
//
//	GET    /provider/alert-channels                     → {"items":[Channel]}
//	POST   /provider/alert-channels                     {name, type, scope_filter?, min_severity?, enabled?, url?, recipients?, reason} → 201 {"channel", "secret"?}
//	PATCH  /provider/alert-channels/{id}                {name?, scope_filter?, min_severity?, enabled?, url?, recipients?, reason} → 200 Channel
//	DELETE /provider/alert-channels/{id}                {reason} → 204
//	POST   /provider/alert-channels/{id}/test           {reason} → 200 TestResult
//	POST   /provider/alert-channels/{id}/rotate-secret  {reason} → 200 {"secret"}
//	GET    /provider/alert-events/{id}/deliveries       → {"items":[Delivery]}

// ChannelHandler serves the channel API.
type ChannelHandler struct {
	Channels      *ChannelStore
	Deliverer     *Deliverer
	Authz         *provider.Authz
	ProviderStore *provider.Store
}

func (h *ChannelHandler) actorFor(w http.ResponseWriter, r *http.Request, action, id string) (provider.Actor, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("alerting: request without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return actor, false
	}
	if err := h.Authz.CanManageAlertChannels(actor, action, provider.Target{Type: "alert_channel", ID: id}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return actor, false
	}
	return actor, true
}

func writeChannelError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, ErrChannelNameTaken):
		provider.WriteError(w, http.StatusConflict, "channel_name_taken")
	case errors.Is(err, ErrInvalidURL):
		provider.WriteError(w, http.StatusBadRequest, "invalid_url")
	case errors.Is(err, ErrInvalidChannel):
		provider.WriteError(w, http.StatusBadRequest, "invalid_channel")
	default:
		log.Printf("alerting: channel %s: %v", what, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
	}
}

func (h *ChannelHandler) audit(r *http.Request, actor provider.Actor, action, reason string, extra map[string]any) ChannelAuditFunc {
	return func(ctx context.Context, tx pgx.Tx, before, after *Channel) error {
		var b, a map[string]any
		id := ""
		if before != nil {
			b, id = before.AuditView(), before.ID
		}
		if after != nil {
			a, id = after.AuditView(), after.ID
		}
		details := map[string]any{"reason": reason}
		for k, v := range extra {
			details[k] = v
		}
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID, ProviderEmail: actor.Email, Action: action,
			TargetType: "alert_channel", TargetID: id,
			Details:   provider.WithChange(details, b, a),
			IPAddress: provider.ClientIP(r),
		})
	}
}

// channelRequest is a create or update body. Recipients is raw so that an
// explicit null ("every super-admin") differs from leaving it out.
type channelRequest struct {
	Name        *string         `json:"name"`
	Type        string          `json:"type"`
	ScopeFilter *string         `json:"scope_filter"`
	MinSeverity *string         `json:"min_severity"`
	Enabled     *bool           `json:"enabled"`
	URL         *string         `json:"url"`
	Recipients  json.RawMessage `json:"recipients"`
	Reason      string          `json:"reason"`
}

func (req channelRequest) input() (ChannelInput, error) {
	in := ChannelInput{Name: req.Name, Type: req.Type, ScopeFilter: req.ScopeFilter, MinSeverity: req.MinSeverity,
		Enabled: req.Enabled, URL: req.URL}
	raw := bytes.TrimSpace(req.Recipients)
	switch {
	case len(raw) == 0:
	case bytes.Equal(raw, []byte("null")):
		var none []string
		in.Recipients = &none
	default:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return ChannelInput{}, ErrInvalidChannel
		}
		if list == nil {
			list = []string{}
		}
		in.Recipients = &list
	}
	return in, nil
}

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

// List: GET /provider/alert-channels.
func (h *ChannelHandler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actorFor(w, r, provider.ActionAlertChannelRead, ""); !ok {
		return
	}
	list, err := h.Channels.ListChannels(r.Context())
	if err != nil {
		writeChannelError(w, "list", err)
		return
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

// Create: POST /provider/alert-channels.
func (h *ChannelHandler) Create(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actorFor(w, r, provider.ActionAlertChannelCreate, "")
	if !ok {
		return
	}
	var req channelRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	in, err := req.input()
	if err != nil {
		writeChannelError(w, "create", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ch, secret, err := h.Channels.CreateChannel(ctx, in, h.audit(r, actor, provider.ActionAlertChannelCreate, reason, nil))
	if err != nil {
		writeChannelError(w, "create", err)
		return
	}
	noStore(w)
	body := map[string]any{"channel": ch}
	if secret != "" {
		body["secret"] = secret
	}
	provider.WriteJSON(w, http.StatusCreated, body)
}

// Update: PATCH /provider/alert-channels/{id}.
func (h *ChannelHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := h.actorFor(w, r, provider.ActionAlertChannelUpdate, id)
	if !ok {
		return
	}
	var req channelRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	in, err := req.input()
	if err != nil {
		writeChannelError(w, "update", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ch, err := h.Channels.UpdateChannel(ctx, id, in, h.audit(r, actor, provider.ActionAlertChannelUpdate, reason, nil))
	if err != nil {
		writeChannelError(w, "update", err)
		return
	}
	provider.WriteJSON(w, http.StatusOK, ch)
}

// Delete: DELETE /provider/alert-channels/{id}.
func (h *ChannelHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := h.actorFor(w, r, provider.ActionAlertChannelDelete, id)
	if !ok {
		return
	}
	var req reasonRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.Channels.DeleteChannel(ctx, id, h.audit(r, actor, provider.ActionAlertChannelDelete, reason, nil)); err != nil {
		writeChannelError(w, "delete", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Rotate: POST /provider/alert-channels/{id}/rotate-secret.
func (h *ChannelHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := h.actorFor(w, r, provider.ActionAlertChannelRotate, id)
	if !ok {
		return
	}
	var req reasonRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	secret, err := h.Channels.RotateSecret(ctx, id, h.audit(r, actor, provider.ActionAlertChannelRotate, reason,
		map[string]any{"secret_rotated": true}))
	if err != nil {
		writeChannelError(w, "rotate", err)
		return
	}
	noStore(w)
	provider.WriteJSON(w, http.StatusOK, map[string]any{"secret": secret})
}

// Test: POST /provider/alert-channels/{id}/test. Sends now; audited with the
// outcome (never the secret or URL).
func (h *ChannelHandler) Test(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	actor, ok := h.actorFor(w, r, provider.ActionAlertChannelTest, id)
	if !ok {
		return
	}
	var req reasonRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := h.Deliverer.SendTest(ctx, id)
	if err != nil {
		writeChannelError(w, "test", err)
		return
	}
	if err := h.ProviderStore.InsertAudit(ctx, provider.AuditEntry{
		ProviderUserID: &actor.UserID, ProviderEmail: actor.Email, Action: provider.ActionAlertChannelTest,
		TargetType: "alert_channel", TargetID: id,
		Details:   map[string]any{"reason": reason, "delivered": res.Delivered, "http_status": res.HTTPStatus, "error": res.Error},
		IPAddress: provider.ClientIP(r),
	}); err != nil {
		log.Printf("alerting: audit channel test: %v", err)
	}
	provider.WriteJSON(w, http.StatusOK, res)
}

// Delivery is one attempt to deliver an event over a channel.
type Delivery struct {
	ChannelID   string    `json:"channel_id"`
	ChannelName string    `json:"channel_name"`
	Attempt     int       `json:"attempt"`
	Status      string    `json:"status"`
	HTTPStatus  *int      `json:"http_status"`
	Error       *string   `json:"error"`
	At          time.Time `json:"at"`
}

// Deliveries: GET /provider/alert-events/{id}/deliveries (super-admin).
func (h *ChannelHandler) Deliveries(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if _, ok := h.actorFor(w, r, provider.ActionAlertChannelRead, ""); !ok {
		return
	}
	rows, err := h.Channels.store.pool.Query(r.Context(), `
SELECT d.channel_id::text, c.name, d.attempt, d.status, d.http_status, d.error, d.at
  FROM alert_deliveries d JOIN alert_channels c ON c.id = d.channel_id
 WHERE d.event_id = $1 ORDER BY d.at, d.attempt LIMIT 500`, id)
	if err != nil {
		writeChannelError(w, "deliveries", err)
		return
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := r.Scan(&d.ChannelID, &d.ChannelName, &d.Attempt, &d.Status, &d.HTTPStatus, &d.Error, &d.At)
		d.At = d.At.UTC()
		return d, err
	})
	if err != nil {
		writeChannelError(w, "deliveries", err)
		return
	}
	if list == nil {
		list = []Delivery{}
	}
	provider.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}
