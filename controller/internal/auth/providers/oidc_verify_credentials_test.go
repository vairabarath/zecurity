package providers

// Coverage for VerifyClientCredentials — the single credential-verification
// primitive behind createIdpConnection, updateIdpConnection and
// testIdpConnection (graph/resolvers/idp_helpers.go: verifyOIDCCredentials).
//
// The classification is the whole security value, and it rests on one
// empirically-established fact: IdPs authenticate the CLIENT before they
// evaluate the grant. So `invalid_client` means the credentials are wrong, and
// any other OAuth error means the client authenticated. Both error envelopes
// seen in practice must be understood:
//
//	{"error":"invalid_client"}      RFC 6749 §5.2
//	{"errorCode":"invalid_client"}  Okta's org authorization server (HTTP 400)
//
// A misread here is not a cosmetic bug: reading a real rejection as "valid"
// re-opens the swapped client-ID/secret hole, and reading a valid connection as
// invalid makes the IdP unusable under the block-unless-proven policy.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// tokenEndpointStub serves a discovery document pointing at its own token
// endpoint, which replies with the supplied status and body.
func tokenEndpointStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			"jwks_uri":               srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	return srv
}

func verifyAgainst(t *testing.T, srv *httptest.Server) (CredentialVerdict, string) {
	t.Helper()
	resetDiscoveryCache(t)
	p := NewOIDCProvider("okta", srv.URL, "the-client-id", "the-client-secret", "", "openid email profile")
	return p.VerifyClientCredentials(context.Background(), "http://localhost:8080/auth/callback")
}

func TestVerifyClientCredentials_Classification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   CredentialVerdict
	}{
		// --- definitive rejection -----------------------------------------
		{
			// The shape Okta actually returns — HTTP 400, `errorCode`, NOT the
			// RFC's 401 + `error`. Keyed only on the spec shape this would be
			// misread as "some other error" => valid.
			name: "okta envelope invalid_client", status: http.StatusBadRequest,
			body: `{"errorCode":"invalid_client","errorSummary":"Invalid value for 'client_id' parameter."}`,
			want: CredentialsInvalid,
		},
		{
			name: "rfc envelope invalid_client", status: http.StatusUnauthorized,
			body: `{"error":"invalid_client","error_description":"client authentication failed"}`,
			want: CredentialsInvalid,
		},
		{
			// RFC 6749 §5.2 pairs client-auth failure with 401; honour the
			// status even with nothing parseable in the body.
			name: "401 with no parseable code", status: http.StatusUnauthorized, body: ``,
			want: CredentialsInvalid,
		},

		// --- authenticated, then the grant was refused => VALID -----------
		{
			// The expected reply for CORRECT credentials + our bogus code.
			name: "invalid_grant means the client authenticated", status: http.StatusBadRequest,
			body: `{"error":"invalid_grant","error_description":"the code is invalid"}`,
			want: CredentialsValid,
		},
		{
			name: "unsupported_grant_type means the client authenticated", status: http.StatusBadRequest,
			body: `{"error":"unsupported_grant_type"}`,
			want: CredentialsValid,
		},
		{
			name: "okta envelope invalid_grant", status: http.StatusBadRequest,
			body: `{"errorCode":"invalid_grant","errorSummary":"The authorization code is invalid or has expired."}`,
			want: CredentialsValid,
		},
		{
			name: "200 proves authentication", status: http.StatusOK,
			body: `{"access_token":"x"}`,
			want: CredentialsValid,
		},

		// --- no verdict => INCONCLUSIVE (blocked under strict policy) -----
		{
			name: "500 with no oauth code", status: http.StatusInternalServerError,
			body: `<html>gateway error</html>`,
			want: CredentialsInconclusive,
		},
		{
			name: "400 with unparseable body", status: http.StatusBadRequest,
			body: `not json at all`,
			want: CredentialsInconclusive,
		},
		{
			name: "400 with json but no error field", status: http.StatusBadRequest,
			body: `{"something":"else"}`,
			want: CredentialsInconclusive,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := verifyAgainst(t, tokenEndpointStub(t, tc.status, tc.body))
			if got != tc.want {
				t.Fatalf("want %q, got %q (reason: %s)", tc.want, got, reason)
			}
		})
	}
}

