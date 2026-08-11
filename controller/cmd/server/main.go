package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	clientpb "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/graph/resolvers"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	clientsvc "github.com/yourorg/ztna/controller/internal/client"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/discovery"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/metrics"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/netutil"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/relay"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/shield"
	"github.com/yourorg/ztna/controller/internal/transport"
	// "golang.org/x/text/cases"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func main() {
	loadOptionalEnv()

	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)

	defer stop()
	var wg sync.WaitGroup

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

	// Identity-connection store (Bootstrap + Enterprise IdPs). Reuses the PKI
	// service to decrypt per-workspace OIDC client secrets at rest (PENDING-04).
	idpStore := idp.NewStore(db.Pool, pkiService)

	// Identity pipeline (PENDING-04 Phase 5): resolve → lifecycle → link →
	// Principal → event. bootstrapSvc is the workspace-creating Provisioner it
	// invokes on a resolver miss; the audit sink writes identity events to
	// audit_logs.
	identitySvc := identity.NewService(
		db.Pool,
		identity.NewLinker(bootstrapSvc),
		identity.NewAuditSink(db.Pool),
	)

	authSvc, err := auth.NewService(auth.Config{
		Pool:               db.Pool,
		IdentityService:    identitySvc,
		IdpStore:           idpStore,
		JWTSecret:          mustEnv("JWT_SECRET"),
		JWTIssuer:          appmeta.ControllerIssuer,
		GoogleClientID:     mustEnv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: mustEnv("GOOGLE_CLIENT_SECRET"),
		RedirectURI:        mustEnv("GOOGLE_REDIRECT_URI"),
		ValkeyURL:          mustEnv("VALKEY_URL"),
		AllowedOrigin:      envOr("APP_BASE_URL", "http://localhost:5173"),
	})
	if err != nil {
		log.Fatalf("auth init: %v", err)
	}

	// Session-generation revoker (PENDING-04 Phase 6): admin actions that remove
	// a login path (disable/delete a connection) bump users.identity_generation
	// and drop the live refresh session — authSvc satisfies
	// identity.SessionInvalidator. Break-glass admins may always authenticate via
	// the platform IdP so a workspace can never lock itself out (ADR-024 §5).
	identityRevoker := identity.NewRevoker(db.Pool, authSvc, identity.NewAuditSink(db.Pool))
	breakGlassEmails := parseBreakGlassEmails(os.Getenv("IDP_BREAK_GLASS_EMAILS"))

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
	relayStore := relay.NewStore(db.Pool)
	providerStore := provider.NewStore(db.Pool)
	seedProviderUsers(ctx, providerStore)
	relaySvc := relay.NewService(pkiService, relayStore, mustDuration("RELAY_CERT_TTL", 30*24*time.Hour)).
		WithHeartbeatCache(
			valkeycompat.NewAdapter(connectorValkey),
			mustDuration("RELAY_HEARTBEAT_DB_WRITE_INTERVAL", 5*time.Minute),
		).WithProvisioningAuth(mustEnv("JWT_SECRET"))
	relayRevChecker := connector.NewRelayRevocationChecker(func(ctx context.Context) ([]string, error) {
		rs, err := relayStore.ListRevokedRelaySerials(ctx)
		if err != nil {
			return nil, err
		}
		serials := make([]string, len(rs))
		for i, r := range rs {
			serials[i] = r.Serial
		}
		return serials, nil
	})
	if err := relayRevChecker.Refresh(ctx); err != nil {
		log.Printf("relay revocation checker: initial load failed (relays denied u succeeds): %v", err)
	}
	relayRevChecker.SpawnRefresh(ctx, 60*time.Second, func(err error) {
		log.Printf("relay revocation checker: refresh failed: %v", err)
	})
	connectorRegistry := connector.NewConnectorRegistry()

	inviteStore := invitation.NewStore(db.Pool)
	inviteEmailer := invitation.NewEmailer(
		envOr("SMTP_HOST", ""),
		envOr("SMTP_PORT", "587"),
		envOr("SMTP_FROM", ""),
		envOr("SMTP_PASSWORD", ""),
		envOr("APP_BASE_URL", "http://localhost:5173"),
	)
	inviteHandler := invitation.NewHandler(inviteStore, inviteEmailer)

	policyStore := policy.NewStore(db.Pool)
	policyCache := policy.NewSnapshotCache()
	policyNotifier := policy.NewNotifier(policyCache)
	policy.RegisterExpiryNotifier(func(workspaceID string) {
		if err := policyNotifier.NotifyPolicyChange(
			context.Background(),
			workspaceID,
		); err != nil {
			log.Printf("expiry policy notification: %v", err)
		}
	})
	// ADR-015/017 Track B: transport (connectivity) plane, independent of the
	// ACL (authorization) plane. Relay metadata/eviction and connector relay
	// placement changes drive transportNotifier.NotifyTopologyChange — never
	// the policy notifier — so they never recompile or bump the ACL.
	transportStore := transport.NewStore(db.Pool)
	transportCache := transport.NewSnapshotCache()
	transportNotifier := transport.NewNotifier(transportCache)
	transportCompiler := transport.NewCompiler(transportStore, transportCache, transportNotifier)
	relaySvc.WithTransportNotifier(transportNotifier)
	postureStore := posture.NewStore(db.Pool)
	postureEvaluator := posture.NewEvaluator(postureStore, policyNotifier)

	// ADR-016 C5: build a fresh LabelledRelayList and fan it out to all
	// connected connectors. Triggered on capacity-tier promotion, address
	// changes, and eviction. The version is a content fingerprint set by
	// BuildLabelledRelayList (F11): both this broadcast path and the
	// connect-time push in control_stream.go agree on it, and it changes iff
	// the eligible set / an address / a label changes — so a connector
	// re-probes exactly when the pool actually changed.
	broadcastRelayList := func(ctx context.Context) {
		list, err := relayStore.BuildLabelledRelayList(ctx)
		if err != nil {
			log.Printf("relay pool broadcast: build list: %v", err)
			return
		}
		connectorRegistry.BroadcastRelayList(list)
	}
	relaySvc.WithRelayPoolBroadcaster(broadcastRelayList)

	gqlSrv := handler.NewDefaultServer(
		graph.NewExecutableSchema(graph.Config{
			Resolvers: &resolvers.Resolver{
				TenantDB:          tenantDB,
				AuthService:       authSvc,
				ConnectorCfg:      connectorCfg,
				ConnectorRegistry: connectorRegistry,
				Redis:             valkeycompat.NewAdapter(connectorValkey),
				Pool:              db.Pool,
				ShieldSvc:         shieldSvc,
				ResourceCfg:       resource.NewConfig(db.Pool),
				InvitationStore:   inviteStore,
				InvitationEmailer: inviteEmailer,
				PolicyStore:       policyStore,
				PostureStore:      postureStore,
				PostureEvaluator:  postureEvaluator,
				PolicyNotifier:    policyNotifier,
				TransportNotifier: transportNotifier,
				IdpStore:          idpStore,
				Revoker:           identityRevoker,
				BreakGlassEmails:  breakGlassEmails,
			},
			Directives: graph.DirectiveRoot{
				HasRole: resolvers.HasRole,
			},
		}),
	)
	// Fail-closed error masking: only apperr.UserError + gqlgen parse/validation
	// errors reach clients; raw resolver/DB errors are logged and genericized.
	gqlSrv.SetErrorPresenter(resolvers.ErrorPresenter)

	// Disable GraphQL introspection outside dev — gqlgen's NewDefaultServer
	// enables it by default via extension.Introspection{}, which exposes the
	// full schema to any caller of /graphql. This is the standard pentest
	// finding (CWE-200). Closes STAGE3-F1 of the connector flow audit.
	//
	// The introspectionDisabler extension below runs AFTER NewDefaultServer's
	// extension.Introspection (extensions are applied in install order; the
	// later one wins for shared OperationContext fields).
	if os.Getenv("ENV") != "development" {
		gqlSrv.Use(introspectionDisabler{})
	}

	mux := http.NewServeMux()
	pInit, pCallback, err := auth.ProviderRoutes(
		authSvc,
		providerStore,
		mustEnv("PROVIDER_GOOGLE_REDIRECT_URI"),
		15*time.Minute,
	)
	if err != nil {
		log.Fatalf("provider auth routes: %v", err)
	}
	mux.Handle("/provider/auth/initiate", pInit)
	mux.Handle("/provider/auth/callback", pCallback)
	// Provider plane (PENDING-07a): routes behind RequireProvider — provider JWT
	// (aud=provider) + active provider_users allowlist. NEVER WorkspaceGuard;
	// provider identity has no tenant. M2 hangs POST /provider/relays here.
	providerAuthz := provider.NewAuthz()
	providerHandlers := provider.NewHandlers(providerStore, providerAuthz)
	requireProvider := middleware.RequireProvider(mustEnv("JWT_SECRET"), providerStore)
	mux.Handle("GET /provider/me", requireProvider(http.HandlerFunc(providerHandlers.Me)))
	mux.Handle("GET /provider/users", requireProvider(http.HandlerFunc(providerHandlers.ListUsers)))
	mux.Handle("/auth/callback", authSvc.CallbackHandler())
	// Public, read-only login discovery (workspace-first). PENDING-04 / ADR-024.
	mux.Handle("GET /workspaces/{slug}/auth", authSvc.DiscoveryHandler())
	mux.Handle("/auth/refresh", authSvc.RefreshHandler())
	mux.Handle("/auth/logout", authSvc.LogoutHandler())
	mux.Handle("/health", healthHandler())

	if os.Getenv("ENV") == "development" {
		mux.Handle("/playground", playground.Handler(appmeta.ProductName, "/graphql"))
	}

	protected := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.WorkspaceGuard(db.Pool)(gqlSrv),
	)
	mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))

	mux.HandleFunc("/ca.crt", connector.CAEndpointHandler(db.Pool))
	mux.HandleFunc("/ca.crl", connector.CRLEndpointHandler(db.Pool, pkiService))
	mux.HandleFunc("/relay.crl", relay.RelayCRLHandler(pkiService, relayStore))

	// REST endpoints: invitations
	inviteCreateRoute := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.RequireRole("admin")(
			middleware.WorkspaceGuard(db.Pool)(
				http.HandlerFunc(inviteHandler.Create),
			),
		),
	)
	mux.Handle("POST /api/invitations", inviteCreateRoute)
	mux.Handle("GET /api/invitations/{token}", http.HandlerFunc(inviteHandler.Get))
	inviteAcceptRoute := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.WorkspaceGuard(db.Pool)(
			http.HandlerFunc(inviteHandler.Accept),
		),
	)
	mux.Handle("POST /api/invitations/{token}/accept", inviteAcceptRoute)

	// REST endpoint: POST /connectors/{id}/token — regenerates enrollment token.
	connectorTokenRoute := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.RequireRole("admin")(
			middleware.WorkspaceGuard(db.Pool)(
				connector.RegenerateTokenHandler(db.Pool, connectorCfg, valkeycompat.NewAdapter(connectorValkey)),
			),
		),
	)
	mux.Handle("/api/connectors/", connectorTokenRoute)

	// REST endpoint: POST /shields/{id}/token — regenerates enrollment token.
	shieldTokenRoute := middleware.AuthMiddleware(mustEnv("JWT_SECRET"))(
		middleware.RequireRole("admin")(
			middleware.WorkspaceGuard(db.Pool)(
				shieldSvc.TokenHandler(),
			),
		),
	)
	mux.Handle("/api/shields/", shieldTokenRoute)

	// REST endpoint: POST /provider/relays — creates a relay registration +
	// provisioning token. Provider-plane action (PENDING-07a): guarded by
	// RequireProvider (aud=provider + active allowlist), NOT the tenant model.
	// The handler runs the Authz chokepoint and writes a provider_audit_logs row.
	relayAdminHandler := &relay.AdminHandler{
		Store:         relayStore,
		Redis:         valkeycompat.NewAdapter(connectorValkey),
		JWTSecret:     mustEnv("JWT_SECRET"),
		Authz:         providerAuthz,
		ProviderStore: providerStore,
	}
	// Post-commit convergence when a relay is revoked/deleted: rebuild+broadcast the
	// relay list (connectors migrate) AND invalidate the transport snapshot for each
	// affected workspace (Sprint-13 clients re-poll) — mirrors the expiry path.

	relayAdminHandler.OnRelayRevoked = func(ctx context.Context, relayID string) {
		if err := relayRevChecker.Refresh(ctx); err != nil {
			log.Printf("relay revoke fan-out: refresh revocation checker for %s: %v", relayID, err)
		}
		broadcastRelayList(ctx)
		conns, err := relayStore.ListConnectorsForRelay(ctx, relayID)
		if err != nil {
			log.Printf("relay revoke fan-out: list connectors for %s: %v", relayID, err)
			return
		}
		for wsID, connectorIDs := range conns {
			if err := transportNotifier.NotifyTopologyChange(ctx, wsID, connectorIDs); err != nil {
				log.Printf("relay revoke fan-out: transport notify ws=%s: %v", wsID, err)
			}
		}
	}
	mux.Handle("POST /provider/relays", requireProvider(http.HandlerFunc(relayAdminHandler.Create)))
	mux.Handle("POST /provider/relays/{id}/revoke", requireProvider(http.HandlerFunc(relayAdminHandler.Revoke)))
	mux.Handle("DELETE /provider/relays/{id}", requireProvider(http.HandlerFunc(relayAdminHandler.Delete)))
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
		grpc.UnaryInterceptor(connector.UnarySPIFFEInterceptor(validator, connectorStore, relayRevChecker)),
		grpc.StreamInterceptor(connector.StreamSPIFFEInterceptor(validator, connectorStore, relayRevChecker)),
	)

	connectorSvc := &connector.EnrollmentHandler{
		Cfg:               connectorCfg,
		Pool:              db.Pool,
		Redis:             valkeycompat.NewAdapter(connectorValkey),
		PKIService:        pkiService,
		ShieldSvc:         shieldSvc,
		Registry:          connectorRegistry,
		PostureStore:      postureStore,
		PolicyStore:       policyStore,
		PolicyCache:       policyCache,
		PolicyNotifier:    policyNotifier,
		RelayStore:        relayStore,
		RelayListSrc:      relayStore,
		TransportNotifier: transportNotifier,
	}
	pb.RegisterConnectorServiceServer(grpcServer, connectorSvc)
	shieldpb.RegisterShieldServiceServer(grpcServer, shieldSvc)
	relaypb.RegisterRelayServiceServer(grpcServer, relaySvc)

	// Proactive ACL propagation: after a policy change bumps the version and
	// invalidates the cache, push the fresh snapshot to all connected connectors
	// in the workspace immediately instead of waiting for the next heartbeat.
	// Heartbeat reconciliation remains the fallback for offline/missed connectors.
	aclPusher := connector.NewACLPusher(connectorRegistry, policyStore, postureStore, policyCache, policyNotifier)
	policyNotifier.RegisterPushHook(aclPusher.PushWorkspace)

	// Transport plane has no proactive push: the client is the sole consumer and
	// polls GetTransportSnapshot (plus an immediate re-poll on relay failure,
	// Phase C). A topology change just bumps the transport version + invalidates
	// the cache; the client's next poll picks it up. Connectors do not consume
	// the transport snapshot, so nothing is pushed to them.

	clientSvc := clientsvc.NewService(
		db.Pool,
		authSvc,
		pkiService,
		idpStore,
		mustEnv("CLIENT_GOOGLE_CLIENT_ID"),
		mustEnv("CLIENT_GOOGLE_CLIENT_SECRET"),
		mustEnv("CONTROLLER_HOST"),
		mustEnv("CONTROLLER_HTTP_URL"),
		policyStore,
		policyCache,
		policyNotifier,
		transportCompiler,
		postureStore,
		postureEvaluator,
	)
	clientpb.RegisterClientServiceServer(grpcServer, clientSvc)

	// REST endpoint: Google OAuth callback for CLI authentication (Option B flow).
	// Google redirects here after user consent; controller exchanges the code
	// server-side and redirects the browser to the CLI's local loopback server.
	mux.Handle("GET /api/clients/callback", clientSvc.AuthCallbackHandler())

	wg.Add(1)

	go func() {
		defer wg.Done()
		connector.RunDisconnectWatcher(ctx, db.Pool, connectorCfg, policyNotifier)
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()
		shieldSvc.RunDisconnectWatcher(ctx)
	}()

	wg.Add(1)

	go func() {
		defer wg.Done()
		relay.RunExpiryLoop(ctx, relayStore, transportNotifier, 60*time.Second, 90*time.Second, broadcastRelayList)
	}()

	retentionDays := envOrInt(
		"POSTURE_RETENTION_DAYS",
		30,
	)

	retentionBatchSize := envOrInt(
		"POSTURE_RETENTION_BATCH_SIZE",
		2000,
	)

	retentionWorker := posture.NewRetentionWorker(
		postureStore,
		time.Duration(retentionDays)*24*time.Hour,
		retentionBatchSize,
		nil,
	)
	wg.Add(1)

	go func() {
		defer wg.Done()
		retentionWorker.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-ticker.C:
				cutoff := time.Now().UTC().Add(-discovery.ScanResultTTL)
				if err := discovery.PurgeScanResults(ctx, db.Pool, cutoff); err != nil {
					log.Printf("discovery: purge scan results: %v", err)
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Printf("gRPC server listening on :%s", connectorCfg.GRPCPort)
		if err := grpcServer.Serve(grpcListener); err != nil && err != grpc.ErrServerStopped {
			log.Fatalf("grpc serve: %v", err)
		}
	}()

	// Metrics on a SEPARATE internal listener — they leak operational data, so they
	// must not sit on the public mux. Defaults to loopback; set METRICS_ADDR (e.g.
	// ":9102") to expose to a network scraper behind your own firewall. A failure
	// here is logged, not fatal — metrics are non-critical to serving traffic.

	metricsAddr := envOr("METRICS_ADDR", "127.0.0.1:9102")
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", metrics.Handler())
	metricsServer := &http.Server{
		Addr:    metricsAddr,
		Handler: metricsMux,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		log.Printf("metrics listenin on %s/metrics", metricsAddr)

		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("metrics server stopped: %v", err)
		}
	}()

	addr := ":" + envOr("PORT", "8080")
	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	log.Printf("listening on %s", addr)

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http serve: %v", err)

		}
	}()
	<-ctx.Done()
	log.Printf("shutdown requested")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http server shutdown error: %v", err)
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("metrics server shutdown error: %v", err)
	}
	grpcStopped := make(chan struct{})

	go func() {
		grpcServer.GracefulStop()
		close(grpcStopped)
	}()
	select {
	case <-grpcStopped:
		// Success. Nothing more required.
	case <-shutdownCtx.Done():
		log.Printf("gRPC graceful shutdown timed out; forcing stop")
		grpcServer.Stop()
	}

	wg.Wait()
}

