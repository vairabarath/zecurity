Zecurity — Codebase Improvement Analysis
Authored: 2026-04-18 | Sprint 4 baseline

Priority legend: 🔴 Critical  🟠 High  🟡 Medium  🟢 Low

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
1. SECURITY ISSUES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔴 1.1 — BurnEnrollmentJTI is NOT truly atomic
  Where: controller/internal/connector/token.go, shield/token.go
  Problem: Redis GET + DEL in a pipeline is two commands, not atomic.
           A race window exists where two simultaneous Enroll requests
           both GET the JTI before either DEL fires — both enrollments succeed.
  Fix: Replace with Redis GETDEL command (Redis 6.2+) or a Lua script:
       local v = redis.call('GET', KEYS[1])
       if v then redis.call('DEL', KEYS[1]) end
       return v

🔴 1.2 — No certificate revocation mechanism
  Where: controller/internal/pki/, connector/src/tls.rs, shield/src/tls.rs
  Problem: If a Connector or Shield cert is compromised, there is no OCSP/CRL.
           The only mitigation is revoking in DB and waiting 7 days for expiry.
           A revoked connector can still establish mTLS and send heartbeats.
  Fix: In UnarySPIFFEInterceptor (controller), after SPIFFE verification,
       query DB: SELECT status FROM connectors WHERE cert_serial=$1.
       If status='revoked' → return codes.PermissionDenied.
       This turns DB status into real-time cert gating.

🟠 1.3 — First /ca.crt fetch is unauthenticated plain HTTP
  Where: connector/src/enrollment.rs, shield/src/enrollment.rs (step 1)
  Problem: The very first request fetches the CA cert over plain HTTP before
           any TLS verification is possible. A MITM could serve a rogue CA.
           The SHA-256 fingerprint check (step 2) mitigates this ONLY if the
           fingerprint in the JWT is trusted — which requires trusting the JWT
           delivery channel.
  Fix: Include the CA cert PEM directly in the enrollment JWT as a claim,
       removing the HTTP fetch entirely. Or enforce HTTPS with system root CAs
       for the /ca.crt endpoint.

🟠 1.4 — Shield nftables rules accumulate on restart
  Where: shield/src/network.rs
  Problem: Every Shield restart calls setup() which adds nftables rules without
           first flushing the existing table. After 3 restarts there are 3x the
           rules. Also, rtnetlink fails if zecurity0 already exists.
  Fix:
    a) At top of setup(): try to delete table inet zecurity (ignore if missing)
    b) Check if zecurity0 exists before creating: if exists, just re-configure
    c) Use `flush table inet zecurity` before adding rules

🟠 1.5 — Shield does not verify the Shield ID in mTLS cert from Connector
  Where: connector/src/agent_server.rs
  Problem: When a Shield heartbeats to agent_server.rs, the Connector verifies
           the SPIFFE URI format but does NOT check that the shield_id in the cert
           matches the shield_id in the HeartbeatRequest body.
           Any enrolled Shield could report health on behalf of another.
  Fix: In agent_server.rs::Heartbeat(), extract SPIFFE shield_id from peer cert,
       assert it equals req.shield_id. Reject with PERMISSION_DENIED if mismatch.

🟠 1.6 — Updater trusts GitHub checksums.txt without signature verification
  Where: connector/src/updater.rs, shield/src/updater.rs
  Problem: SHA-256 from checksums.txt stops accidental corruption but NOT a
           supply-chain attack. If GitHub releases or the release pipeline is
           compromised, attacker replaces binary + checksums.txt together.
  Fix: Sign releases with a GPG/sigstore key baked into the binary at build time.
       Verify signature before checksum check.

🟡 1.7 — Secrets still in .env files
  Where: controller/.env (JWT_SECRET, PKI_MASTER_SECRET, GOOGLE_CLIENT_SECRET)
  Problem: .env files are a common leakage vector (git commits, container images,
           log dumps). PKI_MASTER_SECRET loss = all CA keys become undecryptable.
  Fix: Move to a secrets manager (HashiCorp Vault, AWS Secrets Manager, or
       Doppler). At minimum add .env to .gitignore and document this explicitly.

