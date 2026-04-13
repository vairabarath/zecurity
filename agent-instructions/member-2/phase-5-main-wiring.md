# Phase 5 — main.go Wiring

## Objective

Wire everything from Phases 1-4 into the controller's `main.go`: add the `mustDuration` helper, populate `connector.Config` from env vars, register the `/ca.crt` HTTP route, and start the gRPC server with the SPIFFE interceptor.

---

## Prerequisites

- **Phase 1** completed (proto stubs generated)
- **Phase 2** completed (Config struct exists)
- **Phase 4** completed (CAEndpointHandler exists)
- **Phase 6** completed or in progress (.env has new vars)
- **Member 3's `spiffe.go`** merged (for `UnarySPIFFEInterceptor` + `NewTrustDomainValidator`)
  - If not yet merged: wire with `// TODO` placeholder — see fallback below

---

## File to Modify

```
controller/cmd/server/main.go
```

---

## Current State of main.go

```go
// Existing imports (lines 3-21):
// "context", "errors", "log", "net/http", "os"
// gqlgen, godotenv, appmeta, auth, bootstrap, db, middleware, pki

// Existing functions:
// main()          — lines 23-86
// routeGraphQL()  — lines 88-97
// healthHandler() — lines 99-109
// mustEnv()       — lines 111-118
// envOr()         — lines 120-126
// loadOptionalEnv() — lines 128-147
```

---

## Changes to Make

### 1. Add new imports

Add these to the existing import block:

```go
"net"
"time"

"google.golang.org/grpc"
"google.golang.org/grpc/credentials"

"github.com/yourorg/ztna/controller/internal/connector"
"github.com/yourorg/ztna/controller/internal/appmeta"     // already imported
pb "github.com/yourorg/ztna/controller/proto/connector"
```

Note: `appmeta` is already imported. Don't duplicate it. `net` and `time` are new stdlib imports.

### 2. Add `mustDuration` helper

Add after the existing `envOr` function (after line 126):

```go
// mustDuration reads an env var as a time.Duration with a fallback default.
// Same pattern as mustEnv/envOr — env vars override defaults, missing vars
// silently use the fallback. Invalid durations are fatal (startup error).
func mustDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("env var %s is not a valid duration: %s", key, v)
	}
	return d
}
```

### 3. Add ConnectorConfig + gRPC server wiring in `main()`

Add after the existing `mux.Handle("/graphql", ...)` line (after line 81), before `addr := ...` (line 83):

```go
	// ── Connector subsystem ─────────────────────────────────────────────
	connectorCfg := connector.Config{
		CertTTL:             mustDuration("CONNECTOR_CERT_TTL", 7*24*time.Hour),
		EnrollmentTokenTTL:  mustDuration("CONNECTOR_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
		HeartbeatInterval:   mustDuration("CONNECTOR_HEARTBEAT_INTERVAL", 30*time.Second),
		DisconnectThreshold: mustDuration("CONNECTOR_DISCONNECT_THRESHOLD", 90*time.Second),
		GRPCPort:            envOr("GRPC_PORT", "9090"),
		JWTSecret:           mustEnv("JWT_SECRET"),
	}

	// Serve Intermediate CA cert for connector enrollment trust bootstrap.
	mux.HandleFunc("/ca.crt", connector.CAEndpointHandler(db.Pool))

	// ── gRPC server for connector Enroll + Heartbeat RPCs ───────────────
	grpcListener, err := net.Listen("tcp", ":"+connectorCfg.GRPCPort)
	if err != nil {
		log.Fatalf("grpc listen: %v", err)
	}

	// TODO: Add TLS credentials once controller TLS cert is configured.
	// For development, use insecure credentials.
	grpcServer := grpc.NewServer(
		// TODO: grpc.Creds(tlsCreds),
		// TODO: Wire UnarySPIFFEInterceptor once Member 3's spiffe.go lands:
		// grpc.UnaryInterceptor(
		//     connector.UnarySPIFFEInterceptor(
		//         connector.NewTrustDomainValidator(
		//             appmeta.SPIFFEGlobalTrustDomain,
		//             workspaceStore,
		//         ),
		//     ),
		// ),
	)

	// TODO: Register ConnectorService once Member 3's service implementation lands:
	// pb.RegisterConnectorServiceServer(grpcServer, connector.NewService(connectorCfg, db.Pool, pkiService, redisClient))

	go func() {
		log.Printf("gRPC server listening on :%s", connectorCfg.GRPCPort)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("grpc serve: %v", err)
		}
	}()
```

