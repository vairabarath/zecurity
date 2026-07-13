package relay

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// ProvisioningTokenTTL is the lifetime of a relay provisioning token.
// Matches the connector enrollment TTL (24h).
const ProvisioningTokenTTL = 24 * time.Hour

// AdminHandler serves POST /provider/relays.
// RequireProvider must precede this: it verifies the provider JWT (aud=provider),
// confirms an active provider_users row, and injects the provider Actor.
type AdminHandler struct {
	Store         *Store
	Redis         valkeycompat.Cmdable
	JWTSecret     string
	Authz         *provider.Authz
	ProviderStore *provider.Store
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
}

func (h *AdminHandler) Create(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Provider identity injected by RequireProvider. Absent = wiring bug (handler
	// registered without the middleware), never an anonymous call — fail closed.
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		http.Error(w, "no provider actor in context", http.StatusInternalServerError)
		return
	}
	// Authz chokepoint: only an actor permitted the relay.* namespace may issue a
	// provisioning token. Alpha matrix: super-admin + relay-ops both pass.
	if err := h.Authz.CanIssueProvisioningToken(actor, provider.Target{Type: "relay"}); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req createRelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	dnsAllowlist, err := validateDNSSANs(req.DNSAllowlist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ipAddresses, err := validateIPSANs(req.IPAllowlist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ipAllowlist := make([]string, len(ipAddresses))
	for i, ip := range ipAddresses {
		ipAllowlist[i] = ip.String()
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	relayID, err := h.Store.CreateRelay(ctx, req.Name, dnsAllowlist, ipAllowlist)
	if err != nil {
		http.Error(w, "failed to create relay: "+err.Error(), http.StatusInternalServerError)
		return
	}

	tokenString, jti, err := IssueProvisioningToken(h.JWTSecret, relayID, ProvisioningTokenTTL)
	if err != nil {
		http.Error(w, "failed to issue token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := StoreProvisioningJTI(ctx, h.Redis, jti, relayID, ProvisioningTokenTTL); err != nil {
		http.Error(w, "failed to persist token jti: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.Store.AttachJTI(ctx, relayID, jti); err != nil {
		http.Error(w, "failed to record token jti: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Append-only provider audit. Best-effort: the relay + token already exist, so
	// a failed audit write is logged, not surfaced to the caller.
	if err := h.ProviderStore.InsertAudit(ctx, provider.AuditEntry{
		ProviderUserID: &actor.UserID,
		ProviderEmail:  actor.Email,
		Action:         provider.ActionRelayCreate,
		TargetType:     "relay",
		TargetID:       relayID,
		Details: map[string]any{
			"name": req.Name,
			"ttl":  ProvisioningTokenTTL.String(),
			"dns":  dnsAllowlist,
			"ip":   ipAllowlist,
		},
		IPAddress: r.RemoteAddr,
	}); err != nil {
		log.Printf("provider audit (relay.create) failed for relay %s: %v", relayID, err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(createRelayResponse{
		RelayID:           relayID,
		ProvisioningToken: tokenString,
		ExpiresAt:         time.Now().Add(ProvisioningTokenTTL),
	})
}

// Delete soft-deletes a relay — the PENDING-02 seam. Guarded by CanDeleteRelay
// and audited (relay.delete). It marks the relay 'deleted' (no longer served to
// connectors) but does NOT revoke the certificate; CRL enforcement lands in
// PENDING-02.
//
// DELETE /provider/relays/{id}
func (h *AdminHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor, ok := provider.ActorFromContext(r.Context())
	if !ok {
		http.Error(w, "no provider actor in context", http.StatusInternalServerError)
		return
	}
	relayID := r.PathValue("id")
	if relayID == "" {
		http.Error(w, "relay id required", http.StatusBadRequest)
		return
	}
	if err := h.Authz.CanDeleteRelay(actor, provider.Target{Type: "relay", ID: relayID}); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := h.Store.MarkDeleted(ctx, relayID); err != nil {
		if errors.Is(err, ErrRelayNotFound) {
			http.Error(w, "relay not found", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to delete relay: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Append-only audit. Best-effort: the delete already committed.
	if err := h.ProviderStore.InsertAudit(ctx, provider.AuditEntry{
		ProviderUserID: &actor.UserID,
		ProviderEmail:  actor.Email,
		Action:         provider.ActionRelayDelete,
		TargetType:     "relay",
		TargetID:       relayID,
		Details:        map[string]any{"note": "soft-delete; certificate NOT revoked (PENDING-02)"},
		IPAddress:      r.RemoteAddr,
	}); err != nil {
		log.Printf("provider audit (relay.delete) failed for relay %s: %v", relayID, err)
	}

	w.WriteHeader(http.StatusNoContent)
}