🟡 1.8 — CORS wildcard risk
  Where: controller/cmd/server/main.go (ALLOWED_ORIGIN env var)
  Problem: If ALLOWED_ORIGIN is set to "*" in any environment, XSS on any
           site can make authenticated GraphQL calls.
  Fix: Validate ALLOWED_ORIGIN at startup — reject "*" and require exact origin.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
2. CORRECTNESS / BUGS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔴 2.1 — alive_shields map leaks stale entries on Connector restart
  Where: connector/src/agent_server.rs (alive_shields HashMap)
  Problem: The map is in-memory only. If Connector restarts:
           - All shield health is lost
           - Controller's next heartbeat receives shields=[] 
           - Controller's disconnect watcher marks all those shields DISCONNECTED
             within 120s, even if the Shields are healthy
  Fix: Persist alive_shields to state_dir/shields.json on each update.
       On startup, load it back. This survives Connector restarts gracefully.

🔴 2.2 — assignInterfaceAddr() linear scan is O(n) and error-prone
  Where: controller/internal/shield/token.go
  Problem: Iterates 100.64.0.0/10 (/32 per Shield = 4M addresses) linearly,
           checking DB for each. At scale this is very slow and a potential
           DoS vector (create many shields to force slow scan).
  Fix: Track allocated addresses in a separate table (shield_ip_pool) with
       an index on (tenant_id, interface_addr). Use a SELECT ... FOR UPDATE
       to atomically claim the next free address in one query.

🟠 2.3 — selectConnector() has no fallback if no Connectors are active
  Where: controller/internal/shield/token.go
  Problem: If remoteNetworkId has no active Connectors, selectConnector()
           returns no rows → token generation fails with a DB error, not a
           clean user-facing message.
  Fix: Check for empty result explicitly and return a GraphQL error:
       "No active connector available in this network. Deploy a connector first."

🟠 2.4 — RunDisconnectWatcher goroutine panics silently
  Where: controller/internal/connector/heartbeat.go,
         controller/internal/shield/heartbeat.go
  Problem: If the goroutine panics (e.g. DB connection lost), it dies with
           no restart. Connectors and Shields will never be marked disconnected
           until Controller restarts.
  Fix: Wrap loop body in recover(). On panic, log the error and sleep before
       retrying. Or use a supervised goroutine pattern.

🟠 2.5 — Shield network setup not idempotent on reboot
  Where: shield/src/network.rs
  Problem: On Linux, TUN interfaces are ephemeral — they vanish on reboot.
           After reboot, Shield starts, finds state.json (enrolled), skips
           enrollment, jumps to heartbeat — but zecurity0 doesn't exist.
           Heartbeat loop works but network isolation is broken.
  Fix: In main.rs, after loading existing state, ALWAYS call
       network::setup(state.interface_addr, state.connector_addr) before
       spawning heartbeat. setup() must be idempotent (see 1.4).

🟡 2.6 — GraphQL N+1 query problem
  Where: controller/graph/resolvers/connector.resolvers.go,
         controller/graph/resolvers/shield.resolvers.go
  Problem: GetRemoteNetworks returns networks, each with nested connectors
           and shields. If there are 20 networks, that's 1 + 20 + 20 = 41
           DB queries for one request.
  Fix: Use a DataLoader pattern (gqlgen supports this via dataloaden).
       Batch: SELECT * FROM connectors WHERE remote_network_id = ANY($1)
       and group by network_id in-process.

🟡 2.7 — Controller TLS cert is ephemeral, breaks Connectors on restart
  Where: controller/cmd/server/main.go → pki.GenerateControllerServerTLS()
  Problem: A new TLS cert is generated each startup. Connectors verify
           Controller's SPIFFE identity (not pin the cert), so mTLS itself is
           fine. But if Connector has a cached TCP connection from before the
           restart, it gets a TLS error on the next heartbeat.
  Note: This is minor since backoff handles reconnect, but the first heartbeat
        after Controller restart always fails. Fix: persist Controller TLS cert
        to DB with a reasonable TTL (e.g. 30 days).

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
3. PERFORMANCE
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟠 3.1 — Missing DB indices on hot query columns
  Where: controller/migrations/ (all 3 files)
  Problem: RunDisconnectWatcher runs every 60s querying:
             WHERE status='active' AND last_heartbeat_at < NOW() - interval
           Without an index on (status, last_heartbeat_at), this is a full
           table scan at every tick.
  Fix: Add to migrations:
       CREATE INDEX idx_connectors_status_heartbeat ON connectors(status, last_heartbeat_at);
       CREATE INDEX idx_shields_status_heartbeat    ON shields(status, last_heartbeat_at);
       CREATE INDEX idx_shields_tenant_network      ON shields(tenant_id, remote_network_id);
       CREATE INDEX idx_connectors_tenant_network   ON connectors(tenant_id, remote_network_id);