### 4. Full main() function after changes

For reference, the complete `main()` will look like:

```go
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

	// ── Connector subsystem ─────────────────────────────────────────────
	connectorCfg := connector.Config{
		CertTTL:             mustDuration("CONNECTOR_CERT_TTL", 7*24*time.Hour),
		EnrollmentTokenTTL:  mustDuration("CONNECTOR_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
		HeartbeatInterval:   mustDuration("CONNECTOR_HEARTBEAT_INTERVAL", 30*time.Second),
		DisconnectThreshold: mustDuration("CONNECTOR_DISCONNECT_THRESHOLD", 90*time.Second),
		GRPCPort:            envOr("GRPC_PORT", "9090"),
		JWTSecret:           mustEnv("JWT_SECRET"),
	}

	mux.HandleFunc("/ca.crt", connector.CAEndpointHandler(db.Pool))

	grpcListener, err := net.Listen("tcp", ":"+connectorCfg.GRPCPort)
	if err != nil {
		log.Fatalf("grpc listen: %v", err)
	}

	grpcServer := grpc.NewServer()

	go func() {
		log.Printf("gRPC server listening on :%s", connectorCfg.GRPCPort)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Fatalf("grpc serve: %v", err)
		}
	}()

	addr := ":" + envOr("PORT", "8080")
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
```

---

## Fallback: If Member 3's spiffe.go is NOT merged

Use a bare `grpc.NewServer()` without the interceptor. The gRPC server will start and accept connections but won't validate SPIFFE identity. This is fine for development — the interceptor is wired in once Member 3 delivers `spiffe.go`.

Leave these TODO comments in the code:

```go
// TODO: Add TLS credentials for production
// TODO: Wire UnarySPIFFEInterceptor from connector.spiffe.go
// TODO: Register ConnectorServiceServer from connector.NewService()
```

---

## Verification

```bash
cd controller && go build ./cmd/server/
```

Then run:

```bash
cd controller && go run ./cmd/server/
```

Expected log output:

```
loaded environment from controller/.env
gRPC server listening on :9090
listening on :8080
```

Test the CA endpoint:

```bash
curl -s http://localhost:8080/ca.crt | head -1
# Expected: -----BEGIN CERTIFICATE-----
```

- [ ] `mustDuration` function added after `envOr`
- [ ] `connector.Config{}` populated from env vars
- [ ] `/ca.crt` route registered on HTTP mux
- [ ] gRPC listener created on `connectorCfg.GRPCPort`
- [ ] gRPC server started in goroutine
- [ ] `go build ./cmd/server/` compiles
- [ ] Server starts and logs both HTTP and gRPC ports
- [ ] `curl /ca.crt` returns PEM certificate

---

## DO NOT TOUCH

- Existing routes (`/auth/callback`, `/auth/refresh`, `/health`, `/graphql`, `/playground`) — unchanged
- `routeGraphQL()`, `healthHandler()` — unchanged
- `mustEnv()`, `envOr()`, `loadOptionalEnv()` — unchanged (only ADD `mustDuration`)
- Auth service initialization — unchanged
- PKI initialization — unchanged

---

## After This Phase

Proceed to Phase 6 (.env updates). Then Member 2's work is complete pending Member 3's interceptor + service implementation.
