package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GoogleTokenResponse is what Google returns from the token endpoint.
// Used by: exchangeCodeForTokens() below.
// Consumed by: CallbackHandler() in callback.go (Step 4) to extract the id_token.
type GoogleTokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

const googleTokenURL = "https://oauth2.googleapis.com/token"

// exchangeCodeForTokens exchanges the authorization code for tokens.
// This is a server-to-server call — the client_secret is never exposed to the browser.
// Called by: CallbackHandler() in callback.go (Step 4).
//
// The code_verifier must match the code_challenge sent in the auth URL (oidc.go).
// Google verifies this server-side — if it doesn't match, the exchange fails.
// This is the PKCE guarantee: even if the auth code is intercepted,
// it cannot be exchanged without the verifier that only our server has.
func (s *serviceImpl) exchangeCodeForTokens(ctx context.Context, code, codeVerifier string) (*GoogleTokenResponse, error) {
	body := url.Values{}
	body.Set("code", code)
	body.Set("code_verifier", codeVerifier)
	body.Set("client_id", s.cfg.GoogleClientID)
	body.Set("client_secret", s.cfg.GoogleClientSecret)
	body.Set("redirect_uri", s.cfg.RedirectURI)
	body.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		googleTokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(resp.Body).Decode(&errBody)
		return nil, fmt.Errorf("google token exchange failed: status=%d body=%v",
			resp.StatusCode, errBody)
	}

	var tokenResp GoogleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	if tokenResp.IDToken == "" {
		return nil, fmt.Errorf("google did not return id_token")
	}

	return &tokenResp, nil
}