🟠 3.2 — Admin UI uses polling instead of subscriptions
  Where: admin/src/pages/Shields.tsx, Connectors.tsx, RemoteNetworks.tsx
  Problem: Every page polls every 30 seconds. With 5 admins + 10 pages open,
           that's 150+ GraphQL requests/minute hitting the Controller for
           nothing when there's no change.
  Fix: Replace Apollo polling with GraphQL subscriptions (gqlgen supports
       subscriptions over WebSocket). Connector/Shield status changes push
       to UI instead of pull. Keep polling as fallback.

🟡 3.3 — pgxpool uses default settings
  Where: controller/internal/db/db.go
  Problem: No MaxConns, MinConns, MaxConnIdleTime configured. Default pool
           size is 4 connections — too low for production with multiple
           goroutines (heartbeat handler + disconnect watchers + GraphQL).
  Fix: Set pool config based on environment:
       pgxpool.Config{ MaxConns: 20, MinConns: 5, MaxConnIdleTime: 5*time.Minute }

🟡 3.4 — No caching for NetworkHealth computation
  Where: controller/graph/resolvers/connector.resolvers.go
  Problem: NetworkHealth is recomputed per GetRemoteNetworks request by
           iterating connector list. With many networks and frequent polling,
           this is redundant work.
  Fix: Cache NetworkHealth per remote_network_id in Redis with a 5s TTL.
       Invalidate on connector status change.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
4. RELIABILITY / OPERATIONAL
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟠 4.1 — No observability (metrics + tracing)
  Where: Controller (Go), Connector (Rust), Shield (Rust)
  Problem: No Prometheus metrics, no OpenTelemetry tracing. Zero visibility
           into enrollment latency, heartbeat failure rates, cert renewal
           errors, or DB query performance.
  Fix:
    Controller: Add prometheus/client_golang metrics:
      - zecurity_enrollments_total{component, status}
      - zecurity_heartbeat_latency_seconds{component}
      - zecurity_active_agents{type}
      - zecurity_cert_renewals_total{component, status}
    Connector/Shield: Use metrics-rs or opentelemetry-rust.
    Expose /metrics endpoint on Controller (HTTP, scrape-able by Prometheus).

🟠 4.2 — No graceful HTTP server shutdown
  Where: controller/cmd/server/main.go
  Problem: SIGTERM kills the process immediately. In-flight GraphQL requests
           and ongoing gRPC enrollments are dropped mid-transaction.
  Fix:
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
    defer stop()
    // on context done:
    grpcServer.GracefulStop()
    httpServer.Shutdown(ctx with 30s timeout)

🟠 4.3 — No input validation on GraphQL mutations
  Where: controller/graph/resolvers/connector.resolvers.go,
         controller/graph/resolvers/shield.resolvers.go
  Problem: createRemoteNetwork(name, location) and generateShieldToken(name)
           accept arbitrary strings. No length limits, no character allowlists,
           no SQL injection guards beyond parameterized queries.
  Fix: Add validation layer in resolvers:
       - name: max 64 chars, alphanumeric + hyphens only
       - location: max 64 chars
       - Return GraphQL validation errors, not gRPC/DB errors

🟠 4.4 — No resource limits (Connector/Shield count per workspace)
  Where: controller/internal/connector/token.go, shield/token.go
  Problem: One workspace can generate unlimited Connectors and Shields.
           Each one creates a DB row, Redis JTI, and eventually a mTLS
           connection to the Controller.
  Fix: Add configurable limits (env vars):
       MAX_CONNECTORS_PER_NETWORK (default 10)
       MAX_SHIELDS_PER_NETWORK (default 100)
       Check count before INSERT; return 429-equivalent GraphQL error.

