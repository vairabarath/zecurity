package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/graph/resolvers"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/shield"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
		ValkeyURL:          mustEnv("VALKEY_URL"),
		AllowedOrigin:      mustEnv("ALLOWED_ORIGIN"),
	})
	if err != nil {
		log.Fatalf("auth init: %v", err)
	}

	connectorCfg := connector.Config{
		CertTTL:             mustDuration("CONNECTOR_CERT_TTL", 7*24*time.Hour),
		EnrollmentTokenTTL:  mustDuration("CONNECTOR_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
		HeartbeatInterval:   mustDuration("CONNECTOR_HEARTBEAT_INTERVAL", 30*time.Second),
		DisconnectThreshold: mustDuration("CONNECTOR_DISCONNECT_THRESHOLD", 90*time.Second),
		GRPCPort:            envOr("GRPC_PORT", "9090"),
		JWTSecret:           mustEnv("JWT_SECRET"),
		RenewalWindow:       mustDuration("CONNECTOR_RENEWAL_WINDOW", 48*time.Hour),
	}

	shieldCfg := shield.Config{
		CertTTL:             mustDuration("SHIELD_CERT_TTL", 7*24*time.Hour),
		RenewalWindow:       mustDuration("SHIELD_RENEWAL_WINDOW", 48*time.Hour),
		EnrollmentTokenTTL:  mustDuration("SHIELD_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
		DisconnectThreshold: mustDuration("SHIELD_DISCONNECT_THRESHOLD", 120*time.Second),
		JWTSecret:           mustEnv("JWT_SECRET"),
	}

	connectorValkey, err := newConnectorValkeyClient(ctx, mustEnv("VALKEY_URL"))
	if err != nil {
		log.Fatalf("connector valkey init: %v", err)
	}

	shieldSvc := shield.NewService(shieldCfg, db.Pool, pkiService, valkeycompat.NewAdapter(connectorValkey))

	gqlSrv := handler.NewDefaultServer(
		graph.NewExecutableSchema(graph.Config{
			Resolvers: &resolvers.Resolver{
				TenantDB:     tenantDB,
				AuthService:  authSvc,
				ConnectorCfg: connectorCfg,
				Redis:        valkeycompat.NewAdapter(connectorValkey),
				Pool:         db.Pool,
				ShieldSvc:    shieldSvc,
				ResourceCfg:  resource.NewConfig(db.Pool),
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

	mux.HandleFunc("/ca.crt", connector.CAEndpointHandler(db.Pool))

	// REST endpoint: POST /connectors/{id}/token — regenerates enrollment token.
	connectorTokenRoute := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.WorkspaceGuard(db.Pool)(
			connector.RegenerateTokenHandler(db.Pool, connectorCfg, valkeycompat.NewAdapter(connectorValkey)),
		),
	)
	mux.Handle("/api/connectors/", connectorTokenRoute)

	grpcListener, err := net.Listen("tcp", ":"+connectorCfg.GRPCPort)
	if err != nil {
		log.Fatalf("grpc listen: %v", err)
	}

	connectorStore := connectorWorkspaceStore{pool: db.Pool}
	controllerTLS, err := pkiService.GenerateControllerServerTLS(ctx, controllerCertHosts(connectorCfg.GRPCPort), connectorCfg.CertTTL)
	if err != nil {
		log.Fatalf("generate controller gRPC TLS cert: %v", err)
	}
	controllerCert, err := tls.X509KeyPair([]byte(controllerTLS.CertificatePEM), []byte(controllerTLS.PrivateKeyPEM))
	if err != nil {
		log.Fatalf("load controller gRPC TLS keypair: %v", err)
	}

	validator := connector.NewTrustDomainValidator(appmeta.SPIFFEGlobalTrustDomain, connectorStore)
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{controllerCert},
			ClientAuth:   tls.RequestClientCert,
			MinVersion:   tls.VersionTLS13,
		})),
		grpc.UnaryInterceptor(connector.UnarySPIFFEInterceptor(validator, connectorStore)),
	)

	connectorSvc := &connector.EnrollmentHandler{
		Cfg:        connectorCfg,
		Pool:       db.Pool,
		Redis:      valkeycompat.NewAdapter(connectorValkey),
		PKIService: pkiService,
		ShieldSvc: shieldSvc,
	}
	pb.RegisterConnectorServiceServer(grpcServer, connectorSvc)
	shieldpb.RegisterShieldServiceServer(grpcServer, shieldSvc)

	go connector.RunDisconnectWatcher(ctx, db.Pool, connectorCfg)
	go shieldSvc.RunDisconnectWatcher(ctx)

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

// publicOperations is the set of GraphQL operation names that may be called
// without a JWT. The frontend sets `X-Public-Operation: <OperationName>` for
// these via admin/src/apollo/links/auth.ts. Names match the PascalCase
// `mutation ... { ... }` / `query ... { ... }` names in admin/src/graphql/.
var publicOperations = map[string]struct{}{
	"InitiateAuth":            {}, // login redirect (Member 2)
	"LookupWorkspace":         {}, // signup slug availability (Member 1 login flow)
	"LookupWorkspacesByEmail": {}, // login workspace picker (Member 1 login flow)
}

func routeGraphQL(protected, public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := r.Header.Get("X-Public-Operation")
		if _, ok := publicOperations[op]; ok {
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

func newConnectorValkeyClient(ctx context.Context, valkeyURL string) (valkey.Client, error) {
	addr, err := parseValkeyAddr(valkeyURL)
	if err != nil {
		return nil, fmt.Errorf("parse valkey URL: %w", err)
	}

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		return nil, fmt.Errorf("create valkey client: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Do(pingCtx, client.B().Ping().Build()).Error(); err != nil {
		return nil, fmt.Errorf("ping valkey: %w", err)
	}

	checkValkeyVersion(ctx, client)

	return client, nil
}

func checkValkeyVersion(ctx context.Context, client valkey.Client) {
	resp := client.Do(ctx, client.B().Info().Section("server").Build())
	info, err := resp.ToString()
	if err != nil {
		log.Fatalf("valkey: cannot get server info: %v", err)
	}

	if strings.Contains(info, "valkey_version:") {
		version := parseVersionFromInfo(info, "valkey_version")
		log.Printf("✓ Valkey %s connected", version)
		return
	}

	if strings.Contains(info, "redis_version:") {
		version := parseVersionFromInfo(info, "redis_version")
		log.Printf("✓ Redis %s connected (Valkey recommended)", version)
		if !versionAtLeast(version, 6, 2) {
			log.Fatalf("Redis %s too old. Requires 6.2+ for GETDEL. "+
				"Switch to Valkey 7.2+", version)
		}
		return
	}

	log.Fatal("cache: unrecognized server. Expected Valkey 7.2+ or Redis 6.2+")
}

func parseVersionFromInfo(info, key string) string {
	for _, line := range strings.Split(info, "\r\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return "unknown"
}

func versionAtLeast(version string, major, minor int) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && min >= minor)
}

func parseValkeyAddr(rawURL string) (string, error) {
	after, found := strings.CutPrefix(rawURL, "redis://")
	if !found {
		return "", fmt.Errorf("expected redis:// URL, got: %s", rawURL)
	}
	if idx := strings.LastIndex(after, "@"); idx != -1 {
		after = after[idx+1:]
	}
	return after, nil
}

type connectorWorkspaceStore struct {
	pool *pgxpool.Pool
}

func (s connectorWorkspaceStore) GetByTrustDomain(ctx context.Context, domain string) (*connector.WorkspaceLookup, error) {
	var workspace connector.WorkspaceLookup
	err := s.pool.QueryRow(ctx,
		`SELECT id, status FROM workspaces WHERE trust_domain = $1`,
		domain,
	).Scan(&workspace.ID, &workspace.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace by trust domain: %w", err)
	}

	return &workspace, nil
}

func (s connectorWorkspaceStore) GetWorkspaceCAByTrustDomain(ctx context.Context, domain string) (*x509.Certificate, error) {
	var certPEM string
	err := s.pool.QueryRow(ctx,
		`SELECT wk.certificate_pem
		   FROM workspace_ca_keys wk
		   JOIN workspaces w ON w.id = wk.tenant_id
		  WHERE w.trust_domain = $1
		    AND w.status = 'active'`,
		domain,
	).Scan(&certPEM)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get workspace CA by trust domain: %w", err)
	}

	return parseCertPEM(certPEM)
}

func (s connectorWorkspaceStore) GetIntermediateCA(ctx context.Context) (*x509.Certificate, error) {
	var certPEM string
	err := s.pool.QueryRow(ctx,
		`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
	).Scan(&certPEM)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get intermediate CA: %w", err)
	}

	return parseCertPEM(certPEM)
}

func parseCertPEM(certPEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return nil, fmt.Errorf("decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}

	return cert, nil
}

func controllerCertHosts(grpcPort string) []string {
	hosts := map[string]struct{}{
		"localhost": {},
		"127.0.0.1": {},
		"::1":       {},
	}

	if addr := os.Getenv("CONTROLLER_ADDR"); addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			hosts[host] = struct{}{}
		}
	}

	if addr := os.Getenv("CONTROLLER_HTTP_ADDR"); addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
			hosts[host] = struct{}{}
		}
	}

	result := make([]string, 0, len(hosts))
	for host := range hosts {
		result = append(result, host)
	}

	return result
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
