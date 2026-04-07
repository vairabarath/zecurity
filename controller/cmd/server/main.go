package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/joho/godotenv"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/graph/resolvers"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func main() {
	loadOptionalEnv()

	ctx := context.Background()

	if err := db.Init(ctx); err != nil {
		log.Fatalf("db init: %v", err)
	}
	defer db.Close()

	pkiService, err := pki.Init(ctx, db.Pool)
	if err != nil {
		log.Fatalf("pki init: %v", err)
	}

	bootstrapSvc := &bootstrap.Service{
		Pool:       db.Pool,
		PKIService: pkiService,
	}

	tenantDB := db.NewTenantDB(db.Pool)

	authSvc, err := auth.NewService(auth.Config{
		Pool:               db.Pool,
		BootstrapService:   bootstrapSvc,
		JWTSecret:          mustEnv("JWT_SECRET"),
		JWTIssuer:          appmeta.ControllerIssuer,
		GoogleClientID:     mustEnv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: mustEnv("GOOGLE_CLIENT_SECRET"),
		RedirectURI:        mustEnv("GOOGLE_REDIRECT_URI"),
		RedisURL:           mustEnv("REDIS_URL"),
		AllowedOrigin:      mustEnv("ALLOWED_ORIGIN"),
	})
	if err != nil {
		log.Fatalf("auth init: %v", err)
	}

	gqlSrv := handler.NewDefaultServer(
		graph.NewExecutableSchema(graph.Config{
			Resolvers: &resolvers.Resolver{
				TenantDB:    tenantDB,
				AuthService: authSvc,
			},
		}),
	)

	mux := http.NewServeMux()
	mux.Handle("/auth/callback", authSvc.CallbackHandler())
	mux.Handle("/auth/refresh", authSvc.RefreshHandler())
	mux.Handle("/health", healthHandler())

	if os.Getenv("ENV") == "development" {
		mux.Handle("/playground", playground.Handler(appmeta.ProductName, "/graphql"))
	}

	protected := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.WorkspaceGuard(db.Pool)(gqlSrv),
	)
	mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))

	addr := ":" + envOr("PORT", "8080")
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

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
		_, _ = w.Write([]byte(`{"status":"ok"}`))
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

func loadOptionalEnv() {
	candidates := []string{
		".env",
		"controller/.env",
	}

	for _, path := range candidates {
		err := godotenv.Load(path)
		if err == nil {
			log.Printf("loaded environment from %s", path)
			return
		}

		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		log.Fatalf("load %s: %v", path, err)
	}
}