🟡 4.5 — Migration runner not wired into startup
  Where: controller/cmd/server/main.go, controller/migrations/
  Problem: There is no evidence of automatic migration running. Migrations
           must be applied manually. If they're skipped, the app starts
           against a stale schema and fails at runtime.
  Fix: Integrate golang-migrate or atlas into main.go startup:
       if err := runMigrations(databaseURL); err != nil { log.Fatal(err) }
       This makes schema and app version always in sync.

🟡 4.6 — No audit log
  Where: controller/internal/connector/, shield/, auth/
  Problem: No record of: who created a connector, who revoked a shield,
           which admin initiated an enrollment. Compliance requirement for
           ZTNA platforms.
  Fix: Add audit_logs table:
       (id, tenant_id, actor_id, action, resource_type, resource_id, ip_addr, created_at)
       Write audit entries in every resolver mutation.

🟡 4.7 — Connector :9091 server has no TLS configuration documented
  Where: connector/src/agent_server.rs::serve()
  Problem: The Shield-facing gRPC server listens on :9091. It needs to serve
           a TLS cert for Shields to verify. It's unclear if it uses the
           Connector leaf cert or a separate cert. If it uses the leaf cert,
           what happens after renewal? Does the :9091 server need restart?
  Fix: Document the cert used by :9091. Ensure that after renewal::renew_cert()
       completes, the agent_server TLS config is hot-reloaded without restart.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
5. CODE QUALITY
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟡 5.1 — UnarySPIFFEInterceptor not applied to Enroll RPC
  Where: controller/cmd/server/main.go (gRPC interceptor registration)
  Problem: Enroll uses plain TLS (no client cert), so SPIFFE verification is
           correctly skipped for it. But the interceptor likely runs on ALL
           RPCs including Enroll, causing it to fail if it tries to read the
           client cert. Needs explicit allowlisting.
  Fix: In the interceptor, check method name:
       if method == "/connector.v1.ConnectorService/Enroll" → skip SPIFFE check

🟡 5.2 — helpers.go is a catch-all file
  Where: controller/graph/resolvers/helpers.go
  Problem: helpers.go grows into a dumping ground for utility functions.
           Functions that belong to specific resolvers end up here.
  Fix: Move resolver-specific helpers next to the resolver file.
       Keep helpers.go only for truly shared, domain-agnostic utilities.

🟡 5.3 — Rust crates have duplicated crypto.rs and tls.rs
  Where: connector/src/crypto.rs + shield/src/crypto.rs (identical)
         connector/src/tls.rs + shield/src/tls.rs (near-identical)
  Problem: Same code in two places. A bug fix in one is forgotten in the other.
  Fix: Extract a shared `zecurity-common` crate in a workspace Cargo.toml:
       [workspace]
       members = ["connector", "shield", "common"]
       Both crates depend on `common` for crypto, tls, appmeta, util.

