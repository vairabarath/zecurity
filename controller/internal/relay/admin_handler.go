package relay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// ProvisioningTokenTTL is the lifetime of a relay provisioning token.
// Matches the connector enrollment TTL (24h).
const ProvisioningTokenTTL = 24 * time.Hour

// Relay admin API (Sprint 22 Phase A; Decision Record D-17, D-31, D-14):
//
//	POST   /provider/relays               super-admin, relay-ops  {name, dns_allowlist, ip_allowlist} → 201 {relay_id, provisioning_token, expires_at, ca_fingerprint, controller_grpc_addr, controller_http_addr}
//	POST   /provider/relays/{id}/revoke   super-admin             {reason, confirm_name} → 204
//	DELETE /provider/relays/{id}          super-admin             {reason, confirm_name} → 204
//	POST   /provider/relays/{id}/drain    super-admin, relay-ops  {reason?} → 204 (Phase D, D-32)
//	POST   /provider/relays/{id}/resume   super-admin, relay-ops  none or {} → 204 (Phase D, D-32)
//
// Every error is {"error":"<code>"} (with a safe "detail" for SAN validation);
// internal errors are logged, never returned. Revoke and delete check the
// operator's typed relay name against the stored name under the row lock and
// write the audit row in the same transaction: an audit failure rolls the
// whole mutation back. The provisioning token is returned once, with
// Cache-Control: no-store, and never logged or audited.
const (
	maxRelayNameRunes = 128
	maxReasonRunes    = 500
)

var (
	errAlreadyRevoked       = errors.New("relay already revoked")
	errAlreadyDeleted       = errors.New("relay already deleted")
	errConfirmationMismatch = errors.New("typed relay name does not match")
)

// AdminHandler serves the relay admin routes. RequireProvider must precede
// it: it verifies the provider JWT (aud=provider), confirms an active
// provider_users row (TOTP-backed for super-admins) and injects the Actor.
type AdminHandler struct {
	Store         *Store
	Redis         valkeycompat.Cmdable
	JWTSecret     string
	Authz         *provider.Authz
	ProviderStore *provider.Store
	// CAFingerprint returns the pinned Intermediate CA fingerprint
	// (RELAY_CA_FINGERPRINT) for the install command (Phase E, D-30).
	CAFingerprint func(ctx context.Context) (string, error)
	// ControllerGRPCAddr and ControllerHTTPAddr are the addresses a relay host
	// uses to reach the controller (CONTROLLER_ADDR, CONTROLLER_HTTP_ADDR).
	// Optional: when empty the console shows placeholders.
	ControllerGRPCAddr string
	ControllerHTTPAddr string
	// OnRelayRevoked, if set, runs post-commit after a relay is revoked or deleted:
	// it rebuilds+broadcasts the relay list and invalidates the transport snapshot
	// for affected workspaces so connectors AND clients converge. Wired in main.go.
	OnRelayRevoked func(ctx context.Context, relayID string)
	// OnRelayDrainChanged, if set, runs post-commit after a drain or resume
	// that changed state: it sets or clears the heartbeat draining flag and
	// broadcasts the relay list. Never on a no-op or an error. Wired in main.go.
	OnRelayDrainChanged func(ctx context.Context, relayID string, draining bool)
}

type createRelayRequest struct {
	Name         string   `json:"name"`
	DNSAllowlist []string `json:"dns_allowlist"`
	IPAllowlist  []string `json:"ip_allowlist"`
}

