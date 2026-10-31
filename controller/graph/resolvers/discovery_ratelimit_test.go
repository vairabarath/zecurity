package resolvers

// FU-11: the public workspace-discovery fields are rate limited per client IP
// (shared with GET /workspaces/{slug}/auth) and, for lookupWorkspacesByEmail,
// per email identity. These tests drive the REAL executable schema and
// resolvers through gqlgen's default server, wired exactly as cmd/server wires
// it (error presenter, discoverylimit.Extension, discoverylimit.Middleware),
// on a throwaway database (PKI_TEST_DATABASE_URL) and an in-process Valkey.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/discoverylimit"
	"github.com/yourorg/ztna/controller/internal/middleware"
)

const discoveryJWTSecret = "fu11-discovery-test-jwt-secret-0123456789"

type discoveryEnv struct {
	h       *lookupIdpHarness
	limiter *discoverylimit.Limiter
	mr      *miniredis.Miniredis
	srv     *handler.Server
	public  http.Handler // /graphql as the public route sees it
	authed  http.Handler // /graphql behind the auth middleware
	rest    http.Handler // GET /workspaces/{slug}/auth stand-in sharing the limiter
	wsID    string
}

func newDiscoveryEnv(t *testing.T, cfg discoverylimit.Config) *discoveryEnv {
	t.Helper()
	h := newLookupIdpHarness(t)
	t.Cleanup(h.teardown)
	mr := miniredis.RunT(t)
	vc, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{mr.Addr()}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(vc.Close)
	l, err := discoverylimit.NewLimiter(valkeycompat.NewAdapter(vc), cfg, "fu11-discovery-test-master-secret")
	if err != nil {
		t.Fatal(err)
	}
	srv := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers:  &Resolver{Pool: h.pool, IdpStore: h.idpStore},
		Directives: graph.DirectiveRoot{HasRole: HasRole},
	}))
	srv.SetErrorPresenter(ErrorPresenter)
	srv.Use(discoverylimit.Extension{})
	ips := discoverylimit.NewIPResolver(nil)
	wrap := discoverylimit.Middleware(l, ips)
	e := &discoveryEnv{
		h: h, limiter: l, mr: mr, srv: srv,
		public: wrap(srv),
		authed: wrap(middleware.AuthMiddleware(discoveryJWTSecret)(srv)),
		rest: discoverylimit.ProtectHTTP(l, ips, discoverylimit.OpRESTAuth, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})),
	}
	e.wsID = h.seedWorkspace("acme")
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, 'victim@example.com', 'google', 'sub-victim', 'member', 'active')`, e.wsID); err != nil {
		t.Fatal(err)
	}
	return e
}

type gqlResult struct {
	status int
	header http.Header
	body   string
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message    string         `json:"message"`
		Extensions map[string]any `json:"extensions"`
	} `json:"errors"`
}

func (e *discoveryEnv) do(t *testing.T, h http.Handler, req *http.Request) gqlResult {
	t.Helper()
	if req.RemoteAddr == "" || req.RemoteAddr == "192.0.2.1:1234" {
		req.RemoteAddr = "203.0.113.7:4000"
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	r := gqlResult{status: w.Code, header: w.Header(), body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func post(query string, vars map[string]any) *http.Request {
	b, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func (r gqlResult) code() string {
	if len(r.Errors) == 0 {
		return ""
	}
	c, _ := r.Errors[0].Extensions["code"].(string)
	return c
}

const slugQuery = `{ lookupWorkspace(slug:"acme") { found workspace { name } } }`

// Legitimate traffic below the limits is untouched.
func TestDiscoveryLimit_BelowLimitsUnchanged(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 5, EmailLimit: 5})
	r := e.do(t, e.public, post(slugQuery, nil))
	if r.status != 200 || len(r.Errors) != 0 || !strings.Contains(string(r.Data), "Corp acme") {
		t.Fatalf("slug lookup: %d %s", r.status, r.body)
	}
	r = e.do(t, e.public, post(`query($e:String!){ lookupWorkspacesByEmail(email:$e){ workspaces { slug } } }`, map[string]any{"e": "Victim@Example.com"}))
	if r.status != 200 || !strings.Contains(string(r.Data), `"acme"`) {
		t.Fatalf("email lookup: %d %s", r.status, r.body)
	}
	r = e.do(t, e.public, post(`{ lookupIdpConnections(workspaceSlug:"acme") { id } }`, nil))
	if r.status != 200 || len(r.Errors) != 0 {
		t.Fatalf("idp lookup: %d %s", r.status, r.body)
	}
	if r.header.Get("Retry-After") != "" {
		t.Fatal("Retry-After on an allowed response")
	}
}

// Each of the three GraphQL discovery fields is charged on its own: with a
// budget of 1, the second call of any one of them is refused.
func TestDiscoveryLimit_EachOperationIsCharged(t *testing.T) {
	for _, q := range []string{
		slugQuery,
		`{ lookupWorkspacesByEmail(email:"victim@example.com") { workspaces { slug } } }`,
		`{ lookupIdpConnections(workspaceSlug:"acme") { id } }`,
	} {
		e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 1, EmailLimit: 100})
		if r := e.do(t, e.public, post(q, nil)); r.status != 200 {
			t.Fatalf("%s: first call %d %s", q, r.status, r.body)
		}
		if r := e.do(t, e.public, post(q, nil)); r.status != http.StatusTooManyRequests {
			t.Fatalf("%s: second call over a budget of 1: %d %s", q, r.status, r.body)
		}
	}
}

// Every alias is one execution: an aliased request over the budget is refused
// as a whole, with no workspace data from the aliases that ran first.
func TestDiscoveryLimit_AliasesCannotBypassOrLeakPartialResults(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 3})
	q := `{ a: lookupWorkspace(slug:"acme") { found workspace { name } }
	        b: lookupWorkspace(slug:"acme") { found workspace { name } }
	        c: lookupWorkspacesByEmail(email:"victim@example.com") { workspaces { name slug } }
	        d: lookupWorkspace(slug:"acme") { found workspace { name } }
	        e: lookupIdpConnections(workspaceSlug:"acme") { id } }`
	r := e.do(t, e.public, post(q, nil))
	if r.status != http.StatusTooManyRequests || r.code() != discoverylimit.CodeRateLimited {
		t.Fatalf("5 aliases over a budget of 3: %d %s", r.status, r.body)
	}
	if string(r.Data) != "null" || strings.Contains(r.body, "Corp acme") || strings.Contains(r.body, `"acme"`) || strings.Contains(r.body, "victim") {
		t.Fatalf("partial results escaped: %s", r.body)
	}
	if len(r.Errors) != 1 || r.header.Get("Retry-After") == "" {
		t.Fatalf("want one error and Retry-After: %s", r.body)
	}
}

// Fragments and inline fragments run the same resolvers, so they are charged.
func TestDiscoveryLimit_FragmentsAreCharged(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 2})
	q := `query { ...F ... on Query { lookupWorkspace(slug:"acme") { found } } }
	      fragment F on Query { x: lookupWorkspace(slug:"acme") { found } y: lookupWorkspace(slug:"acme") { found } }`
	// Fragments at the root are an authenticated-route shape; the charge is the
	// same on either route because it happens in the resolver.
	r := e.do(t, e.authed, withToken(t, e, post(q, nil)))
	if r.status != http.StatusTooManyRequests || string(r.Data) != "null" {
		t.Fatalf("3 fragment-selected lookups over a budget of 2: %d %s", r.status, r.body)
	}
}

func withToken(t *testing.T, e *discoveryEnv, req *http.Request) *http.Request {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		TenantID: e.wsID, Role: "admin", Email: "admin@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: "user-1", Issuer: appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte(discoveryJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

// Signing in does not lift the limit: the authenticated route charges the same
// shared budget.
func TestDiscoveryLimit_AuthenticatedPathSharesBudget(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 2})
	if r := e.do(t, e.public, post(slugQuery, nil)); r.status != 200 {
		t.Fatalf("public: %d", r.status)
	}
	if r := e.do(t, e.authed, withToken(t, e, post(slugQuery, nil))); r.status != 200 || len(r.Errors) != 0 {
		t.Fatalf("authenticated: %d %s", r.status, r.body)
	}
	if r := e.do(t, e.authed, withToken(t, e, post(slugQuery, nil))); r.status != http.StatusTooManyRequests {
		t.Fatalf("authenticated, over the shared budget: %d %s", r.status, r.body)
	}
	// Unrelated behaviour is unchanged: no token is still a plain 401.
	if r := e.do(t, e.authed, post(slugQuery, nil)); r.status != http.StatusUnauthorized || r.header.Get("Retry-After") != "" {
		t.Fatalf("missing token: %d", r.status)
	}
}

// GraphQL and REST share one IP budget.
func TestDiscoveryLimit_GraphQLAndRESTShareBudget(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 3})
	rest := func() int {
		req := httptest.NewRequest("GET", "/workspaces/acme/auth", nil)
		req.RemoteAddr = "203.0.113.7:4000"
		w := httptest.NewRecorder()
		e.rest.ServeHTTP(w, req)
		return w.Code
	}
	if c := rest(); c != 200 {
		t.Fatalf("rest 1: %d", c)
	}
	if r := e.do(t, e.public, post(slugQuery, nil)); r.status != 200 {
		t.Fatalf("graphql: %d", r.status)
	}
	if c := rest(); c != 200 {
		t.Fatalf("rest 2: %d", c)
	}
	if r := e.do(t, e.public, post(slugQuery, nil)); r.status != http.StatusTooManyRequests {
		t.Fatalf("graphql after 3 mixed requests: %d", r.status)
	}
	if c := rest(); c != http.StatusTooManyRequests {
		t.Fatalf("rest after the budget: %d", c)
	}
}

// The GET and multipart transports are charged like POST.
func TestDiscoveryLimit_OtherHTTPTransports(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 1})
	get := httptest.NewRequest("GET", "/graphql?query="+url.QueryEscape(slugQuery), nil)
	if r := e.do(t, e.public, get); r.status != 200 || !strings.Contains(string(r.Data), "Corp acme") {
		t.Fatalf("GET below the limit: %d %s", r.status, r.body)
	}
	get = httptest.NewRequest("GET", "/graphql?query="+url.QueryEscape(slugQuery), nil)
	if r := e.do(t, e.public, get); r.status != http.StatusTooManyRequests || string(r.Data) != "null" {
		t.Fatalf("GET over the limit: %d %s", r.status, r.body)
	}
}

// A websocket message has no HTTP status: a refused operation arrives as a
// GraphQL error with extensions.code and no data.
func TestDiscoveryLimit_Websocket(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 1})
	c := client.New(e.public)
	var resp struct {
		LookupWorkspace *struct{ Found bool }
	}
	if err := c.WebsocketOnce(`query { lookupWorkspace(slug:"acme") { found } }`, &resp); err != nil {
		t.Fatalf("websocket below the limit: %v", err)
	}
	if resp.LookupWorkspace == nil || !resp.LookupWorkspace.Found {
		t.Fatalf("websocket answer: %+v", resp)
	}
	resp.LookupWorkspace = nil
	err := c.WebsocketOnce(`query { a: lookupWorkspace(slug:"acme") { found } }`, &resp)
	var raw client.RawJsonError
	if !errors.As(err, &raw) || !strings.Contains(string(raw.RawMessage), discoverylimit.CodeRateLimited) {
		t.Fatalf("websocket over the limit: err=%v", err)
	}
	if resp.LookupWorkspace != nil {
		t.Fatalf("websocket returned data with the refusal: %+v", resp)
	}
}

// The per-email budget: canonical variants share it; other identities don't.
func TestDiscoveryLimit_EmailBudget(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 100, EmailLimit: 2})
	q := `query($e:String!){ lookupWorkspacesByEmail(email:$e){ workspaces { slug } } }`
	for i, v := range []string{"victim@example.com", " VICTIM@example.COM "} {
		req := post(q, map[string]any{"e": v})
		req.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":4000"
		if r := e.do(t, e.public, req); r.status != 200 {
			t.Fatalf("attempt %d: %d %s", i, r.status, r.body)
		}
	}
	req := post(q, map[string]any{"e": "victim@example.com"})
	req.RemoteAddr = "198.51.100.9:4000"
	r := e.do(t, e.public, req)
	if r.status != http.StatusTooManyRequests || strings.Contains(r.body, "acme") {
		t.Fatalf("3rd attempt from a fresh IP: %d %s", r.status, r.body)
	}
	if r := e.do(t, e.public, post(q, map[string]any{"e": "other@example.com"})); r.status != 200 {
		t.Fatalf("another identity: %d", r.status)
	}
	// The slug lookup has no email budget.
	if r := e.do(t, e.public, post(slugQuery, nil)); r.status != 200 {
		t.Fatalf("slug lookup: %d", r.status)
	}
	for _, k := range e.mr.Keys() {
		if strings.Contains(k, "@") || strings.Contains(k, "victim") {
			t.Fatalf("raw email in a Valkey key: %q", k)
		}
	}
}

// Valkey down: fail closed with 503 and a generic body, and no data.
func TestDiscoveryLimit_ValkeyDownFailsClosed(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{})
	addr := e.mr.Addr()
	e.mr.Close()
	r := e.do(t, e.public, post(slugQuery, nil))
	if r.status != http.StatusServiceUnavailable || r.code() != discoverylimit.CodeUnavailable || string(r.Data) != "null" {
		t.Fatalf("Valkey down: %d %s", r.status, r.body)
	}
	if strings.Contains(r.body, addr) || strings.Contains(strings.ToLower(r.body), "valkey") || strings.Contains(r.body, "dial") || r.header.Get("Retry-After") != "" {
		t.Fatalf("infrastructure detail in the response: %s", r.body)
	}
}

// Non-discovery errors keep gqlgen's own handling: a query that fails
// validation is still 422 and never touches the limiter.
func TestDiscoveryLimit_UnrelatedErrorsUnchanged(t *testing.T) {
	e := newDiscoveryEnv(t, discoverylimit.Config{IPLimit: 1})
	r := e.do(t, e.public, post(`{ lookupWorkspace(slug:"acme") { nosuchfield } }`, nil))
	if r.status != http.StatusUnprocessableEntity {
		t.Fatalf("validation error: %d %s", r.status, r.body)
	}
	if r := e.do(t, e.public, post(slugQuery, nil)); r.status != 200 {
		t.Fatalf("the invalid query must not have spent the budget: %d", r.status)
	}
}