// The reason is surfaced verbatim in a GraphQL error, so it must never carry the
// credential — even when the IdP echoes it back in its own error body.
func TestVerifyClientCredentials_ReasonNeverLeaksTheSecret(t *testing.T) {
	const secret = "super-secret-value-do-not-leak"
	srv := tokenEndpointStub(t, http.StatusBadRequest,
		`{"errorCode":"invalid_client","errorSummary":"bad secret `+secret+` supplied"}`)

	resetDiscoveryCache(t)
	p := NewOIDCProvider("okta", srv.URL, "the-client-id", secret, "", "openid")
	verdict, reason := p.VerifyClientCredentials(context.Background(), "http://localhost:8080/auth/callback")

	if verdict != CredentialsInvalid {
		t.Fatalf("want invalid, got %q", verdict)
	}
	if strings.Contains(reason, secret) {
		t.Fatalf("reason leaked the client secret: %q", reason)
	}
}

// An unreachable IdP is not evidence about the credentials.
func TestVerifyClientCredentials_UnreachableIsInconclusive(t *testing.T) {
	resetDiscoveryCache(t)
	// Closed immediately, so nothing is listening.
	srv := httptest.NewServer(http.NewServeMux())
	url := srv.URL
	srv.Close()

	p := NewOIDCProvider("okta", url, "id", "secret", "", "openid")
	verdict, reason := p.VerifyClientCredentials(context.Background(), "http://localhost:8080/auth/callback")
	if verdict != CredentialsInconclusive {
		t.Fatalf("want inconclusive, got %q (%s)", verdict, reason)
	}
}

// Nothing to prove means nothing is proven — never "valid" by default.
func TestVerifyClientCredentials_MissingCredentialsIsInconclusive(t *testing.T) {
	srv := tokenEndpointStub(t, http.StatusOK, `{}`)
	for _, tc := range []struct{ name, id, secret string }{
		{"no secret", "an-id", ""},
		{"no id", "", "a-secret"},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDiscoveryCache(t)
			p := NewOIDCProvider("okta", srv.URL, tc.id, tc.secret, "", "openid")
			verdict, _ := p.VerifyClientCredentials(context.Background(), "http://x/cb")
			if verdict != CredentialsInconclusive {
				t.Fatalf("want inconclusive, got %q", verdict)
			}
		})
	}
}

// The probe must send the credentials the way the login path does
// (client_secret_post) and must not smuggle a redeemable code.
func TestVerifyClientCredentials_SendsClientSecretPostAndAnInvalidCode(t *testing.T) {
	var gotForm url.Values
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/a",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/j",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})

	resetDiscoveryCache(t)
	p := NewOIDCProvider("okta", srv.URL, "cid", "csecret", "", "openid")
	if v, _ := p.VerifyClientCredentials(context.Background(), "http://localhost:8080/auth/callback"); v != CredentialsValid {
		t.Fatalf("want valid, got %q", v)
	}

	if gotForm.Get("client_id") != "cid" || gotForm.Get("client_secret") != "csecret" {
		t.Fatalf("credentials not sent as client_secret_post: %v", gotForm)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Fatalf("want authorization_code (client_credentials is not universally enabled), got %q",
			gotForm.Get("grant_type"))
	}
	if gotForm.Get("code") != credentialProbeCode {
		t.Fatalf("probe must use the sentinel code, got %q", gotForm.Get("code"))
	}
	if gotForm.Get("redirect_uri") != "http://localhost:8080/auth/callback" {
		t.Fatalf("the real registered redirect_uri must be sent, got %q", gotForm.Get("redirect_uri"))
	}
}
