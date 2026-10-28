package relay

import (
	"context"
	"errors"
	"fmt"
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

// destructive is the shared revoke/delete path: authorize, validate the id and
// the D-17 body, then revoke under the row lock with the name/status guard and
// the audit row in the same transaction, and fan out only after commit.
func (h *AdminHandler) destructive(w http.ResponseWriter, r *http.Request, action string,
	authorize func(provider.Actor, provider.Target) error) {
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		log.Printf("relay admin: %s without provider actor (wiring bug)", action)
		provider.WriteError(w, http.StatusInternalServerError, "server_error")
		return
	}
	relayID := r.PathValue("id")
	if relayID == "" {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := authorize(actor, provider.Target{Type: "relay", ID: relayID}); err != nil {
		provider.WriteError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Validated before any query, so a malformed id never reaches a UUID cast.
	if _, err := uuid.Parse(relayID); err != nil {
		provider.WriteError(w, http.StatusNotFound, "not_found")
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
		h.OnRelayRevoked(ctx, relayID)
	}
	w.WriteHeader(http.StatusNoContent)
}
