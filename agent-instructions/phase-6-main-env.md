# Phase 6 — main.go + Environment

Wire everything together. The HTTP server starts only after DB, PKI, and Auth are ready.

---

## File 1: `controller/cmd/server/main.go`

**Path:** `controller/cmd/server/main.go`

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func main() {
	ctx := context.Background()

	// 1. Connect to Postgres
	if err := db.Init(ctx); err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer db.Close()
	log.Println("✓ database connected")

	// 2. Initialize PKI — HTTP server does not start until CAs exist
	pkiService, err := pki.Init(ctx, db.Pool)
	if err != nil {
		log.Fatalf("pki init: %v", err)
	}
	log.Println("✓ PKI ready (root CA + intermediate CA)")

	// 3. Build shared infrastructure
	tenantDB := db.NewTenantDB(db.Pool)

	// 4. Build Auth service (Member 2 implements auth.NewService)
	authSvc := auth.NewService(auth.Config{
		Pool:               db.Pool,
		PKIService:         pkiService,
		JWTSecret:          mustEnv("JWT_SECRET"),
		GoogleClientID:     mustEnv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: mustEnv("GOOGLE_CLIENT_SECRET"),
		GoogleRedirectURI:  mustEnv("GOOGLE_REDIRECT_URI"),
		RedisURL:           mustEnv("REDIS_URL"),
	})

	// 5. Build GraphQL server
	gqlSrv := handler.NewDefaultServer(
		graph.NewExecutableSchema(graph.Config{
			Resolvers: &graph.Resolver{
				TenantDB:    tenantDB,
				AuthService: authSvc,
			},
		}),
	)

	// 6. Register routes
	mux := http.NewServeMux()

	// Public — no auth middleware
	mux.Handle("/auth/callback", authSvc.CallbackHandler())
	mux.Handle("/auth/refresh", authSvc.RefreshHandler())
	mux.Handle("/health", healthHandler())

	// Playground — development only
	if os.Getenv("ENV") == "development" {
		mux.Handle("/playground", playground.Handler("ZTNA", "/graphql"))
		log.Println("✓ playground available at /playground")
	}

	// Protected GraphQL endpoint
	// Chain: AuthMiddleware → WorkspaceGuard → GraphQL handler
	// Public mutations (initiateAuth) bypass via operationName check
	jwtSecret := mustEnv("JWT_SECRET")
	protected := middleware.AuthMiddleware(jwtSecret)(
		middleware.WorkspaceGuard(db.Pool)(gqlSrv),
	)
	mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))

	// 7. Start server
	addr := ":" + envOr("PORT", "8080")
	log.Printf("✓ listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// routeGraphQL sends public mutations directly to gqlSrv (no auth)
// and everything else through the protected chain.
// Public mutations are identified by the X-Operation header,
// set by Apollo Client for the initiateAuth mutation only.
func routeGraphQL(protected, public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Public-Operation") == "initiateAuth" {
			public.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := db.Pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

---

## File 2: `controller/.env.example`

**Path:** `controller/.env.example`

```env
DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform
REDIS_URL=redis://localhost:6379
PORT=8080
ENV=development

JWT_SECRET=replace_with_32_plus_random_bytes
JWT_ISSUER=ztna-controller

GOOGLE_CLIENT_ID=your_google_client_id
GOOGLE_CLIENT_SECRET=your_google_client_secret
GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback

PKI_MASTER_SECRET=replace_with_64_plus_random_bytes

ALLOWED_ORIGIN=http://localhost:5173
```

---

## Verification Checklist

```
[ ] server starts and /health returns 200
[ ] /graphql returns 401 without JWT
[ ] /graphql returns user data with valid JWT
[ ] /auth/callback route registered (Member 2's handler)
[ ] /playground available in development mode only
```
