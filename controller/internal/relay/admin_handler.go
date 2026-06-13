package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
)

// ProvisioningTokenTTL is the lifetime of a relay provisioning token.
// Matches the connector enrollment TTL (24h).
const ProvisioningTokenTTL = 24 * time.Hour

// AdminHandler serves POST /api/relays.
// The middleware stack (AuthMiddleware → RequireRole("admin")) must precede this.
type AdminHandler struct {
	Store     *Store
	Redis     valkeycompat.Cmdable
	JWTSecret string
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
	var req createRelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	for _, s := range req.IPAllowlist {
		if net.ParseIP(s) == nil {
			http.Error(w, fmt.Sprintf("invalid ip_allowlist entry %q", s), http.StatusBadRequest)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	relayID, err := h.Store.CreateRelay(ctx, req.Name, req.DNSAllowlist, req.IPAllowlist)
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(createRelayResponse{
		RelayID:           relayID,
		ProvisioningToken: tokenString,
		ExpiresAt:         time.Now().Add(ProvisioningTokenTTL),
	})
}