type createRelayResponse struct {
	RelayID           string    `json:"relay_id"`
	ProvisioningToken string    `json:"provisioning_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	// Install parameters (Phase E). None of them is secret.
	CAFingerprint      string `json:"ca_fingerprint"`
	ControllerGRPCAddr string `json:"controller_grpc_addr"`
	ControllerHTTPAddr string `json:"controller_http_addr"`
}

// destructiveRequest is the D-17 body of revoke and delete.
type destructiveRequest struct {
	Reason      string `json:"reason"`
	ConfirmName string `json:"confirm_name"`
}

// ValidateInstallAddr checks a controller address for the relay install
// command (PROVIDER_RELAY_CONTROLLER_ADDR / _HTTP_ADDR): host:port with no
// scheme or path, since the relay adds the scheme itself. Empty is allowed
// (not configured; the console shows a placeholder).
func ValidateInstallAddr(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return fmt.Errorf("%q has an invalid host", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has an invalid port", addr)
	}
	return nil
}

// validRelayName: trimmed, 1–128 runes, no control characters.
func validRelayName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > maxRelayNameRunes {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Create: POST /provider/relays. Every input is validated before the first
// write, so an invalid request leaves no relay row and no token state.
func (h *AdminHandler) Create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		provider.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	// Provider identity injected by RequireProvider. Absent = wiring bug (handler
	// registered without the middleware), never an anonymous call — fail closed.
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("relay admin: create without provider actor (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	// Authz chokepoint: super-admin and relay-ops may issue a provisioning token (D-31).
	if err := h.Authz.CanIssueProvisioningToken(actor, provider.Target{Type: "relay"}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	var req createRelayRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validRelayName(name) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_name")
		return
	}
	dnsAllowlist, err := validateDNSSANs(req.DNSAllowlist)
	if err != nil {
		provider.WriteErrorDetail(w, http.StatusBadRequest, "invalid_dns_san", err.Error())
		return
	}
	ipAddresses, err := validateIPSANs(req.IPAllowlist)
	if err != nil {
		provider.WriteErrorDetail(w, http.StatusBadRequest, "invalid_ip_san", err.Error())
		return
	}
	ipAllowlist := make([]string, len(ipAddresses))
	for i, ip := range ipAddresses {
		ipAllowlist[i] = ip.String()
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Read the fingerprint before the first write: if it fails, nothing exists.
	if h.CAFingerprint == nil {
		log.Printf("relay admin: create without CA fingerprint source (wiring bug)")
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	caFingerprint, err := h.CAFingerprint(ctx)
	if err != nil {
		log.Printf("relay admin: read intermediate CA fingerprint: %v", err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}

	// D-14 (FU-1): the relay row, its token jti and the relay.create audit
	// commit together. The Valkey jti is written inside the transaction, so a
	// committed relay always has a usable token; if the transaction then fails,
	// the jti is removed again. (A crash between the two leaves at worst a
	// Valkey jti for a relay that doesn't exist: provisioning refuses it, and
	// it expires with the token.)
	var tokenString, jti string
	relayID, err := h.Store.CreateRelayAudited(ctx, name, dnsAllowlist, ipAllowlist,
		func(ctx context.Context, relayID string) (string, error) {
			tok, j, err := IssueProvisioningToken(h.JWTSecret, relayID, ProvisioningTokenTTL)
			if err != nil {
				return "", err
			}
			if err := StoreProvisioningJTI(ctx, h.Redis, j, relayID, ProvisioningTokenTTL); err != nil {
				return "", fmt.Errorf("persist provisioning jti: %w", err)
			}
			tokenString, jti = tok, j
			return j, nil
		},
		func(ctx context.Context, tx pgx.Tx, relayID string) error {
			return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
				ProviderUserID: &actor.UserID,
				ProviderEmail:  actor.Email,
				Action:         provider.ActionRelayCreate,
				TargetType:     "relay",
				TargetID:       relayID,
				Details: map[string]any{
					"name": name,
					"ttl":  ProvisioningTokenTTL.String(),
					"dns":  dnsAllowlist,
					"ip":   ipAllowlist,
				},
				IPAddress: provider.ClientIP(r),
			})
		})
	if err != nil {
		if jti != "" {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
			if delErr := h.Redis.Del(cleanupCtx, provisioningJTIPrefix+jti).Err(); delErr != nil {
				log.Printf("relay admin: remove provisioning jti after failed create: %v", delErr)
			}
			cancelCleanup()
		}
		log.Printf("relay admin: create relay: %v", err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	provider.WriteJSON(w, http.StatusCreated, createRelayResponse{
		RelayID:            relayID,
		ProvisioningToken:  tokenString,
		ExpiresAt:          time.Now().Add(ProvisioningTokenTTL),
		CAFingerprint:      caFingerprint,
		ControllerGRPCAddr: h.ControllerGRPCAddr,
		ControllerHTTPAddr: h.ControllerHTTPAddr,
	})
}

// Delete: DELETE /provider/relays/{id} {reason, confirm_name}. Super-admin
// only (D-31). Revoke-then-remove in one transaction: every unexpired serial
// goes on the CRL, the relay becomes 'deleted', and the audit row commits with
// them. A revoked relay may be deleted; a deleted one can't be again.
func (h *AdminHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		provider.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	h.destructive(w, r, provider.ActionRelayDelete, func(actor provider.Actor, target provider.Target) error {
		return h.Authz.CanDeleteRelay(actor, target)
	})
}

// Revoke: POST /provider/relays/{id}/revoke {reason, confirm_name}. Super-admin
// only (D-31). Revokes the relay's certificates WITHOUT removing it: the relay
// becomes 'revoked' (terminal), every unexpired serial goes on the CRL, and the
// audit row commits with them.
func (h *AdminHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		provider.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	h.destructive(w, r, provider.ActionRelayRevoke, func(actor provider.Actor, target provider.Target) error {
		return h.Authz.CanRevokeRelay(actor, target)
	})
}

// authorizeRelayAction is the shared start of every {id} route: the injected
// actor, the authorization decision, then the id. ok=false means the response
// is written.
func (h *AdminHandler) authorizeRelayAction(w http.ResponseWriter, r *http.Request, action string,
	authorize func(provider.Actor, provider.Target) error) (provider.Actor, string, bool) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("relay admin: %s without provider actor (wiring bug)", action)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return provider.Actor{}, "", false
	}
	relayID := r.PathValue("id")
	if relayID == "" {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return provider.Actor{}, "", false
	}
	if err := authorize(actor, provider.Target{Type: "relay", ID: relayID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return provider.Actor{}, "", false
	}
	// Validated before any query, so a malformed id never reaches a UUID cast.
	if _, err := uuid.Parse(relayID); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return provider.Actor{}, "", false
	}
	return actor, relayID, true
}

// destructive is the shared revoke/delete path: authorize, validate the id and
// the D-17 body, then revoke under the row lock with the name/status guard and
// the audit row in the same transaction, and fan out only after commit.
func (h *AdminHandler) destructive(w http.ResponseWriter, r *http.Request, action string,
	authorize func(provider.Actor, provider.Target) error) {
	actor, relayID, ok := h.authorizeRelayAction(w, r, action, authorize)
	if !ok {
		return
	}
	var req destructiveRequest
	if !provider.DecodeJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		provider.WriteError(w, http.StatusBadRequest, "reason_required")
		return
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	guard := func(name, status string) error {
		switch {
		case status == "deleted":
			return errAlreadyDeleted
		case action == provider.ActionRelayRevoke && status == "revoked":
			return errAlreadyRevoked
		case req.ConfirmName != name:
			return errConfirmationMismatch
		}
		return nil
	}
	_, err := h.Store.RevokeRelayGuarded(ctx, relayID, reason, guard, func(ctx context.Context, tx pgx.Tx, revoked int) error {
		if action == provider.ActionRelayDelete {
			if _, err := tx.Exec(ctx,
				`UPDATE relays SET status = 'deleted', updated_at = NOW() WHERE id = $1`, relayID); err != nil {
				return fmt.Errorf("mark relay deleted: %w", err)
			}
		}
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         action,
			TargetType:     "relay",
			TargetID:       relayID,
			Details:        map[string]any{"serials_revoked": revoked, "reason": reason},
			IPAddress:      provider.ClientIP(r),
		})
	})
	switch {
	case errors.Is(err, ErrRelayNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, errAlreadyDeleted):
		provider.WriteError(w, http.StatusConflict, "already_deleted")
		return
	case errors.Is(err, errAlreadyRevoked):
		provider.WriteError(w, http.StatusConflict, "already_revoked")
		return
	case errors.Is(err, errConfirmationMismatch):
		provider.WriteError(w, http.StatusBadRequest, "confirmation_mismatch")
		return
	case err != nil:
		log.Printf("relay admin: %s relay %s: %v", action, relayID, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	// Convergence fan-out (broadcast + transport invalidation), post-commit.
	if h.OnRelayRevoked != nil {
		hookCtx, cancelHook := fanoutContext(r)
		defer cancelHook()
		h.OnRelayRevoked(hookCtx, relayID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// drainRequest is the drain body. The reason is optional (drain is reversible;
// D-17 applies to revoke and delete) and is audited when given.
type drainRequest struct {
	Reason string `json:"reason"`
}

// Drain: POST /provider/relays/{id}/drain {reason?}. Super-admin and relay-ops
// (D-31). The relay leaves the published relay list; connectors migrate away
// through their existing path (D-32). Refused for the last eligible relay.
// Draining a draining relay is a 204 no-op: not audited, no broadcast.
func (h *AdminHandler) Drain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		provider.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	actor, relayID, ok := h.authorizeRelayAction(w, r, provider.ActionRelayDrain, h.Authz.CanDrainRelay)
	if !ok {
		return
	}
	var req drainRequest
	if !decodeOptionalJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		provider.WriteError(w, http.StatusBadRequest, "reason_too_long")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	changed, err := h.Store.DrainRelay(ctx, relayID, func(ctx context.Context, tx pgx.Tx, _ time.Time) error {
		details := map[string]any{}
		if reason != "" {
			details["reason"] = reason
		}
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayDrain,
			TargetType:     "relay",
			TargetID:       relayID,
			Details:        details,
			IPAddress:      provider.ClientIP(r),
		})
	})
	h.finishDrainChange(w, r, provider.ActionRelayDrain, relayID, true, changed, err)
}

// Resume: POST /provider/relays/{id}/resume (no body, or {}). Super-admin and
// relay-ops (D-31). The relay returns to the published list. Resuming a relay
// that isn't draining is a 204 no-op: not audited, no broadcast.
func (h *AdminHandler) Resume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		provider.WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	actor, relayID, ok := h.authorizeRelayAction(w, r, provider.ActionRelayResume, h.Authz.CanResumeRelay)
	if !ok {
		return
	}
	var req struct{}
	if !decodeOptionalJSON(w, r, &req) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	changed, err := h.Store.ResumeRelay(ctx, relayID, func(ctx context.Context, tx pgx.Tx, drainedSince time.Time) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayResume,
			TargetType:     "relay",
			TargetID:       relayID,
			Details:        map[string]any{"drained_since": drainedSince.UTC()},
			IPAddress:      provider.ClientIP(r),
		})
	})
	h.finishDrainChange(w, r, provider.ActionRelayResume, relayID, false, changed, err)
}

// finishDrainChange maps a drain/resume result to the response and runs the
// post-commit hook only when the state changed.
func (h *AdminHandler) finishDrainChange(w http.ResponseWriter, r *http.Request, action, relayID string, draining, changed bool, err error) {
	switch {
	case errors.Is(err, ErrRelayNotFound):
		provider.WriteError(w, http.StatusNotFound, "not_found")
		return
	case errors.Is(err, errAlreadyDeleted):
		provider.WriteError(w, http.StatusConflict, "already_deleted")
		return
	case errors.Is(err, errAlreadyRevoked):
		provider.WriteError(w, http.StatusConflict, "already_revoked")
		return
	case errors.Is(err, errInvalidState):
		provider.WriteError(w, http.StatusConflict, "invalid_state")
		return
	case errors.Is(err, errLastEligibleRelay):
		provider.WriteError(w, http.StatusConflict, "last_eligible_relay")
		return
	case err != nil:
		log.Printf("relay admin: %s relay %s: %v", action, relayID, err)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	if changed && h.OnRelayDrainChanged != nil {
		hookCtx, cancelHook := fanoutContext(r)
		defer cancelHook()
		h.OnRelayDrainChanged(hookCtx, relayID, draining)
	}
	w.WriteHeader(http.StatusNoContent)
}

// fanoutTimeout bounds a post-commit fan-out hook.
const fanoutTimeout = 10 * time.Second

// fanoutContext is the context for a post-commit hook: detached from the
// request, so a client that disconnects (or a request whose 5 s budget went on
// lock waits) can't cancel the broadcast of a change that is already
// committed; bounded by its own timeout. Request values are kept.
func fanoutContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), fanoutTimeout)
}

// decodeOptionalJSON is provider.DecodeJSON for a body that may be empty: an
// empty body leaves dst as it is. Anything else must be a valid, bounded JSON
// object without unknown fields.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return true
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return provider.DecodeJSON(w, r, dst)
}