// publicRootFields are the GraphQL root fields callable WITHOUT authentication —
// the pre-login entry points (login redirect + workspace discovery). This map is
// the SOLE source of truth for public routing: there is no client header and no
// duplicated frontend list. The allowlist is keyed on schema FIELD names (the
// server-executed contract), not client-chosen operation names.
//
// NOTE: `workspace`, `me`, `myDevices` have no @hasRole but DO require a tenant
// (they call tenant.MustGet) — they are authenticated, not public, and are
// intentionally absent here so they keep routing through the auth middleware.
var publicRootFields = map[string]struct{}{
	"initiateAuth":            {}, // login redirect
	"lookupWorkspace":         {}, // signup slug availability / login flow
	"lookupWorkspacesByEmail": {}, // login workspace picker
}

// routeGraphQL decides server-side whether a /graphql request may bypass the auth
// middleware. The decision is derived solely from the parsed request body (no
// client-controlled header): a request is public only if it is a single
// query/mutation whose every root field is in publicRootFields. Any ambiguity is
// fail-closed to the protected (auth-required) handler.
func routeGraphQL(protected, public http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicGraphQLRequest(r) {
			public.ServeHTTP(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// isPublicGraphQLRequest reads and restores the request body, then classifies it.
// Only JSON POST bodies are eligible; everything else (GET, multipart form,
// websocket upgrade, OPTIONS) is treated as protected.
func isPublicGraphQLRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	// Restore the body so the downstream gqlgen handler can read it again.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return requestSelectsOnlyPublicFields(body)
}

// requestSelectsOnlyPublicFields is the pure routing predicate (no net/http
// types, so it is unit-testable). It returns true only if body is a single
// GraphQL query/mutation whose every root selection is a plain field listed in
// publicRootFields. It is fail-closed on every deviation: batch arrays,
// APQ-hash-only (empty query), multiple operations, subscriptions, fragment
// spreads / inline fragments at the root, unknown fields, or parse errors all
// return false.
func requestSelectsOnlyPublicFields(body []byte) bool {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(body, &params); err != nil {
		return false // not a single JSON object (e.g. a batch array) or malformed
	}
	if strings.TrimSpace(params.Query) == "" {
		return false // APQ hash-only request, or no query present
	}

	doc, err := parser.ParseQuery(&ast.Source{Input: params.Query})
	if err != nil || doc == nil {
		return false
	}
	if len(doc.Operations) != 1 {
		return false // multi-operation document — which one runs is ambiguous
	}
	op := doc.Operations[0]
	if op.Operation != ast.Query && op.Operation != ast.Mutation {
		return false // subscriptions are never public
	}
	if len(op.SelectionSet) == 0 {
		return false
	}
	for _, sel := range op.SelectionSet {
		field, ok := sel.(*ast.Field)
		if !ok {
			return false // fragment spread or inline fragment at the root → protected
		}
		// Match on the real field name, never the alias.
		if _, public := publicRootFields[field.Name]; !public {
			return false
		}
	}
	return true
}

// introspectionDisabler is a gqlgen extension that turns introspection OFF
// per request by flipping OperationContext.DisableIntrospection back to true.
//
// gqlgen's NewDefaultServer installs extension.Introspection{}, which sets
// DisableIntrospection=false (enabling introspection). Installing this
// extension AFTER it flips the field back so introspection queries
// (__schema, __type) return an error to the caller. See STAGE3-F1.
type introspectionDisabler struct{}

func (introspectionDisabler) ExtensionName() string { return "DisableIntrospection" }

func (introspectionDisabler) Validate(_ graphql.ExecutableSchema) error { return nil }

func (introspectionDisabler) MutateOperationContext(
	_ context.Context,
	opCtx *graphql.OperationContext,
) *gqlerror.Error {
	opCtx.DisableIntrospection = true
	return nil
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

	// Always include the detected LAN IP so connectors on other LAN machines
	// can verify the controller cert when connecting via its LAN address.
	// Also add the nip.io DNS name (192-168-1-x.nip.io) so clients connecting
	// via nip.io hostname pass TLS validation without a real DNS certificate.
	if lanIP := netutil.DetectLANIP(); !netutil.IsLocalhost(lanIP) {
		hosts[lanIP] = struct{}{}
		hosts[strings.ReplaceAll(lanIP, ".", "-")+".nip.io"] = struct{}{}
	}

	if addr := os.Getenv("CONTROLLER_ADDR"); addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" && !netutil.IsLocalhost(host) {
			hosts[host] = struct{}{}
		}
	}

	if addr := os.Getenv("CONTROLLER_HTTP_ADDR"); addr != "" {
		if host, _, err := net.SplitHostPort(addr); err == nil && host != "" && !netutil.IsLocalhost(host) {
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

// parseBreakGlassEmails parses IDP_BREAK_GLASS_EMAILS (comma-separated) into a
// lowercased set of workspace admins who may always authenticate via the
// platform IdP, so a workspace can never lock itself out (ADR-024 §5). Consulted
// by the no-lockout guard. Empty/unset → no break-glass admins.
func parseBreakGlassEmails(raw string) map[string]bool {
	out := map[string]bool{}
	for _, e := range strings.Split(raw, ",") {
		if email := strings.ToLower(strings.TrimSpace(e)); email != "" {
			out[email] = true
		}
	}
	return out
}

// seedProviderUsers upserts each email in PROVIDER_BOOTSTRAP_EMAILS as an active
// super-admin. This is the ONLY way the first provider user comes into existence
// — there is no self-registration. Idempotent: safe to run on every startup.
func seedProviderUsers(ctx context.Context, store *provider.Store) {
	raw := strings.TrimSpace(os.Getenv("PROVIDER_BOOTSTRAP_EMAILS"))
	if raw == "" {
		log.Printf("PROVIDER_BOOTSTRAP_EMAILS unset — no provider super-admins seeded")
		return
	}
	for _, e := range strings.Split(raw, ",") {
		email := strings.TrimSpace(e)
		if email == "" {
			continue
		}
		if err := store.UpsertSuperAdmin(ctx, email); err != nil {
			log.Printf("seed provider super-admin %q: %v", email, err)
			continue
		}
		log.Printf("seeded provider super-admin: %s", email)
	}
}

func envOrInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}

	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf(
			"invalid value for %s=%q, using default %d",
			key,
			v,
			fallback,
		)
		return fallback
	}

	return n
}
