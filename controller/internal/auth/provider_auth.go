package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// providerAuth wires the provider login flow onto the auth service's existing
// Google OIDC machinery (state HMAC, PKCE-in-Redis, code exchange, ID-token
// verify), but gates on the provider_users allowlist and issues a provider JWT
// (aud=provider) instead of a tenant token. No tenant bootstrap, no cookies,
// no React redirect — the token is returned as JSON for the CLI-driven alpha.
type providerAuth struct {
	s           *serviceImpl
	store       *provider.Store
	redirectURI string // provider callback URL, registered with Google
	tokenTTL    time.Duration
}

// ProviderRoutes returns the provider-login handlers (initiate, callback),
// backed by the given auth service. svc must be the concrete service returned by
// NewService. redirectURI must be the /provider/auth/callback URL registered
// with Google (Google requires an exact redirect_uri match on exchange).
func ProviderRoutes(svc Service, store *provider.Store, redirectURI string, tokenTTL time.Duration) (initiate, callback http.Handler, err error) {
	s, ok := svc.(*serviceImpl)
	if !ok {
		return nil, nil, fmt.Errorf("auth: ProviderRoutes requires the concrete auth service")
	}
	if store == nil || redirectURI == "" {
		return nil, nil, fmt.Errorf("auth: ProviderRoutes requires store and redirectURI")
	}
	if tokenTTL <= 0 {
		tokenTTL = 15 * time.Minute
	}
	pa := &providerAuth{s: s, store: store, redirectURI: redirectURI, tokenTTL: tokenTTL}
	return pa.initiateHandler(), pa.callbackHandler(), nil
}

// initiateHandler: GET /provider/auth/initiate — mints PKCE + signed state,
// stores the verifier, and returns the Google auth URL as JSON.
func (pa *providerAuth) initiateHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifierBytes := make([]byte, 64)
		if _, err := rand.Read(verifierBytes); err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
		hash := sha256.Sum256([]byte(codeVerifier))
		codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])

		state, err := generateSignedState(pa.s.cfg.JWTSecret)
		if err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		if err := pa.s.redisClient.SetPKCEState(r.Context(), state, PKCEState{CodeVerifier: codeVerifier}); err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}

		params := url.Values{}
		params.Set("client_id", pa.s.cfg.GoogleClientID)
		params.Set("redirect_uri", pa.redirectURI)
		params.Set("response_type", "code")
		params.Set("scope", "openid email profile")
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", "S256")
		params.Set("state", state)
		authURL := "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()

		writeProviderAuthJSON(w, http.StatusOK, map[string]string{"auth_url": authURL, "state": state})
	})
}

// callbackHandler: GET /provider/auth/callback — verifies state, exchanges the
// code, verifies the Google ID token, gates on the provider_users allowlist,
// and returns a provider JWT (aud=provider) as JSON.
func (pa *providerAuth) callbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" || state == "" {
			writeProviderAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_params"})
			return
		}
		if err := verifySignedState(state, pa.s.cfg.JWTSecret); err != nil {
			writeProviderAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_state"})
			return
		}
		pkce, found, err := pa.s.redisClient.GetAndDeletePKCEState(ctx, state)
		if err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		if !found {
			writeProviderAuthJSON(w, http.StatusBadRequest, map[string]string{"error": "state_expired"})
			return
		}
		codeVerifier := pkce.CodeVerifier
		tokenResp, err := pa.s.ExchangeCode(ctx, code, codeVerifier, pa.redirectURI)
		if err != nil {
			writeProviderAuthJSON(w, http.StatusBadGateway, map[string]string{"error": "token_exchange_failed"})
			return
		}
		googleClaims, err := pa.s.VerifyIDToken(ctx, tokenResp.IDToken)
		if err != nil {
			writeProviderAuthJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_id_token"})
			return
		}

		// Allowlist gate: the Google email must be an ACTIVE provider_users row.
		user, err := pa.store.GetByEmail(ctx, googleClaims.Email)
		if errors.Is(err, provider.ErrProviderUserNotFound) {
			writeProviderAuthJSON(w, http.StatusForbidden, map[string]string{"error": "not_a_provider_user"})
			return
		}
		if err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}

		token, err := provider.IssueProviderToken(pa.s.cfg.JWTSecret, user.ID, user.Role, user.Email, pa.tokenTTL)
		if err != nil {
			writeProviderAuthJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_issue_failed"})
			return
		}
		writeProviderAuthJSON(w, http.StatusOK, map[string]any{
			"token":      token,
			"expires_in": int64(pa.tokenTTL.Seconds()),
		})
	})
}

func writeProviderAuthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
