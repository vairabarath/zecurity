package discoverylimit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// GraphQL extensions.code values (and the REST error codes in lower case).
const (
	CodeRateLimited = "RATE_LIMITED"
	CodeUnavailable = "SERVICE_UNAVAILABLE"
)

const (
	msgRateLimited = "Too many requests. Try again later."
	msgUnavailable = "Service temporarily unavailable. Try again later."
)

// Denial is a refused discovery request: 429 (over a budget) or 503 (limiter
// unavailable).
type Denial struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

func denialFor(d Decision, err error) *Denial {
	if err != nil {
		return &Denial{Status: http.StatusServiceUnavailable, Code: CodeUnavailable}
	}
	if d.Allowed {
		return nil
	}
	return &Denial{Status: http.StatusTooManyRequests, Code: CodeRateLimited, RetryAfter: d.RetryAfter}
}

func retrySeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(math.Ceil(d.Seconds()))
}

// requestState is the limiter and client of one HTTP request (a websocket
// connection counts as one request).
type requestState struct {
	limiter   *Limiter
	clientKey string

	mu     sync.Mutex
	denial *Denial // the first denial in this request, for the HTTP status
}

// operationState collects denials within one GraphQL operation.
type operationState struct {
	mu     sync.Mutex
	denial *Denial
}

type requestKey struct{}
type operationKey struct{}

func record(mu *sync.Mutex, slot **Denial, d *Denial) {
	mu.Lock()
	defer mu.Unlock()
	if *slot == nil {
		*slot = d
	}
}

// WithRequest attaches the limiter and the client's bucket to ctx. Middleware
// does this for /graphql; tests that call a discovery resolver directly use it
// too (a resolver without it refuses to run).
func WithRequest(ctx context.Context, l *Limiter, clientKey string) context.Context {
	return context.WithValue(ctx, requestKey{}, &requestState{limiter: l, clientKey: clientKey})
}

// Charge is the first call of every discovery resolver. It charges one
// execution — so each alias, each fragment-selected field, on every transport
// and on the authenticated path alike — and returns a GraphQL error when the
// request must be refused. The Extension then replaces the whole response, so
// no other discovery field of the same operation returns data. email is the
// canonical email for lookupWorkspacesByEmail, empty otherwise.
func Charge(ctx context.Context, op Operation, email string) error {
	rs, _ := ctx.Value(requestKey{}).(*requestState)
	var d *Denial
	if rs == nil || rs.limiter == nil {
		// Wiring bug: the resolver was reached without the middleware. Refuse.
		log.Printf("discovery: no limiter in the request context, failing closed op=%s", op)
		d = &Denial{Status: http.StatusServiceUnavailable, Code: CodeUnavailable}
	} else {
		d = denialFor(rs.limiter.Allow(ctx, op, rs.clientKey, email))
		if d != nil {
			record(&rs.mu, &rs.denial, d)
		}
	}
	if d == nil {
		return nil
	}
	if st, _ := ctx.Value(operationKey{}).(*operationState); st != nil {
		record(&st.mu, &st.denial, d)
	}
	return gqlError(d)
}

func gqlError(d *Denial) *gqlerror.Error {
	e := &gqlerror.Error{Message: msgRateLimited, Extensions: map[string]any{"code": d.Code}}
	if d.Code == CodeUnavailable {
		e.Message = msgUnavailable
	}
	if s := retrySeconds(d.RetryAfter); s > 0 {
		e.Extensions["retryAfterSeconds"] = s
	}
	return e
}

// deniedResponse is the whole response of an operation in which any discovery
// field was refused: no data at all, one error. Replacing the response (rather
// than letting gqlgen null only the refused field) keeps an aliased or mixed
// request from returning the fields that ran before the budget ran out.
func deniedResponse(d *Denial) *graphql.Response {
	return &graphql.Response{Data: json.RawMessage("null"), Errors: gqlerror.List{gqlError(d)}}
}

// Extension is the gqlgen hook that gives each operation its own denial slot
// and replaces the response of a refused one. It applies to every transport,
// including websocket, where it is the only signal (a websocket message has no
// HTTP status). Responses without a denial pass through unchanged.
type Extension struct{}

var _ interface {
	graphql.HandlerExtension
	graphql.ResponseInterceptor
} = Extension{}

func (Extension) ExtensionName() string                          { return "DiscoveryRateLimit" }
func (Extension) Validate(schema graphql.ExecutableSchema) error { return nil }

func (Extension) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	st := &operationState{}
	resp := next(context.WithValue(ctx, operationKey{}, st))
	st.mu.Lock()
	d := st.denial
	st.mu.Unlock()
	if d != nil {
		return deniedResponse(d)
	}
	return resp
}

// Middleware wraps /graphql (both the public and the authenticated route): it
// attaches the request state, and when a discovery field was refused it sends
// the response with 429 (+ Retry-After) or 503 instead of gqlgen's status.
func Middleware(l *Limiter, ips IPResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithRequest(r.Context(), l, ClientKey(ips.ClientAddr(r)))
			rs := ctx.Value(requestKey{}).(*requestState)
			next.ServeHTTP(&statusWriter{ResponseWriter: w, rs: rs}, r.WithContext(ctx))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	rs          *requestState
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.rs.mu.Lock()
		d := w.rs.denial
		w.rs.mu.Unlock()
		if d != nil {
			code = d.Status
			if s := retrySeconds(d.RetryAfter); s > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(s))
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack lets the websocket transport take over the connection.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("discovery: underlying ResponseWriter is not an http.Hijacker")
	}
	return hj.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ProtectHTTP charges a REST discovery request before next runs: 429
// {"error":"rate_limited"} with Retry-After over a budget, 503
// {"error":"service_unavailable"} if the limiter is unavailable. next's own
// answers are untouched.
func ProtectHTTP(l *Limiter, ips IPResolver, op Operation, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d := denialFor(l.Allow(r.Context(), op, ClientKey(ips.ClientAddr(r)), ""))
		if d == nil {
			next.ServeHTTP(w, r)
			return
		}
		code := "rate_limited"
		if d.Code == CodeUnavailable {
			code = "service_unavailable"
		}
		if s := retrySeconds(d.RetryAfter); s > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(d.Status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
	})
}
