package providers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockIssuer stands up a minimal OIDC provider: discovery + JWKS + token
// endpoint that signs an id_token from `claims`. Handlers read key/kid live, so
// rotateKey() takes effect for both JWKS and freshly issued tokens.
type mockIssuer struct {
	server          *httptest.Server
	key             *rsa.PrivateKey
	kid             string
	claims          jwt.MapClaims
	brokenJWKS      bool
	brokenDiscovery bool
}

func newMockIssuer(t *testing.T, clientID string) *mockIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	mi := &mockIssuer{key: key, kid: "kid-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if mi.brokenDiscovery {
			http.Error(w, "discovery boom", http.StatusInternalServerError)
			return
		}
		base := mi.server.URL
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"jwks_uri":               base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		if mi.brokenJWKS {
			w.Write([]byte("{ not valid json"))
			return
		}
		pub := mi.key.PublicKey
		n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{"kty": "RSA", "kid": mi.kid, "n": n, "e": e}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		signed, err := mi.signIDToken()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": signed})
	})
	mi.server = httptest.NewServer(mux)
	t.Cleanup(mi.server.Close)

	mi.claims = jwt.MapClaims{
		"iss":            mi.server.URL,
		"aud":            clientID,
		"sub":            "user-123",
		"email":          "user@acme.com",
		"email_verified": true,
		"name":           "Test User",
		"nonce":          "expected-nonce",
		"exp":            time.Now().Add(time.Hour).Unix(),
		"iat":            time.Now().Unix(),
	}
	return mi
}

func (mi *mockIssuer) signIDToken() (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, mi.claims)
	tok.Header["kid"] = mi.kid
	return tok.SignedString(mi.key)
}

func (mi *mockIssuer) rotateKey(t *testing.T, newKid string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	mi.key = k
	mi.kid = newKid
}

func newTestOIDC(mi *mockIssuer, clientID string) *OIDCProvider {
	return NewOIDCProvider("oidc", mi.server.URL, clientID, "test-secret", "", "")
}

// authenticate is the common happy-path call.
func authenticate(p *OIDCProvider) (*AuthenticationContext, error) {
	return p.Authenticate(context.Background(), "code", "verifier", "http://localhost/cb", "expected-nonce")
}

func TestOIDC_Authenticate_Success(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	ac, err := authenticate(newTestOIDC(mi, "test-client"))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ac.Provider != "oidc" || ac.Issuer != mi.server.URL || ac.Subject != "user-123" {
		t.Fatalf("unexpected context: %+v", ac)
	}
	if ac.Email != "user@acme.com" || !ac.EmailVerified {
		t.Fatalf("email mapping wrong: %+v", ac)
	}
}

func TestOIDC_Authenticate_NonceMismatch(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	p := newTestOIDC(mi, "test-client")
	if _, err := p.Authenticate(context.Background(), "code", "verifier", "http://localhost/cb", "wrong-nonce"); err == nil {
		t.Fatal("expected nonce mismatch error")
	}
}

func TestOIDC_Authenticate_AudienceMismatch(t *testing.T) {
	mi := newMockIssuer(t, "issued-for-other-app") // token aud != adapter clientID
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected audience mismatch rejection")
	}
}

func TestOIDC_Authenticate_EmailNotVerified(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.claims["email_verified"] = false
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected email-not-verified rejection")
	}
}

func TestOIDC_Authenticate_Expired(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.claims["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected expired-token rejection")
	}
}

func TestOIDC_Authenticate_NotYetValid(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.claims["nbf"] = time.Now().Add(time.Hour).Unix() // nbf in the future
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected not-yet-valid (nbf) rejection")
	}
}

func TestOIDC_Authenticate_IssuerMismatch(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.claims["iss"] = "https://evil.example.com" // token iss != configured issuer
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected issuer mismatch rejection")
	}
}

func TestOIDC_Authenticate_MissingSubject(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.claims["sub"] = ""
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected missing-subject rejection")
	}
}

// TestOIDC_RotatedKid_ForcesRefetch is the key-rotation guarantee: after the
// JWKS cache is warm with kid-1, a token signed by a new kid must trigger a JWKS
// refetch (not a stale-cache failure).
func TestOIDC_RotatedKid_ForcesRefetch(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	p := newTestOIDC(mi, "test-client")

	if _, err := authenticate(p); err != nil {
		t.Fatalf("first login: %v", err)
	}
	mi.rotateKey(t, "kid-2") // JWKS + new tokens now use kid-2
	if _, err := authenticate(p); err != nil {
		t.Fatalf("rotated-kid login should refetch JWKS and succeed, got: %v", err)
	}
}

func TestOIDC_Authenticate_MalformedJWKS(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.brokenJWKS = true
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected verification failure on malformed JWKS")
	}
}

func TestOIDC_Authenticate_DiscoveryFailure(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	mi.brokenDiscovery = true
	if _, err := authenticate(newTestOIDC(mi, "test-client")); err == nil {
		t.Fatal("expected discovery failure to surface")
	}
}

func TestOIDC_AuthURL(t *testing.T) {
	mi := newMockIssuer(t, "test-client")
	p := newTestOIDC(mi, "test-client")
	u, err := p.AuthURL(context.Background(), AuthURLParams{
		State: "st", Nonce: "nn", CodeChallenge: "cc", RedirectURI: "http://localhost/cb",
	})
	if err != nil {
		t.Fatalf("AuthURL: %v", err)
	}
	for _, want := range []string{mi.server.URL + "/authorize", "client_id=test-client", "code_challenge=cc", "code_challenge_method=S256", "nonce=nn", "state=st"} {
		if !strings.Contains(u, want) {
			t.Fatalf("AuthURL missing %q: %s", want, u)
		}
	}
}