🟡 5.4 — No integration tests
  Where: controller/**, connector/**, shield/**
  Problem: Unit tests exist but no integration test verifies the full
           enrollment flow against a real DB + Redis. The disconnect watcher
           timing has never been tested end-to-end.
  Fix: Add a Docker Compose test environment:
       - Real Postgres + Redis + Controller
       - Rust integration tests using tonic test client
       - Test cases: full enrollment, heartbeat, re_enroll, goodbye, watcher

🟡 5.5 — Generated GraphQL code not validated in CI
  Where: admin/src/generated/graphql.ts, admin/codegen.yml
  Problem: If someone changes the Controller GraphQL schema without running
           codegen, the generated TypeScript types go stale. TypeScript
           compilation still passes (old generated code), but runtime breaks.
  Fix: Add codegen step to CI that runs codegen and fails if git diff is non-empty:
       npm run codegen && git diff --exit-code src/generated/

🟢 5.6 — No health endpoint on Connector or Shield
  Where: connector/src/main.rs, shield/src/main.rs
  Problem: Systemd knows the process is running but can't tell if it's healthy
           (e.g. stuck in backoff, can't reach Controller).
  Fix: Add a lightweight HTTP health endpoint (e.g. /health on :9099) that
       returns 200 if last heartbeat was within 2x interval, 503 otherwise.
       Reference in systemd unit: ExecStartPost=/usr/bin/curl --retry 5 /health

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
6. ADMIN UI
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟠 6.1 — Enrollment token shown in plain text, no expiry warning
  Where: admin/src/components/InstallCommandModal.tsx
  Problem: The JWT is embedded in the install command and shown in the UI.
           If the modal is left open or screenshot is taken, the token is
           exposed. The 24h TTL is not shown to the user.
  Fix:
    a) Show remaining TTL: "This token expires in 23h 58m"
    b) Add "Copied!" flash — don't persist the token visible longer than needed
    c) Consider a "Reveal" toggle (hidden by default)

🟠 6.2 — No React error boundaries
  Where: admin/src/App.tsx, all page components
  Problem: A single runtime error in any component crashes the entire app.
           Users see a blank white screen with no recovery path.
  Fix: Wrap each page route in an ErrorBoundary component.
       Show: "Something went wrong. [Refresh page]" fallback UI.

🟡 6.3 — No optimistic updates on mutations
  Where: Shields.tsx, Connectors.tsx (RevokeShield, RevokeConnector)
  Problem: After clicking "Revoke", the UI waits for the mutation + refetch
           before showing the updated status. Feels slow.
  Fix: Use Apollo optimistic updates:
       optimisticResponse: { revokeShield: { id, status: 'revoked', __typename: 'Shield' } }

🟡 6.4 — Token stored in Zustand memory is lost on page refresh
  Where: admin/src/store/auth.ts
  Problem: accessToken lives only in React memory. Hard refresh → user is
           logged out even if refresh cookie is valid.
  Fix: On app mount (before rendering protected routes), attempt a silent
       POST /auth/refresh. If it succeeds, set the token. Only then render
       protected routes. Currently useRequireAuth() does this but there's a
       flash of redirect before refresh completes.
  Better fix: Use React.Suspense to hold rendering until refresh resolves.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
7. MISSING RATE LIMITING & DOS PROTECTION
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔴 7.1 — No rate limiting on GraphQL or gRPC enrollment endpoints
  Where: controller/cmd/server/main.go
  Problem: POST /graphql and gRPC Enroll have no rate limiting.
           An attacker can flood the Enroll RPC (even with invalid tokens)
           to exhaust DB connections and Redis. The enrollment flow does
           a DB read + Redis GET+DEL per request — trivial to DoS.
  Fix:
    HTTP: golang.org/x/time/rate middleware, keyed by IP.
    gRPC: token-bucket interceptor before UnarySPIFFEInterceptor.
    Limits: 10 enrollments/min per IP, 100 GraphQL req/min per JWT.

🟠 7.2 — No request timeout middleware on HTTP or gRPC
  Where: controller/cmd/server/main.go
  Problem: A slow DB query (e.g. during assignInterfaceAddr linear scan)
           can hold an HTTP goroutine indefinitely. Under load, all
           goroutines hang and the server stops responding.
  Fix:
    HTTP: context timeout middleware (e.g. 10s per request).
    gRPC: grpc.UnaryInterceptor wrapping context.WithTimeout(ctx, 10s).

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
8. SCALABILITY / MISSING PAGINATION
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟠 8.1 — No pagination on any GraphQL query
  Where: controller/graph/resolvers/connector.resolvers.go,
         controller/graph/resolvers/shield.resolvers.go
  Problem: GetShields, GetConnectors, GetRemoteNetworks return ALL rows.
           A workspace with 500 shields returns them all in one request,
           loading them all into memory + sending over the wire.
  Fix: Add cursor-based pagination to all list queries:
       shields(remoteNetworkId, first: Int, after: String): ShieldConnection
       Use LIMIT + cursor (last seen ID) in SQL.

🟠 8.2 — No pagination on Admin UI tables
  Where: admin/src/pages/Shields.tsx, Connectors.tsx
  Problem: All records rendered into DOM at once. 200 shields = 200 DOM
           rows, each with status badges and relative timestamps. Causes
           jank and memory pressure in the browser.
  Fix: Add virtual scrolling (react-virtual) or server-side pagination
       tied to 8.1 above. Also add search/filter by name and status.

🟡 8.3 — Controller is a single point of failure
  Where: Architecture level
  Problem: One Controller process. If it crashes or is deployed, all
           Connectors lose heartbeat and start hitting disconnect threshold
           within 90s. All Shields follow within 120s.
  Fix (long term): Stateless Controller design — DB + Redis are already
       external. Multiple Controller replicas behind a load balancer work
       immediately since each request is independent. Add a health check
       endpoint for LB probing.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
9. AUTH & SESSION SECURITY
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔴 9.1 — Refresh token not rotated on use
  Where: controller/internal/auth/refresh.go
  Problem: If the httpOnly refresh cookie is stolen (XSS, log exposure),
           the attacker has permanent session access. Refresh tokens must
           be rotated: each /auth/refresh issues a new refresh token and
           invalidates the old one.
  Fix: Store refresh token JTI in Redis. On /auth/refresh:
       1. Verify token + burn JTI (GETDEL)
       2. Issue new access token + new refresh token with new JTI
       3. Set new httpOnly cookie
       Stolen old cookie becomes invalid after first legitimate use.

🟠 9.2 — No workspace slug validation (trust domain injection)
  Where: controller/internal/bootstrap/ (workspace creation)
  Problem: workspace slug is used directly in trust domain:
           WorkspaceTrustDomain(slug) → "ws-{slug}.zecurity.in"
           If slug contains dots or special chars (e.g. "evil.corp"),
           trust domain becomes "ws-evil.corp.zecurity.in" which could
           confuse SPIFFE parsers or overlap with another workspace.
  Fix: Validate slug at creation: ^[a-z0-9][a-z0-9-]{2,62}[a-z0-9]$
       Reject anything with dots, underscores, or leading hyphens.

🟠 9.3 — Google OAuth state parameter CSRF not verified
  Where: controller/internal/auth/exchange.go (OAuth callback)
  Problem: OAuth state parameter (anti-CSRF) must be tied to the initiating
           session. If the callback handler doesn't verify state matches what
           was stored in the user's session cookie, CSRF login attacks are
           possible.
  Fix: On InitiateAuth: generate state = random 32 bytes, store in signed
       cookie. On /auth/callback: verify state param matches cookie, then
       delete cookie.

🟡 9.4 — No server-side session invalidation on logout
  Where: controller/internal/auth/
  Problem: JWTs are stateless — logging out in the Admin UI just clears
           the in-memory token. The JWT remains valid until expiry. If it
           was copied (browser history, logs), it still works.
  Fix: Store JWT JTI in Redis on issue. On logout: add JTI to a Redis
       blocklist (SET jti:blocked:{jti} 1 EX {remaining_ttl}).
       authMiddleware checks blocklist on every request.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
10. RELIABILITY GAPS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🔴 10.1 — No DB connection retry at Controller startup
  Where: controller/internal/db/db.go → db.Init()
  Problem: If PostgreSQL is not ready (container startup race, transient
           network blip), pgxpool.Connect() fails and main() calls
           log.Fatal() → Controller exits. Kubernetes restarts it but
           this creates flapping.
  Fix: Retry with backoff on startup:
       for attempt := 0; attempt < 10; attempt++ {
         pool, err = pgxpool.Connect(ctx, dsn)
         if err == nil { break }
         time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
       }

🟠 10.2 — Shield has no Connector failover
  Where: shield/src/control_stream.rs (formerly heartbeat.rs), controller/internal/shield/token.go
  Problem: At enrollment time, Shield is assigned ONE Connector. If that
           Connector is permanently decommissioned or crashes without
           recovery, the Shield is stuck — it keeps trying to heartbeat
           to a dead address and eventually goes DISCONNECTED with no
           way to recover without re-enrollment.
  Fix (short term): On persistent heartbeat failure (N retries exceeded),
       Shield could re-read state.json and call Controller directly to
       request a new connector_addr assignment.
  Fix (long term): Shield enrollment response includes a list of available
       Connectors in the network. Shield tries them in order.

🟠 10.3 — Connector :9091 TLS not hot-reloaded after cert renewal
  Where: connector/src/agent_server.rs, connector/src/renewal.rs
  Problem: The Shield-facing gRPC server on :9091 uses the Connector's
           TLS cert. After renewal::renew_cert() writes a new connector.crt,
           the running tonic server still uses the old cert in memory.
           Shields connecting after renewal see the old cert and may reject
           it once it expires.
  Fix: After renewal, signal the ShieldServer to reload its TLS config.
       Tonic supports TcpIncoming with dynamic cert loading via
       tokio_rustls::TlsAcceptor with an Arc<RwLock<ServerConfig>>.

🟡 10.4 — No backup/recovery strategy for workspace_ca_keys
  Where: controller/internal/pki/, controller/migrations/001_schema.sql
  Problem: workspace_ca_keys stores the AES-GCM encrypted Workspace CA
           private key. If this table is lost (DB corruption, accidental
           DELETE), the workspace CA is gone. No new certs can be signed.
           All existing certs continue working until they expire (7 days),
           then the workspace is permanently broken.
  Fix: Export encrypted CA key backups to object storage (S3/GCS) on
       creation and on any update. Key is already encrypted with
       PKI_MASTER_SECRET so it's safe to store externally.

🟡 10.5 — No local development Docker Compose
  Where: repo root
  Problem: No docker-compose.yml for spinning up Postgres + Redis +
           Controller locally. Each developer manually manages their own
           environment, leading to "works on my machine" issues.
  Fix: Add docker-compose.yml with:
       services: postgres, redis, controller (with .env.example)
       Include wait-for-it or health checks so Controller waits for DB.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
11. NETWORK / SHIELD COMPLETENESS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

🟠 11.1 — nftables rules don't restrict outbound traffic from Shield
  Where: shield/src/network.rs
  Problem: The current nftables base table only has an input chain (DROP
           by default on zecurity0 input). There is no output chain.
           The protected resource can still make outbound connections to
           arbitrary destinations via zecurity0. This breaks zero-trust
           egress policy.
  Fix: Add output chain to the nftables table:
       chain output { type filter hook output priority 0; policy drop;
         oif lo accept;
         ip daddr {connector_ip} accept;
         drop;
       }

🟡 11.2 — No routing table entry for zecurity0 traffic
  Where: shield/src/network.rs
  Problem: Creating the TUN interface and assigning a /32 IP is necessary
           but not sufficient for traffic to actually route through it.
           Without an ip route entry pointing traffic at zecurity0, the
           interface exists but handles no traffic.
  Note: This is likely Sprint 5 work (data plane), but the current
        network.rs setup is incomplete as a standalone unit.
  Fix: Add route: ip route add 100.64.0.0/10 dev zecurity0

🟡 11.3 — No IPv6 support in the interface address pool
  Where: controller/internal/shield/token.go, shield/src/network.rs
  Problem: 100.64.0.0/10 is IPv4 CGNAT only. Modern Linux environments
           increasingly run dual-stack. If the host has only IPv6
           connectivity to the Connector, heartbeats over IPv4 will fail.
  Fix: Design decision needed — either add an IPv6 ULA pool (fc00::/7)
       or document IPv4-only as a known limitation.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
PRIORITY SUMMARY
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

Fix before any production deployment:
  1.1  Atomic JTI burn (Redis GETDEL)
  1.2  Cert revocation check in gRPC interceptor
  2.1  Persist alive_shields map to disk
  2.3  Graceful error if no active Connector for Shield token
  2.5  Network setup on every Shield start (idempotent)
  4.2  Graceful HTTP/gRPC shutdown on SIGTERM
  7.1  Rate limiting on GraphQL + gRPC Enroll
  9.1  Refresh token rotation on every use
  9.2  Workspace slug validation (trust domain injection)
  10.1 DB connection retry at Controller startup

Fix in next sprint:
  1.4  Idempotent nftables rules (flush before apply)
  1.5  Verify shield_id matches mTLS cert in agent_server
  2.2  Replace linear IP scan with pooled table
  3.1  Add DB indices on (status, last_heartbeat_at)
  4.1  Add Prometheus metrics
  4.3  GraphQL input validation
  5.3  Extract zecurity-common Rust crate
  7.2  Request timeout middleware
  9.3  OAuth state CSRF verification
  10.3 Hot-reload TLS on Connector :9091 after cert renewal
  11.1 nftables output chain (restrict Shield egress)

Nice to have (Sprint 6+):
  3.2  WebSocket subscriptions in Admin UI
  4.4  Connector/Shield count limits per workspace
  4.5  Auto-run migrations at startup
  4.6  Audit log table
  5.4  Integration test suite
  6.1  Token expiry warning in InstallCommandModal
  8.1  Cursor-based pagination on all GraphQL list queries
  9.4  JWT JTI blocklist for logout
  10.2 Shield Connector failover
  10.4 Workspace CA key backup to object storage
  10.5 Docker Compose for local development
  11.2 Route table entry for zecurity0 traffic (Sprint 5 data plane)
